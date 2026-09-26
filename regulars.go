package discordsignup

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// Regulars: people who get a place on every date of a recurring event. The
// roster is per date — when an occurrence ends, everyone on it is withdrawn —
// so a regular would otherwise have to join again each week. Being a regular
// puts them back on as going each time the date rolls over, past the limit if
// it comes to that, as an organiser's Going does. The host becomes a regular
// the first time the event repeats, and can stop being one like anyone.
//
// Stored in event_pins, a name kept from when they were called pins; its
// pinned_* and unpinned_* columns are when someone became and stopped being
// a regular.

// ActorRegular is the actor recorded when being a regular puts someone on a
// new date, and when the host is made one.
const ActorRegular = "regular"

// EventRegular is one person who is, or was, a regular at one event.
type EventRegular struct {
	ID            int64  `json:"id"`
	EventID       int64  `json:"event_id"`
	DiscordUserID string `json:"discord_user_id"`
	DisplayName   string `json:"display_name"`
	// ReadableName is the short name set for them, joined on read.
	ReadableName string `json:"readable_name,omitempty"`
	AddedBy      string `json:"added_by"`
	AddedAt      int64  `json:"added_at"`
	// EndedAt is when they stopped being a regular; 0 while they are one.
	EndedAt int64  `json:"ended_at"`
	EndedBy string `json:"ended_by"`
}

