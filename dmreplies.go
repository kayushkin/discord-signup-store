package discordsignup

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// Replies to this service's DMs. Every DM sent about an event is recorded
// with the event; when someone writes back, the gateway hands their message
// over, and it is put with the event it answers and shown on that event's
// page. One DM channel carries every event's messages to a person, so the
// event is found from what they replied to — Discord's Reply on one of our
// DMs names it exactly — or, for a plain message, from the latest DM we sent
// them in the last replyWindow, and the page says which.
//
// A DM from a person to the bot carries its text without the privileged
// Message Content intent; DIRECT_MESSAGES, which is not privileged, is what
// delivers it.

// replyWindow is how long after our last DM a plain message is taken as an
// answer to it.
const replyWindow = 14 * 24 * time.Hour

// Kinds of DM sent about an event.
const (
	DMKindMessage  = "message"  // an organiser's message
	DMKindInvite   = "invite"   // an invite
	DMKindPlaced   = "placed"   // an organiser put them on a list
	DMKindPromoted = "promoted" // they were given a place
)

// How a reply was put with its event.
const (
	ReplyMatchedReply  = "reply"
	ReplyMatchedLatest = "latest"
)

// dmFooter goes under every DM about an event, so nobody writes back
// thinking it is private to the bot.
const dmFooter = "\n-# Replies here are passed to the organisers."

// RecordEventDM notes a DM sent about an event.
func (s *Store) RecordEventDM(messageID, channelID string, eventID int64, userID, kind, summary string) error {
	_, err := s.db.Exec(`INSERT INTO event_dms (message_id, channel_id, event_id, discord_user_id, kind, summary, sent_at)
		VALUES (?,?,?,?,?,?,?) ON CONFLICT(message_id) DO NOTHING`,
		messageID, channelID, eventID, userID, kind, summary, now())
	if err != nil {
		return fmt.Errorf("record event dm: %w", err)
	}
	return nil
}

// EventForDMReply finds the event a DM from a person is about: the DM they
// replied to, if they used Reply on one of ours, or else the latest DM we
// sent them since since. ErrNotFound when neither.
func (s *Store) EventForDMReply(userID, repliedTo string, since int64) (eventID int64, matched string, err error) {
	if repliedTo != "" {
		err = s.db.QueryRow(`SELECT event_id FROM event_dms WHERE message_id = ? AND discord_user_id = ?`,
			repliedTo, userID).Scan(&eventID)
		if err == nil {
			return eventID, ReplyMatchedReply, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, "", fmt.Errorf("find replied-to dm: %w", err)
		}
	}
	err = s.db.QueryRow(`SELECT event_id FROM event_dms WHERE discord_user_id = ? AND sent_at >= ?
		ORDER BY sent_at DESC, rowid DESC LIMIT 1`, userID, since).Scan(&eventID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", ErrNotFound
	}
	if err != nil {
		return 0, "", fmt.Errorf("find latest dm: %w", err)
	}
	return eventID, ReplyMatchedLatest, nil
}

// DMReply is one message someone wrote back.
type DMReply struct {
	ID            int64  `json:"id"`
	EventID       int64  `json:"event_id"`
	DiscordUserID string `json:"discord_user_id"`
	DisplayName   string `json:"display_name"`
	ReadableName  string `json:"readable_name,omitempty"`
	MessageID     string `json:"message_id"`
	Content       string `json:"content"`
	Attachments   int    `json:"attachments"`
	RepliedTo     string `json:"replied_to"`
	Matched       string `json:"matched"`
	At            int64  `json:"at"`
	// AnsweringKind and AnsweringSummary say what our DM it answers was: the
	// one replied to, or for a plain message the latest before it.
	AnsweringKind    string `json:"answering_kind"`
	AnsweringSummary string `json:"answering_summary"`
}

// RecordDMReply stores one reply. A message already stored — the gateway can
// deliver one twice across a resume — is no change.
func (s *Store) RecordDMReply(r DMReply) error {
	_, err := s.db.Exec(`INSERT INTO event_dm_replies (event_id, discord_user_id, display_name, message_id,
		content, attachments, replied_to, matched, at) VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(message_id) DO NOTHING`,
		r.EventID, r.DiscordUserID, r.DisplayName, r.MessageID, r.Content, r.Attachments, r.RepliedTo, r.Matched, r.At)
	if err != nil {
		return fmt.Errorf("record dm reply: %w", err)
	}
	return nil
}

