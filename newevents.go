package discordsignup

import (
	"errors"
	"fmt"
	"log"
	"net/http"
)

// #new-events holds one message for every event that has not gone to past
// events: posted when the event is first published, edited in place whenever
// it changes, and deleted when its line goes to past events or it is
// cancelled, so an event is in exactly one of the two channels.
//
// Each message is the event's #events text and nothing else — plain content,
// no container and no buttons — so that forwarding it to somebody carries the
// whole of it. Buttons do not work from a forwarded copy.

// newEventsMessageBelongs reports whether an event should have a message in
// #new-events: it has not finished and has not been cancelled.
func newEventsMessageBelongs(ev *Event) bool {
	return ev.Status != StatusCompleted && ev.Status != StatusCancelled
}

// newEventsMessageMissing reports whether an event that should have a message
// in its guild's #new-events has none — never posted, or a post that failed.
// The publish signature cannot see this, since a guild gaining the channel
// changes nothing about the event.
func (s *Server) newEventsMessageMissing(ev *Event) bool {
	return newEventsMessageBelongs(ev) && ev.NewEventsMessageID == "" &&
		s.guildChannels(ev.GuildID).NewEvents != ""
}

// refreshNewEventsMessage brings the event's #new-events message up to date: posts
// it if there is none, edits it if there is, and deletes it if the event no
// longer belongs there. A message somebody deleted by hand is posted again.
func (s *Server) refreshNewEventsMessage(ev *Event, roster []Signup) error {
	if s.discord == nil {
		return nil
	}
	if !newEventsMessageBelongs(ev) {
		return s.removeNewEventsMessage(ev)
	}
	channelID := s.guildChannels(ev.GuildID).NewEvents
	if channelID == "" {
		return nil
	}
	payload := map[string]any{
		"content":          eventTableText(ev, roster, newEventsMessageLimit, newEventsMessageLimit),
		"allowed_mentions": map[string]any{"parse": []string{}},
	}
	if ev.NewEventsMessageID != "" {
		err := s.discord.EditMessage(channelID, ev.NewEventsMessageID, payload)
		if err == nil {
			return nil
		}
		if !isDiscordNotFound(err) {
			return fmt.Errorf("edit new-events message: %w", err)
		}
		log.Printf("[discord-signup] event %d's new-events message %s is gone; posting it again",
			ev.ID, ev.NewEventsMessageID)
	}
	messageID, err := s.discord.CreateMessage(channelID, payload)
	if err != nil {
		return fmt.Errorf("post new-events message: %w", err)
	}
	return s.store.SetNewEventsMessageID(ev.ID, messageID)
}

// newEventsMessageLimit is Discord's cap on a plain message's content.
const newEventsMessageLimit = 2000

// removeNewEventsMessage deletes the event's #new-events message, if it has one.
// A message already gone counts as deleted.
func (s *Server) removeNewEventsMessage(ev *Event) error {
	if s.discord == nil || ev.NewEventsMessageID == "" {
		return nil
	}
	channelID := s.guildChannels(ev.GuildID).NewEvents
	if channelID != "" {
		if err := s.discord.DeleteMessage(channelID, ev.NewEventsMessageID); err != nil && !isDiscordNotFound(err) {
			return fmt.Errorf("delete new-events message: %w", err)
		}
	}
	return s.store.SetNewEventsMessageID(ev.ID, "")
}

// isDiscordNotFound reports a 404: the message or channel no longer exists.
func isDiscordNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}
