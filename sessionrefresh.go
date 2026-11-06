package discordsignup

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"
)

// A login lasts 30 days from the last visit, but the server permissions saved
// on it are read again every 12 hours, by the bot rather than the person: the
// bot can see each member's roles and each role's bits, which is all Discord
// itself uses to work out a member's server-wide permissions. So a permission
// taken away in Discord stops working here within 12 hours, and nobody has to
// sign in again for that to happen.
//
// The bot can only see servers it is in. A refresh keeps exactly the bot's
// servers the person is still a member of, and drops the rest: nothing here
// acts in a server without the bot, and a permission nobody can re-check
// should not live for 30 days.

// permissionsAllBits is what the owner of a server holds: everything.
const permissionsAllBits = ^uint64(0)

// keepSessionCurrent re-reads a session's permissions when they are old and
// moves its end forward. Neither failing ends the login: the person keeps the
// permissions they had, never more, and the next request tries again.
func (s *Server) keepSessionCurrent(w http.ResponseWriter, session *WebSession) {
	if time.Since(time.Unix(session.PermissionsCheckedAt, 0)) >= permissionRefreshInterval {
		if err := s.refreshSessionPermissions(session); err != nil {
			log.Printf("[discord-signup] refresh permissions for %s: %v (kept the ones from %s)",
				session.DiscordUserID, err, time.Unix(session.PermissionsCheckedAt, 0).UTC().Format(time.RFC3339))
		}
	}

	expires := now() + int64(sessionIdleLifetime.Seconds())
	if expires-session.ExpiresAt < int64(sessionExtendStep.Seconds()) {
		return
	}
	if err := s.store.ExtendWebSession(session.Token, expires); err != nil {
		log.Printf("[discord-signup] extend session for %s: %v", session.DiscordUserID, err)
		return
	}
	session.ExpiresAt = expires
	setSessionCookie(w, session)
}

func (s *Server) refreshSessionPermissions(session *WebSession) error {
	if s.discord == nil {
		return errors.New("no Discord client to read them with")
	}
	guilds, err := s.store.BotGuilds()
	if err != nil {
		return fmt.Errorf("list the bot's servers: %w", err)
	}
	perms := make(map[string]uint64, len(guilds))
	for _, g := range guilds {
		bits, member, err := s.memberPermissions(g.ID, session.DiscordUserID)
		if err != nil {
			return fmt.Errorf("server %s: %w", g.ID, err)
		}
		if member {
			perms[g.ID] = bits
		}
	}
	checked := now()
	if err := s.store.ReplaceWebSessionPermissions(session.Token, perms, checked); err != nil {
		return err
	}
	session.GuildPermissions = perms
	session.PermissionsCheckedAt = checked
	return nil
}

// memberPermissions works out someone's server-wide permission bits the way
// Discord does: the owner holds everything; anyone else holds the @everyone
// role's bits (that role's id is the server's id) together with each of their
// roles' bits. member is false when they are not in the server.
func (s *Server) memberPermissions(guildID, userID string) (bits uint64, member bool, err error) {
	roleIDs, err := s.discord.GuildMemberRoleIDs(guildID, userID)
	if errors.Is(err, ErrUnknownMember) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read member roles: %w", err)
	}
	guild, err := s.discord.GuildOwnerAndRoles(guildID)
	if err != nil {
		return 0, false, fmt.Errorf("read server roles: %w", err)
	}
	if guild.OwnerID == userID {
		return permissionsAllBits, true, nil
	}
	held := map[string]bool{guildID: true}
	for _, id := range roleIDs {
		held[id] = true
	}
	for _, role := range guild.Roles {
		if !held[role.ID] {
			continue
		}
		roleBits, err := strconv.ParseUint(role.Permissions, 10, 64)
		if err != nil {
			return 0, false, fmt.Errorf("role %s has unreadable permissions %q: %w", role.ID, role.Permissions, err)
		}
		bits |= roleBits
	}
	return bits, true, nil
}
