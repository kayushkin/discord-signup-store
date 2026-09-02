package discordsignup

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Join puts someone on the roster, giving them a place if there is one and a
// position in line if there is not.
//
// The whole method is one BEGIN IMMEDIATE transaction because the capacity
// check and the insert have to be a single decision. See the note on the DSN
// in Open(): with a deferred transaction, two people racing for the last place
// both read "there is room".
//
// Idempotent by design. The button is in a Discord message that stays on screen
// for days, and people press it twice. A second press by someone who already
// holds a place returns that place unchanged, writes no history row, and does
// not cost them their position.
// via is how they arrived — one of JoinedViaButton, JoinedViaInterested or
// JoinedViaOperator. It is stored rather than inferred because it changes what
// a later Discord signal is allowed to do to this row.
func (s *Store) Join(eventID int64, discordUserID, displayName, via string) (*JoinResult, error) {
	discordUserID = strings.TrimSpace(discordUserID)
	if via == "" {
		via = JoinedViaButton
	}
	if !validJoinedVia[via] {
		return nil, fmt.Errorf("%w: joined_via %q is not one of %v",
			ErrInvalidEvent, via, ValidJoinedVia())
	}
	if discordUserID == "" {
		return nil, fmt.Errorf("%w: discord_user_id is required", ErrInvalidEvent)
	}

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
	if status != StatusOpen {
		return nil, fmt.Errorf("%w (status is %q)", ErrEventNotOpen, status)
	}

	// Does this person already have a row on this roster?
	var existing Signup
	err = tx.QueryRow(`
		SELECT id, event_id, discord_user_id, display_name, state, signed_up_at,
		       state_changed_at, joined_via, discord_interested
		FROM signups WHERE event_id = ? AND discord_user_id = ?`, eventID, discordUserID).
		Scan(&existing.ID, &existing.EventID, &existing.DiscordUserID, &existing.DisplayName,
			&existing.State, &existing.SignedUpAt, &existing.StateChangedAt,
			&existing.JoinedVia, &existing.DiscordInterested)
	hasExisting := !errors.Is(err, sql.ErrNoRows)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("load existing signup: %w", err)
	}

	if hasExisting && existing.State != StateWithdrawn {
		// Already holds this place. Nothing to do, and deliberately no history
		// row — a double click is not an event.
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit: %w", err)
		}
		if err := s.fillWaitlistPlace(&existing); err != nil {
			return nil, err
		}
		return &JoinResult{Signup: existing, AlreadySignedUp: true}, nil
	}

	var attending int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM signups WHERE event_id = ? AND state = ?`,
		eventID, StateAttending).Scan(&attending); err != nil {
		return nil, fmt.Errorf("count attending: %w", err)
	}

	// capacity 0 means unlimited, matching Discord's own convention.
	newState := StateAttending
	action := ActionJoined
	if capacity > 0 && attending >= capacity {
		newState = StateWaitlisted
		action = ActionWaitlisted
	}

	ts := now()
	var signupID int64

	if hasExisting {
		// Re-joining after withdrawing puts you at the back. Keeping your old
		// arrival would let someone hold a place in a full event by leaving and
		// rejoining ahead of people who waited.
		//
		// The old row is DELETED and a new one inserted, rather than updated in
		// place, and that is what makes arrival order derivable at all. Order is
		// (signed_up_at, id): signed_up_at is only accurate to the second, so id
		// breaks ties — and an updated row keeps its old, lower id, which would
		// sort a rejoiner AHEAD of somebody who never left when both land in the
		// same second. A new arrival gets a new id, so the tiebreak points the
		// right way. discord_interested is carried across because it is a fact
		// about Discord's list, not about this row.
		if action == ActionJoined {
			action = ActionRejoined
		}
		if _, err = tx.Exec(`DELETE FROM signups WHERE id = ?`, existing.ID); err != nil {
			return nil, fmt.Errorf("clear withdrawn signup: %w", err)
		}
		res, err := tx.Exec(`
			INSERT INTO signups (event_id, discord_user_id, display_name, state,
			                     signed_up_at, state_changed_at, joined_via, discord_interested)
			VALUES (?,?,?,?,?,?,?,?)`,
			eventID, discordUserID, displayName, newState, ts, ts, via,
			boolToInt(existing.DiscordInterested))
		if err != nil {
			return nil, fmt.Errorf("reactivate signup: %w", err)
		}
		if signupID, err = res.LastInsertId(); err != nil {
			return nil, fmt.Errorf("read reactivated id: %w", err)
		}
	} else {
		res, err := tx.Exec(`
			INSERT INTO signups (event_id, discord_user_id, display_name, state,
			                     signed_up_at, state_changed_at, joined_via)
			VALUES (?,?,?,?,?,?,?)`,
			eventID, discordUserID, displayName, newState, ts, ts, via)
		if err != nil {
			return nil, fmt.Errorf("insert signup: %w", err)
		}
		signupID, err = res.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("read inserted id: %w", err)
		}
	}

	fromState := ""
	if hasExisting {
		fromState = existing.State
	}
	if err := logSignupUpdate(tx, eventID, discordUserID, action, fromState, newState, ActorUser, ts); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	out := Signup{
		ID: signupID, EventID: eventID, DiscordUserID: discordUserID, DisplayName: displayName,
		State: newState, SignedUpAt: ts, StateChangedAt: ts, JoinedVia: via,
	}
	if err := s.fillWaitlistPlace(&out); err != nil {
		return nil, err
	}
	return &JoinResult{Signup: out}, nil
}

// Leave takes someone off the roster and, if that freed a place, promotes the
// person who has been waiting longest.
//
// One transaction for the same reason Join is: the withdrawal and the promotion
// are one decision, and a crash between them would leave a place empty with a
// queue in front of it.
//
// actor names who caused this — ActorUser for a button press, or an operator
// name for an API override. It is recorded on the withdrawal but never on the
// promotion, which is always ActorPromotion.
func (s *Store) Leave(eventID int64, discordUserID, actor string) (*LeaveResult, error) {
	if actor == "" {
		actor = ActorUser
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	var capacity int
	err = tx.QueryRow(`SELECT capacity FROM events WHERE id = ? AND deleted_at = 0`, eventID).Scan(&capacity)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load event: %w", err)
	}

	var leaver Signup
	err = tx.QueryRow(`
		SELECT id, event_id, discord_user_id, display_name, state, signed_up_at,
		       state_changed_at, joined_via, discord_interested
		FROM signups WHERE event_id = ? AND discord_user_id = ?`, eventID, discordUserID).
		Scan(&leaver.ID, &leaver.EventID, &leaver.DiscordUserID, &leaver.DisplayName,
			&leaver.State, &leaver.SignedUpAt, &leaver.StateChangedAt,
			&leaver.JoinedVia, &leaver.DiscordInterested)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load signup: %w", err)
	}
	if leaver.State == StateWithdrawn {
		return nil, ErrNotFound
	}

	ts := now()
	wasAttending := leaver.State == StateAttending

	if _, err := tx.Exec(`UPDATE signups SET state = ?, state_changed_at = ? WHERE id = ?`,
		StateWithdrawn, ts, leaver.ID); err != nil {
		return nil, fmt.Errorf("withdraw signup: %w", err)
	}
	if err := logSignupUpdate(tx, eventID, discordUserID, ActionWithdrew, leaver.State, StateWithdrawn,
		actor, ts); err != nil {
		return nil, err
	}
	leaver.State = StateWithdrawn
	leaver.StateChangedAt = ts

	result := &LeaveResult{Signup: leaver}

	// Only an attending person leaving frees a place. Someone abandoning the
	// waitlist promotes nobody.
	if wasAttending && capacity > 0 {
		var next Signup
		err := tx.QueryRow(`
			SELECT id, event_id, discord_user_id, display_name, state, signed_up_at,
			       state_changed_at, joined_via, discord_interested
			FROM signups WHERE event_id = ? AND state = ?
			ORDER BY signed_up_at ASC, id ASC LIMIT 1`, eventID, StateWaitlisted).
			Scan(&next.ID, &next.EventID, &next.DiscordUserID, &next.DisplayName,
				&next.State, &next.SignedUpAt, &next.StateChangedAt,
				&next.JoinedVia, &next.DiscordInterested)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("find next in line: %w", err)
		}
		if err == nil {
			if _, err := tx.Exec(`UPDATE signups SET state = ?, state_changed_at = ? WHERE id = ?`,
				StateAttending, ts, next.ID); err != nil {
				return nil, fmt.Errorf("promote signup: %w", err)
			}
			if err := logSignupUpdate(tx, eventID, next.DiscordUserID, ActionPromoted, StateWaitlisted,
				StateAttending, ActorPromotion, ts); err != nil {
				return nil, err
			}
			next.State = StateAttending
			next.StateChangedAt = ts
			result.Promoted = &next
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return result, nil
}

// Roster returns the people on an event, attending first and then the waitlist
// in the order they will be promoted. Withdrawn rows are excluded unless
// includeWithdrawn is set.
func (s *Store) Roster(eventID int64, includeWithdrawn bool) ([]Signup, error) {
	query := `
		SELECT id, event_id, discord_user_id, display_name, state, signed_up_at,
		       state_changed_at, joined_via, discord_interested
		FROM signups WHERE event_id = ?`
	args := []any{eventID}
	if !includeWithdrawn {
		query += ` AND state != ?`
		args = append(args, StateWithdrawn)
	}
	// Attending before waitlisted before withdrawn, then arrival order within
	// each. CASE rather than alphabetical: 'attending' < 'waitlisted' happens
	// to sort correctly today and would break the moment a state is renamed.
	query += `
		ORDER BY CASE state WHEN 'attending' THEN 0 WHEN 'waitlisted' THEN 1 ELSE 2 END,
		         signed_up_at ASC, id ASC`

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("read roster: %w", err)
	}
	defer rows.Close()

	out := []Signup{}
	place := 0
	for rows.Next() {
		var sg Signup
		if err := rows.Scan(&sg.ID, &sg.EventID, &sg.DiscordUserID, &sg.DisplayName,
			&sg.State, &sg.SignedUpAt, &sg.StateChangedAt,
			&sg.JoinedVia, &sg.DiscordInterested); err != nil {
			return nil, fmt.Errorf("scan signup: %w", err)
		}
		if sg.State == StateWaitlisted {
			place++
			sg.WaitlistPlace = place
		}
		out = append(out, sg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate roster: %w", err)
	}
	return out, nil
}

// History returns the append-only signup update log for an event, oldest first.
// This is the record Discord does not keep.
func (s *Store) History(eventID int64, limit int) ([]SignupUpdate, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	// LEFT JOIN, not JOIN: every signup update is written alongside a signup row
	// today, but a missing one must show the id rather than drop the row out of
	// the history entirely. An audit log that silently omits entries is worse
	// than one with an ugly name in it.
	rows, err := s.db.Query(`
		SELECT t.id, t.event_id, t.discord_user_id, t.action, t.from_state, t.to_state,
		       t.actor, t.at, COALESCE(s.display_name, '')
		FROM signup_updates t
		LEFT JOIN signups s ON s.event_id = t.event_id AND s.discord_user_id = t.discord_user_id
		WHERE t.event_id = ? ORDER BY t.at ASC, t.id ASC LIMIT ?`, eventID, limit)
	if err != nil {
		return nil, fmt.Errorf("read history: %w", err)
	}
	defer rows.Close()

	out := []SignupUpdate{}
	for rows.Next() {
		var t SignupUpdate
		if err := rows.Scan(&t.ID, &t.EventID, &t.DiscordUserID, &t.Action, &t.FromState,
			&t.ToState, &t.Actor, &t.At, &t.DisplayName); err != nil {
			return nil, fmt.Errorf("scan signup update: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate history: %w", err)
	}
	return out, nil
}

// fillWaitlistPlace computes how many waitlisted people sit ahead of this one.
func (s *Store) fillWaitlistPlace(sg *Signup) error {
	if sg.State != StateWaitlisted {
		sg.WaitlistPlace = 0
		return nil
	}
	var ahead int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM signups
		WHERE event_id = ? AND state = ?
		  AND (signed_up_at < ? OR (signed_up_at = ? AND id < ?))`,
		sg.EventID, StateWaitlisted, sg.SignedUpAt, sg.SignedUpAt, sg.ID).Scan(&ahead)
	if err != nil {
		return fmt.Errorf("count waitlist ahead: %w", err)
	}
	sg.WaitlistPlace = ahead + 1
	return nil
}

