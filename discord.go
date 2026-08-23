package discordsignup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"
)

// DiscordAPIBase is the versioned REST root. Pinned to a version on purpose:
// Discord ships breaking changes behind version numbers and an unversioned
// path would move under us.
const DiscordAPIBase = "https://discord.com/api/v10"

// errorCodeCannotMessageUser is Discord's 50007, returned when a DM is refused
// because the recipient has direct messages from server members turned off.
// It is a normal outcome, not a fault: it means the news has to be delivered
// some other way, and it is why the reply to a button click is ephemeral rather
// than a DM.
const errorCodeCannotMessageUser = 50007

// ErrCannotMessageUser reports a DM that Discord refused to deliver.
var ErrCannotMessageUser = errors.New("cannot send direct message to this user")

// DiscordClient talks to the Discord REST API as the bot.
type DiscordClient struct {
	baseURL    string
	httpClient *http.Client
	resolve    TokenResolver

	mu     sync.Mutex
	cached string
}

// TokenResolver hands back the bot token. It is a function rather than a string
// so the token is never held in a config struct or a unit file — see
// AuthStoreTokenResolver.
type TokenResolver func() (string, error)

// NewDiscordClient builds a client. baseURL is overridable for tests only.
func NewDiscordClient(baseURL string, resolve TokenResolver) *DiscordClient {
	if baseURL == "" {
		baseURL = DiscordAPIBase
	}
	return &DiscordClient{
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: 15 * time.Second},
		resolve:    resolve,
	}
}

// AuthStoreTokenResolver reads the bot token out of auth-store, which is the
// canonical credential vault on this host. The token is deliberately not an
// environment variable on the unit: a unit file is world-readable and a token
// in one is a token in every process listing and every backup.
//
// Store it first with auth_type "api_key" and refresh_mode "none" — a bot token
// is a static secret, not an OAuth grant, so there is nothing to refresh.
func AuthStoreTokenResolver(authStoreURL, authStoreToken, provider, account string) TokenResolver {
	return func() (string, error) {
		url := fmt.Sprintf("%s/api/resolve/%s?account=%s", authStoreURL, provider, account)
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return "", err
		}
		if authStoreToken != "" {
			req.Header.Set("Authorization", "Bearer "+authStoreToken)
		}
		req.Header.Set("X-Auth-App", "discord-signup-store")
		req.Header.Set("X-Auth-Reason", "discord:bot")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("auth-store unavailable: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return "", fmt.Errorf("auth-store error (%d): %s", resp.StatusCode, string(body))
		}
		var out struct {
			APIKey      string `json:"api_key"`
			AccessToken string `json:"access_token"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return "", fmt.Errorf("decode auth-store response: %w", err)
		}
		// A bot token is stored as api_key. access_token is read too so a
		// credential filed under the wrong auth_type produces a working client
		// rather than a silent empty Authorization header.
		if out.APIKey != "" {
			return out.APIKey, nil
		}
		if out.AccessToken != "" {
			return out.AccessToken, nil
		}
		return "", errors.New("auth-store returned no discord bot token")
	}
}

func (c *DiscordClient) token() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != "" {
		return c.cached, nil
	}
	tok, err := c.resolve()
	if err != nil {
		return "", err
	}
	c.cached = tok
	return tok, nil
}

// forgetToken drops the cached token so the next call re-resolves. Called on a
// 401, which for a static bot token means it was rotated or revoked.
func (c *DiscordClient) forgetToken() {
	c.mu.Lock()
	c.cached = ""
	c.mu.Unlock()
}

// APIError carries what Discord said went wrong. Kept whole rather than
// flattened to a string so a caller can branch on Code — 50007 in particular.
type APIError struct {
	Status  int    `json:"-"`
	Code    int    `json:"code"`
	Message string `json:"message"`
	Raw     string `json:"-"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("discord api %d (code %d): %s", e.Status, e.Code, e.Message)
}

// escapePathSegment makes a value safe to place in exactly one path segment.
//
// Every id this client puts in a Discord path — guild, channel, message, user,
// role, event, thread — is a snowflake, and a snowflake is decimal digits, so
// none of them can carry a path-significant character. That is an argument
// about the data, not a property of the code: nothing in this package checks
// snowflake-ness at the point the value enters the URL, and the argument stops
// holding the first time an id arrives from somewhere new. Escaping here makes
// it a property of the code instead, at the cost of one call.
//
// url.PathEscape is the right encoding for this position and url.QueryEscape is
// not: QueryEscape leaves "/" alone and turns a space into "+", so it would
// repair nothing and corrupt the well-formed case.
func escapePathSegment(segment string) string {
	return url.PathEscape(segment)
}

// do performs one authenticated request, retrying once on a rate limit.
//
// Discord answers 429 with retry_after in seconds. Honouring it once is enough
// for this service's traffic — a handful of role writes per signup — and a
// bounded single retry is better than an unbounded loop that hides a genuine
// rate-limit problem instead of surfacing it.
func (c *DiscordClient) do(method, path string, body any) ([]byte, error) {
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := c.token()
		if err != nil {
			return nil, err
		}
		var payload io.Reader
		if body != nil {
			encoded, err := json.Marshal(body)
			if err != nil {
				return nil, fmt.Errorf("encode request: %w", err)
			}
			payload = bytes.NewReader(encoded)
		}
		req, err := http.NewRequest(method, c.baseURL+path, payload)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bot "+tok)
		req.Header.Set("User-Agent", "discord-signup-store (https://github.com/kayushkin/discord-signup-store, 0.1)")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		// A PUT with no body still needs an explicit zero length, or Discord's
		// edge answers 411 Length Required instead of doing the thing.
		if body == nil {
			req.Header.Set("Content-Length", "0")
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("discord request: %w", err)
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read discord response: %w", readErr)
		}

		switch {
		case resp.StatusCode == http.StatusTooManyRequests && attempt == 0:
			wait := retryAfter(resp, raw)
			time.Sleep(wait)
			continue
		case resp.StatusCode == http.StatusUnauthorized && attempt == 0:
			c.forgetToken()
			continue
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			return raw, nil
		default:
			apiErr := &APIError{Status: resp.StatusCode, Raw: string(raw)}
			_ = json.Unmarshal(raw, apiErr)
			if apiErr.Code == errorCodeCannotMessageUser {
				return nil, fmt.Errorf("%w: %s", ErrCannotMessageUser, apiErr.Message)
			}
			return nil, apiErr
		}
	}
	return nil, errors.New("discord request exhausted its single retry")
}

