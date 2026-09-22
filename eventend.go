package discordsignup

import (
	"errors"
	"fmt"
	"log"
	"net/http"
)

// Ending an event by hand: the End button on the management table and on the
// web page.
//
// Without it an event ended in one of two ways, and neither was a person on
// this service saying so: its end time passing, or somebody pressing End on
// the native Discord event and the ten-minute sync noticing. Both still work;
// this is a third way in, and it lands on the same finishing steps.

// eventIsUnderway reports whether an event has started and is not yet over —
// the only time End is offered. Before the start, ending would be cancelling
// under another name, and Cancel already exists for that.
func eventIsUnderway(ev *Event) bool {
	return !IsArchived(ev.Status) && ev.StartsAt > 0 && ev.StartsAt <= now()
}

// EndCustomID is the management row's End button; it asks for a confirm.
func EndCustomID(eventID int64) string {
	return fmt.Sprintf("%s:end:%d", customIDPrefix, eventID)
}

// EndConfirmCustomID is the button on that private confirm.
func EndConfirmCustomID(eventID int64) string {
	return fmt.Sprintf("%s:end-confirm:%d", customIDPrefix, eventID)
}

// CompleteLiveEvent marks one open or closed event completed, and reports
// whether this call did it. The status check is in the UPDATE itself, so an
// End pressed while the time sweep completes the same event finishes it once:
// whoever loses the race sees changed=false and posts nothing.
func (s *Store) CompleteLiveEvent(id int64) (changed bool, err error) {
	result, err := s.db.Exec(
		`UPDATE events SET status = ?, updated_at = ? WHERE id = ? AND deleted_at = 0 AND status IN (?, ?)`,
		StatusCompleted, now(), id, StatusOpen, StatusClosed)
	if err != nil {
		return false, fmt.Errorf("complete event %d: %w", id, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("complete event %d: %w", id, err)
	}
	return rows == 1, nil
}

// EndScheduledEvent moves a native event to COMPLETED.
//
// Discord only completes an ACTIVE event, so one it never started is started
// first. One already completed, cancelled or deleted is left alone: it is
// already over, which is all this was asked to make true.
func (c *DiscordClient) EndScheduledEvent(guildID, eventID string) error {
	native, exists, err := c.GetScheduledEvent(guildID, eventID)
	if err != nil {
		return err
	}
	if !exists || native.Status == discordEventCompleted || native.Status == discordEventCanceled {
		return nil
	}
	if native.Status == discordEventScheduled {
		if err := c.ModifyScheduledEvent(guildID, eventID, map[string]any{"status": discordEventActive}); err != nil {
			return fmt.Errorf("start it before ending it: %w", err)
		}
	}
	return c.ModifyScheduledEvent(guildID, eventID, map[string]any{"status": discordEventCompleted})
}

// errEventNotUnderway is End refused: the event has not started, or is over.
var errEventNotUnderway = errors.New("event is not underway")

// endResult says what EndEventNow did, so each surface can word it.
type endResult struct {
	// RolledTo is the next occurrence's start when a recurring event moved on
	// rather than finishing; 0 when the event itself finished. RolledToEnd is
	// that occurrence's end, 0 when the event has no end time.
	RolledTo, RolledToEnd int64
	// AlreadyEnded is true when something else finished it first.
	AlreadyEnded bool
}

// EndEventNow ends an underway event in this service's store and records who
// did it. The Discord side — past events, tables, forum, thread, the native
// event — is left to settleEndedEvent, because a button press has three
// seconds to be answered and that work does not fit in them.
//
// A recurring event does what its end time passing would do: this date goes
// to past events and the event moves on to the next one. Pressing End means
// "this one is over", not "stop the series" — Cancel is that. A rule with no
// next date left finishes the event instead.
func (s *Server) EndEventNow(ev *Event, actor string) (endResult, error) {
	if !eventIsUnderway(ev) {
		return endResult{}, errEventNotUnderway
	}
	if ev.RecurrenceRule != "" {
		if nextStart, nextEnd, ok := s.nextOccurrenceOf(ev); ok {
			log.Printf("[discord-signup] event %d (%q) ended early by %s; rolling to its next occurrence",
				ev.ID, ev.Name, actor)
			return endResult{RolledTo: nextStart, RolledToEnd: nextEnd}, nil
		}
	}
	changed, err := s.store.CompleteLiveEvent(ev.ID)
	if err != nil {
		return endResult{}, err
	}
	if !changed {
		return endResult{AlreadyEnded: true}, nil
	}
	log.Printf("[discord-signup] event %d (%q) ended by %s", ev.ID, ev.Name, actor)
	if after, err := s.store.GetEvent(ev.ID); err == nil {
		if err := s.store.LogEventUpdates(ev, after, actor); err != nil {
			log.Printf("[discord-signup] log end of event %d: %v", ev.ID, err)
		}
	}
	return endResult{}, nil
}

// settleEndedEvent does the Discord half of an End. For a finished event it
// returns the native event's failure, if any: that is the one step nothing
// else will retry, because the reconcile leaves completed events alone.
func (s *Server) settleEndedEvent(ev *Event, result endResult) error {
	if result.AlreadyEnded {
		return nil
	}
	if result.RolledTo != 0 {
		s.rollOverOccurrence(ev, result.RolledTo, result.RolledToEnd)
		return nil
	}
	s.finishEventEverywhere(ev.ID)
	if s.discord == nil || ev.DiscordScheduledEventID == "" {
		return nil
	}
	if err := s.discord.EndScheduledEvent(ev.GuildID, ev.DiscordScheduledEventID); err != nil {
		log.Printf("[discord-signup] end native event for %d: %v", ev.ID, err)
		return err
	}
	return nil
}

// handleEndButton asks before ending. Ending cannot be undone — Join goes and
// the card moves to past events — and the management row is dense enough to
// press the wrong button on, so the press opens a private confirm rather than
// acting.
func (s *Server) handleEndButton(w http.ResponseWriter, in *Interaction, eventID int64) {
	ev, err := s.store.GetEvent(eventID)
	if err != nil {
		s.replyEphemeral(w, "That event no longer exists.")
		return
	}
	if ok, why := s.mayEdit(in, ev); !ok {
		s.replyEphemeral(w, why)
		return
	}
	if !eventIsUnderway(ev) {
		s.replyEphemeral(w, fmt.Sprintf("**%s** is not underway, so there is nothing to end.", ev.Name))
		return
	}
	question := fmt.Sprintf("End **%s** now? Signups stop and it moves to past events. This cannot be undone.", ev.Name)
	if ev.RecurrenceRule != "" {
		question = fmt.Sprintf("End this date of **%s** now? It moves to past events and the event moves on to its next date with an empty roster.", ev.Name)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"type": callbackTypeChannelMessageWithSrc,
		"data": map[string]any{
			"content": question,
			"flags":   messageFlagEphemeral,
			"components": []any{map[string]any{"type": componentTypeActionRow, "components": []any{
				map[string]any{"type": componentTypeButton, "style": buttonStyleDanger,
					"label": "End it now", "custom_id": EndConfirmCustomID(ev.ID)},
			}}},
		},
	})
}

