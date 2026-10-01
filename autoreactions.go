package discordsignup

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"unicode/utf8"
)

// Auto-reactions: the bot reacts with an emoji to every message one person
// posts in one server, in every channel and thread it can see. The operator
// sets them on dash's Messages pages, through the machine API below; the
// gateway's GUILD_MESSAGES events drive them (onGuildMessage in gateway.go).
// Only the author's id is read from a message, never its text, so the
// privileged Message Content intent is not needed.

// AutoReaction is one auto_reactions row. MemberDisplayName and GuildName are
// for display only, read from member_names and bot_guilds by id; either is ""
// when this service has not seen it.
type AutoReaction struct {
	ID                int64  `json:"id"`
	GuildID           string `json:"guild_id"`
	GuildName         string `json:"guild_name"`
	DiscordUserID     string `json:"discord_user_id"`
	MemberDisplayName string `json:"member_display_name"`
	Emoji             string `json:"emoji"`
	Enabled           bool   `json:"enabled"`
	ReactionCount     int64  `json:"reaction_count"`
	LastReactedAt     int64  `json:"last_reacted_at"`
	LastError         string `json:"last_error"`
	LastErrorAt       int64  `json:"last_error_at"`
	CreatedAt         int64  `json:"created_at"`
	UpdatedAt         int64  `json:"updated_at"`
}

// maxAutoReactionEmojiBytes bounds the emoji field. The longest unicode emoji
// (a family with skin tones) is under 40 bytes; a custom emoji is name:id,
// with a name of at most 32 characters and a 20-digit id.
const maxAutoReactionEmojiBytes = 64

// checkAutoReactionEmoji refuses what cannot be a reaction: nothing, text with
// a slash or spaces (it goes into a URL path), or something far too long. A
// custom emoji is written name:id, as Discord's reaction route takes it.
func checkAutoReactionEmoji(emoji string) error {
	if emoji == "" {
		return fmt.Errorf("%w: an emoji is required", ErrInvalidEvent)
	}
	if !utf8.ValidString(emoji) || len(emoji) > maxAutoReactionEmojiBytes || strings.ContainsAny(emoji, "/ \t\n") {
		return fmt.Errorf("%w: %q is not an emoji; give a unicode emoji or a custom one as name:id", ErrInvalidEvent, emoji)
	}
	return nil
}

const autoReactionColumns = `a.id, a.guild_id, COALESCE(g.name, ''), a.discord_user_id, COALESCE(m.display_name, ''),
	a.emoji, a.enabled, a.reaction_count, a.last_reacted_at, a.last_error, a.last_error_at, a.created_at, a.updated_at`

const autoReactionFrom = `auto_reactions a
	LEFT JOIN bot_guilds g ON g.guild_id = a.guild_id
	LEFT JOIN member_names m ON m.guild_id = a.guild_id AND m.discord_user_id = a.discord_user_id`

