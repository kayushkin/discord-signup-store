package discordsignup

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// DM handoffs. Some people's DMs to the bot are answered by another service —
// an agent session linked to their DM, say. Their messages are then not
// replies about an event: they are not put with one, and event pages leave
// out what they wrote while the handoff was active.

// DMHandoff is one person's DMs handed to another service.
type DMHandoff struct {
	ID            int64  `json:"id"`
	DiscordUserID string `json:"discord_user_id"`
	// HandledBy names the record that answers them, by its owner's id.
	HandledBy string `json:"handled_by"`
	Reason    string `json:"reason"`
	Since     int64  `json:"since"`
	EndedAt   int64  `json:"ended_at"`
	CreatedAt int64  `json:"created_at"`
}

// ErrDMHandoffActive: the person already has an active handoff.
var ErrDMHandoffActive = errors.New("this person's DMs are already handed off; end that handoff first")

// ErrInvalidDMHandoff: a handoff missing what it needs.
var ErrInvalidDMHandoff = errors.New("invalid dm handoff")

const dmHandoffColumns = `id, discord_user_id, handled_by, reason, since, ended_at, created_at`

func scanDMHandoff(row interface{ Scan(...any) error }) (DMHandoff, error) {
	var h DMHandoff
	err := row.Scan(&h.ID, &h.DiscordUserID, &h.HandledBy, &h.Reason, &h.Since, &h.EndedAt, &h.CreatedAt)
	return h, err
}

// CreateDMHandoff starts a handoff. since 0 means now.
func (s *Store) CreateDMHandoff(h DMHandoff) (DMHandoff, error) {
	h.DiscordUserID = strings.TrimSpace(h.DiscordUserID)
	h.HandledBy = strings.TrimSpace(h.HandledBy)
	if h.DiscordUserID == "" || h.HandledBy == "" {
		return DMHandoff{}, fmt.Errorf("%w: discord_user_id and handled_by are required", ErrInvalidDMHandoff)
	}
	h.CreatedAt = now()
	if h.Since == 0 {
		h.Since = h.CreatedAt
	}
	if _, found, err := s.ActiveDMHandoff(h.DiscordUserID); err != nil {
		return DMHandoff{}, err
	} else if found {
		return DMHandoff{}, ErrDMHandoffActive
	}
	result, err := s.db.Exec(`INSERT INTO dm_handoffs (discord_user_id, handled_by, reason, since, created_at) VALUES (?,?,?,?,?)`,
		h.DiscordUserID, h.HandledBy, h.Reason, h.Since, h.CreatedAt)
	if err != nil {
		return DMHandoff{}, fmt.Errorf("create dm handoff: %w", err)
	}
	h.ID, _ = result.LastInsertId()
	return h, nil
}

// ActiveDMHandoff is the person's active handoff, if any.
func (s *Store) ActiveDMHandoff(discordUserID string) (DMHandoff, bool, error) {
	h, err := scanDMHandoff(s.db.QueryRow(`SELECT `+dmHandoffColumns+` FROM dm_handoffs WHERE discord_user_id = ? AND ended_at = 0`, discordUserID))
	if errors.Is(err, sql.ErrNoRows) {
		return DMHandoff{}, false, nil
	}
	if err != nil {
		return DMHandoff{}, false, fmt.Errorf("read dm handoff: %w", err)
	}
	return h, true, nil
}

// DMHandoffs lists every handoff, newest first.
func (s *Store) DMHandoffs() ([]DMHandoff, error) {
	rows, err := s.db.Query(`SELECT ` + dmHandoffColumns + ` FROM dm_handoffs ORDER BY id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list dm handoffs: %w", err)
	}
	defer rows.Close()
	out := []DMHandoff{}
	for rows.Next() {
		h, err := scanDMHandoff(rows)
		if err != nil {
			return nil, fmt.Errorf("scan dm handoff: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// EndDMHandoff ends a handoff; the person's DMs go with events again.
func (s *Store) EndDMHandoff(id int64) (DMHandoff, error) {
	result, err := s.db.Exec(`UPDATE dm_handoffs SET ended_at = ? WHERE id = ? AND ended_at = 0`, now(), id)
	if err != nil {
		return DMHandoff{}, fmt.Errorf("end dm handoff: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return DMHandoff{}, ErrNotFound
	}
	return scanDMHandoff(s.db.QueryRow(`SELECT `+dmHandoffColumns+` FROM dm_handoffs WHERE id = ?`, id))
}

func (s *Server) handleListDMHandoffs(w http.ResponseWriter, r *http.Request) {
	handoffs, err := s.store.DMHandoffs()
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dm_handoffs": handoffs})
}

func (s *Server) handleCreateDMHandoff(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DiscordUserID string `json:"discord_user_id"`
		HandledBy     string `json:"handled_by"`
		Reason        string `json:"reason"`
		Since         int64  `json:"since"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad body: " + err.Error()})
		return
	}
	h, err := s.store.CreateDMHandoff(DMHandoff{DiscordUserID: body.DiscordUserID, HandledBy: body.HandledBy, Reason: body.Reason, Since: body.Since})
	if errors.Is(err, ErrDMHandoffActive) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	if errors.Is(err, ErrInvalidDMHandoff) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, h)
}

func (s *Server) handleEndDMHandoff(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad id"})
		return
	}
	h, err := s.store.EndDMHandoff(id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, h)
}