// applyEndConfirm ends the event and replaces the confirm with the outcome.
func (s *Server) applyEndConfirm(w http.ResponseWriter, in *Interaction, eventID int64) {
	answer := func(content string) {
		writeJSON(w, http.StatusOK, map[string]any{
			"type": callbackTypeUpdateMessage,
			"data": map[string]any{"content": content, "components": []any{}},
		})
	}
	ev, err := s.store.GetEvent(eventID)
	if err != nil {
		answer("That event no longer exists.")
		return
	}
	if ok, why := s.mayEdit(in, ev); !ok {
		answer(why)
		return
	}
	userID, _ := in.actor()
	result, err := s.EndEventNow(ev, userID)
	if errors.Is(err, errEventNotUnderway) {
		answer(fmt.Sprintf("**%s** is not underway any more, so nothing was ended.", ev.Name))
		return
	}
	if err != nil {
		log.Printf("[discord-signup] end event %d: %v", ev.ID, err)
		answer(fmt.Sprintf("Something went wrong — **%s** is NOT ended.", ev.Name))
		return
	}
	switch {
	case result.AlreadyEnded:
		answer(fmt.Sprintf("**%s** had already ended.", ev.Name))
	case result.RolledTo != 0:
		answer(fmt.Sprintf("This date of **%s** is over. It has moved on to <t:%d:F>.", ev.Name, result.RolledTo))
	default:
		answer(fmt.Sprintf("**%s** has ended. It is moving to past events now.", ev.Name))
	}
	// After the answer, not before: the store already says it is over, and
	// the Discord writes that follow take longer than the press may wait.
	s.inBackground(func() {
		if err := s.settleEndedEvent(ev, result); err != nil {
			log.Printf("[discord-signup] event %d ended here, but its Discord event did not end: %v", ev.ID, err)
		}
	})
}

// handleWebEndEvent is the web page's End button. It has no three-second
// limit, so it waits for the Discord writes and says if the native event
// could not be ended.
func (s *Server) handleWebEndEvent(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	ev, canManage := s.webEvent(w, r, session)
	if ev == nil {
		return
	}
	if !canManage {
		http.Error(w, "you cannot edit this event", http.StatusForbidden)
		return
	}
	result, err := s.EndEventNow(ev, "web:"+session.DiscordUserID)
	if errors.Is(err, errEventNotUnderway) {
		s.redirectWithNotice(w, r, ev.ID, "It is not underway, so nothing was ended.")
		return
	}
	if err != nil {
		log.Printf("[discord-signup] end event %d: %v", ev.ID, err)
		s.redirectWithNotice(w, r, ev.ID, "Could not end it: "+err.Error())
		return
	}
	nativeErr := s.settleEndedEvent(ev, result)
	var notice string
	switch {
	case result.AlreadyEnded:
		notice = "It had already ended."
	case result.RolledTo != 0:
		notice = "This date is over and has gone to past events. The event has moved on to its next date."
	case nativeErr != nil:
		notice = "Ended here, but its Discord event could not be ended: " + nativeErr.Error()
	default:
		notice = "Ended. It has moved to past events."
	}
	s.redirectWithNotice(w, r, ev.ID, notice)
}
