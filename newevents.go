package discordsignup

import (
	"errors"
	"fmt"
	"log"
	"net/http"
)

// #new-events holds one line for every event that has not gone to past
// events: posted when the event is first published, edited in place whenever
// it changes, and deleted when its line goes to past events or it is
// cancelled. The same folded line past events uses, so an event reads the
// same in both channels and is in exactly one of them.

// newEventsLineBelongs reports whether an event should have a line in
// #new-events: it has not finished and has not been cancelled.
func newEventsLineBelongs(ev *Event) bool {
	return ev.Status != StatusCompleted && ev.Status != StatusCancelled
}

// newEventsLineMissing reports whether an event that should have a line in
// its guild's #new-events has none — never posted, or a post that failed.
// The publish signature cannot see this, since a guild gaining the channel
// changes nothing about the event.
func (s *Server) newEventsLineMissing(ev *Event) bool {
	return newEventsLineBelongs(ev) && ev.NewEventsMessageID == "" &&
		s.guildChannels(ev.GuildID).NewEvents != ""
}

// refreshNewEventsLine brings the event's #new-events line up to date: posts
// it if there is none, edits it if there is, and deletes it if the event no
// longer belongs there. A line somebody deleted by hand is posted again.
func (s *Server) refreshNewEventsLine(ev *Event, roster []Signup) error {
	if s.discord == nil {
		return nil
	}
	if !newEventsLineBelongs(ev) {
		return s.removeNewEventsLine(ev)
	}
	channelID := s.guildChannels(ev.GuildID).NewEvents
	if channelID == "" {
		return nil
	}
	payload := map[string]any{
		"content":          foldedEventLine(ev, roster),
		"allowed_mentions": map[string]any{"parse": []string{}},
	}
	if ev.NewEventsMessageID != "" {
		err := s.discord.EditMessage(channelID, ev.NewEventsMessageID, payload)
		if err == nil {
			return nil
		}
		if !isDiscordNotFound(err) {
			return fmt.Errorf("edit new-events line: %w", err)
		}
		log.Printf("[discord-signup] event %d's new-events line %s is gone; posting it again",
			ev.ID, ev.NewEventsMessageID)
	}
	messageID, err := s.discord.CreateMessage(channelID, payload)
	if err != nil {
		return fmt.Errorf("post new-events line: %w", err)
	}
	return s.store.SetNewEventsMessageID(ev.ID, messageID)
}

// removeNewEventsLine deletes the event's #new-events line, if it has one. A
// line already gone counts as deleted.
func (s *Server) removeNewEventsLine(ev *Event) error {
	if s.discord == nil || ev.NewEventsMessageID == "" {
		return nil
	}
	channelID := s.guildChannels(ev.GuildID).NewEvents
	if channelID != "" {
		if err := s.discord.DeleteMessage(channelID, ev.NewEventsMessageID); err != nil && !isDiscordNotFound(err) {
			return fmt.Errorf("delete new-events line: %w", err)
		}
	}
	return s.store.SetNewEventsMessageID(ev.ID, "")
}

// isDiscordNotFound reports a 404: the message or channel no longer exists.
func isDiscordNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}
