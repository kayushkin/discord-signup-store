package discordsignup

import (
	"database/sql"
	"errors"
	"fmt"
)

// Held places: an invite that keeps a place for the person asked until they
// answer. A held place counts against the limit exactly as someone going
// does — nobody else can join into it and the waitlist does not move up into
// it — so an event of 8 with 5 going and 1 held has 2 places free. The
// person holding it takes it by pressing Join, even if the event is otherwise
// full; pressing Maybe or Can't go, an organiser releasing it, or the date
// rolling over gives it back, and the next person waiting moves up into it.
//
// When no place is free, an invite can let them in past the limit instead
// (past_limit): nothing is kept and nothing counts against the limit, but
// their Join is never waitlisted or turned away. It ends the same ways a held
// place does, and while it lasts it is "their pass" in the words below.
//
// The limit itself is never changed. The hold lives on the invite row
// (event_invites.holds_place, hold_ended_at, hold_outcome), so the number the
// organiser set stays the number they set, and a hold that ends cannot leave
// a limit one out.

// Hold outcomes: how a held place stopped being held.
const (
	HoldOutcomeJoined      = "joined"      // they pressed Join and took it
	HoldOutcomeDeclined    = "declined"    // they pressed Maybe or Can't go
	HoldOutcomeReleased    = "released"    // an organiser gave it back
	HoldOutcomeUndelivered = "undelivered" // the invite's DM never arrived
	HoldOutcomeExpired     = "expired"     // the date it was held for passed
)

// ErrNoPlaceToHold is a hold asked for when no place is free: the event is
// full counting the places already held, or has no limit, so there is
// nothing to keep.
var ErrNoPlaceToHold = errors.New("no free place to hold")

// heldPlacesTx counts the places held on an event for anyone but exceptUserID
// ("" for everyone).
func heldPlacesTx(tx *sql.Tx, eventID int64, exceptUserID string) (int, error) {
	var n int
	err := tx.QueryRow(`SELECT COUNT(*) FROM event_invites
		WHERE event_id = ? AND holds_place = 1 AND hold_ended_at = 0 AND discord_user_id != ?`,
		eventID, exceptUserID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count held places: %w", err)
	}
	return n, nil
}

// endHoldTx ends someone's held place on an event, if they have one, and
// reports whether they did.
func endHoldTx(tx *sql.Tx, eventID int64, userID, outcome, actor string, ts int64) (bool, error) {
	res, err := tx.Exec(`UPDATE event_invites SET hold_ended_at = ?, hold_outcome = ?, hold_ended_by = ?
		WHERE event_id = ? AND discord_user_id = ? AND (holds_place = 1 OR past_limit = 1) AND hold_ended_at = 0`,
		ts, outcome, actor, eventID, userID)
	if err != nil {
		return false, fmt.Errorf("end held place: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("end held place: %w", err)
	}
	return n > 0, nil
}

// RecordInvite stores one invite. One that holds a place is checked and
// written in one transaction with the count it depends on, as a Join is:
// two organisers holding the last place at once cannot both get it.
func (s *Store) RecordInvite(inv EventInvite) (*EventInvite, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	inv.At = now()
	if inv.HoldsPlace && inv.PastLimit {
		return nil, fmt.Errorf("%w: an invite holds a place or lets them in past the limit, not both", ErrInvalidEvent)
	}
	if inv.PastLimit {
		if _, err := endHoldTx(tx, inv.EventID, inv.DiscordUserID, HoldOutcomeReleased, inv.InvitedBy, inv.At); err != nil {
			return nil, err
		}
	}
	if inv.HoldsPlace {
		var capacity int
		if err := tx.QueryRow(`SELECT capacity FROM events WHERE id = ? AND deleted_at = 0`, inv.EventID).
			Scan(&capacity); err != nil {
			return nil, fmt.Errorf("load event: %w", err)
		}
		var going int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM signups WHERE event_id = ? AND state = ?`,
			inv.EventID, StateAttending).Scan(&going); err != nil {
			return nil, fmt.Errorf("count attending: %w", err)
		}
		held, err := heldPlacesTx(tx, inv.EventID, "")
		if err != nil {
			return nil, err
		}
		if capacity == 0 || going+held >= capacity {
			return nil, ErrNoPlaceToHold
		}
		// One held place per person: a second invite keeps the first hold
		// rather than taking another place.
		if _, err := endHoldTx(tx, inv.EventID, inv.DiscordUserID, HoldOutcomeReleased, inv.InvitedBy, inv.At); err != nil {
			return nil, err
		}
	}
	res, err := tx.Exec(`
		INSERT INTO event_invites (event_id, discord_user_id, display_name, invited_by,
		                           delivery, delivery_error, at, holds_place, past_limit)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		inv.EventID, inv.DiscordUserID, inv.DisplayName, inv.InvitedBy,
		inv.Delivery, inv.DeliveryError, inv.At, boolToInt(inv.HoldsPlace), boolToInt(inv.PastLimit))
	if err != nil {
		return nil, fmt.Errorf("record invite: %w", err)
	}
	if inv.ID, err = res.LastInsertId(); err != nil {
		return nil, fmt.Errorf("read invite id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return &inv, nil
}

// SetInviteDelivery records whether Discord took an invite's DM, after it
// was sent. An invite that never arrived gives back any place it held.
func (s *Store) SetInviteDelivery(inviteID int64, delivery, deliveryError string) (*Signup, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	var eventID int64
	var userID string
	var holds bool
	var ended int64
	if err := tx.QueryRow(`SELECT event_id, discord_user_id, holds_place OR past_limit, hold_ended_at FROM event_invites WHERE id = ?`,
		inviteID).Scan(&eventID, &userID, &holds, &ended); err != nil {
		return nil, fmt.Errorf("load invite: %w", err)
	}
	ts := now()
	if _, err := tx.Exec(`UPDATE event_invites SET delivery = ?, delivery_error = ? WHERE id = ?`,
		delivery, deliveryError, inviteID); err != nil {
		return nil, fmt.Errorf("record delivery: %w", err)
	}
	var promoted *Signup
	if holds && ended == 0 && delivery != InviteDeliverySent {
		if _, err := tx.Exec(`UPDATE event_invites SET hold_ended_at = ?, hold_outcome = ?, hold_ended_by = ''
			WHERE id = ?`, ts, HoldOutcomeUndelivered, inviteID); err != nil {
			return nil, fmt.Errorf("end held place: %w", err)
		}
		if promoted, err = promoteIfPlaceFreeTx(tx, eventID, ts); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return promoted, nil
}

// GiveBackHeldPlace ends someone's held place — they said no, or an
// organiser released it — and moves the next person waiting into the place
// it frees. ErrNotFound when they hold none.
func (s *Store) GiveBackHeldPlace(eventID int64, userID, outcome, actor string) (*Signup, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	ts := now()
	ended, err := endHoldTx(tx, eventID, userID, outcome, actor, ts)
	if err != nil {
		return nil, err
	}
	if !ended {
		return nil, ErrNotFound
	}
	promoted, err := promoteIfPlaceFreeTx(tx, eventID, ts)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return promoted, nil
}