func retryAfter(resp *http.Response, body []byte) time.Duration {
	var parsed struct {
		RetryAfter float64 `json:"retry_after"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.RetryAfter > 0 {
		return time.Duration(parsed.RetryAfter * float64(time.Second))
	}
	if header := resp.Header.Get("Retry-After"); header != "" {
		if secs, err := strconv.ParseFloat(header, 64); err == nil && secs > 0 {
			return time.Duration(secs * float64(time.Second))
		}
	}
	return time.Second
}

// AddMemberRole grants a role.
//
// Needs MANAGE_ROLES, and needs the bot's own highest role to sit ABOVE the
// role being granted in the guild's role list. Get the hierarchy wrong and this
// returns 403 while the permission looks correctly granted in the UI — which is
// the single most common way this breaks.
func (c *DiscordClient) AddMemberRole(guildID, userID, roleID string) error {
	_, err := c.do(http.MethodPut,
		fmt.Sprintf("/guilds/%s/members/%s/roles/%s",
			escapePathSegment(guildID), escapePathSegment(userID), escapePathSegment(roleID)), nil)
	return err
}

// RemoveMemberRole revokes a role. Same hierarchy rule as AddMemberRole.
func (c *DiscordClient) RemoveMemberRole(guildID, userID, roleID string) error {
	_, err := c.do(http.MethodDelete,
		fmt.Sprintf("/guilds/%s/members/%s/roles/%s",
			escapePathSegment(guildID), escapePathSegment(userID), escapePathSegment(roleID)), nil)
	return err
}

// CreateMessage posts a message and returns its id. Store that id on the event
// — it is how a later button click finds its roster.
func (c *DiscordClient) CreateMessage(channelID string, payload any) (string, error) {
	raw, err := c.do(http.MethodPost, "/channels/"+escapePathSegment(channelID)+"/messages", payload)
	if err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decode created message: %w", err)
	}
	return out.ID, nil
}

// EditMessage rewrites a message the bot posted.
func (c *DiscordClient) EditMessage(channelID, messageID string, payload any) error {
	_, err := c.do(http.MethodPatch, "/channels/"+escapePathSegment(channelID)+"/messages/"+escapePathSegment(messageID), payload)
	return err
}

// SendDirectMessage opens a DM channel and posts to it.
//
// Returns ErrCannotMessageUser when the recipient has DMs from server members
// off. Callers must treat that as expected and fall back to a channel mention;
// a promotion nobody hears about is a place nobody takes.
func (c *DiscordClient) SendDirectMessage(userID, content string) error {
	raw, err := c.do(http.MethodPost, "/users/@me/channels",
		map[string]any{"recipient_id": userID})
	if err != nil {
		return err
	}
	var channel struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &channel); err != nil {
		return fmt.Errorf("decode dm channel: %w", err)
	}
	_, err = c.do(http.MethodPost, "/channels/"+escapePathSegment(channel.ID)+"/messages",
		map[string]any{"content": content})
	return err
}

// Guild is the little Discord tells a bot about a server it is in.
type Guild struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ListBotGuilds returns the servers the bot has been invited to.
//
// The web page offers these, intersected with what the logged-in user may
// manage. Using the bot's list rather than the user's is deliberate: a server
// the user administers but the bot is not in cannot host a roster, and offering
// it would produce an event whose signup message can never be posted.
func (c *DiscordClient) ListBotGuilds() ([]Guild, error) {
	raw, err := c.do(http.MethodGet, "/users/@me/guilds", nil)
	if err != nil {
		return nil, err
	}
	var out []Guild
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode guilds: %w", err)
	}
	return out, nil
}

// Role is a guild role, with the position that decides what the bot may grant.
type Role struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Position int    `json:"position"`
	Managed  bool   `json:"managed"`
}

// ListGuildRoles returns a guild's roles, ordered highest first.
func (c *DiscordClient) ListGuildRoles(guildID string) ([]Role, error) {
	raw, err := c.do(http.MethodGet, "/guilds/"+escapePathSegment(guildID)+"/roles", nil)
	if err != nil {
		return nil, err
	}
	var out []Role
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode roles: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Position > out[j].Position })
	return out, nil
}

// AssignableRoles filters to the roles this bot can actually grant, so the web
// page cannot offer a choice that will 403 later.
//
// The comparison is not simply "position below the bot". When two roles share a
// position Discord breaks the tie by id, and the OLDER role — the smaller
// snowflake — ranks higher. Measured, not assumed: a role created after the bot
// joined, sitting at the same position, is grantable; one created before it is
// not. Nothing in Discord's UI shows this, and getting it wrong produces a 403
// that reads exactly like a missing permission.
func AssignableRoles(roles []Role, botRoleIDs []string) []Role {
	highest := Role{Position: -1}
	owned := map[string]bool{}
	for _, id := range botRoleIDs {
		owned[id] = true
	}
	for _, r := range roles {
		if !owned[r.ID] {
			continue
		}
		if r.Position > highest.Position || (r.Position == highest.Position && r.ID < highest.ID) {
			highest = r
		}
	}
	if highest.Position < 0 {
		return nil
	}
	var out []Role
	for _, r := range roles {
		if r.ID == highest.ID || r.Managed || r.Name == "@everyone" {
			continue
		}
		if r.Position < highest.Position || (r.Position == highest.Position && highest.ID < r.ID) {
			out = append(out, r)
		}
	}
	return out
}

// GuildMemberRoleIDs reports which roles a member holds.
func (c *DiscordClient) GuildMemberRoleIDs(guildID, userID string) ([]string, error) {
	raw, err := c.do(http.MethodGet, "/guilds/"+escapePathSegment(guildID)+"/members/"+escapePathSegment(userID), nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Roles []string `json:"roles"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode member: %w", err)
	}
	return out.Roles, nil
}

