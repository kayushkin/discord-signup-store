package discordsignup

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
)

// Who may edit events, and who may create them, per server.
//
// The default is Discord's own rule: Manage Events (or Administrator) edits
// any event, and whoever created an event edits that one. A server can
// replace the first half with a role of its own choosing — its editor role.
// Then an event is edited by whoever created it, anyone holding that role, or
// the server's owner, and Manage Events and Administrator no longer count
// here: the server decided that organising events is a role, not a Discord
// permission. Separately, a server can let any member create events.

// GuildEditingRule is one server's answer to those two questions. The zero
// value — no row — is the default rule.
type GuildEditingRule struct {
	GuildID string `json:"guild_id"`
	// EditorRoleID, when set, replaces Manage Events and Administrator as the
	// way to edit every event in this server. A role id, never a role name:
	// a renamed role keeps working and two roles sharing a name cannot swap.
	EditorRoleID string `json:"editor_role_id"`
	// AnyoneMayCreate lets every member create events, rather than only those
	// Discord lets create them (Create Events, Manage Events, Administrator).
	AnyoneMayCreate bool  `json:"anyone_may_create"`
	UpdatedAt       int64 `json:"updated_at"`
}

// GuildEditingRule reads a server's rule; a server with none gets the default.
func (s *Store) GuildEditingRule(guildID string) (GuildEditingRule, error) {
	rule := GuildEditingRule{GuildID: guildID}
	err := s.db.QueryRow(
		`SELECT editor_role_id, anyone_may_create, updated_at FROM guild_editing_rules WHERE guild_id = ?`,
		guildID).Scan(&rule.EditorRoleID, &rule.AnyoneMayCreate, &rule.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return rule, nil
	}
	if err != nil {
		return rule, fmt.Errorf("read editing rule for %s: %w", guildID, err)
	}
	return rule, nil
}

// SetGuildEditingRule replaces a server's rule.
func (s *Store) SetGuildEditingRule(rule GuildEditingRule) (GuildEditingRule, error) {
	rule.UpdatedAt = now()
	_, err := s.db.Exec(`
		INSERT INTO guild_editing_rules (guild_id, editor_role_id, anyone_may_create, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(guild_id) DO UPDATE SET editor_role_id = excluded.editor_role_id,
			anyone_may_create = excluded.anyone_may_create, updated_at = excluded.updated_at`,
		rule.GuildID, rule.EditorRoleID, rule.AnyoneMayCreate, rule.UpdatedAt)
	if err != nil {
		return rule, fmt.Errorf("store editing rule for %s: %w", rule.GuildID, err)
	}
	return rule, nil
}

// editActor is the person asking, as far as the asking surface knows them.
//
// A button press knows their roles — Discord sends them with it. A web page
// knows only the permission bits copied at login, so their roles are fetched
// when the server's rule needs them, and fetched fresh each time: someone
// whose role is taken away loses the right at once, not when their login
// expires.
type editActor struct {
	GuildID string
	UserID  string
	// PermissionBits is Discord's permission field for them: per channel for
	// a press, per server as of login for a web page.
	PermissionBits uint64
	RoleIDs        []string
	RolesKnown     bool
}

func (i *Interaction) editActor() editActor {
	userID, _ := i.actor()
	// An unreadable permission field means no permissions, never all of them.
	bits, _ := strconv.ParseUint(i.Member.Permissions, 10, 64)
	return editActor{GuildID: i.GuildID, UserID: userID, PermissionBits: bits,
		RoleIDs: i.Member.Roles, RolesKnown: i.Member.User.ID != ""}
}

func (s *WebSession) editActor(guildID string) editActor {
	return editActor{GuildID: guildID, UserID: s.DiscordUserID, PermissionBits: s.GuildPermissions[guildID]}
}