func scanAutoReaction(row interface{ Scan(...any) error }) (AutoReaction, error) {
	var a AutoReaction
	err := row.Scan(&a.ID, &a.GuildID, &a.GuildName, &a.DiscordUserID, &a.MemberDisplayName,
		&a.Emoji, &a.Enabled, &a.ReactionCount, &a.LastReactedAt, &a.LastError, &a.LastErrorAt, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

func (s *Store) queryAutoReactions(where string, args ...any) ([]AutoReaction, error) {
	rows, err := s.db.Query(`SELECT `+autoReactionColumns+` FROM `+autoReactionFrom+` `+where+` ORDER BY a.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list auto-reactions: %w", err)
	}
	defer rows.Close()
	out := []AutoReaction{}
	for rows.Next() {
		a, err := scanAutoReaction(rows)
		if err != nil {
			return nil, fmt.Errorf("scan auto-reaction: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AutoReactions lists every rule, on or off.
func (s *Store) AutoReactions() ([]AutoReaction, error) {
	return s.queryAutoReactions("")
}

// EnabledAutoReactionsFor lists the rules that fire on a message by this
// person in this server.
func (s *Store) EnabledAutoReactionsFor(guildID, discordUserID string) ([]AutoReaction, error) {
	return s.queryAutoReactions(`WHERE a.guild_id = ? AND a.discord_user_id = ? AND a.enabled = 1`, guildID, discordUserID)
}

// GetAutoReaction reads one rule.
func (s *Store) GetAutoReaction(id int64) (AutoReaction, error) {
	a, err := scanAutoReaction(s.db.QueryRow(`SELECT `+autoReactionColumns+` FROM `+autoReactionFrom+` WHERE a.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, fmt.Errorf("read auto-reaction %d: %w", id, err)
	}
	return a, nil
}

// CreateAutoReaction adds a rule. The same person, server and emoji twice is
// refused rather than merged, so the caller learns the rule already exists.
func (s *Store) CreateAutoReaction(guildID, discordUserID, emoji string, enabled bool) (AutoReaction, error) {
	guildID, discordUserID, emoji = strings.TrimSpace(guildID), strings.TrimSpace(discordUserID), strings.TrimSpace(emoji)
	if guildID == "" || discordUserID == "" {
		return AutoReaction{}, fmt.Errorf("%w: guild_id and discord_user_id are required", ErrInvalidEvent)
	}
	if err := checkAutoReactionEmoji(emoji); err != nil {
		return AutoReaction{}, err
	}
	at := now()
	result, err := s.db.Exec(`INSERT INTO auto_reactions (guild_id, discord_user_id, emoji, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`, guildID, discordUserID, emoji, enabled, at, at)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return AutoReaction{}, fmt.Errorf("%w: %s already gets %s in %s", ErrInvalidEvent, discordUserID, emoji, guildID)
		}
		return AutoReaction{}, fmt.Errorf("create auto-reaction: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return AutoReaction{}, fmt.Errorf("create auto-reaction: %w", err)
	}
	return s.GetAutoReaction(id)
}

// AutoReactionChange is a PATCH: a nil field is left alone.
type AutoReactionChange struct {
	Emoji   *string `json:"emoji"`
	Enabled *bool   `json:"enabled"`
}

// UpdateAutoReaction changes a rule's emoji or switches it on or off.
// Changing the emoji clears the last error, which belonged to the old one.
func (s *Store) UpdateAutoReaction(id int64, change AutoReactionChange) (AutoReaction, error) {
	current, err := s.GetAutoReaction(id)
	if err != nil {
		return current, err
	}
	emoji, enabled, lastError, lastErrorAt := current.Emoji, current.Enabled, current.LastError, current.LastErrorAt
	if change.Emoji != nil {
		emoji = strings.TrimSpace(*change.Emoji)
		if err := checkAutoReactionEmoji(emoji); err != nil {
			return current, err
		}
		if emoji != current.Emoji {
			lastError, lastErrorAt = "", 0
		}
	}
	if change.Enabled != nil {
		enabled = *change.Enabled
	}
	_, err = s.db.Exec(`UPDATE auto_reactions SET emoji = ?, enabled = ?, last_error = ?, last_error_at = ?, updated_at = ? WHERE id = ?`,
		emoji, enabled, lastError, lastErrorAt, now(), id)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return current, fmt.Errorf("%w: %s already gets %s in %s", ErrInvalidEvent, current.DiscordUserID, emoji, current.GuildID)
		}
		return current, fmt.Errorf("update auto-reaction %d: %w", id, err)
	}
	return s.GetAutoReaction(id)
}