// CurrentUserID returns the bot's own Discord user id.
func (c *DiscordClient) CurrentUserID() (string, error) {
	raw, err := c.do(http.MethodGet, "/users/@me", nil)
	if err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decode current user: %w", err)
	}
	return out.ID, nil
}

// GuildMemberDisplayName returns how someone appears in a server: their
// nickname if they set one there, otherwise their global display name,
// otherwise their username.
//
// That order is what Discord itself shows in a member list, so a roster built
// from it reads the way the server does.
func (c *DiscordClient) GuildMemberDisplayName(guildID, userID string) (string, error) {
	raw, err := c.do(http.MethodGet, "/guilds/"+escapePathSegment(guildID)+"/members/"+escapePathSegment(userID), nil)
	if err != nil {
		return "", err
	}
	var member struct {
		Nick string `json:"nick"`
		User struct {
			Username   string `json:"username"`
			GlobalName string `json:"global_name"`
		} `json:"user"`
	}
	if err := json.Unmarshal(raw, &member); err != nil {
		return "", fmt.Errorf("decode member: %w", err)
	}
	if member.Nick != "" {
		return member.Nick, nil
	}
	if member.User.GlobalName != "" {
		return member.User.GlobalName, nil
	}
	return member.User.Username, nil
}

// PinMessage pins a message.
//
// Needs PIN_MESSAGES (1<<51), NOT MANAGE_MESSAGES. Discord split the two, so a
// bot that can delete other people's messages can still be refused a pin — a
// 403 that reads like a mistake until you know.
//
// Uses the current pins route. The older /channels/{id}/pins/{id} is deprecated
// and refuses this identically, so switching back would not help.
func (c *DiscordClient) PinMessage(channelID, messageID string) error {
	_, err := c.do(http.MethodPut, "/channels/"+escapePathSegment(channelID)+"/messages/pins/"+escapePathSegment(messageID), nil)
	return err
}

