package discordsignup

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
)

// Finding people by any part of their name, and keeping the names this
// service has written down current.
//
// Discord's member search matches only the START of a username, display name
// or nickname — "Evolved Woah/ Noah" is found by "Evo" and never by "Woah" —
// and listing a server's members to search them here needs the privileged
// GUILD_MEMBERS intent, which this application does not have. So a search
// asks Discord, and also looks through everyone this service has seen in the
// server — on a roster, invited or pinned — matching anywhere in their name
// or their short name. Those it finds only here are looked up in Discord, so
// they show as they are now, and anyone who has left drops out.
//
// A name is written down when someone signs up and was never read again, so
// a changed nickname stayed wrong on every page. The sync refreshes the names
// of everyone on a live event from Discord.

// knownPerson is someone this service has seen in a server.
type knownPerson struct {
	UserID       string
	DisplayName  string
	ReadableName string
}

// KnownPeopleInGuild is everyone on a roster, invited or pinned on any event
// in a server, each with the most recent name recorded for them and their
// short name.
func (s *Store) KnownPeopleInGuild(guildID string) ([]knownPerson, error) {
	rows, err := s.db.Query(`
		WITH seen AS (
			SELECT sg.discord_user_id AS uid, sg.display_name AS name, sg.state_changed_at AS at
			FROM signups sg JOIN events e ON e.id = sg.event_id
			WHERE e.guild_id = ? AND e.deleted_at = 0
			UNION ALL
			SELECT i.discord_user_id, i.display_name, i.at
			FROM event_invites i JOIN events e ON e.id = i.event_id
			WHERE e.guild_id = ? AND e.deleted_at = 0
			UNION ALL
			SELECT p.discord_user_id, p.display_name, p.pinned_at
			FROM event_pins p JOIN events e ON e.id = p.event_id
			WHERE e.guild_id = ? AND e.deleted_at = 0
		)
		SELECT u.uid,
		       COALESCE((SELECT name FROM seen s2 WHERE s2.uid = u.uid AND s2.name != ''
		                 ORDER BY s2.at DESC LIMIT 1), ''),
		       COALESCE(r.readable_name, '')
		FROM (SELECT DISTINCT uid FROM seen) u
		LEFT JOIN readable_names r ON r.discord_user_id = u.uid`, guildID, guildID, guildID)
	if err != nil {
		return nil, fmt.Errorf("list people seen in guild: %w", err)
	}
	defer rows.Close()
	var out []knownPerson
	for rows.Next() {
		var p knownPerson
		if err := rows.Scan(&p.UserID, &p.DisplayName, &p.ReadableName); err != nil {
			return nil, fmt.Errorf("scan person: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PeopleOnLiveEvents is everyone on a roster, invited or pinned on an event
// in a server that is not over: whose names the pages and tables show.
func (s *Store) PeopleOnLiveEvents(guildID string) ([]string, error) {
	rows, err := s.db.Query(`
		SELECT sg.discord_user_id FROM signups sg JOIN events e ON e.id = sg.event_id
		WHERE e.guild_id = ? AND e.deleted_at = 0 AND e.status IN (?, ?) AND sg.state != ?
		UNION
		SELECT i.discord_user_id FROM event_invites i JOIN events e ON e.id = i.event_id
		WHERE e.guild_id = ? AND e.deleted_at = 0 AND e.status IN (?, ?)
		UNION
		SELECT p.discord_user_id FROM event_pins p JOIN events e ON e.id = p.event_id
		WHERE e.guild_id = ? AND e.deleted_at = 0 AND p.unpinned_at = 0`,
		guildID, StatusOpen, StatusClosed, StateWithdrawn,
		guildID, StatusOpen, StatusClosed,
		guildID)
	if err != nil {
		return nil, fmt.Errorf("list people on live events: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan user id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// RenameInGuild records someone's current name in a server on every row that
// carries it — their signups, invites and pins there — and reports how many
// changed. Display only: nothing joins on a name.
func (s *Store) RenameInGuild(guildID, userID, name string) (int64, error) {
	if name == "" {
		return 0, nil
	}
	var total int64
	for _, table := range []string{"signups", "event_invites", "event_pins"} {
		res, err := s.db.Exec(`UPDATE `+table+` SET display_name = ?
			WHERE discord_user_id = ? AND display_name != ?
			  AND event_id IN (SELECT id FROM events WHERE guild_id = ?)`, name, userID, name, guildID)
		if err != nil {
			return total, fmt.Errorf("rename in %s: %w", table, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("rename in %s: %w", table, err)
		}
		total += n
	}
	return total, nil
}

// RefreshDisplayNames looks up everyone on a server's live events in Discord
// and records the name each goes by now. It returns how many people's names
// changed. Someone who has left keeps the last name recorded.
func (s *Server) RefreshDisplayNames(guildID string) (int, error) {
	if s.discord == nil {
		return 0, nil
	}
	people, err := s.store.PeopleOnLiveEvents(guildID)
	if err != nil {
		return 0, err
	}
	renamed := 0
	var problems []string
	for _, userID := range people {
		member, err := s.discord.GuildMember(guildID, userID)
		if errors.Is(err, ErrUnknownMember) {
			continue
		}
		if err != nil {
			problems = append(problems, userID+": "+err.Error())
			continue
		}
		n, err := s.store.RenameInGuild(guildID, userID, member.DisplayName)
		if err != nil {
			return renamed, err
		}
		if n > 0 {
			renamed++
			log.Printf("[discord-signup] %s in %s now goes by %q", userID, guildID, member.DisplayName)
		}
	}
	if len(problems) > 0 {
		return renamed, fmt.Errorf("could not look up %d people: %s", len(problems), strings.Join(problems, "; "))
	}
	return renamed, nil
}

// findMembers is the add boxes' search: Discord's own, which matches the
// start of a name, then everyone seen in the server whose name or short name
// contains what was typed, looked up in Discord so they show as they are now.
// At most limit people, Discord's matches first.
func (s *Server) findMembers(guildID, query string, limit int) ([]MemberMatch, error) {
	fromDiscord, err := s.discord.SearchGuildMembers(guildID, query, limit)
	if err != nil {
		return nil, err
	}
	out := fromDiscord
	found := map[string]bool{}
	for _, m := range fromDiscord {
		found[m.UserID] = true
	}
	known, err := s.store.KnownPeopleInGuild(guildID)
	if err != nil {
		return nil, err
	}
	needle := strings.ToLower(query)
	var extra []string
	for _, p := range known {
		if found[p.UserID] || len(out)+len(extra) >= limit {
			continue
		}
		if strings.Contains(strings.ToLower(p.DisplayName), needle) || strings.Contains(strings.ToLower(p.ReadableName), needle) {
			extra = append(extra, p.UserID)
		}
	}
	// Looked up together: a search runs as someone types.
	looked := make([]MemberMatch, len(extra))
	lookErr := make([]error, len(extra))
	var wg sync.WaitGroup
	for i, userID := range extra {
		wg.Add(1)
		go func() {
			defer wg.Done()
			looked[i], lookErr[i] = s.discord.GuildMember(guildID, userID)
		}()
	}
	wg.Wait()
	for i, m := range looked {
		switch {
		case errors.Is(lookErr[i], ErrUnknownMember):
			continue // left the server
		case lookErr[i] != nil:
			return nil, fmt.Errorf("look up %s: %w", extra[i], lookErr[i])
		}
		out = append(out, m)
		// The lookup is the current name; the rows carry it from now on.
		if _, err := s.store.RenameInGuild(guildID, m.UserID, m.DisplayName); err != nil {
			log.Printf("[discord-signup] record current name of %s: %v", m.UserID, err)
		}
	}
	return out, nil
}
