package discordsignup

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// The home page's server filter: someone in several servers can choose to see
// one server's events rather than all of them. Saved per Discord user, not per
// browser login, so it holds across logins and devices.

// HomeGuildOf is the server a person chose for their home page, or "" for all.
func (s *Store) HomeGuildOf(discordUserID string) (string, error) {
	var guildID string
	err := s.db.QueryRow(`SELECT home_guild_id FROM user_preferences WHERE discord_user_id = ?`,
		discordUserID).Scan(&guildID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read home server: %w", err)
	}
	return guildID, nil
}

// SetHomeGuild saves it; "" means every server.
func (s *Store) SetHomeGuild(discordUserID, guildID string) error {
	_, err := s.db.Exec(`
		INSERT INTO user_preferences (discord_user_id, home_guild_id, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(discord_user_id) DO UPDATE SET home_guild_id = excluded.home_guild_id,
			updated_at = excluded.updated_at`, discordUserID, strings.TrimSpace(guildID), now())
	if err != nil {
		return fmt.Errorf("save home server: %w", err)
	}
	return nil
}

// viewableGuilds is the servers the bot is in that this viewer may see — the
// home page filter's choices.
func (s *Server) viewableGuilds(session *WebSession) ([]Guild, error) {
	if s.discord == nil {
		return nil, errors.New("no discord client configured")
	}
	botGuilds, err := s.discord.ListBotGuilds()
	if err != nil {
		return nil, fmt.Errorf("list the bot's servers: %w", err)
	}
	var out []Guild
	for _, g := range botGuilds {
		ok, err := s.mayViewGuild(session, g.ID)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, g)
		}
	}
	return out, nil
}

// handleWebSetHomeServer saves the filter and goes back to the home page.
func (s *Server) handleWebSetHomeServer(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	guildID := strings.TrimSpace(r.FormValue("guild_id"))
	if guildID != "" {
		if ok, err := s.mayViewGuild(session, guildID); err != nil || !ok {
			http.Error(w, "you are not in that server", http.StatusForbidden)
			return
		}
	}
	if err := s.store.SetHomeGuild(session.DiscordUserID, guildID); err != nil {
		http.Redirect(w, r, "/?"+noticeQuery("Could not save the filter: "+err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