// CreateGuildChannel makes a text channel. Needs MANAGE_CHANNELS.
func (c *DiscordClient) CreateGuildChannel(guildID string, payload any) (*Channel, error) {
	raw, err := c.do(http.MethodPost, "/guilds/"+escapePathSegment(guildID)+"/channels", payload)
	if err != nil {
		return nil, err
	}
	var out Channel
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode created channel: %w", err)
	}
	return &out, nil
}

// Channel is the little of a Discord channel this service reads.
type Channel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type int    `json:"type"`
}

// DeleteMessage removes a message the bot posted.
func (c *DiscordClient) DeleteMessage(channelID, messageID string) error {
	_, err := c.do(http.MethodDelete, "/channels/"+escapePathSegment(channelID)+"/messages/"+escapePathSegment(messageID), nil)
	return err
}

// CreateThreadFromMessage starts a public thread on a message and returns the
// thread's channel id — which Discord makes the SAME as the message's id, a
// fact worth knowing when reading logs but never relied on in code.
func (c *DiscordClient) CreateThreadFromMessage(channelID, messageID, name string) (string, error) {
	raw, err := c.do(http.MethodPost,
		"/channels/"+escapePathSegment(channelID)+"/messages/"+escapePathSegment(messageID)+"/threads",
		map[string]any{
			"name": name,
			// The longest Discord offers. Inactivity only hides a thread from
			// the channel list; a new message unarchives it automatically, so
			// nothing is lost either way.
			"auto_archive_duration": 10080,
		})
	if err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decode created thread: %w", err)
	}
	return out.ID, nil
}

// ArchiveThread closes a discussion thread. Not locked: someone posting a
// "how did it go?" into an archived thread should reopen it, and locking would
// make that a permission error instead.
func (c *DiscordClient) ArchiveThread(threadID string) error {
	_, err := c.do(http.MethodPatch, "/channels/"+escapePathSegment(threadID),
		map[string]any{"archived": true})
	return err
}
