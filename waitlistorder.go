package discordsignup

import (
	"errors"
	"fmt"
)

// The waitlist's order: arrival, unless an organiser moved people. Arrival is
// (signed_up_at, id) and cannot be recovered from anywhere else; a move does
// not rewrite it — the roster still says when each person signed up — but
// ranks the line in waitlist_rank, 1 at the front. Anyone joining the line
// after a move has rank 0 and waits behind every ranked person, who all
// arrived before them. Rejoining is a new row, so it starts at rank 0 too.

// waitlistOrder is the ORDER BY for a waitlist, over a table alias prefix
// ("s." or ""). Every query that says who is next goes through it, so the
// line cannot be one order on the page and another at promotion.
func waitlistOrder(alias string) string {
	return fmt.Sprintf("(%[1]swaitlist_rank = 0), %[1]swaitlist_rank, %[1]ssigned_up_at ASC, %[1]sid ASC", alias)
}

// ErrNotWaitlisted is a move asked for someone who is not in the line.
var ErrNotWaitlisted = errors.New("they are not on the waitlist")

// MoveInWaitlist puts someone at a place in an event's waitlist, 1 at the
// front, and ranks the whole line in its new order. A place past either end
// is the end. actor names the organiser, for the history.
func (s *Store) MoveInWaitlist(eventID int64, discordUserID string, toPlace int, actor string) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT id, discord_user_id FROM signups WHERE event_id = ? AND state = ?
		ORDER BY `+waitlistOrder(""), eventID, StateWaitlisted)
	if err != nil {
		return 0, fmt.Errorf("read waitlist: %w", err)
	}
	type waiting struct {
		id     int64
		userID string
	}
	var line []waiting
	from := -1
	for rows.Next() {
		var w waiting
		if err := rows.Scan(&w.id, &w.userID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan waitlist: %w", err)
		}
		if w.userID == discordUserID {
			from = len(line)
		}
		line = append(line, w)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("read waitlist: %w", err)
	}
	if from < 0 {
		return 0, ErrNotWaitlisted
	}
	to := min(max(toPlace, 1), len(line)) - 1
	if to == from {
		return from + 1, tx.Commit()
	}
	moving := line[from]
	line = append(line[:from], line[from+1:]...)
	line = append(line[:to], append([]waiting{moving}, line[to:]...)...)
	for rank, w := range line {
		if _, err := tx.Exec(`UPDATE signups SET waitlist_rank = ? WHERE id = ?`, rank+1, w.id); err != nil {
			return 0, fmt.Errorf("rank waitlist: %w", err)
		}
	}
	if err := logSignupUpdate(tx, eventID, discordUserID, ActionMoved, StateWaitlisted, StateWaitlisted, actor, now()); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return to + 1, nil
}
