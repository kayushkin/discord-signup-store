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

	query := `SELECT id, event_id, discord_user_id, display_name, state, signed_up_at,
	                 state_changed_at, joined_via, discord_interested
	          FROM signups WHERE event_id = ? AND state = ? ORDER BY signed_up_at ASC, id ASC`
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
			&sg.State, &sg.SignedUpAt, &sg.StateChangedAt,
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
		if err := logSignupUpdate(tx, eventID, promoting[i].DiscordUserID, ActionPromoted,
			StateWaitlisted, StateAttending, ActorPromotion, ts); err != nil {
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
	log.Printf("[discord-signup] capacity of event %d set to %d by %s", eventID, capacity, actor)
	return s.applyEventEdit(before, EventPatch{Capacity: &capacity}, actor)
}

// applyEventEdit saves a patch, settles whoever a raised capacity lets in, and
// publishes the result to every copy of the event.
//
// The one path every edit takes. The three surfaces collect different fields —
// the Discord modal has no end time, the web form has no recurrence box, the
// capacity command has one number — so each builds its own patch, but what
// happens after the save is identical for all of them and lives here.
//
// It lives in one function because the alternative was tried: the web form
// carried its own partial copy that promoted people and redrew the card, and
// silently never touched the native scheduled event. Its title kept whatever
// count it had at the last signup, so raising a limit from the web page left
// Discord telling the server "[3/8]" while the card underneath said 3/10.
func (s *Server) applyEventEdit(before *Event, patch EventPatch, actor string) (*Event, []Signup, error) {
	if _, err := s.store.UpdateEvent(before.ID, patch); err != nil {
		return nil, nil, err
	}

	var promoted []Signup
	// Only a raise can free places. Lowering never demotes anyone — see the
	// note on UpdateEvent — so there is nothing to settle. A patch that does
	// not carry a capacity at all has not changed one.
	if patch.Capacity != nil && (*patch.Capacity == 0 || *patch.Capacity > before.Capacity) {
		var err error
		promoted, err = s.store.PromoteToFillCapacity(before.ID)
		if err != nil {
			return nil, nil, err
		}
	}

	after, err := s.store.GetEvent(before.ID)
	if err != nil {
		return nil, nil, err
	}
	// Logged here because this is the one function every edit passes through —
	// the Discord form, the web form, the capacity command and the machine API
	// all end up on this line, so there is no surface that can change an event
	// and leave no trace.
	if err := s.store.LogEventUpdates(before, after, actor); err != nil {
		log.Printf("[discord-signup] log edits to event %d: %v", before.ID, err)
	}
	log.Printf("[discord-signup] event %d edited by %s; %d promoted", before.ID, actor, len(promoted))

	changes := make([]stateChange, 0, len(promoted))
	for _, sg := range promoted {
		changes = append(changes, stateChange{UserID: sg.DiscordUserID, State: StateAttending})
	}
	// One call covers the roles, the card, the table row, the forum post and
	// the native event's title. It runs even when nobody moved, because a
	// rename or a new limit changes what all five of them say. After the reply,
	// like the DMs below: the person editing must not wait on Discord, and a
	// copy failing to update must not make a saved edit look failed.
	go s.syncAfterChange(after.ID, changes)
	for i := range promoted {
		go s.notifyPromoted(after, &promoted[i])
	}
	return after, promoted, nil
}

// ApplyEventForm saves an edit from the Discord form and settles what follows.
//
// One path for the whole form rather than a method per field: the promotion
// only depends on capacity, but the card has to be rewritten whatever changed,
// and having two functions that each rewrite it is how one of them stops.
func (s *Server) ApplyEventForm(before *Event, values *EventFormResult, zone, actor string) (*Event, []Signup, error) {
	// EndsAt is deliberately absent. The Discord form carries no end time, so
	// including it here would send zero every time and wipe an end somebody set
	// on the web page. A field the form does not collect is a field this must
	// not touch.
	patch := EventPatch{
		Name:        &values.Name,
		StartsAt:    &values.StartsAt,
		Capacity:    &values.Capacity,
		Location:    &values.Location,
		Description: &values.Description,
	}
	// Only stamp the zone when the event has none. Overwriting a zone chosen on
	// the web page with the deployment default would silently move every time
	// on the event by the offset between them.
	if before.Timezone == "" {
		patch.Timezone = &zone
	}
	return s.applyEventEdit(before, patch, actor)
}

// moveCardToPastEvents takes a finished event's card off the board and puts a
// final one in the past-events channel.
//
// Discord cannot move a message between channels, so this is a post followed by
// a delete — in that order, deliberately. If the post fails, the original card
// stays where it is and nothing is lost; doing it the other way round would
// delete the only copy and then discover the new channel is unreachable.
//
// message_id and channel_id are repointed at the new card rather than a second
// pair of columns being added, because they mean "where this event's card is"
// and after this that is the past-events channel. A finished card carries no
// buttons, so nothing resolves a click through it any more.
func (s *Server) moveCardToPastEvents(eventID int64) error {
	if s.discord == nil || s.pastChannelID == "" {
		return nil
	}
	ev, err := s.store.GetEvent(eventID)
	if err != nil {
		return err
	}
	if ev.ChannelID == s.pastChannelID {
		return nil // already moved
	}
	roster, err := s.store.Roster(eventID, false)
	if err != nil {
		return err
	}

	newMessageID, err := s.discord.CreateMessage(s.pastChannelID, RenderSignupMessage(ev, roster))
	if err != nil {
		return fmt.Errorf("post to past events: %w", err)
	}
	// Only now is the original expendable. A failure here leaves a duplicate
	// rather than a hole, which is the better of the two.
	if ev.MessageID != "" {
		if err := s.discord.DeleteMessage(ev.ChannelID, ev.MessageID); err != nil {
			log.Printf("[discord-signup] could not remove event %d's card from the board: %v",
				eventID, err)
		}
	}
	past := s.pastChannelID
	if _, err := s.store.UpdateEvent(eventID, EventPatch{
		MessageID: &newMessageID, ChannelID: &past,
	}); err != nil {
		return fmt.Errorf("repoint card: %w", err)
	}
	log.Printf("[discord-signup] event %d finished; card moved to past events", eventID)
	return nil
}
