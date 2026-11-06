package discordsignup

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// sessionIdleLifetime is how long a browser login lasts without a visit. Each
// visit moves the end forward, so someone who uses the site keeps their login.
const sessionIdleLifetime = 30 * 24 * time.Hour

// sessionExtendStep is how far the end must move before a visit writes it,
// so a burst of requests writes the row once rather than once each.
const sessionExtendStep = time.Hour

// permissionRefreshInterval is how old guild_permissions may get before the bot
// reads them again. It bounds how long a permission taken away in Discord, such
// as MANAGE_EVENTS, keeps working here.
const permissionRefreshInterval = 12 * time.Hour

// Discord permission bits this service checks. Named rather than inlined so a
// permission test reads as what it means.
const (
	permissionManageEvents  = 1 << 33
	permissionAdministrator = 1 << 3
)

// WebSession is a logged-in browser.
type WebSession struct {
	Token         string
	DiscordUserID string
	DisplayName   string
	Avatar        string
	// GuildPermissions maps guild id to the permission bits Discord reported
	// for this user in that guild, as of login.
	GuildPermissions map[string]uint64
	ExpiresAt        int64
	// PermissionsCheckedAt is when GuildPermissions was last read from Discord.
	PermissionsCheckedAt int64
}

// IsMemberOf reports whether Discord listed this guild for the user at login.
func (s *WebSession) IsMemberOf(guildID string) bool {
	_, ok := s.GuildPermissions[guildID]
	return ok
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// CreateWebSession stores a login and returns its token.
func (s *Store) CreateWebSession(userID, displayName, avatar string, perms map[string]uint64) (*WebSession, error) {
	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(stringifyPermissions(perms))
	if err != nil {
		return nil, fmt.Errorf("encode guild permissions: %w", err)
	}
	created := now()
	expires := created + int64(sessionIdleLifetime.Seconds())
	_, err = s.db.Exec(`
		INSERT INTO web_sessions (token, discord_user_id, display_name, avatar,
		                          guild_permissions, created_at, expires_at, permissions_checked_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		token, userID, displayName, avatar, string(encoded), created, expires, created)
	if err != nil {
		return nil, fmt.Errorf("insert session: %w", err)
	}
	return &WebSession{
		Token: token, DiscordUserID: userID, DisplayName: displayName,
		Avatar: avatar, GuildPermissions: perms, ExpiresAt: expires,
		PermissionsCheckedAt: created,
	}, nil
}

// WebSessionByToken looks up a live session. An expired one is treated as
// absent and swept, so a stale cookie logs the person out rather than half
// working.
func (s *Store) WebSessionByToken(token string) (*WebSession, error) {
	var ws WebSession
	var encoded string
	err := s.db.QueryRow(`
		SELECT token, discord_user_id, display_name, avatar, guild_permissions, expires_at,
		       permissions_checked_at
		FROM web_sessions WHERE token = ?`, token).
		Scan(&ws.Token, &ws.DiscordUserID, &ws.DisplayName, &ws.Avatar, &encoded, &ws.ExpiresAt,
			&ws.PermissionsCheckedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan session: %w", err)
	}
	if ws.ExpiresAt <= now() {
		_, _ = s.db.Exec(`DELETE FROM web_sessions WHERE token = ?`, token)
		return nil, ErrNotFound
	}
	var raw map[string]string
	if err := json.Unmarshal([]byte(encoded), &raw); err != nil {
		return nil, fmt.Errorf("decode guild permissions: %w", err)
	}
	ws.GuildPermissions = parsePermissions(raw)
	return &ws, nil
}

// ExtendWebSession moves a login's end to expiresAt.
func (s *Store) ExtendWebSession(token string, expiresAt int64) error {
	if _, err := s.db.Exec(`UPDATE web_sessions SET expires_at = ? WHERE token = ?`,
		expiresAt, token); err != nil {
		return fmt.Errorf("extend session: %w", err)
	}
	return nil
}

// ReplaceWebSessionPermissions stores permissions the bot has just read.
func (s *Store) ReplaceWebSessionPermissions(token string, perms map[string]uint64, checkedAt int64) error {
	encoded, err := json.Marshal(stringifyPermissions(perms))
	if err != nil {
		return fmt.Errorf("encode guild permissions: %w", err)
	}
	if _, err := s.db.Exec(`
		UPDATE web_sessions SET guild_permissions = ?, permissions_checked_at = ? WHERE token = ?`,
		string(encoded), checkedAt, token); err != nil {
		return fmt.Errorf("replace session permissions: %w", err)
	}
	return nil
}

// DeleteWebSession logs someone out.
func (s *Store) DeleteWebSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM web_sessions WHERE token = ?`, token)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// SweepExpiredSessions removes sessions and abandoned login attempts.
func (s *Store) SweepExpiredSessions() error {
	if _, err := s.db.Exec(`DELETE FROM web_sessions WHERE expires_at <= ?`, now()); err != nil {
		return fmt.Errorf("sweep sessions: %w", err)
	}
	// A login that never came back. Ten minutes is far longer than the flow
	// takes and short enough that the table cannot grow without bound.
	if _, err := s.db.Exec(`DELETE FROM oauth_states WHERE created_at <= ?`, now()-600); err != nil {
		return fmt.Errorf("sweep oauth states: %w", err)
	}
	return nil
}

// CreateOAuthState records a login attempt and returns its single-use state.
func (s *Store) CreateOAuthState(redirect string) (string, error) {
	state, err := randomToken()
	if err != nil {
		return "", err
	}
	if redirect == "" {
		redirect = "/"
	}
	if _, err := s.db.Exec(
		`INSERT INTO oauth_states (state, redirect, created_at) VALUES (?,?,?)`,
		state, redirect, now()); err != nil {
		return "", fmt.Errorf("insert oauth state: %w", err)
	}
	return state, nil
}

// ConsumeOAuthState validates a callback's state and destroys it.
//
// Single-use is the entire point. A state that survives its callback can be
// replayed to finish someone else's login, which is exactly the CSRF the
// parameter exists to prevent — so this deletes first and reports whether it
// deleted anything, rather than checking and then deleting.
func (s *Store) ConsumeOAuthState(state string) (redirect string, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	err = tx.QueryRow(`SELECT redirect FROM oauth_states WHERE state = ?`, state).Scan(&redirect)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read oauth state: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM oauth_states WHERE state = ?`, state); err != nil {
		return "", fmt.Errorf("consume oauth state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	return redirect, nil
}

// Discord sends permission bit fields as decimal STRINGS, because they exceed
// what a JSON number can hold exactly. They are parsed to uint64 for testing
// and stored back as strings for the same reason.
func stringifyPermissions(perms map[string]uint64) map[string]string {
	out := make(map[string]string, len(perms))
	for guild, bits := range perms {
		out[guild] = strconv.FormatUint(bits, 10)
	}
	return out
}

func parsePermissions(raw map[string]string) map[string]uint64 {
	out := make(map[string]uint64, len(raw))
	for guild, s := range raw {
		bits, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			// A permission field we cannot read means no permissions, never
			// all of them. Failing open here would hand out MANAGE_EVENTS.
			continue
		}
		out[guild] = bits
	}
	return out
}
