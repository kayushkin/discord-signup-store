package discordsignup

import (
	"database/sql"
	"errors"
	"fmt"
)

// InterestOutcome names what a Discord Interested signal actually did.
type InterestOutcome string

const (
	// OutcomeJoined means they took a place.
	OutcomeJoined InterestOutcome = "joined"
	// OutcomeWaitlisted means the event was full and they went into line.
	OutcomeWaitlisted InterestOutcome = "waitlisted"
	// OutcomeAlreadyOn means they were already on the roster and nothing moved.
	OutcomeAlreadyOn InterestOutcome = "already_on"
	// OutcomeRespectedWithdrawal is the important one: they pressed Leave on
	// the signup message and Discord still lists them as Interested, because
	// there is no API to remove a subscriber. Their choice wins over the stale
	// signal and the roster is left alone.
	OutcomeRespectedWithdrawal InterestOutcome = "respected_withdrawal"
	// OutcomeEventClosed means the event is not taking signups.
	OutcomeEventClosed InterestOutcome = "event_closed"
	// OutcomeLeft means un-marking Interested took them off the roster,
	// because Interested is how they got on in the first place.
	OutcomeLeft InterestOutcome = "left"
	// OutcomeKeptPlace means they un-marked Interested but had pressed Join, so
	// their place stands. The two signals are independent and the stronger one
	// holds.
	OutcomeKeptPlace InterestOutcome = "kept_place"
	// OutcomeNotOnRoster means there was nothing to change.
	OutcomeNotOnRoster InterestOutcome = "not_on_roster"
)

// InterestResult is what one Discord RSVP signal did.
type InterestResult struct {
	Outcome InterestOutcome `json:"outcome"`
	Signup  *Signup         `json:"signup,omitempty"`
	// Promoted is whoever moved up because this person left.
	Promoted *Signup `json:"promoted,omitempty"`
}

// RosterChanged reports whether Discord needs telling — the roles, the signup
// message, or both.
func (r *InterestResult) RosterChanged() bool {
	switch r.Outcome {
	case OutcomeJoined, OutcomeWaitlisted, OutcomeLeft:
		return true
	default:
		return false
	}
}

