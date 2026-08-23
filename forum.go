package discordsignup

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// The forum surface: one forum post per event, running ALONGSIDE the cards and
// the table so the shapes can be compared on the same live data.
//
// A forum post is a thread whose first message is required and shares the
// thread's id. That one message carries the same card RenderSignupMessage
// already draws — same buttons, same custom_ids, one handler — and the replies
// under it are the discussion. The post's own title carries the capacity badge
// and its tags flip between open/full/finished/cancelled, which is what makes
// the forum's list view a filterable table for free.

// GuildForum is where a guild's forum surface lives, tags by id.
type GuildForum struct {
	GuildID      string `json:"guild_id"`
	ChannelID    string `json:"channel_id"`
	TagOpen      string `json:"tag_open"`
	TagFull      string `json:"tag_full"`
	TagFinished  string `json:"tag_finished"`
	TagCancelled string `json:"tag_cancelled"`
}

// SetGuildForum records the forum and its tag ids.
func (s *Store) SetGuildForum(f GuildForum) error {
	_, err := s.db.Exec(`
		INSERT INTO guild_forums (guild_id, channel_id, tag_open, tag_full,
		                          tag_finished, tag_cancelled, updated_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(guild_id) DO UPDATE SET channel_id = excluded.channel_id,
			tag_open = excluded.tag_open, tag_full = excluded.tag_full,
			tag_finished = excluded.tag_finished, tag_cancelled = excluded.tag_cancelled,
			updated_at = excluded.updated_at`,
		f.GuildID, f.ChannelID, f.TagOpen, f.TagFull, f.TagFinished, f.TagCancelled, now())
	if err != nil {
		return fmt.Errorf("set guild forum: %w", err)
	}
	return nil
}

// GuildForum reads a guild's forum config.
func (s *Store) GuildForum(guildID string) (*GuildForum, error) {
	var f GuildForum
	err := s.db.QueryRow(`
		SELECT guild_id, channel_id, tag_open, tag_full, tag_finished, tag_cancelled
		FROM guild_forums WHERE guild_id = ?`, guildID).
		Scan(&f.GuildID, &f.ChannelID, &f.TagOpen, &f.TagFull, &f.TagFinished, &f.TagCancelled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read guild forum: %w", err)
	}
	return &f, nil
}

// forumTag names the tags this service manages, matched by name when adopting
// a forum and joined by id ever after.
var forumTagNames = []string{"open", "full", "finished", "cancelled"}

// CreateForumPost starts a post: a thread plus its required first message.
func (c *DiscordClient) CreateForumPost(forumChannelID, name string, appliedTags []string, message map[string]any) (string, error) {
	raw, err := c.do(http.MethodPost, "/channels/"+forumChannelID+"/threads", map[string]any{
		"name":                  name,
		"applied_tags":          appliedTags,
		"auto_archive_duration": 10080,
		"message":               message,
	})
	if err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decode created forum post: %w", err)
	}
	return out.ID, nil
}

// ModifyThread patches a thread — title, tags, archived.
func (c *DiscordClient) ModifyThread(threadID string, patch map[string]any) error {
	_, err := c.do(http.MethodPatch, "/channels/"+threadID, patch)
	return err
}

// ForumChannelTags reads a forum's available tags as name → id.
func (c *DiscordClient) ForumChannelTags(channelID string) (map[string]string, error) {
	raw, err := c.do(http.MethodGet, "/channels/"+channelID, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Type          int `json:"type"`
		AvailableTags []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"available_tags"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode forum channel: %w", err)
	}
	if out.Type != 15 {
		return nil, fmt.Errorf("channel %s is type %d, not a forum (15)", channelID, out.Type)
	}
	tags := map[string]string{}
	for _, t := range out.AvailableTags {
		tags[strings.ToLower(t.Name)] = t.ID
	}
	return tags, nil
}

