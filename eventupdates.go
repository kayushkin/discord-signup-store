package discordsignup

import (
	"fmt"
	"strconv"
)

// EventUpdate is one field of one event changing, once.
type EventUpdate struct {
	ID        int64  `json:"id"`
	EventID   int64  `json:"event_id"`
	Field     string `json:"field"`
	FromValue string `json:"from_value"`
	ToValue   string `json:"to_value"`
	Actor     string `json:"actor"`
	At        int64  `json:"at"`
}

// loggedEventFields are the fields a person can change and would ask about
// later.
//
// A list rather than reflection over the struct, because the point is that most
// of an event row is NOT this: message ids, thread and forum ids, the publish
// signature and the reminder stamps all change without anybody editing
// anything, and a history that recorded them would bury the four rows somebody
// cares about under a hundred nobody does.
var loggedEventFields = []struct {
	name  string
	value func(*Event) string
}{
	{"name", func(e *Event) string { return e.Name }},
	{"description", func(e *Event) string { return e.Description }},
	{"capacity", func(e *Event) string { return strconv.Itoa(e.Capacity) }},
	{"status", func(e *Event) string { return e.Status }},
	{"starts_at", func(e *Event) string { return strconv.FormatInt(e.StartsAt, 10) }},
	{"ends_at", func(e *Event) string { return strconv.FormatInt(e.EndsAt, 10) }},
	{"location", func(e *Event) string { return e.Location }},
	{"timezone", func(e *Event) string { return e.Timezone }},
	{"recurrence_rule", func(e *Event) string { return e.RecurrenceRule }},
	{"attending_role_id", func(e *Event) string { return e.AttendingRoleID }},
	{"waitlist_role_id", func(e *Event) string { return e.WaitlistRoleID }},
}

// LogEventUpdates records what changed between two readings of an event.
//
// Values are stored raw — a time as the integer it is stored as, not a
// rendering of it — so the row means the same thing whoever reads it later and
// in whatever zone. Presentation belongs at the edge.
func (s *Store) LogEventUpdates(before, after *Event, actor string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	ts := now()
	for _, f := range loggedEventFields {
		from, to := f.value(before), f.value(after)
		if from == to {
			continue
		}
		if _, err := tx.Exec(`
			INSERT INTO event_updates (event_id, field, from_value, to_value, actor, at)
			VALUES (?,?,?,?,?,?)`, after.ID, f.name, from, to, actor, ts); err != nil {
			return fmt.Errorf("log event update %s: %w", f.name, err)
		}
	}
	return tx.Commit()
}

// EventUpdates is one event's history of edits, oldest first.
func (s *Store) EventUpdates(eventID int64) ([]EventUpdate, error) {
	rows, err := s.db.Query(`
		SELECT id, event_id, field, from_value, to_value, actor, at
		FROM event_updates WHERE event_id = ? ORDER BY at ASC, id ASC`, eventID)
	if err != nil {
		return nil, fmt.Errorf("list event updates: %w", err)
	}
	defer rows.Close()

	out := []EventUpdate{}
	for rows.Next() {
		var u EventUpdate
		if err := rows.Scan(&u.ID, &u.EventID, &u.Field, &u.FromValue,
			&u.ToValue, &u.Actor, &u.At); err != nil {
			return nil, fmt.Errorf("scan event update: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate event updates: %w", err)
	}
	return out, nil
}