// MarkInterested handles GUILD_SCHEDULED_EVENT_USER_ADD: someone pressed
// Interested on the native Discord event.
//
// It behaves like the Join button with exactly one difference, and that
// difference is the whole reason this is a separate method rather than a call
// to Join with a different `via`.
//
// **A withdrawal is sticky against this signal.** Discord has no endpoint to
// remove a subscriber, so someone who presses Interested and then presses Leave
// on the signup message stays Interested on Discord for good. Treating that
// lingering signal as fresh intent would put them straight back on a roster
// they deliberately left, and every reconnect or reconciliation would do it
// again. So a withdrawn row is only revived if we have since seen them un-mark
// Interested — which is a real action, and is recorded in discord_interested.
func (s *Store) MarkInterested(eventID int64, discordUserID, displayName string) (*InterestResult, error) {
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

	existing, found, err := loadSignupTx(tx, eventID, discordUserID)
	if err != nil {
		return nil, err
	}

	// Recorded first and unconditionally. Even when the roster does not move,
	// knowing Discord currently lists them is what makes the NEXT signal
	// readable: without it, un-marking and re-marking is indistinguishable from
	// a duplicate event.
	if found {
		if _, err := tx.Exec(`UPDATE signups SET discord_interested = 1 WHERE id = ?`, existing.ID); err != nil {
			return nil, fmt.Errorf("record interest: %w", err)
		}
	}

	switch {
	case found && existing.State != StateWithdrawn:
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit: %w", err)
		}
		existing.DiscordInterested = true
		return &InterestResult{Outcome: OutcomeAlreadyOn, Signup: existing}, nil

	case found && existing.State == StateWithdrawn && existing.DiscordInterested:
		// They were already Interested when they chose to leave. The signal is
		// the leftover of that same RSVP, not a new one.
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit: %w", err)
		}
		existing.DiscordInterested = true
		return &InterestResult{Outcome: OutcomeRespectedWithdrawal, Signup: existing}, nil
	}

	// Everything below actually puts them on the roster: either they are new,
	// or they withdrew and have since un-marked and re-marked Interested.
	if status != StatusOpen {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit: %w", err)
		}
		return &InterestResult{Outcome: OutcomeEventClosed}, nil
	}

	var attending int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM signups WHERE event_id = ? AND state = ?`,
		eventID, StateAttending).Scan(&attending); err != nil {
		return nil, fmt.Errorf("count attending: %w", err)
	}
	newState, action := StateAttending, ActionJoined
	if capacity > 0 && attending >= capacity {
		newState, action = StateWaitlisted, ActionWaitlisted
	}

	ts := now()
	position, err := nextPosition(tx, eventID)
	if err != nil {
		return nil, err
	}
	var signupID int64
	fromState := ""
	if found {
		fromState = existing.State
		action = ActionRejoined
		if newState == StateWaitlisted {
			action = ActionWaitlisted
		}
		_, err = tx.Exec(`
			UPDATE signups SET display_name = ?, position = ?, state = ?, signed_up_at = ?,
			       state_changed_at = ?, joined_via = ?, discord_interested = 1
			WHERE id = ?`,
			displayName, position, newState, ts, ts, JoinedViaInterested, existing.ID)
		if err != nil {
			return nil, fmt.Errorf("reactivate signup: %w", err)
		}
		signupID = existing.ID
	} else {
		res, err := tx.Exec(`
			INSERT INTO signups (event_id, discord_user_id, display_name, position, state,
			                     signed_up_at, state_changed_at, joined_via, discord_interested)
			VALUES (?,?,?,?,?,?,?,?,1)`,
			eventID, discordUserID, displayName, position, newState, ts, ts, JoinedViaInterested)
		if err != nil {
			return nil, fmt.Errorf("insert signup: %w", err)
		}
		if signupID, err = res.LastInsertId(); err != nil {
			return nil, fmt.Errorf("read inserted id: %w", err)
		}
	}
	if err := logSignupUpdate(tx, eventID, discordUserID, action, fromState, newState,
		position, ActorInterested, ts); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	out := &Signup{
		ID: signupID, EventID: eventID, DiscordUserID: discordUserID, DisplayName: displayName,
		Position: position, State: newState, SignedUpAt: ts, StateChangedAt: ts,
		JoinedVia: JoinedViaInterested, DiscordInterested: true,
	}
	if err := s.fillWaitlistPlace(out); err != nil {
		return nil, err
	}
	outcome := OutcomeJoined
	if newState == StateWaitlisted {
		outcome = OutcomeWaitlisted
	}
	return &InterestResult{Outcome: outcome, Signup: out}, nil
}

// MarkNotInterested handles GUILD_SCHEDULED_EVENT_USER_REMOVE.
//
// Un-marking Interested takes the person off the roster, however they got on.
//
// An earlier version removed only people who had joined VIA Interested, on the
// reasoning that turning off a notification is not forfeiting a seat. That was
// wrong in practice: the two buttons are presented as doing the same thing, so
// people reasonably expect undoing either one to have the same effect, and
// someone who un-marks Interested and stays on a roster has no idea they are
// still counted. Matching Join and Leave beats a distinction only this code
// could see.
//
// It always clears discord_interested, which is what lets a later Interested
// signal count as fresh intent instead of being suppressed as stale.
func (s *Store) MarkNotInterested(eventID int64, discordUserID string) (*InterestResult, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	existing, found, err := loadSignupTx(tx, eventID, discordUserID)
	if err != nil {
		return nil, err
	}
	if !found {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit: %w", err)
		}
		return &InterestResult{Outcome: OutcomeNotOnRoster}, nil
	}
	if _, err := tx.Exec(`UPDATE signups SET discord_interested = 0 WHERE id = ?`, existing.ID); err != nil {
		return nil, fmt.Errorf("clear interest: %w", err)
	}
	shouldLeave := existing.State != StateWithdrawn
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	if !shouldLeave {
		// Already off the roster, so there is nothing to undo.
		return &InterestResult{Outcome: OutcomeNotOnRoster, Signup: existing}, nil
	}

	// Leave runs in its own transaction so the promotion and its history are
	// one atomic step, exactly as they are for the Leave button.
	left, err := s.Leave(eventID, discordUserID, ActorInterested)
	if err != nil {
		return nil, err
	}
	return &InterestResult{Outcome: OutcomeLeft, Signup: &left.Signup, Promoted: left.Promoted}, nil
}

// loadSignupTx reads one person's row inside a transaction.
func loadSignupTx(tx *sql.Tx, eventID int64, discordUserID string) (*Signup, bool, error) {
	var sg Signup
	err := tx.QueryRow(`
		SELECT id, event_id, discord_user_id, display_name, position, state, signed_up_at,
		       state_changed_at, joined_via, discord_interested
		FROM signups WHERE event_id = ? AND discord_user_id = ?`, eventID, discordUserID).
		Scan(&sg.ID, &sg.EventID, &sg.DiscordUserID, &sg.DisplayName, &sg.Position, &sg.State,
			&sg.SignedUpAt, &sg.StateChangedAt, &sg.JoinedVia, &sg.DiscordInterested)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("load signup: %w", err)
	}
	return &sg, true, nil
}