// DeleteAutoReaction removes a rule.
func (s *Store) DeleteAutoReaction(id int64) error {
	result, err := s.db.Exec(`DELETE FROM auto_reactions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete auto-reaction %d: %w", id, err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// recordAutoReactionOutcome counts a reaction made, or keeps the error that
// stopped it, so the page can say whether a rule works.
func (s *Store) recordAutoReactionOutcome(id int64, reactionErr error) error {
	var err error
	if reactionErr == nil {
		_, err = s.db.Exec(`UPDATE auto_reactions SET reaction_count = reaction_count + 1, last_reacted_at = ? WHERE id = ?`, now(), id)
	} else {
		_, err = s.db.Exec(`UPDATE auto_reactions SET last_error = ?, last_error_at = ? WHERE id = ?`, reactionErr.Error(), now(), id)
	}
	if err != nil {
		return fmt.Errorf("record the outcome of auto-reaction %d: %w", id, err)
	}
	return nil
}

// addAutoReactions reacts to one message with every rule that matches its
// author. Called from the gateway for each message posted in a server.
func (s *Server) addAutoReactions(guildID, channelID, messageID, authorID string) {
	if s.discord == nil {
		return
	}
	rules, err := s.store.EnabledAutoReactionsFor(guildID, authorID)
	if err != nil {
		log.Printf("[discord-signup] auto-reactions for user=%s guild=%s: %v", authorID, guildID, err)
		return
	}
	for _, rule := range rules {
		reactionErr := s.discord.CreateOwnReaction(channelID, messageID, rule.Emoji)
		if reactionErr != nil {
			log.Printf("[discord-signup] auto-reaction %d %s on message=%s channel=%s: %v", rule.ID, rule.Emoji, messageID, channelID, reactionErr)
		}
		if err := s.store.recordAutoReactionOutcome(rule.ID, reactionErr); err != nil {
			log.Printf("[discord-signup] %v", err)
		}
	}
}

func (s *Server) handleListAutoReactions(w http.ResponseWriter, r *http.Request) {
	rules, err := s.store.AutoReactions()
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"auto_reactions": rules})
}

// handleCreateAutoReaction takes guild_id, discord_user_id, emoji and
// optionally enabled (true when left out). member_display_name, when given,
// is recorded in member_names, the name a member search just showed.
func (s *Server) handleCreateAutoReaction(w http.ResponseWriter, r *http.Request) {
	var in struct {
		GuildID           string `json:"guild_id"`
		DiscordUserID     string `json:"discord_user_id"`
		MemberDisplayName string `json:"member_display_name"`
		Emoji             string `json:"emoji"`
		Enabled           *bool  `json:"enabled"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read the body: " + err.Error()})
		return
	}
	enabled := in.Enabled == nil || *in.Enabled
	if name := strings.TrimSpace(in.MemberDisplayName); name != "" && in.GuildID != "" && in.DiscordUserID != "" {
		if err := s.store.RecordMemberName(strings.TrimSpace(in.GuildID), strings.TrimSpace(in.DiscordUserID), name); err != nil {
			writeStoreError(w, err)
			return
		}
	}
	rule, err := s.store.CreateAutoReaction(in.GuildID, in.DiscordUserID, in.Emoji, enabled)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rule)
}

func (s *Server) handleUpdateAutoReaction(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad id"})
		return
	}
	var change AutoReactionChange
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&change); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read the body: " + err.Error()})
		return
	}
	rule, err := s.store.UpdateAutoReaction(id, change)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rule)
}

func (s *Server) handleDeleteAutoReaction(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad id"})
		return
	}
	if err := s.store.DeleteAutoReaction(id); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListGuilds lists the servers the bot is in, for a page that picks one.
func (s *Server) handleListGuilds(w http.ResponseWriter, r *http.Request) {
	guilds, err := s.store.BotGuilds()
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if guilds == nil {
		guilds = []Guild{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"guilds": guilds})
}

// handleSearchGuildMembers finds members of a server by the start of a name,
// for a page that picks a person. Same search as the event pages' picker.
func (s *Server) handleSearchGuildMembers(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeJSON(w, http.StatusOK, map[string]any{"members": []MemberMatch{}})
		return
	}
	if s.discord == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no Discord client configured"})
		return
	}
	guildID := r.PathValue("guildID")
	matches, err := s.findMembers(guildID, query, memberSearchLimit)
	if err != nil {
		log.Printf("[discord-signup] member search in %s for %q: %v", guildID, query, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "Discord member search failed: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": matches})
}