// Regulars lists everyone who has been a regular at an event, current and
// past, oldest first.
func (s *Store) Regulars(eventID int64) ([]EventRegular, error) {
	rows, err := s.db.Query(`
		SELECT p.id, p.event_id, p.discord_user_id, p.display_name, COALESCE(r.readable_name, ''),
		       p.pinned_by, p.pinned_at, p.unpinned_at, p.unpinned_by
		FROM event_pins p LEFT JOIN readable_names r ON r.discord_user_id = p.discord_user_id
		WHERE p.event_id = ? ORDER BY p.pinned_at, p.id`, eventID)
	if err != nil {
		return nil, fmt.Errorf("list regulars: %w", err)
	}
	defer rows.Close()
	out := []EventRegular{}
	for rows.Next() {
		var p EventRegular
		if err := rows.Scan(&p.ID, &p.EventID, &p.DiscordUserID, &p.DisplayName, &p.ReadableName,
			&p.AddedBy, &p.AddedAt, &p.EndedAt, &p.EndedBy); err != nil {
			return nil, fmt.Errorf("scan regular: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MakeRegular makes someone a regular at an event. Already one is no change
// and reports false.
func (s *Store) MakeRegular(eventID int64, userID, displayName, actor string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	made, err := makeRegularTx(tx, eventID, userID, displayName, actor, now())
	if err != nil {
		return false, err
	}
	return made, tx.Commit()
}

func makeRegularTx(tx *sql.Tx, eventID int64, userID, displayName, actor string, ts int64) (bool, error) {
	var current int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM event_pins WHERE event_id = ? AND discord_user_id = ? AND unpinned_at = 0`,
		eventID, userID).Scan(&current); err != nil {
		return false, fmt.Errorf("read regular: %w", err)
	}
	if current > 0 {
		return false, nil
	}
	if _, err := tx.Exec(`INSERT INTO event_pins (event_id, discord_user_id, display_name, pinned_by, pinned_at)
		VALUES (?,?,?,?,?)`, eventID, userID, displayName, actor, ts); err != nil {
		return false, fmt.Errorf("make regular: %w", err)
	}
	return true, nil
}

// EndRegular stops someone being a regular. Their place on the current date
// stays; they are not put back on the next one. ErrNotFound when they were
// not a regular.
func (s *Store) EndRegular(eventID int64, userID, actor string) error {
	res, err := s.db.Exec(`UPDATE event_pins SET unpinned_at = ?, unpinned_by = ?
		WHERE event_id = ? AND discord_user_id = ? AND unpinned_at = 0`, now(), actor, eventID, userID)
	if err != nil {
		return fmt.Errorf("end regular: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// makeHostRegularTx makes a recurring event's host a regular if they have
// never been one there. Stopping is a row too, so a host who stopped being a
// regular is not made one again.
func makeHostRegularTx(tx *sql.Tx, eventID int64, ts int64) error {
	var host, rule, name string
	err := tx.QueryRow(`SELECT e.created_by, e.recurrence_rule,
		       COALESCE((SELECT display_name FROM signups WHERE event_id = e.id AND discord_user_id = e.created_by), '')
		FROM events e WHERE e.id = ?`, eventID).Scan(&host, &rule, &name)
	if err != nil {
		return fmt.Errorf("load event host: %w", err)
	}
	if host == "" || rule == "" {
		return nil
	}
	var ever int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM event_pins WHERE event_id = ? AND discord_user_id = ?`,
		eventID, host).Scan(&ever); err != nil {
		return fmt.Errorf("read host regular: %w", err)
	}
	if ever > 0 {
		return nil
	}
	_, err = makeRegularTx(tx, eventID, host, name, ActorRegular, ts)
	return err
}

// MakeHostRegular makes an event's host a regular, once: when it repeats.
func (s *Store) MakeHostRegular(eventID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	if err := makeHostRegularTx(tx, eventID, now()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) makeHostsOfRecurringEventsRegular() error {
	rows, err := s.db.Query(`SELECT id FROM events WHERE deleted_at = 0 AND recurrence_rule != '' AND created_by != ''`)
	if err != nil {
		return fmt.Errorf("list recurring events: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan event id: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.MakeHostRegular(id); err != nil {
			return fmt.Errorf("make host of event %d a regular: %w", id, err)
		}
	}
	return nil
}

// seatRegularsTx puts every current regular on as going, inside the
// rollover's transaction, after the roster has cleared. Each is a new row,
// arriving at the start of the date; the limit is not checked.
func seatRegularsTx(tx *sql.Tx, eventID int64, ts int64) ([]Signup, error) {
	rows, err := tx.Query(`SELECT discord_user_id, display_name FROM event_pins
		WHERE event_id = ? AND unpinned_at = 0 ORDER BY pinned_at, id`, eventID)
	if err != nil {
		return nil, fmt.Errorf("read regulars: %w", err)
	}
	type regular struct{ userID, name string }
	var regulars []regular
	for rows.Next() {
		var p regular
		if err := rows.Scan(&p.userID, &p.name); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan regular: %w", err)
		}
		regulars = append(regulars, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var seated []Signup
	for _, p := range regulars {
		existing, found, err := loadSignupTx(tx, eventID, p.userID)
		if err != nil {
			return nil, err
		}
		name, interested, from := p.name, false, ""
		if found {
			if existing.DisplayName != "" {
				name = existing.DisplayName
			}
			interested, from = existing.DiscordInterested, existing.State
			if _, err := tx.Exec(`DELETE FROM signups WHERE id = ?`, existing.ID); err != nil {
				return nil, fmt.Errorf("clear signup of regular %s: %w", p.userID, err)
			}
		}
		res, err := tx.Exec(`
			INSERT INTO signups (event_id, discord_user_id, display_name, state,
			                     signed_up_at, state_changed_at, joined_via, discord_interested)
			VALUES (?,?,?,?,?,?,?,?)`,
			eventID, p.userID, name, StateAttending, ts, ts, JoinedViaRegular, boolToInt(interested))
		if err != nil {
			return nil, fmt.Errorf("seat regular %s: %w", p.userID, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("read seated id: %w", err)
		}
		if err := logSignupUpdate(tx, eventID, p.userID, ActionAdded, from, StateAttending, ActorRegular, ts); err != nil {
			return nil, err
		}
		seated = append(seated, Signup{ID: id, EventID: eventID, DiscordUserID: p.userID, DisplayName: name,
			State: StateAttending, SignedUpAt: ts, StateChangedAt: ts, JoinedVia: JoinedViaRegular,
			DiscordInterested: interested})
	}
	return seated, nil
}

// renameStoredPinValues rewrites the values stored while regulars were
// called pins — joined_via "pinned" and the actor "pin" — to what the code
// now writes and reads. A no-op after the first boot.
func renameStoredPinValues(db *sql.DB) error {
	for _, stmt := range []struct{ sql, value, was string }{
		{`UPDATE signups SET joined_via = ? WHERE joined_via = ?`, JoinedViaRegular, "pinned"},
		{`UPDATE signup_updates SET actor = ? WHERE actor = ?`, ActorRegular, "pin"},
		{`UPDATE event_pins SET pinned_by = ? WHERE pinned_by = ?`, ActorRegular, "pin"},
		{`UPDATE event_pins SET unpinned_by = ? WHERE unpinned_by = ?`, ActorRegular, "pin"},
	} {
		if _, err := db.Exec(stmt.sql, stmt.value, stmt.was); err != nil {
			return fmt.Errorf("rename stored %q to %q: %w", stmt.was, stmt.value, err)
		}
	}
	return nil
}

// handleWebRegular makes someone a regular or stops them being one
// (regular=true or false). Making someone a regular also puts them on the
// current date as going, if they are not already.
func (s *Server) handleWebRegular(w http.ResponseWriter, r *http.Request) {
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
	if ev.RecurrenceRule == "" {
		s.redirectWithNotice(w, r, ev.ID, "Only a repeating event has regulars: there is no next date to keep a place on.")
		return
	}
	userID := strings.TrimSpace(r.FormValue("discord_user_id"))
	actor := "web:" + session.DiscordUserID
	if r.FormValue("regular") != "true" {
		err := s.store.EndRegular(ev.ID, userID, actor)
		if errors.Is(err, ErrNotFound) {
			s.redirectWithNotice(w, r, ev.ID, "They were not a regular.")
			return
		}
		if err != nil {
			log.Printf("[discord-signup] end regular %s on %d: %v", userID, ev.ID, err)
			http.Error(w, "could not stop them being a regular: "+err.Error(), http.StatusInternalServerError)
			return
		}
		s.redirectWithNotice(w, r, ev.ID, "No longer a regular. They keep this date's place, and are not put on the next one.")
		return
	}
	s.redirectWithNotice(w, r, ev.ID, s.makePersonRegular(ev, session, userID))
}

// makePersonRegular makes one person a regular and puts them on the current
// date as going, and says what happened in a sentence.
func (s *Server) makePersonRegular(ev *Event, session *WebSession, userID string) string {
	placed := s.placePerson(ev, session, userID, StateAttending)
	name := userID
	if roster, err := s.store.Roster(ev.ID, false); err == nil {
		for _, sg := range roster {
			if sg.DiscordUserID == userID {
				name = sg.NameOnDiscord()
			}
		}
	}
	made, err := s.store.MakeRegular(ev.ID, userID, name, "web:"+session.DiscordUserID)
	if err != nil {
		log.Printf("[discord-signup] make %s a regular on %d: %v", userID, ev.ID, err)
		return placed + " Could not make " + name + " a regular: " + err.Error() + "."
	}
	if !made {
		return placed + " " + name + " was already a regular."
	}
	return placed + " " + name + " is a regular now: a place on every date."
}
