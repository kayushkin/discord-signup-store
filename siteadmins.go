package discordsignup

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Site admins: people who may do everything in every server this bot is in —
// see every event, edit, end and cancel any of them, create anywhere, and set
// names — whatever their Discord roles say. For whoever runs the bot, who
// otherwise has only the standing each server's Discord roles give them.
// Listed by Discord user id; set through the machine API only, so the web
// pages cannot grant it.

// IsSiteAdmin reports whether a Discord user is a site admin.
func (s *Store) IsSiteAdmin(discordUserID string) (bool, error) {
	if discordUserID == "" {
		return false, nil
	}
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM site_admins WHERE discord_user_id = ?`, discordUserID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read site admins: %w", err)
	}
	return true, nil
}

// SiteAdmin is one row.
type SiteAdmin struct {
	DiscordUserID string `json:"discord_user_id"`
	AddedAt       int64  `json:"added_at"`
}

// SiteAdmins lists them.
func (s *Store) SiteAdmins() ([]SiteAdmin, error) {
	rows, err := s.db.Query(`SELECT discord_user_id, added_at FROM site_admins ORDER BY added_at`)
	if err != nil {
		return nil, fmt.Errorf("list site admins: %w", err)
	}
	defer rows.Close()
	out := []SiteAdmin{}
	for rows.Next() {
		var a SiteAdmin
		if err := rows.Scan(&a.DiscordUserID, &a.AddedAt); err != nil {
			return nil, fmt.Errorf("scan site admin: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AddSiteAdmin makes someone a site admin. Adding one who already is changes
// nothing.
func (s *Store) AddSiteAdmin(discordUserID string) (SiteAdmin, error) {
	a := SiteAdmin{DiscordUserID: strings.TrimSpace(discordUserID), AddedAt: now()}
	if a.DiscordUserID == "" {
		return a, fmt.Errorf("%w: a discord user id is required", ErrInvalidEvent)
	}
	if _, err := s.db.Exec(`INSERT INTO site_admins (discord_user_id, added_at) VALUES (?, ?)
		ON CONFLICT(discord_user_id) DO NOTHING`, a.DiscordUserID, a.AddedAt); err != nil {
		return a, fmt.Errorf("add site admin: %w", err)
	}
	return a, nil
}

// RemoveSiteAdmin takes it away.
func (s *Store) RemoveSiteAdmin(discordUserID string) error {
	result, err := s.db.Exec(`DELETE FROM site_admins WHERE discord_user_id = ?`, discordUserID)
	if err != nil {
		return fmt.Errorf("remove site admin: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// mayViewGuild reports whether a web viewer may see a server's events: its
// members, and site admins.
func (s *Server) mayViewGuild(session *WebSession, guildID string) (bool, error) {
	if session.IsMemberOf(guildID) {
		return true, nil
	}
	return s.store.IsSiteAdmin(session.DiscordUserID)
}

func (s *Server) handleListSiteAdmins(w http.ResponseWriter, r *http.Request) {
	admins, err := s.store.SiteAdmins()
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"site_admins": admins})
}

func (s *Server) handleAddSiteAdmin(w http.ResponseWriter, r *http.Request) {
	a, err := s.store.AddSiteAdmin(r.PathValue("userID"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) handleRemoveSiteAdmin(w http.ResponseWriter, r *http.Request) {
	if err := s.store.RemoveSiteAdmin(r.PathValue("userID")); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
