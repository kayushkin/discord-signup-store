package discordsignup

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
)

// The servers the bot is in, kept in bot_guilds so a web page never has to ask
// Discord for them. See schema.sql for why.

// RecordBotGuild saves a server the bot is in, with its owner — what the
// gateway knows about one.
func (s *Store) RecordBotGuild(guildID, name, ownerID string) error {
	_, err := s.db.Exec(`
		INSERT INTO bot_guilds (guild_id, name, owner_id, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(guild_id) DO UPDATE SET name = excluded.name, owner_id = excluded.owner_id,
			updated_at = excluded.updated_at`, guildID, name, ownerID, now())
	if err != nil {
		return fmt.Errorf("record bot guild %s: %w", guildID, err)
	}
	return nil
}

// RecordBotGuildName saves a server the bot is in without touching its owner:
// Discord's list of the bot's servers carries names but no owners.
func (s *Store) RecordBotGuildName(guildID, name string) error {
	_, err := s.db.Exec(`
		INSERT INTO bot_guilds (guild_id, name, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(guild_id) DO UPDATE SET name = excluded.name, updated_at = excluded.updated_at`,
		guildID, name, now())
	if err != nil {
		return fmt.Errorf("record bot guild %s: %w", guildID, err)
	}
	return nil
}

// RemoveBotGuild forgets a server the bot has left.
func (s *Store) RemoveBotGuild(guildID string) error {
	if _, err := s.db.Exec(`DELETE FROM bot_guilds WHERE guild_id = ?`, guildID); err != nil {
		return fmt.Errorf("remove bot guild %s: %w", guildID, err)
	}
	return nil
}

// KeepOnlyBotGuilds forgets every server not in guildIDs — Discord's whole
// list — so a server the bot left while nobody was listening goes too.
func (s *Store) KeepOnlyBotGuilds(guildIDs []string) error {
	query := `DELETE FROM bot_guilds`
	args := make([]any, len(guildIDs))
	for i, id := range guildIDs {
		args[i] = id
	}
	if len(guildIDs) > 0 {
		query += ` WHERE guild_id NOT IN (?` + strings.Repeat(`, ?`, len(guildIDs)-1) + `)`
	}
	if _, err := s.db.Exec(query, args...); err != nil {
		return fmt.Errorf("prune bot guilds: %w", err)
	}
	return nil
}

// BotGuilds lists the servers the bot is in, by name.
func (s *Store) BotGuilds() ([]Guild, error) {
	rows, err := s.db.Query(`SELECT guild_id, name FROM bot_guilds ORDER BY name, guild_id`)
	if err != nil {
		return nil, fmt.Errorf("list bot guilds: %w", err)
	}
	defer rows.Close()
	var out []Guild
	for rows.Next() {
		var g Guild
		if err := rows.Scan(&g.ID, &g.Name); err != nil {
			return nil, fmt.Errorf("scan bot guild: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// BotGuildOwnerID returns the Discord user id of a server's owner. A server
// the bot is not in, or one whose owner has not been read yet, is an error
// rather than "nobody": the caller is deciding who may edit.
func (s *Store) BotGuildOwnerID(guildID string) (string, error) {
	var ownerID string
	err := s.db.QueryRow(`SELECT owner_id FROM bot_guilds WHERE guild_id = ?`, guildID).Scan(&ownerID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("the bot is not recorded as being in server %s", guildID)
	}
	if err != nil {
		return "", fmt.Errorf("read the owner of server %s: %w", guildID, err)
	}
	if ownerID == "" {
		return "", fmt.Errorf("the owner of server %s has not been read from Discord yet", guildID)
	}
	return ownerID, nil
}

// RefreshBotGuilds rewrites bot_guilds from Discord's REST API: every server
// the bot is in, and the owner of any whose owner is not yet known. For the
// ten-minute sync, which is what keeps the table right with the gateway off.
func (s *Server) RefreshBotGuilds() error {
	if s.discord == nil {
		return errors.New("no discord client configured")
	}
	guilds, err := s.discord.ListBotGuilds()
	if err != nil {
		return fmt.Errorf("list bot guilds: %w", err)
	}
	ids := make([]string, 0, len(guilds))
	for _, g := range guilds {
		if err := s.store.RecordBotGuildName(g.ID, g.Name); err != nil {
			return err
		}
		ids = append(ids, g.ID)
	}
	if err := s.store.KeepOnlyBotGuilds(ids); err != nil {
		return err
	}
	for _, g := range guilds {
		if _, err := s.store.BotGuildOwnerID(g.ID); err == nil {
			continue
		}
		ownerID, err := s.discord.GuildOwnerID(g.ID)
		if err != nil {
			return fmt.Errorf("read the owner of %s: %w", g.Name, err)
		}
		if err := s.store.RecordBotGuild(g.ID, g.Name, ownerID); err != nil {
			return err
		}
	}
	return nil
}

// recordGatewayGuild saves what a GUILD_CREATE or GUILD_UPDATE said.
func (s *Server) recordGatewayGuild(guildID, name, ownerID string) {
	if err := s.store.RecordBotGuild(guildID, name, ownerID); err != nil {
		log.Printf("[discord-signup] %v", err)
	}
}
