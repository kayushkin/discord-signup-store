package discordsignup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// sessionCookieName is the only cookie this app sets. It holds an opaque token
// and nothing else — no claims, no user id, nothing to tamper with.
const sessionCookieName = "dss_session"

// oauthScopes are the least this app can work with.
//
// identify gives the user id and display name. guilds gives the list of servers
// they are in AND their permission bits in each, which is how MANAGE_EVENTS is
// checked without the privileged GUILD_MEMBERS intent or a per-request call to
// the bot API. guilds.members.read would be more precise and is deliberately
// not requested: it asks the user for more than this needs.
const oauthScopes = "identify guilds"

// OAuthConfig is what the login flow needs. ClientSecret is resolved from
// auth-store at use, never held in a config file or a unit.
type OAuthConfig struct {
	ClientID    string
	RedirectURL string
	// ResolveClientSecret is a TokenResolver over the auth-store credential
	// holding the OAuth2 client secret — a different credential from the bot
	// token, because they are different secrets with different blast radii.
	ResolveClientSecret TokenResolver
}

// LoginURL builds the Discord authorization URL for one login attempt.
func (c *OAuthConfig) LoginURL(state string) string {
	q := url.Values{}
	q.Set("client_id", c.ClientID)
	q.Set("redirect_uri", c.RedirectURL)
	q.Set("response_type", "code")
	q.Set("scope", oauthScopes)
	q.Set("state", state)
	// Force the consent screen off for repeat logins; Discord remembers the
	// grant and this keeps the round trip to one redirect.
	q.Set("prompt", "none")
	return "https://discord.com/oauth2/authorize?" + q.Encode()
}

// discordIdentity is who logged in, and where they can act.
type discordIdentity struct {
	UserID      string
	DisplayName string
	Avatar      string
	Guilds      map[string]uint64
}

// exchangeCode turns an authorization code into the user's identity.
//
// The access token is used once, here, and then dropped. This app never stores
// a user's Discord token: it needs to know who they are and what they may do,
// and after that its own session is enough. Keeping the token would mean
// holding a credential for every visitor with nothing to spend it on.
func (c *OAuthConfig) exchangeCode(code string) (*discordIdentity, error) {
	secret, err := c.ResolveClientSecret()
	if err != nil {
		return nil, fmt.Errorf("resolve oauth client secret: %w", err)
	}

	form := url.Values{}
	form.Set("client_id", c.ClientID)
	form.Set("client_secret", secret)
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", c.RedirectURL)

	resp, err := http.PostForm(DiscordAPIBase+"/oauth2/token", form)
	if err != nil {
		return nil, fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange failed (%d): %s", resp.StatusCode, string(raw))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		Scope       string `json:"scope"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if tok.AccessToken == "" {
		return nil, errors.New("discord returned no access token")
	}

	user, err := getBearer[struct {
		ID         string `json:"id"`
		Username   string `json:"username"`
		GlobalName string `json:"global_name"`
		Avatar     string `json:"avatar"`
	}](tok.AccessToken, "/users/@me")
	if err != nil {
		return nil, err
	}
	guilds, err := getBearer[[]struct {
		ID          string `json:"id"`
		Permissions string `json:"permissions"`
	}](tok.AccessToken, "/users/@me/guilds")
	if err != nil {
		return nil, err
	}

	name := user.GlobalName
	if name == "" {
		name = user.Username
	}
	perms := map[string]uint64{}
	for _, g := range *guilds {
		bits, err := strconv.ParseUint(g.Permissions, 10, 64)
		if err != nil {
			// Unreadable permissions mean none, never all. Recorded as 0 so
			// membership still counts but nothing is granted.
			log.Printf("[discord-signup] unreadable permissions %q for guild %s", g.Permissions, g.ID)
			bits = 0
		}
		perms[g.ID] = bits
	}
	return &discordIdentity{
		UserID: user.ID, DisplayName: name, Avatar: user.Avatar, Guilds: perms,
	}, nil
}

// getBearer calls a Discord endpoint as the logged-in USER, not as the bot.
func getBearer[T any](accessToken, path string) (*T, error) {
	req, err := http.NewRequest(http.MethodGet, DiscordAPIBase+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s failed (%d): %s", path, resp.StatusCode, string(raw))
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return &out, nil
}

// handleLogin starts the flow.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.oauth == nil {
		http.Error(w, "web login is not configured on this server", http.StatusNotImplemented)
		return
	}
	state, err := s.store.CreateOAuthState(safeRedirect(r.URL.Query().Get("next")))
	if err != nil {
		log.Printf("[discord-signup] create oauth state: %v", err)
		http.Error(w, "could not start login", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, s.oauth.LoginURL(state), http.StatusFound)
}

// handleOAuthCallback finishes it.
func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if s.oauth == nil {
		http.Error(w, "web login is not configured on this server", http.StatusNotImplemented)
		return
	}
	q := r.URL.Query()
	if errMsg := q.Get("error"); errMsg != "" {
		// Discord says why it refused; passing that through beats a blank page.
		http.Error(w, "discord refused the login: "+errMsg+" "+q.Get("error_description"),
			http.StatusForbidden)
		return
	}
	redirect, err := s.store.ConsumeOAuthState(q.Get("state"))
	if errors.Is(err, ErrNotFound) {
		// Unknown, expired, or already used. All three mean the same thing to
		// the person: start again.
		http.Error(w, "this login link is stale or was already used — start again at /login",
			http.StatusBadRequest)
		return
	}
	if err != nil {
		log.Printf("[discord-signup] consume oauth state: %v", err)
		http.Error(w, "could not complete login", http.StatusInternalServerError)
		return
	}

	identity, err := s.oauth.exchangeCode(q.Get("code"))
	if err != nil {
		log.Printf("[discord-signup] oauth exchange: %v", err)
		http.Error(w, "could not complete login: "+err.Error(), http.StatusBadGateway)
		return
	}
	session, err := s.store.CreateWebSession(identity.UserID, identity.DisplayName,
		identity.Avatar, identity.Guilds)
	if err != nil {
		log.Printf("[discord-signup] create session: %v", err)
		http.Error(w, "could not complete login", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    session.Token,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Unix(session.ExpiresAt, 0),
	})
	http.Redirect(w, r, redirect, http.StatusFound)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil {
		if err := s.store.DeleteWebSession(c.Value); err != nil {
			log.Printf("[discord-signup] delete session: %v", err)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/",
		HttpOnly: true, Secure: true, MaxAge: -1,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

// sessionFrom reads the caller's login, or nil if there is not one.
func (s *Server) sessionFrom(r *http.Request) *WebSession {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return nil
	}
	session, err := s.store.WebSessionByToken(c.Value)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		log.Printf("[discord-signup] load session: %v", err)
		return nil
	}
	return session
}

// safeRedirect keeps a ?next= parameter pointed at this site.
//
// Without this, /login?next=https://evil.example is an open redirect that
// finishes a real Discord login and then hands the person to someone else's
// page while they are feeling trusted. Only a path is ever accepted, and "//"
// is rejected because a protocol-relative URL is an absolute one.
func safeRedirect(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	return next
}
