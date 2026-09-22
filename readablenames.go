package discordsignup

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Readable names: the short name a person is shown by on Discord, "Matt" for
// "Lil' Fascist Matt 🌟". Server nicknames here carry jokes, handles and
// emoji, and a table listing fifteen of them is hard to read. A name is set
// by hand, per Discord user id; nothing guesses one.

// ReadableName is one person's short name.
type ReadableName struct {
	DiscordUserID string `json:"discord_user_id"`
	ReadableName  string `json:"readable_name"`
	UpdatedAt     int64  `json:"updated_at"`
}

// ReadableNames lists every short name set.
func (s *Store) ReadableNames() ([]ReadableName, error) {
	rows, err := s.db.Query(`SELECT discord_user_id, readable_name, updated_at FROM readable_names
		ORDER BY readable_name`)
	if err != nil {
		return nil, fmt.Errorf("list readable names: %w", err)
	}
	defer rows.Close()
	out := []ReadableName{}
	for rows.Next() {
		var n ReadableName
		if err := rows.Scan(&n.DiscordUserID, &n.ReadableName, &n.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan readable name: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// SetReadableName sets or replaces one person's short name.
func (s *Store) SetReadableName(discordUserID, name string) (ReadableName, error) {
	n := ReadableName{DiscordUserID: strings.TrimSpace(discordUserID), ReadableName: strings.TrimSpace(name), UpdatedAt: now()}
	if n.DiscordUserID == "" || n.ReadableName == "" {
		return n, fmt.Errorf("%w: both a discord user id and a readable name are required", ErrInvalidEvent)
	}
	if _, err := s.db.Exec(`
		INSERT INTO readable_names (discord_user_id, readable_name, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(discord_user_id) DO UPDATE SET readable_name = excluded.readable_name,
			updated_at = excluded.updated_at`, n.DiscordUserID, n.ReadableName, n.UpdatedAt); err != nil {
		return n, fmt.Errorf("store readable name: %w", err)
	}
	return n, nil
}

// DeleteReadableName removes one, so their display name shows again.
func (s *Store) DeleteReadableName(discordUserID string) error {
	result, err := s.db.Exec(`DELETE FROM readable_names WHERE discord_user_id = ?`, discordUserID)
	if err != nil {
		return fmt.Errorf("delete readable name: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Server) handleListReadableNames(w http.ResponseWriter, r *http.Request) {
	names, err := s.store.ReadableNames()
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"readable_names": names})
}

// handleSetReadableName sets a name. The name is part of each event's publish
// signature, so the every-minute sweep redraws every surface that shows it.
func (s *Server) handleSetReadableName(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ReadableName string `json:"readable_name"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed body: " + err.Error()})
		return
	}
	n, err := s.store.SetReadableName(r.PathValue("userID"), body.ReadableName)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, n)
}

func (s *Server) handleDeleteReadableName(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteReadableName(r.PathValue("userID")); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
