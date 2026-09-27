package discordsignup

import (
	"errors"
	"fmt"
	"log"
)

// What people are called in each server, kept in member_names so an event
// page does not ask Discord about every person on every load. See schema.sql.

// memberName is one member_names row.
type memberName struct {
	DisplayName string
	LeftGuild   bool
}

// RecordMemberName saves what someone is called in a server now.
func (s *Store) RecordMemberName(guildID, userID, displayName string) error {
	_, err := s.db.Exec(`
		INSERT INTO member_names (guild_id, discord_user_id, display_name, left_guild, updated_at)
		VALUES (?, ?, ?, 0, ?)
		ON CONFLICT(guild_id, discord_user_id) DO UPDATE SET display_name = excluded.display_name,
			left_guild = 0, updated_at = excluded.updated_at`, guildID, userID, displayName, now())
	if err != nil {
		return fmt.Errorf("record the name of %s in %s: %w", userID, guildID, err)
	}
	return nil
}

// RecordMemberLeft saves that someone is no longer in a server, keeping the
// last name seen for them.
func (s *Store) RecordMemberLeft(guildID, userID string) error {
	_, err := s.db.Exec(`
		INSERT INTO member_names (guild_id, discord_user_id, left_guild, updated_at) VALUES (?, ?, 1, ?)
		ON CONFLICT(guild_id, discord_user_id) DO UPDATE SET left_guild = 1, updated_at = excluded.updated_at`,
		guildID, userID, now())
	if err != nil {
		return fmt.Errorf("record that %s left %s: %w", userID, guildID, err)
	}
	return nil
}

// MemberNames is every recorded person in a server, by Discord user id.
func (s *Store) MemberNames(guildID string) (map[string]memberName, error) {
	rows, err := s.db.Query(`SELECT discord_user_id, display_name, left_guild FROM member_names WHERE guild_id = ?`, guildID)
	if err != nil {
		return nil, fmt.Errorf("read member names in %s: %w", guildID, err)
	}
	defer rows.Close()
	out := map[string]memberName{}
	for rows.Next() {
		var userID string
		var n memberName
		if err := rows.Scan(&userID, &n.DisplayName, &n.LeftGuild); err != nil {
			return nil, fmt.Errorf("scan member name: %w", err)
		}
		out[userID] = n
	}
	return out, rows.Err()
}

// memberNameFor returns what someone is called in a server: from known, the
// server's member_names rows, when it has them, and otherwise from Discord,
// recording the answer in the store and in known so nobody is asked about
// twice. A failed lookup is logged and records nothing, so the next load
// tries again.
func (s *Server) memberNameFor(guildID, userID string, known map[string]memberName) memberName {
	if n, ok := known[userID]; ok || s.discord == nil {
		return n
	}
	member, err := s.discord.GuildMember(guildID, userID)
	if errors.Is(err, ErrUnknownMember) {
		n := memberName{LeftGuild: true}
		known[userID] = n
		if err := s.store.RecordMemberLeft(guildID, userID); err != nil {
			log.Printf("[discord-signup] %v", err)
		}
		return n
	}
	if err != nil {
		log.Printf("[discord-signup] look up %s in %s: %v", userID, guildID, err)
		return memberName{}
	}
	n := memberName{DisplayName: member.DisplayName}
	known[userID] = n
	if err := s.store.RecordMemberName(guildID, userID, member.DisplayName); err != nil {
		log.Printf("[discord-signup] %v", err)
	}
	return n
}
