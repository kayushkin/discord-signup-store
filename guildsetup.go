package discordsignup

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// Setting a server up in one go: the category and the five channels this
// service posts into, recorded on the guild's row, the forum adopted, the
// how-to pinned, the tables drawn. Runs when the bot joins a server and on
// POST /api/guilds/{id}/setup, and is safe to run again: a channel that
// already exists under its name is reused, and a channel already recorded
// that still exists is kept.
//
// The board and the event table are the SAME channel, as they are on every
// server so far: the table is the board's contents, and a card has nowhere
// else to be.

const (
	setupCategoryName        = "Events"
	setupBoardChannelName    = "events"
	setupManagementName      = "event-management"
	setupForumChannelName    = "event-forum"
	setupPastChannelName     = "past-events"
	setupReminderChannelName = "event-reminders"

	discordChannelTypeText     = 0
	discordChannelTypeCategory = 4
	discordChannelTypeForum    = 15
)

// ListGuildChannels reads every channel in a guild.
func (c *DiscordClient) ListGuildChannels(guildID string) ([]Channel, error) {
	raw, err := c.do(http.MethodGet, "/guilds/"+escapePathSegment(guildID)+"/channels", nil)
	if err != nil {
		return nil, err
	}
	var out []Channel
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode guild channels: %w", err)
	}
	return out, nil
}

// SetUpGuild makes a server ready: every channel found or created, the row
// written, the forum adopted, the how-to pinned, the tables drawn. Returns
// the row as it now stands.
func (s *Server) SetUpGuild(guildID string) (*GuildTable, error) {
	if s.discord == nil {
		return nil, errors.New("no discord client configured")
	}
	channels, err := s.discord.ListGuildChannels(guildID)
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	byName := map[string]Channel{}
	byID := map[string]Channel{}
	for _, ch := range channels {
		byName[strings.ToLower(ch.Name)+"/"+fmt.Sprint(ch.Type)] = ch
		byID[ch.ID] = ch
	}
	existing, err := s.store.GuildTable(guildID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if existing == nil {
		existing = &GuildTable{GuildID: guildID}
	}

	// find reuses, in order: the channel already recorded if Discord still
	// has it, then one with the name, and only then creates.
	category := ""
	find := func(recorded, name string, channelType int) (string, error) {
		if recorded != "" {
			if _, still := byID[recorded]; still {
				return recorded, nil
			}
			log.Printf("[discord-signup] guild %s: recorded channel %s is gone; making a new %q", guildID, recorded, name)
		}
		if ch, found := byName[name+"/"+fmt.Sprint(channelType)]; found {
			return ch.ID, nil
		}
		payload := map[string]any{"name": name, "type": channelType}
		if category != "" {
			payload["parent_id"] = category
		}
		created, err := s.discord.CreateGuildChannel(guildID, payload)
		if err != nil {
			return "", fmt.Errorf("create #%s: %w", name, err)
		}
		log.Printf("[discord-signup] guild %s: created #%s (%s)", guildID, name, created.ID)
		return created.ID, nil
	}

	if category, err = find("", setupCategoryName, discordChannelTypeCategory); err != nil {
		return nil, err
	}
	board, err := find(existing.BoardChannelID, setupBoardChannelName, discordChannelTypeText)
	if err != nil {
		return nil, err
	}
	management, err := find(existing.ManagementChannelID, setupManagementName, discordChannelTypeText)
	if err != nil {
		return nil, err
	}
	past, err := find(existing.PastChannelID, setupPastChannelName, discordChannelTypeText)
	if err != nil {
		return nil, err
	}
	reminders, err := find(existing.ReminderChannelID, setupReminderChannelName, discordChannelTypeText)
	if err != nil {
		return nil, err
	}
	forumRecorded := ""
	if f, err := s.store.GuildForum(guildID); err == nil {
		forumRecorded = f.ChannelID
	}
	forum, err := find(forumRecorded, setupForumChannelName, discordChannelTypeForum)
	if err != nil {
		return nil, err
	}

	// Recorded before anything is posted, so a failure below leaves a row
	// that says where things are and a second run picks up from there.
	if err := s.store.SetGuildChannels(guildID, GuildChannels{Board: board, Past: past, Reminder: reminders}); err != nil {
		return nil, err
	}
	if err := s.store.SetGuildTable(guildID, board); err != nil {
		return nil, err
	}
	if err := s.store.SetGuildManagementChannel(guildID, management); err != nil {
		return nil, err
	}
	if _, err := s.AdoptForum(guildID, forum); err != nil {
		return nil, fmt.Errorf("adopt forum: %w", err)
	}
	if _, err := s.PublishHowToMessage(board, ""); err != nil {
		return nil, fmt.Errorf("how-to: %w", err)
	}
	if err := s.RefreshEventTable(guildID); err != nil {
		return nil, fmt.Errorf("draw table: %w", err)
	}
	if err := s.RefreshManagementTable(guildID); err != nil {
		return nil, fmt.Errorf("draw management table: %w", err)
	}
	log.Printf("[discord-signup] guild %s set up: board %s, management %s, forum %s, past %s, reminders %s",
		guildID, board, management, forum, past, reminders)
	return s.store.GuildTable(guildID)
}

// handleSetUpGuild is SetUpGuild over HTTP.
func (s *Server) handleSetUpGuild(w http.ResponseWriter, r *http.Request) {
	table, err := s.SetUpGuild(r.PathValue("guildID"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, table)
}