// DMReplies lists the replies about an event, newest first, each with what
// they answered.
func (s *Store) DMReplies(eventID int64) ([]DMReply, error) {
	rows, err := s.db.Query(`
		SELECT r.id, r.event_id, r.discord_user_id, r.display_name, COALESCE(n.readable_name, ''),
		       r.message_id, r.content, r.attachments, r.replied_to, r.matched, r.at,
		       COALESCE(d.kind, ''), COALESCE(d.summary, '')
		FROM event_dm_replies r
		LEFT JOIN readable_names n ON n.discord_user_id = r.discord_user_id
		LEFT JOIN event_dms d ON d.message_id = CASE WHEN r.replied_to != '' THEN r.replied_to ELSE
			(SELECT message_id FROM event_dms WHERE discord_user_id = r.discord_user_id AND event_id = r.event_id
			 AND sent_at <= r.at ORDER BY sent_at DESC, rowid DESC LIMIT 1) END
		WHERE r.event_id = ? ORDER BY r.at DESC, r.id DESC`, eventID)
	if err != nil {
		return nil, fmt.Errorf("list dm replies: %w", err)
	}
	defer rows.Close()
	out := []DMReply{}
	for rows.Next() {
		var r DMReply
		if err := rows.Scan(&r.ID, &r.EventID, &r.DiscordUserID, &r.DisplayName, &r.ReadableName,
			&r.MessageID, &r.Content, &r.Attachments, &r.RepliedTo, &r.Matched, &r.At,
			&r.AnsweringKind, &r.AnsweringSummary); err != nil {
			return nil, fmt.Errorf("scan dm reply: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// sendEventDM sends a DM about an event and records it, so a reply can be
// traced back. The footer tells them replies reach the organisers.
func (s *Server) sendEventDM(ev *Event, userID string, payload map[string]any, kind, summary string) error {
	if content, ok := payload["content"].(string); ok {
		payload["content"] = content + dmFooter
	}
	channelID, messageID, err := s.discord.SendDirectMessageRecorded(userID, payload)
	if err != nil {
		return err
	}
	if err := s.store.RecordEventDM(messageID, channelID, ev.ID, userID, kind, summary); err != nil {
		log.Printf("[discord-signup] record dm %s to %s about %d: %v", messageID, userID, ev.ID, err)
	}
	return nil
}

// summarise shortens a message body to one line for beside a reply.
func summarise(body string) string {
	line := strings.TrimSpace(strings.SplitN(body, "\n", 2)[0])
	if r := []rune(line); len(r) > 80 {
		line = string(r[:79]) + "…"
	}
	return line
}

// receiveDMReply takes a message someone sent the bot in a DM and puts it
// with the event it answers. Not about any event we wrote to them about
// lately: left alone. It is acknowledged with a 📨 so they know it arrived.
func (s *Server) receiveDMReply(channelID, messageID, userID, content, repliedTo string, attachments int) {
	eventID, matched, err := s.store.EventForDMReply(userID, repliedTo, now()-int64(replyWindow/time.Second))
	if errors.Is(err, ErrNotFound) {
		log.Printf("[discord-signup] dm from %s about no recent event; left alone", userID)
		return
	}
	if err != nil {
		log.Printf("[discord-signup] find event for dm from %s: %v", userID, err)
		return
	}
	ev, err := s.store.GetEvent(eventID)
	if err != nil {
		log.Printf("[discord-signup] load event %d for dm reply: %v", eventID, err)
		return
	}
	name := ""
	if s.discord != nil {
		if member, err := s.discord.GuildMember(ev.GuildID, userID); err == nil {
			name = member.DisplayName
		} else {
			log.Printf("[discord-signup] name dm replier %s in %s: %v", userID, ev.GuildID, err)
		}
	}
	if err := s.store.RecordDMReply(DMReply{EventID: eventID, DiscordUserID: userID, DisplayName: name,
		MessageID: messageID, Content: content, Attachments: attachments, RepliedTo: repliedTo,
		Matched: matched, At: now()}); err != nil {
		log.Printf("[discord-signup] record dm reply from %s: %v", userID, err)
		return
	}
	log.Printf("[discord-signup] dm reply from %s put with event %d (%s)", userID, eventID, matched)
	if s.discord != nil {
		if err := s.discord.CreateOwnReaction(channelID, messageID, "📨"); err != nil {
			log.Printf("[discord-signup] acknowledge dm reply %s: %v", messageID, err)
		}
	}
}

// SendDirectMessageRecorded sends a DM and returns where it went and its id,
// so a reply to it can be traced.
func (c *DiscordClient) SendDirectMessageRecorded(userID string, payload map[string]any) (channelID, messageID string, err error) {
	raw, err := c.do(http.MethodPost, "/users/@me/channels", map[string]any{"recipient_id": userID})
	if err != nil {
		return "", "", err
	}
	var channel struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &channel); err != nil {
		return "", "", fmt.Errorf("decode dm channel: %w", err)
	}
	raw, err = c.do(http.MethodPost, "/channels/"+escapePathSegment(channel.ID)+"/messages", payload)
	if err != nil {
		return "", "", err
	}
	var message struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &message); err != nil {
		return "", "", fmt.Errorf("decode dm message: %w", err)
	}
	return channel.ID, message.ID, nil
}
