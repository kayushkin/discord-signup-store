package discordsignup

import (
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"
)

// Organisers of one event. Whoever may edit an event — its creator, the
// server's editors, or an organiser added here — can make someone else an
// organiser of it, from their row on the event page, and take that back.
// An organiser may do on that event whatever its creator may, and nothing on
// any other event. The pages mark organisers with a badge.

// EventOrganiser is someone made an organiser of an event.
type EventOrganiser struct {
	DiscordUserID string `json:"discord_user_id"`
	AddedAt       int64  `json:"added_at"`
	AddedBy       string `json:"added_by"`
	RemovedAt     int64  `json:"removed_at"`
	RemovedBy     string `json:"removed_by"`
	// DisplayName and ReadableName are display only.
	DisplayName  string `json:"display_name"`
	ReadableName string `json:"readable_name"`
}

// IsEventOrganiser says whether someone was made an organiser of an event.
func (s *Store) IsEventOrganiser(eventID int64, userID string) (bool, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM event_organisers
		WHERE event_id = ? AND discord_user_id = ? AND removed_at = 0`, eventID, userID).Scan(&n); err != nil {
		return false, fmt.Errorf("read event organisers: %w", err)
	}
	return n > 0, nil
}

// EventOrganisers is everyone ever made an organiser of an event, oldest
// first, with when it was taken back.
func (s *Store) EventOrganisers(eventID int64) ([]EventOrganiser, error) {
	rows, err := s.db.Query(`
		SELECT o.discord_user_id, o.added_at, o.added_by, o.removed_at, o.removed_by,
		       COALESCE(s.display_name, ''), COALESCE(r.readable_name, '')
		FROM event_organisers o
		LEFT JOIN signups s ON s.event_id = o.event_id AND s.discord_user_id = o.discord_user_id
		LEFT JOIN readable_names r ON r.discord_user_id = o.discord_user_id
		WHERE o.event_id = ? ORDER BY o.added_at, o.id`, eventID)
	if err != nil {
		return nil, fmt.Errorf("read event organisers: %w", err)
	}
	defer rows.Close()
	var out []EventOrganiser
	for rows.Next() {
		var o EventOrganiser
		if err := rows.Scan(&o.DiscordUserID, &o.AddedAt, &o.AddedBy, &o.RemovedAt, &o.RemovedBy,
			&o.DisplayName, &o.ReadableName); err != nil {
			return nil, fmt.Errorf("scan event organiser: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// AddEventOrganiser makes someone an organiser of an event. Making the
// creator, or someone already one, an organiser is refused.
func (s *Store) AddEventOrganiser(ev *Event, userID, actor string) error {
	if userID == "" {
		return fmt.Errorf("%w: pick someone", ErrInvalidEvent)
	}
	if userID == ev.CreatedBy {
		return fmt.Errorf("%w: they host it, so they already are one", ErrInvalidEvent)
	}
	if already, err := s.IsEventOrganiser(ev.ID, userID); err != nil {
		return err
	} else if already {
		return fmt.Errorf("%w: they already are one", ErrInvalidEvent)
	}
	if _, err := s.db.Exec(`INSERT INTO event_organisers (event_id, discord_user_id, added_at, added_by) VALUES (?,?,?,?)`,
		ev.ID, userID, now(), actor); err != nil {
		return fmt.Errorf("add event organiser: %w", err)
	}
	return nil
}

// RemoveEventOrganiser takes back someone's being an organiser of an event.
func (s *Store) RemoveEventOrganiser(eventID int64, userID, actor string) error {
	res, err := s.db.Exec(`UPDATE event_organisers SET removed_at = ?, removed_by = ?
		WHERE event_id = ? AND discord_user_id = ? AND removed_at = 0`, now(), actor, eventID, userID)
	if err != nil {
		return fmt.Errorf("remove event organiser: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// handleWebEventOrganiser makes someone an organiser of the event, or takes
// it back (organiser=false), for anyone who may edit it.
func (s *Server) handleWebEventOrganiser(w http.ResponseWriter, r *http.Request) {
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
	userID := strings.TrimSpace(r.FormValue("discord_user_id"))
	actor := "web:" + session.DiscordUserID
	if r.FormValue("organiser") == "false" {
		err := s.store.RemoveEventOrganiser(ev.ID, userID, actor)
		if errors.Is(err, ErrNotFound) {
			s.redirectWithNotice(w, r, ev.ID, "They were not an organiser.")
			return
		}
		if err != nil {
			log.Printf("[discord-signup] remove organiser %s of %d: %v", userID, ev.ID, err)
			s.redirectWithNotice(w, r, ev.ID, "Nothing was changed: "+err.Error())
			return
		}
		s.redirectWithNotice(w, r, ev.ID, "No longer an organiser.")
		return
	}
	if err := s.store.AddEventOrganiser(ev, userID, actor); err != nil {
		if errors.Is(err, ErrInvalidEvent) {
			s.redirectWithNotice(w, r, ev.ID, "Nothing was changed: "+plainError(err)+".")
			return
		}
		log.Printf("[discord-signup] add organiser %s of %d: %v", userID, ev.ID, err)
		s.redirectWithNotice(w, r, ev.ID, "Nothing was changed: "+err.Error())
		return
	}
	s.redirectWithNotice(w, r, ev.ID, "Made an organiser of this event.")
}

// organiserLogEntries are organisers added and taken back, in the log.
func organiserLogEntries(organisers []EventOrganiser, names eventLogNames) []eventLogEntry {
	var out []eventLogEntry
	for i, o := range organisers {
		subject := names.person(o.ReadableName, o.DisplayName, o.DiscordUserID)
		out = append(out, eventLogEntry{At: o.AddedAt, order: 9_000_000 + i, Subject: subject,
			What: template.HTML("made an organiser"), By: names.actor(o.AddedBy)})
		if o.RemovedAt > 0 {
			out = append(out, eventLogEntry{At: o.RemovedAt, order: 10_000_000 + i, Subject: subject,
				What: template.HTML("no longer an organiser"), By: names.actor(o.RemovedBy)})
		}
	}
	return out
}

// organiserIDsOf is who gets the organiser badge: the host, and everyone
// made an organiser and not taken back.
func organiserIDsOf(ev *Event, organisers []EventOrganiser) map[string]bool {
	ids := map[string]bool{}
	if ev.CreatedBy != "" {
		ids[ev.CreatedBy] = true
	}
	for _, o := range organisers {
		if o.RemovedAt == 0 {
			ids[o.DiscordUserID] = true
		}
	}
	return ids
}
