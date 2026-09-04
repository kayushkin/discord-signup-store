package discordsignup

import (
	"fmt"
	"log"
	"time"
)

// A recurring event is one row, the way Discord holds it as one scheduled
// event: when an occurrence ends the row's start slides to the next date,
// signups open again from nobody, and the reminders are owed again. The
// occurrence that ended leaves one line in past events, like a finished
// event does, so the history is in the channel and not only in the tables.
//
// The roster does NOT carry over. Discord's Interested list does, but that
// list is a subscription, not a seat; a seat held from last week is a seat
// nobody new can take, and a capped weekly event that carries its roster
// forward is full forever after the first week.
//
// Two things trigger a rollover, and they must do the same work:
//
//	the sweep     every five minutes, an occurrence whose end has passed;
//	              the next date is computed here from the rule
//	the import    Discord slid the native event's start forward past an
//	              occurrence that had ended; the next date is Discord's
//
// Both land in rollOverOccurrence. An organiser moving a date that has not
// happened yet is an edit, not a rollover, and resets nothing.

// ActorRecurrence is the actor recorded when this service withdrew everyone
// because the occurrence they had signed up for ended and the next one took
// its place.
const ActorRecurrence = "recurrence"

// FinishedRecurringOccurrences lists the recurring events whose current
// occurrence has ended: the rows the sweep rolls forward rather than
// completes.
func (s *Store) FinishedRecurringOccurrences() ([]Event, error) {
	rows, err := s.db.Query(`SELECT `+eventColumns+`
		FROM events
		WHERE deleted_at = 0 AND status IN (?, ?) AND recurrence_rule != ''`,
		StatusOpen, StatusClosed)
	if err != nil {
		return nil, fmt.Errorf("scan for finished occurrences: %w", err)
	}
	defer rows.Close()
	var finished []Event
	cutoff := now()
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		if finishedBy(ev) < cutoff {
			finished = append(finished, *ev)
		}
	}
	return finished, rows.Err()
}