// AdoptForum wires a guild's forum surface to an existing forum channel,
// creating any of the managed tags it lacks, and posts every live event into
// it.
func (s *Server) AdoptForum(guildID, channelID string) (*GuildForum, error) {
	if s.discord == nil {
		return nil, errors.New("no discord client configured")
	}
	tags, err := s.discord.ForumChannelTags(channelID)
	if err != nil {
		return nil, err
	}
	var missing []map[string]any
	for _, name := range forumTagNames {
		if _, ok := tags[name]; !ok {
			// finished and cancelled are moderated: they describe facts this
			// service asserts, and a user hand-applying them would lie.
			missing = append(missing, map[string]any{
				"name": name, "moderated": name == "finished" || name == "cancelled"})
		}
	}
	if len(missing) > 0 {
		// PATCHing available_tags replaces the whole list, so the existing
		// tags ride along or they would be destroyed.
		raw, err := s.discord.do(http.MethodGet, "/channels/"+channelID, nil)
		if err != nil {
			return nil, err
		}
		var current struct {
			AvailableTags []map[string]any `json:"available_tags"`
		}
		if err := json.Unmarshal(raw, &current); err != nil {
			return nil, fmt.Errorf("decode tags: %w", err)
		}
		if err := s.discord.ModifyThread(channelID, map[string]any{
			"available_tags": append(current.AvailableTags, missing...)}); err != nil {
			return nil, fmt.Errorf("add managed tags: %w", err)
		}
		if tags, err = s.discord.ForumChannelTags(channelID); err != nil {
			return nil, err
		}
	}
	forum := GuildForum{
		GuildID: guildID, ChannelID: channelID,
		TagOpen: tags["open"], TagFull: tags["full"],
		TagFinished: tags["finished"], TagCancelled: tags["cancelled"],
	}
	if err := s.store.SetGuildForum(forum); err != nil {
		return nil, err
	}
	live, err := s.liveEventsFor(guildID)
	if err != nil {
		return nil, err
	}
	for i := range live {
		s.refreshForumPostQuietly(&live[i])
	}
	return &forum, nil
}

// forumPostTitle is the badge, the name, and the compact date.
func forumPostTitle(ev *Event) string {
	title := capacityPrefix(ev) + ev.Name
	if ev.StartsAt > 0 {
		title += " — " + compactWhen(ev)
	}
	return truncate(title, 100)
}

// forumTagFor picks the one tag that states the event's condition.
func forumTagFor(ev *Event, f *GuildForum) string {
	switch {
	case ev.Status == StatusCancelled:
		return f.TagCancelled
	case ev.Status == StatusCompleted:
		return f.TagFinished
	case ev.Capacity > 0 && ev.AttendingCount >= ev.Capacity:
		return f.TagFull
	default:
		return f.TagOpen
	}
}

// refreshForumPost creates or updates an event's post: the title and tag on
// the thread, and the card in its first message.
//
// The card is RenderSignupMessage verbatim — the same buttons with the same
// custom_ids as the board card, so one handler serves both surfaces and they
// cannot drift.
//
// ⚠️ Discord rate-limits THREAD RENAMES far harder than message edits — about
// two per ten minutes per thread. So the thread PATCH is skipped unless the
// title or tag actually changed; under signup churn the title badge may lag,
// and the card inside (edited freely) stays current.
func (s *Server) refreshForumPost(ev *Event) error {
	if s.discord == nil {
		return nil
	}
	forum, err := s.store.GuildForum(ev.GuildID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	roster, err := s.store.Roster(ev.ID, false)
	if err != nil {
		return err
	}
	card := RenderSignupMessage(ev, roster)
	title := forumPostTitle(ev)
	tag := forumTagFor(ev, forum)

	if ev.ForumPostID == "" {
		if IsArchived(ev.Status) {
			return nil // no post for an event that is already over
		}
		postID, err := s.discord.CreateForumPost(forum.ChannelID, title, []string{tag}, card)
		if err != nil {
			return fmt.Errorf("create forum post: %w", err)
		}
		if _, err := s.store.UpdateEvent(ev.ID, EventPatch{ForumPostID: &postID}); err != nil {
			return err
		}
		ev.ForumPostID = postID
		log.Printf("[discord-signup] opened forum post %s for event %d (%q)", postID, ev.ID, ev.Name)
		return nil
	}

	// The first message shares the post's id.
	if err := s.discord.EditMessage(ev.ForumPostID, ev.ForumPostID, card); err != nil {
		return fmt.Errorf("edit forum card: %w", err)
	}
	patch := map[string]any{"name": title, "applied_tags": []string{tag}}
	if IsArchived(ev.Status) {
		patch["archived"] = true
	}
	if err := s.discord.ModifyThread(ev.ForumPostID, patch); err != nil {
		return fmt.Errorf("retitle forum post: %w", err)
	}
	return nil
}

func (s *Server) refreshForumPostQuietly(ev *Event) {
	if err := s.refreshForumPost(ev); err != nil {
		log.Printf("[discord-signup] refresh forum post for event %d: %v", ev.ID, err)
	}
}
