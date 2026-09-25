package discordsignup

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// Pins: people who get a place on every date of a recurring event. The roster
// is per date — when an occurrence ends, everyone on it is withdrawn — so a
// regular would otherwise have to join again each week. A pin puts them back
// on as going each time the date rolls over, past the limit if it comes to
// that, as an organiser's Going does. The host is pinned the first time the
// event repeats, and can be unpinned like anyone.

// ActorPin is the actor recorded when a pin puts someone on a new date.
const ActorPin = "pin"

// EventPin is one person pinned to one event.
type EventPin struct {
	ID            int64  `json:"id"`
	EventID       int64  `json:"event_id"`
	DiscordUserID string `json:"discord_user_id"`
	DisplayName   string `json:"display_name"`
	// ReadableName is the short name set for them, joined on read.
	ReadableName string `json:"readable_name,omitempty"`
	PinnedBy     string `json:"pinned_by"`
	PinnedAt     int64  `json:"pinned_at"`
	UnpinnedAt   int64  `json:"unpinned_at"`
	UnpinnedBy   string `json:"unpinned_by"`
}

// Pins lists every pin an event has had, live and ended, oldest first.
func (s *Store) Pins(eventID int64) ([]EventPin, error) {
	rows, err := s.db.Query(`
		SELECT p.id, p.event_id, p.discord_user_id, p.display_name, COALESCE(r.readable_name, ''),
		       p.pinned_by, p.pinned_at, p.unpinned_at, p.unpinned_by
		FROM event_pins p LEFT JOIN readable_names r ON r.discord_user_id = p.discord_user_id
		WHERE p.event_id = ? ORDER BY p.pinned_at, p.id`, eventID)
	if err != nil {
		return nil, fmt.Errorf("list pins: %w", err)
	}
	defer rows.Close()
	out := []EventPin{}
	for rows.Next() {
		var p EventPin
		if err := rows.Scan(&p.ID, &p.EventID, &p.DiscordUserID, &p.DisplayName, &p.ReadableName,
			&p.PinnedBy, &p.PinnedAt, &p.UnpinnedAt, &p.UnpinnedBy); err != nil {
			return nil, fmt.Errorf("scan pin: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Pin pins someone to an event. Already pinned is no change and reports
// false.
func (s *Store) Pin(eventID int64, userID, displayName, actor string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	pinned, err := pinTx(tx, eventID, userID, displayName, actor, now())
	if err != nil {
		return false, err
	}
	return pinned, tx.Commit()
}

func pinTx(tx *sql.Tx, eventID int64, userID, displayName, actor string, ts int64) (bool, error) {
	var live int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM event_pins WHERE event_id = ? AND discord_user_id = ? AND unpinned_at = 0`,
		eventID, userID).Scan(&live); err != nil {
		return false, fmt.Errorf("read pin: %w", err)
	}
	if live > 0 {
		return false, nil
	}
	if _, err := tx.Exec(`INSERT INTO event_pins (event_id, discord_user_id, display_name, pinned_by, pinned_at)
		VALUES (?,?,?,?,?)`, eventID, userID, displayName, actor, ts); err != nil {
		return false, fmt.Errorf("pin: %w", err)
	}
	return true, nil
}

// Unpin ends someone's pin. Their place on the current date stays; they are
// not put back on the next one. ErrNotFound when they were not pinned.
func (s *Store) Unpin(eventID int64, userID, actor string) error {
	res, err := s.db.Exec(`UPDATE event_pins SET unpinned_at = ?, unpinned_by = ?
		WHERE event_id = ? AND discord_user_id = ? AND unpinned_at = 0`, now(), actor, eventID, userID)
	if err != nil {
		return fmt.Errorf("unpin: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// pinHostTx pins a recurring event's host if they have never had a pin on it.
// An unpin is a pin row too, so a host who was unpinned stays unpinned.
func pinHostTx(tx *sql.Tx, eventID int64, ts int64) error {
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
		return fmt.Errorf("read host pin: %w", err)
	}
	if ever > 0 {
		return nil
	}
	_, err = pinTx(tx, eventID, host, name, ActorPin, ts)
	return err
}

// PinHost pins an event's host, once: when it is made recurring.
func (s *Store) PinHost(eventID int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	if err := pinHostTx(tx, eventID, now()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) pinHostsOfRecurringEvents() error {
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
		if err := s.PinHost(id); err != nil {
			return fmt.Errorf("pin host of event %d: %w", id, err)
		}
	}
	return nil
}

// seatPinnedTx puts everyone with a live pin on as going, inside the
// rollover's transaction, after the roster has cleared. Each is a new row,
// arriving at the start of the date; the limit is not checked.
func seatPinnedTx(tx *sql.Tx, eventID int64, ts int64) ([]Signup, error) {
	rows, err := tx.Query(`SELECT discord_user_id, display_name FROM event_pins
		WHERE event_id = ? AND unpinned_at = 0 ORDER BY pinned_at, id`, eventID)
	if err != nil {
		return nil, fmt.Errorf("read pins: %w", err)
	}
	type pin struct{ userID, name string }
	var pins []pin
	for rows.Next() {
		var p pin
		if err := rows.Scan(&p.userID, &p.name); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan pin: %w", err)
		}
		pins = append(pins, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var seated []Signup
	for _, p := range pins {
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
				return nil, fmt.Errorf("clear signup of pinned %s: %w", p.userID, err)
			}
		}
		res, err := tx.Exec(`
			INSERT INTO signups (event_id, discord_user_id, display_name, state,
			                     signed_up_at, state_changed_at, joined_via, discord_interested)
			VALUES (?,?,?,?,?,?,?,?)`,
			eventID, p.userID, name, StateAttending, ts, ts, JoinedViaPinned, boolToInt(interested))
		if err != nil {
			return nil, fmt.Errorf("seat pinned %s: %w", p.userID, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("read seated id: %w", err)
		}
		if err := logSignupUpdate(tx, eventID, p.userID, ActionAdded, from, StateAttending, ActorPin, ts); err != nil {
			return nil, err
		}
		seated = append(seated, Signup{ID: id, EventID: eventID, DiscordUserID: p.userID, DisplayName: name,
			State: StateAttending, SignedUpAt: ts, StateChangedAt: ts, JoinedVia: JoinedViaPinned,
			DiscordInterested: interested})
	}
	return seated, nil
}

// handleWebPin pins or unpins someone (pinned=true or false). Pinning also
// puts them on the current date as going, if they are not already.
func (s *Server) handleWebPin(w http.ResponseWriter, r *http.Request) {
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
		s.redirectWithNotice(w, r, ev.ID, "Only a repeating event has pins: there is no next date to keep a place on.")
		return
	}
	userID := strings.TrimSpace(r.FormValue("discord_user_id"))
	actor := "web:" + session.DiscordUserID
	if r.FormValue("pinned") != "true" {
		err := s.store.Unpin(ev.ID, userID, actor)
		if errors.Is(err, ErrNotFound) {
			s.redirectWithNotice(w, r, ev.ID, "They were not pinned.")
			return
		}
		if err != nil {
			log.Printf("[discord-signup] unpin %s on %d: %v", userID, ev.ID, err)
			http.Error(w, "could not unpin: "+err.Error(), http.StatusInternalServerError)
			return
		}
		s.redirectWithNotice(w, r, ev.ID, "Unpinned. They keep this date's place, and are not put on the next one.")
		return
	}
	s.redirectWithNotice(w, r, ev.ID, s.pinPerson(ev, session, userID))
}

// pinPerson pins one person and puts them on the current date as going, and
// says what happened in a sentence.
func (s *Server) pinPerson(ev *Event, session *WebSession, userID string) string {
	placed := s.placePerson(ev, session, userID, StateAttending)
	name := userID
	if roster, err := s.store.Roster(ev.ID, false); err == nil {
		for _, sg := range roster {
			if sg.DiscordUserID == userID {
				name = sg.NameOnDiscord()
			}
		}
	}
	pinned, err := s.store.Pin(ev.ID, userID, name, "web:"+session.DiscordUserID)
	if err != nil {
		log.Printf("[discord-signup] pin %s on %d: %v", userID, ev.ID, err)
		return placed + " Could not pin " + name + ": " + err.Error() + "."
	}
	if !pinned {
		return placed + " " + name + " was already pinned."
	}
	return placed + " " + name + " is pinned: a place on every date."
}