// RollOverOccurrence moves a recurring event on to its next date: the start
// and end move, signups reopen, everyone on the roster is withdrawn, and the
// reminder stamps clear so the next occurrence gets its own. One transaction,
// so a crash between the date moving and the roster clearing cannot leave
// last week's people holding this week's places.
//
// Returns the people withdrawn, so the caller can settle their roles and
// reactions through the same path a Leave uses.
func (s *Store) RollOverOccurrence(eventID, nextStart, nextEnd int64) ([]Signup, error) {
	if nextStart <= 0 {
		return nil, fmt.Errorf("%w: next occurrence needs a start", ErrInvalidEvent)
	}
	before, err := s.GetEvent(eventID)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	ts := now()
	if _, err := tx.Exec(`
		UPDATE events SET starts_at = ?, ends_at = ?, status = ?,
		       reminded_before_at = 0, reminded_start_at = 0, updated_at = ?
		WHERE id = ?`, nextStart, nextEnd, StatusOpen, ts, eventID); err != nil {
		return nil, fmt.Errorf("move event to next occurrence: %w", err)
	}

	rows, err := tx.Query(`
		SELECT id, event_id, discord_user_id, display_name, state, signed_up_at,
		       state_changed_at, joined_via, discord_interested
		FROM signups WHERE event_id = ? AND state != ?
		ORDER BY signed_up_at ASC, id ASC`, eventID, StateWithdrawn)
	if err != nil {
		return nil, fmt.Errorf("read roster: %w", err)
	}
	var withdrawn []Signup
	for rows.Next() {
		var sg Signup
		if err := rows.Scan(&sg.ID, &sg.EventID, &sg.DiscordUserID, &sg.DisplayName, &sg.State,
			&sg.SignedUpAt, &sg.StateChangedAt, &sg.JoinedVia, &sg.DiscordInterested); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan signup: %w", err)
		}
		withdrawn = append(withdrawn, sg)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate roster: %w", err)
	}
	for i := range withdrawn {
		sg := &withdrawn[i]
		if _, err := tx.Exec(`UPDATE signups SET state = ?, state_changed_at = ? WHERE id = ?`,
			StateWithdrawn, ts, sg.ID); err != nil {
			return nil, fmt.Errorf("withdraw %s: %w", sg.DiscordUserID, err)
		}
		if err := logSignupUpdate(tx, eventID, sg.DiscordUserID, ActionWithdrew, sg.State,
			StateWithdrawn, ActorRecurrence, ts); err != nil {
			return nil, err
		}
		sg.State = StateWithdrawn
		sg.StateChangedAt = ts
	}

	// The date moving is an edit to the event and is logged as one, under an
	// actor that says nobody typed it.
	for _, f := range []struct{ field, from, to string }{
		{"starts_at", fmt.Sprint(before.StartsAt), fmt.Sprint(nextStart)},
		{"ends_at", fmt.Sprint(before.EndsAt), fmt.Sprint(nextEnd)},
		{"status", before.Status, StatusOpen},
	} {
		if f.from == f.to {
			continue
		}
		if _, err := tx.Exec(`
			INSERT INTO event_updates (event_id, field, from_value, to_value, actor, at)
			VALUES (?,?,?,?,?,?)`, eventID, f.field, f.from, f.to, ActorRecurrence, ts); err != nil {
			return nil, fmt.Errorf("log %s: %w", f.field, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return withdrawn, nil
}

// nextOccurrenceOf is the occurrence after the current one, keeping the
// event's run time. Zone falls back the way every other reading of the start
// does.
func (s *Server) nextOccurrenceOf(ev *Event) (nextStart, nextEnd int64, ok bool) {
	start := startInZone(ev, s.DefaultTimezone())
	next, ok := nextOccurrence(ev.RecurrenceRule, start, time.Unix(now(), 0))
	if !ok {
		return 0, 0, false
	}
	nextStart = next.Unix()
	if ev.EndsAt > 0 {
		nextEnd = nextStart + (ev.EndsAt - ev.StartsAt)
	}
	return nextStart, nextEnd, true
}

// rollOverOccurrence settles a finished occurrence on every surface and moves
// the event on: one line in past events for the occurrence that ran, then the
// dates move and the roster clears, then roles, reactions, the card, the
// tables and the native event are republished for the empty next one.
func (s *Server) rollOverOccurrence(ev *Event, nextStart, nextEnd int64) {
	if err := s.postOccurrencePastLine(ev); err != nil {
		log.Printf("[discord-signup] past-events line for occurrence of event %d: %v", ev.ID, err)
	}
	withdrawn, err := s.store.RollOverOccurrence(ev.ID, nextStart, nextEnd)
	if err != nil {
		log.Printf("[discord-signup] roll event %d to its next occurrence: %v", ev.ID, err)
		return
	}
	changes := make([]stateChange, 0, len(withdrawn))
	for _, sg := range withdrawn {
		changes = append(changes, stateChange{UserID: sg.DiscordUserID, State: StateWithdrawn})
	}
	log.Printf("[discord-signup] event %d (%q) rolled to its next occurrence at %d; %d withdrawn",
		ev.ID, ev.Name, nextStart, len(withdrawn))
	if s.discord != nil {
		s.syncAfterChange(ev.ID, changes)
	}
}

// rollOverFinishedOccurrences is the sweep's half: every recurring event whose
// occurrence has ended moves to the date the rule gives next.
func (s *Server) rollOverFinishedOccurrences() ([]int64, error) {
	finished, err := s.store.FinishedRecurringOccurrences()
	if err != nil {
		return nil, err
	}
	var rolled []int64
	for i := range finished {
		ev := &finished[i]
		nextStart, nextEnd, ok := s.nextOccurrenceOf(ev)
		if !ok {
			log.Printf("[discord-signup] event %d (%q): rule %q cannot be expanded; occurrence left as is",
				ev.ID, ev.Name, ev.RecurrenceRule)
			continue
		}
		s.rollOverOccurrence(ev, nextStart, nextEnd)
		rolled = append(rolled, ev.ID)
	}
	return rolled, nil
}

// postOccurrencePastLine leaves the finished occurrence's line in past events.
// Unlike a finished event's, it does not repoint the event's channel: the
// event is not over, only this date is.
func (s *Server) postOccurrencePastLine(ev *Event) error {
	if s.discord == nil {
		return nil
	}
	pastChannelID := s.guildChannels(ev.GuildID).Past
	if pastChannelID == "" {
		return nil
	}
	// Re-read rather than trust the caller's copy: the counts are filled in by
	// GetEvent, and a row straight off a scan carries zeros.
	ev, err := s.store.GetEvent(ev.ID)
	if err != nil {
		return err
	}
	roster, err := s.store.Roster(ev.ID, false)
	if err != nil {
		return err
	}
	_, err = s.discord.CreateMessage(pastChannelID, map[string]any{
		"content":          pastEventLine(ev, roster),
		"allowed_mentions": map[string]any{"parse": []string{}},
	})
	if err != nil {
		return fmt.Errorf("post to past events: %w", err)
	}
	return nil
}

// repeatsLabel is the short form every surface uses to say an event recurs:
// "🔁 weekly", "🔁 every 2 weeks", "🔁 monthly". Empty for a one-off.
func repeatsLabel(ev *Event) string {
	if ev.RecurrenceRule == "" {
		return ""
	}
	return "🔁 " + describeRepeat(ev.RecurrenceRule)
}