// mayEditAllEventsIn reports whether someone may edit every event in their
// server, not only their own: the rule's editor role or the owner when the
// server set one, Manage Events or Administrator when it did not. A site
// admin may, everywhere.
func (s *Server) mayEditAllEventsIn(actor editActor) (bool, error) {
	if admin, err := s.store.IsSiteAdmin(actor.UserID); err != nil || admin {
		return admin, err
	}
	rule, err := s.store.GuildEditingRule(actor.GuildID)
	if err != nil {
		return false, err
	}
	if rule.EditorRoleID == "" {
		return actor.PermissionBits&(permissionAdministrator|permissionManageEvents) != 0, nil
	}
	if s.discord == nil {
		return false, errors.New("this server has an editor role, and checking it needs a Discord client")
	}
	roles := actor.RoleIDs
	if !actor.RolesKnown {
		fetched, err := s.discord.GuildMemberRoleIDs(actor.GuildID, actor.UserID)
		if err != nil {
			return false, fmt.Errorf("read your roles from Discord: %w", err)
		}
		roles = fetched
	}
	if slices.Contains(roles, rule.EditorRoleID) {
		return true, nil
	}
	ownerID, err := s.discord.GuildOwnerID(actor.GuildID)
	if err != nil {
		return false, fmt.Errorf("read the server's owner from Discord: %w", err)
	}
	return ownerID == actor.UserID, nil
}

// mayEditEvent reports whether someone may edit one event: whoever created
// it — matched on created_by, the Discord user id, never a name — or anyone
// who may edit every event in its server.
func (s *Server) mayEditEvent(actor editActor, ev *Event) (bool, error) {
	if ev.CreatedBy != "" && ev.CreatedBy == actor.UserID {
		return true, nil
	}
	return s.mayEditAllEventsIn(actor)
}

// mayCreateEventsIn reports whether someone may create an event in their
// server: anyone at all when the server allows it, otherwise whoever Discord
// lets create its own events. The caller has already established that they
// are a member.
func (s *Server) mayCreateEventsIn(actor editActor) (bool, error) {
	if admin, err := s.store.IsSiteAdmin(actor.UserID); err != nil || admin {
		return admin, err
	}
	rule, err := s.store.GuildEditingRule(actor.GuildID)
	if err != nil {
		return false, err
	}
	if rule.AnyoneMayCreate {
		return true, nil
	}
	return actor.PermissionBits&(permissionAdministrator|permissionManageEvents|permissionCreateEvents) != 0, nil
}

// whoMayEditIn says, for a refusal, who may edit events in a server.
func (s *Server) whoMayEditIn(guildID string) string {
	rule, err := s.store.GuildEditingRule(guildID)
	if err == nil && rule.EditorRoleID != "" {
		return fmt.Sprintf("Only whoever created this event, someone with the <@&%s> role, or the server owner can edit it.",
			rule.EditorRoleID)
	}
	return "Only someone with Manage Events, or whoever created this event, can edit it."
}

// GuildOwnerID returns the Discord user id of a server's owner.
func (c *DiscordClient) GuildOwnerID(guildID string) (string, error) {
	raw, err := c.do(http.MethodGet, "/guilds/"+escapePathSegment(guildID), nil)
	if err != nil {
		return "", err
	}
	var out struct {
		OwnerID string `json:"owner_id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decode guild: %w", err)
	}
	if out.OwnerID == "" {
		return "", fmt.Errorf("discord returned no owner for guild %s", guildID)
	}
	return out.OwnerID, nil
}

// handleGetGuildEditing shows a server's rule.
func (s *Server) handleGetGuildEditing(w http.ResponseWriter, r *http.Request) {
	rule, err := s.store.GuildEditingRule(r.PathValue("guildID"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rule)
}

// handleSetGuildEditing replaces a server's rule. The role is checked against
// the server's roles first, so a mistyped id cannot quietly lock every
// organiser out.
func (s *Server) handleSetGuildEditing(w http.ResponseWriter, r *http.Request) {
	guildID := r.PathValue("guildID")
	var body struct {
		EditorRoleID    *string `json:"editor_role_id"`
		AnyoneMayCreate *bool   `json:"anyone_may_create"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed body: " + err.Error()})
		return
	}
	if body.EditorRoleID == nil || body.AnyoneMayCreate == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": `send both "editor_role_id" ("" for the default rule) and "anyone_may_create"`})
		return
	}
	if *body.EditorRoleID != "" {
		if s.discord == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no Discord client to check the role against"})
			return
		}
		roles, err := s.discord.ListGuildRoles(guildID)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "list the server's roles: " + err.Error()})
			return
		}
		if !slices.ContainsFunc(roles, func(role Role) bool { return role.ID == *body.EditorRoleID }) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("role %s is not one of server %s's roles", *body.EditorRoleID, guildID)})
			return
		}
	}
	rule, err := s.store.SetGuildEditingRule(GuildEditingRule{
		GuildID: guildID, EditorRoleID: *body.EditorRoleID, AnyoneMayCreate: *body.AnyoneMayCreate})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rule)
}
