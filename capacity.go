package discordsignup

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
)

// PromoteToFillCapacity moves people off the waitlist until the event is full
// again, in arrival order.
//
// Raising a limit is the only way an event gains places without someone
// leaving, and nothing else in this package handles it: Leave promotes exactly
// one person because exactly one place opened. Without this, raising a cap from
// 5 to 20 would leave fifteen people sitting on a waitlist in front of fifteen
// empty places, and the only way out would be for an attendee to drop.
//
// One BEGIN IMMEDIATE transaction for the same reason Join is: the count and
// the promotions have to be one decision, or two concurrent raises both read
// the same free space and overfill the event between them.
func (s *Store) PromoteToFillCapacity(eventID int64) ([]Signup, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	var capacity int
	var status string
	err = tx.QueryRow(`SELECT capacity, status FROM events WHERE id = ? AND deleted_at = 0`, eventID).
		Scan(&capacity, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load event: %w", err)
	}
	// A closed or finished event keeps its waitlist frozen. Promoting into one
	// would tell somebody they got a place at a thing that is not taking any.
	if status != StatusOpen {
		return nil, nil
	}

	var attending int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM signups WHERE event_id = ? AND state = ?`,
		eventID, StateAttending).Scan(&attending); err != nil {
		return nil, fmt.Errorf("count attending: %w", err)
	}

	// capacity 0 means unlimited, so everyone waiting comes in at once. That is
	// not a special case bolted on: it falls out of "free places" being
	// unbounded, and the LIMIT below is what expresses it.
	free := -1 // unlimited
	if capacity > 0 {
		free = capacity - attending
		if free <= 0 {
			return nil, tx.Commit()
		}
	}

	query := `SELECT id, event_id, discord_user_id, display_name, position, state, signed_up_at,
	                 state_changed_at, joined_via, discord_interested
	          FROM signups WHERE event_id = ? AND state = ? ORDER BY position ASC`
	args := []any{eventID, StateWaitlisted}
	if free >= 0 {
		query += ` LIMIT ?`
		args = append(args, free)
	}
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("read waitlist: %w", err)
	}
	var promoting []Signup
	for rows.Next() {
		var sg Signup
		if err := rows.Scan(&sg.ID, &sg.EventID, &sg.DiscordUserID, &sg.DisplayName,
			&sg.Position, &sg.State, &sg.SignedUpAt, &sg.StateChangedAt,
			&sg.JoinedVia, &sg.DiscordInterested); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan waitlisted: %w", err)
		}
		promoting = append(promoting, sg)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate waitlist: %w", err)
	}

	ts := now()
	for i := range promoting {
		if _, err := tx.Exec(`UPDATE signups SET state = ?, state_changed_at = ? WHERE id = ?`,
			StateAttending, ts, promoting[i].ID); err != nil {
			return nil, fmt.Errorf("promote signup %d: %w", promoting[i].ID, err)
		}
		if err := logTransition(tx, eventID, promoting[i].DiscordUserID, ActionPromoted,
			StateWaitlisted, StateAttending, promoting[i].Position, ActorPromotion, ts); err != nil {
			return nil, err
		}
		promoting[i].State = StateAttending
		promoting[i].StateChangedAt = ts
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return promoting, nil
}

// ParseCapacity reads a limit typed by a person.
//
// Accepts a bare number, and the words people actually reach for when they mean
// no limit. Anything else is refused with a message naming what was wrong,
// because this value arrives from a free text box where a typo is a matter of
// when rather than whether.
func ParseCapacity(input string) (int, error) {
	trimmed := strings.ToLower(strings.TrimSpace(input))
	switch trimmed {
	case "", "0", "none", "no limit", "unlimited", "any", "-":
		return 0, nil
	}
	// Tolerate "20 places" and "20people" rather than rejecting them: the
	// intent is unambiguous and refusing it teaches nothing.
	trimmed = strings.TrimSpace(strings.TrimSuffix(
		strings.TrimSuffix(strings.TrimSpace(strings.TrimSuffix(trimmed, "places")), "place"), "people"))

	value, err := strconv.Atoi(strings.TrimSpace(trimmed))
	if err != nil {
		return 0, fmt.Errorf("%w: %q is not a number — type a number of places, or 0 for no limit",
			ErrInvalidEvent, input)
	}
	if value < 0 {
		return 0, fmt.Errorf("%w: a limit cannot be negative — use 0 for no limit", ErrInvalidEvent)
	}
	// Discord's own hard ceilings are far lower than this; the number exists to
	// catch a mistyped year or phone number, not to express a real policy.
	if value > 100000 {
		return 0, fmt.Errorf("%w: %d places looks like a typo", ErrInvalidEvent, value)
	}
	return value, nil
}

// SetCapacity changes an event's limit and settles everything that follows from
// it: people promoted off the waitlist, their roles, the signup card, and a
// message to each person who just got in.
//
// Returns the updated event and whoever was promoted, so a caller can say what
// actually happened rather than just "saved".
func (s *Server) SetCapacity(eventID int64, capacity int, actor string) (*Event, []Signup, error) {
	before, err := s.store.GetEvent(eventID)
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.store.UpdateEvent(eventID, EventPatch{Capacity: &capacity}); err != nil {
		return nil, nil, err
	}

	var promoted []Signup
	// Only a raise can free places. Lowering never demotes anyone — see the
	// note on UpdateEvent — so there is nothing to settle.
	if capacity == 0 || capacity > before.Capacity {
		promoted, err = s.store.PromoteToFillCapacity(eventID)
		if err != nil {
			return nil, nil, err
		}
	}

	after, err := s.store.GetEvent(eventID)
	if err != nil {
		return nil, nil, err
	}
	log.Printf("[discord-signup] capacity of event %d set to %d by %s; %d promoted",
		eventID, capacity, actor, len(promoted))

	changes := make([]stateChange, 0, len(promoted))
	for _, sg := range promoted {
		changes = append(changes, stateChange{UserID: sg.DiscordUserID, State: StateAttending})
	}
	// Roles and the card in one pass; the card refreshes even when nobody moved,
	// because the number printed on it has changed.
	go s.syncAfterChange(after, changes)
	for i := range promoted {
		go s.notifyPromoted(after, &promoted[i])
	}
	return after, promoted, nil
}