func logSignupUpdate(tx *sql.Tx, eventID int64, userID, action, from, to, actor string, at int64) error {
	if !validActions[action] {
		return fmt.Errorf("refusing to log unknown action %q (known: %v)", action, ValidActions())
	}
	_, err := tx.Exec(`
		INSERT INTO signup_updates (event_id, discord_user_id, action, from_state, to_state, actor, at)
		VALUES (?,?,?,?,?,?,?)`, eventID, userID, action, from, to, actor, at)
	if err != nil {
		return fmt.Errorf("log signup update: %w", err)
	}
	return nil
}

// UserIDsMissingDisplayName lists people on an event whose name was never
// captured — added through the API by id, or seen over the gateway when the
// member lookup failed. These are the rows that render as a raw snowflake.
func (s *Store) UserIDsMissingDisplayName(eventID int64) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT discord_user_id FROM signups WHERE event_id = ? AND display_name = ''`, eventID)
	if err != nil {
		return nil, fmt.Errorf("find missing display names: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan user id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SetDisplayName records how someone appears in the server.
//
// Display only, and overwritten freely: it is never a key and nothing joins on
// it, so a rename is just a better spelling of the same row.
func (s *Store) SetDisplayName(eventID int64, discordUserID, displayName string) error {
	if displayName == "" {
		return nil
	}
	_, err := s.db.Exec(
		`UPDATE signups SET display_name = ? WHERE event_id = ? AND discord_user_id = ?`,
		displayName, eventID, discordUserID)
	if err != nil {
		return fmt.Errorf("set display name: %w", err)
	}
	return nil
}

// UserSignup is one person's standing on one event, for the "My events" reply.
type UserSignup struct {
	Event  Event
	Signup Signup
}

// UserSignupsInGuild lists every live event in a guild the person is currently
// on — attending or waitlisted, never withdrawn.
//
// This exists for the one thing a shared message cannot do: a channel message
// renders identically for every viewer, so "which of these am I in?" can only
// be answered per person, in an ephemeral reply.
func (s *Store) UserSignupsInGuild(guildID, discordUserID string) ([]UserSignup, error) {
	rows, err := s.db.Query(`
		SELECT e.id, e.name, e.capacity, e.status, e.starts_at, e.timezone,
		       sg.id, sg.state
		FROM signups sg
		JOIN events e ON e.id = sg.event_id
		WHERE e.guild_id = ? AND sg.discord_user_id = ?
		  AND sg.state != ? AND e.deleted_at = 0
		ORDER BY e.starts_at ASC`,
		guildID, discordUserID, StateWithdrawn)
	if err != nil {
		return nil, fmt.Errorf("list user signups: %w", err)
	}
	defer rows.Close()

	var out []UserSignup
	for rows.Next() {
		var u UserSignup
		if err := rows.Scan(&u.Event.ID, &u.Event.Name, &u.Event.Capacity, &u.Event.Status,
			&u.Event.StartsAt, &u.Event.Timezone,
			&u.Signup.ID, &u.Signup.State); err != nil {
			return nil, fmt.Errorf("scan user signup: %w", err)
		}
		u.Signup.EventID = u.Event.ID
		u.Signup.DiscordUserID = discordUserID
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate user signups: %w", err)
	}
	// Archived events are excluded here rather than in SQL so the definition
	// of "over" stays in one place, vocabulary.go.
	live := out[:0]
	for _, u := range out {
		if IsArchived(u.Event.Status) {
			continue
		}
		if u.Signup.State == StateWaitlisted {
			if err := s.fillWaitlistPlace(&u.Signup); err != nil {
				return nil, err
			}
		}
		live = append(live, u)
	}
	return live, nil
}

// boolToInt writes a Go bool into the integer column SQLite keeps it in.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
