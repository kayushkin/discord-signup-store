package discordsignup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Filing DM channels with message-store. Discord gives a bot no list of its
// DMs, so message-store's message-import-discord-bot cannot find them alone:
// each DM channel this bot opens or is written to in is filed there as a
// direct conversation with the bot and the person in it, and the importer
// then reads its messages. Only the channel is filed here; the messages come
// from the importer, which reads them from Discord as they are.

// MessageStoreClient files DM channels with message-store.
type MessageStoreClient struct {
	baseURL string
	http    *http.Client

	mu sync.Mutex
	// filed is each channel filed since start, so a burst of DMs to one
	// person files their channel once.
	filed map[string]bool
}

// NewMessageStoreClient is nil when baseURL is empty: DM channels are then
// not filed, and nothing else changes.
func NewMessageStoreClient(baseURL string) *MessageStoreClient {
	if baseURL == "" {
		return nil
	}
	return &MessageStoreClient{baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: 30 * time.Second}, filed: map[string]bool{}}
}

// DirectMessageChannel is a DM channel and the person on the other side.
type DirectMessageChannel struct {
	ChannelID     string
	DiscordUserID string
}

// FileDirectMessageChannels files each channel not filed since start, in one
// message-store import.
func (c *MessageStoreClient) FileDirectMessageChannels(botUserID string, channels []DirectMessageChannel) error {
	if botUserID == "" {
		return fmt.Errorf("file dm channels: the bot's own user id is unknown")
	}
	c.mu.Lock()
	var conversations []map[string]any
	var channelIDs []string
	for _, channel := range channels {
		if channel.ChannelID == "" || c.filed[channel.ChannelID] {
			continue
		}
		c.filed[channel.ChannelID] = true
		channelIDs = append(channelIDs, channel.ChannelID)
		participants := []map[string]any{{"handle": botUserID}}
		if channel.DiscordUserID != "" {
			participants = append(participants, map[string]any{"handle": channel.DiscordUserID})
		}
		conversations = append(conversations, map[string]any{"platform": "discord", "source": "discord", "source_ref": channel.ChannelID,
			"title": "", "is_group": false, "participants": participants})
	}
	c.mu.Unlock()
	if len(conversations) == 0 {
		return nil
	}
	err := c.fileConversations(conversations)
	if err != nil {
		// Not filed: let the next DM try again.
		c.mu.Lock()
		for _, id := range channelIDs {
			delete(c.filed, id)
		}
		c.mu.Unlock()
	}
	return err
}

func (c *MessageStoreClient) fileConversations(conversations []map[string]any) error {
	var run struct {
		ID string `json:"id"`
	}
	if err := c.post("/imports", map[string]any{"importer": "discord-signup-store", "description": "DM channels the Discord bot opened or was written to in"}, &run); err != nil {
		return err
	}
	if err := c.post("/imports/"+run.ID+"/batches", map[string]any{"conversations": conversations, "messages": []any{}, "conversation_days": []any{}}, nil); err != nil {
		return err
	}
	return c.post("/imports/"+run.ID+"/finish", map[string]any{}, nil)
}

func (c *MessageStoreClient) post(path string, body, out any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	response, err := c.http.Post(c.baseURL+path, "application/json", bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("message-store POST %s: %w", path, err)
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("message-store POST %s answered %d: %s", path, response.StatusCode, strings.TrimSpace(string(answer)))
	}
	if out != nil {
		return json.Unmarshal(answer, out)
	}
	return nil
}

// EnableMessageStore files every DM channel the bot opens or is written to in
// with message-store, and at once every one it has recorded sending in.
func (s *Server) EnableMessageStore(messages *MessageStoreClient) {
	s.messages = messages
	s.discord.onDirectMessageChannel = s.fileDirectMessageChannel
	s.background.Add(1)
	go func() {
		defer s.background.Done()
		channels, err := s.store.DirectMessageChannels()
		if err != nil {
			log.Printf("[discord-signup] ERROR list dm channels to file with message-store: %v", err)
			return
		}
		if err := messages.FileDirectMessageChannels(s.applicationUserID(), channels); err != nil {
			log.Printf("[discord-signup] ERROR file %d dm channels with message-store: %v", len(channels), err)
			return
		}
		log.Printf("[discord-signup] filed %d dm channels with message-store", len(channels))
	}()
}

// fileDirectMessageChannel files one DM channel in the background: a DM is
// sent or read whether or not message-store answers.
func (s *Server) fileDirectMessageChannel(channelID, discordUserID string) {
	if s.messages == nil {
		return
	}
	s.background.Add(1)
	go func() {
		defer s.background.Done()
		if err := s.messages.FileDirectMessageChannels(s.applicationUserID(), []DirectMessageChannel{{ChannelID: channelID, DiscordUserID: discordUserID}}); err != nil {
			log.Printf("[discord-signup] ERROR file dm channel %s with message-store: %v", channelID, err)
		}
	}()
}

// DirectMessageChannels lists every DM channel the bot recorded sending in,
// with whom.
func (s *Store) DirectMessageChannels() ([]DirectMessageChannel, error) {
	rows, err := s.db.Query(`SELECT channel_id, MIN(discord_user_id) FROM event_dms WHERE channel_id != '' GROUP BY channel_id ORDER BY channel_id`)
	if err != nil {
		return nil, fmt.Errorf("list dm channels: %w", err)
	}
	defer rows.Close()
	var channels []DirectMessageChannel
	for rows.Next() {
		var channel DirectMessageChannel
		if err := rows.Scan(&channel.ChannelID, &channel.DiscordUserID); err != nil {
			return nil, err
		}
		channels = append(channels, channel)
	}
	return channels, rows.Err()
}
