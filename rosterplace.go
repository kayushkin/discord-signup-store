package discordsignup

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// An organiser putting someone on a list by hand: going, maybe or the
// waitlist, their choice rather than the capacity rule's. Different from an
// invite, which asks and lets the person decide, and from Join, which the
// person presses themselves and which the limit decides.

// PlaceResult is what putting someone on a list did.
type PlaceResult struct {
	Signup Signup `json:"signup"`
	// FromState is where they were: "" when new to this event, or attending,
	// waitlisted, maybe or withdrawn.
	FromState string `json:"from_state"`
	// Unchanged is true when they were already on that list.
	Unchanged bool `json:"unchanged"`
	// Promoted is whoever took the place this person gave up, when they were
	// moved off going. The caller tells them.
	Promoted *Signup `json:"promoted,omitempty"`
}

// PlaceOnList puts someone on an event's going, maybe or waitlist, on an
// organiser's say-so.
//
// Going ignores the limit, as GiveAPlace does: the organiser can take an
// event to 16/15. The waitlist is only offered while the event is full —
// counting everyone going except this person — and has a waitlist, because a
// line with a free place at its head would be promoted at once. Anyone moved
// off going frees a place, which goes to whoever has waited longest, as if
// they had left.
//
// Arrival order follows the same rules as everywhere else: someone new, or
// back after withdrawing, is a new row at the back; moving to the waitlist is
// joining its back; going or Maybe from a list they are already on keeps the
// row. The history names the organiser as the actor.
func (s *Store) PlaceOnList(eventID int64, discordUserID, displayName, state, actor string) (*PlaceResult, error) {
	discordUserID = strings.TrimSpace(discordUserID)
	if discordUserID == "" {
		return nil, fmt.Errorf("%w: discord_user_id is required", ErrInvalidEvent)
	}
	if state != StateAttending && state != StateMaybe && state != StateWaitlisted {
		return nil, fmt.Errorf("%w: an organiser can put someone on %q, %q or %q, not %q",
			ErrInvalidEvent, StateAttending, StateMaybe, StateWaitlisted, state)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	var capacity int
	var status string
	var waitlistDisabled bool
	err = tx.QueryRow(`SELECT capacity, status, waitlist_disabled FROM events WHERE id = ? AND deleted_at = 0`, eventID).
		Scan(&capacity, &status, &waitlistDisabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load event: %w", err)
	}
	// Closed shuts the door on people signing themselves up, not on the
	// organiser. Over is over.
	if IsArchived(status) {
		return nil, fmt.Errorf("%w (status is %q)", ErrEventNotOpen, status)
	}

	existing, found, err := loadSignupTx(tx, eventID, discordUserID)
	if err != nil {
		return nil, err
	}
	result := &PlaceResult{}
	if found {
		result.FromState = existing.State
	}
	if found && existing.State == state {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit: %w", err)
		}
		result.Signup, result.Unchanged = *existing, true
		if err := s.fillWaitlistPlace(&result.Signup); err != nil {
			return nil, err
		}
		return result, nil
	}

	if state == StateWaitlisted {
		if waitlistDisabled {
			return nil, fmt.Errorf("%w: this event's waitlist is turned off", ErrInvalidEvent)
		}
		var othersGoing int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM signups WHERE event_id = ? AND state = ? AND discord_user_id != ?`,
			eventID, StateAttending, discordUserID).Scan(&othersGoing); err != nil {
			return nil, fmt.Errorf("count attending: %w", err)
		}
		othersHeld, err := heldPlacesTx(tx, eventID, discordUserID)
		if err != nil {
			return nil, err
		}
		if capacity == 0 || othersGoing+othersHeld < capacity {
			return nil, fmt.Errorf("%w: the event is not full, so nobody waits — put them down as going", ErrInvalidEvent)
		}
	}

	ts := now()
	// A place held for them ends here: taken if they are put down as going,
	// given back otherwise — and then the next person waiting moves up.
	holdOutcome := HoldOutcomeReleased
	if state == StateAttending {
		holdOutcome = HoldOutcomeJoined
	}
	heldEnded, err := endHoldTx(tx, eventID, discordUserID, holdOutcome, actor, ts)
	if err != nil {
		return nil, err
	}
	// Kept in place: going or Maybe from a list they are already on. The row's
	// arrival stays theirs, as it does when GiveAPlace or MarkMaybe moves it.
	keepRow := found && existing.State != StateWithdrawn && state != StateWaitlisted
	if keepRow {
		if _, err := tx.Exec(`UPDATE signups SET state = ?, state_changed_at = ? WHERE id = ?`,
			state, ts, existing.ID); err != nil {
			return nil, fmt.Errorf("move signup to %s: %w", state, err)
		}
		existing.State, existing.StateChangedAt = state, ts
		result.Signup = *existing
	} else {
		// New, back after withdrawing, or joining the back of the line:
		// deleted and re-inserted, as Join does, so the row's id follows its
		// arrival.
		interested := false
		if found {
			interested = existing.DiscordInterested
			if displayName == "" {
				displayName = existing.DisplayName
			}
			if _, err := tx.Exec(`DELETE FROM signups WHERE id = ?`, existing.ID); err != nil {
				return nil, fmt.Errorf("clear old signup: %w", err)
			}
		}
		res, err := tx.Exec(`
			INSERT INTO signups (event_id, discord_user_id, display_name, state,
			                     signed_up_at, state_changed_at, joined_via, discord_interested)
			VALUES (?,?,?,?,?,?,?,?)`,
			eventID, discordUserID, displayName, state, ts, ts, JoinedViaOperator, boolToInt(interested))
		if err != nil {
			return nil, fmt.Errorf("insert signup: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("read inserted id: %w", err)
		}
		result.Signup = Signup{ID: id, EventID: eventID, DiscordUserID: discordUserID,
			DisplayName: displayName, State: state, SignedUpAt: ts, StateChangedAt: ts,
			JoinedVia: JoinedViaOperator, DiscordInterested: interested}
	}
	if err := logSignupUpdate(tx, eventID, discordUserID, ActionAdded, result.FromState, state, actor, ts); err != nil {
		return nil, err
	}

	// Moved off going: the place they held is free unless the event was
	// over its limit, and the longest-waiting person takes it.
	if result.FromState == StateAttending || (heldEnded && state != StateAttending) {
		if result.Promoted, err = promoteIfPlaceFreeTx(tx, eventID, ts); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	if err := s.fillWaitlistPlace(&result.Signup); err != nil {
		return nil, err
	}
	return result, nil
}
