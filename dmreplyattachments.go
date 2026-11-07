package discordsignup

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// The files people attach when they write back to the bot's DMs. Discord's
// links to them stop working within a day, so an image is downloaded when the
// reply comes and kept in file-store, and the event page shows it to whoever
// may edit the event — the people the DM's footer says replies are passed
// to. A file that is not an image is noted by name and left in the DM.
//
// A reply from before this, or one whose download failed, is kept later by
// POST /api/dm-replies/keep-attachments, which reads the message from
// Discord again for fresh links.

// replyImageMaximumBytes is the largest image kept from a reply. Discord
// takes up to 10 MB a file from someone without Nitro, and more from someone
// with it; a larger image is noted and left in the DM.
const replyImageMaximumBytes = 25 << 20

// discordAttachmentHosts are the only hosts an attachment is downloaded
// from: the links come from Discord, and nothing else should be fetched on
// their say-so.
var discordAttachmentHosts = map[string]bool{
	"cdn.discordapp.com":   true,
	"media.discordapp.net": true,
}

// DMAttachment is one file on a person's DM, as Discord describes it.
type DMAttachment struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
	URL         string `json:"url"`
}

// DMReplyAttachment is one file on a reply, as recorded: kept in file-store
// when FileID is set, or noted with why not.
type DMReplyAttachment struct {
	Position            int    `json:"position"`
	DiscordAttachmentID string `json:"discord_attachment_id"`
	Filename            string `json:"filename"`
	ContentType         string `json:"content_type"`
	Bytes               int    `json:"bytes"`
	FileID              string `json:"file_id,omitempty"`
	NotKept             string `json:"not_kept,omitempty"`
}

// Kept says whether the page can show it.
func (a DMReplyAttachment) Kept() bool { return a.FileID != "" }

// replyAttachmentsUnrecorded lists replies that say they carried files but
// have none recorded, with the DM channel each came in.
func (s *Store) replyAttachmentsUnrecorded() ([]DMReply, map[int64]string, error) {
	rows, err := s.db.Query(`
		SELECT r.id, r.event_id, r.discord_user_id, r.message_id, r.attachments,
		       COALESCE((SELECT channel_id FROM event_dms d WHERE d.discord_user_id = r.discord_user_id
		                 AND d.channel_id != '' LIMIT 1), '')
		FROM event_dm_replies r
		WHERE r.attachments > 0
		  AND NOT EXISTS (SELECT 1 FROM event_dm_reply_attachments a WHERE a.reply_id = r.id)
		ORDER BY r.id`)
	if err != nil {
		return nil, nil, fmt.Errorf("list replies with unrecorded attachments: %w", err)
	}
	defer rows.Close()
	var replies []DMReply
	channels := map[int64]string{}
	for rows.Next() {
		var r DMReply
		var channelID string
		if err := rows.Scan(&r.ID, &r.EventID, &r.DiscordUserID, &r.MessageID, &r.Attachments, &channelID); err != nil {
			return nil, nil, fmt.Errorf("scan reply: %w", err)
		}
		replies = append(replies, r)
		channels[r.ID] = channelID
	}
	return replies, channels, rows.Err()
}

// RecordReplyAttachments records the files on a reply, all at once.
func (s *Store) RecordReplyAttachments(replyID int64, attachments []DMReplyAttachment) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	for _, a := range attachments {
		if _, err := tx.Exec(`INSERT INTO event_dm_reply_attachments (reply_id, position, discord_attachment_id,
			filename, content_type, bytes, file_id, not_kept, recorded_at) VALUES (?,?,?,?,?,?,?,?,?)`,
			replyID, a.Position, a.DiscordAttachmentID, a.Filename, a.ContentType, a.Bytes, a.FileID, a.NotKept, now()); err != nil {
			return fmt.Errorf("record reply attachment %s: %w", a.Filename, err)
		}
	}
	return tx.Commit()
}

// replyAttachmentsByReply reads the files on every reply about an event.
func (s *Store) replyAttachmentsByReply(eventID int64) (map[int64][]DMReplyAttachment, error) {
	rows, err := s.db.Query(`SELECT a.reply_id, a.position, a.discord_attachment_id, a.filename, a.content_type,
		a.bytes, a.file_id, a.not_kept
		FROM event_dm_reply_attachments a JOIN event_dm_replies r ON r.id = a.reply_id
		WHERE r.event_id = ? ORDER BY a.reply_id, a.position`, eventID)
	if err != nil {
		return nil, fmt.Errorf("list reply attachments: %w", err)
	}
	defer rows.Close()
	out := map[int64][]DMReplyAttachment{}
	for rows.Next() {
		var replyID int64
		var a DMReplyAttachment
		if err := rows.Scan(&replyID, &a.Position, &a.DiscordAttachmentID, &a.Filename, &a.ContentType,
			&a.Bytes, &a.FileID, &a.NotKept); err != nil {
			return nil, fmt.Errorf("scan reply attachment: %w", err)
		}
		out[replyID] = append(out[replyID], a)
	}
	return out, rows.Err()
}

// replyIDForMessage is the id of the reply stored for a Discord message.
func (s *Store) replyIDForMessage(messageID string) (int64, error) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM event_dm_replies WHERE message_id = ?`, messageID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

// keepReplyAttachments downloads each image on a reply and keeps it in
// file-store, and records every file, kept or not. Nothing to record is no
// change.
func (s *Server) keepReplyAttachments(reply DMReply, attachments []DMAttachment) error {
	if len(attachments) == 0 {
		return nil
	}
	recorded := make([]DMReplyAttachment, 0, len(attachments))
	for i, a := range attachments {
		recorded = append(recorded, s.keepReplyAttachment(reply, i, a))
	}
	if err := s.store.RecordReplyAttachments(reply.ID, recorded); err != nil {
		// Recording failed, so the files kept are kept for nothing.
		for _, a := range recorded {
			if a.FileID != "" {
				if purgeErr := s.files.PurgePhoto(a.FileID); purgeErr != nil {
					log.Printf("[discord-signup] purge unrecorded reply image %s: %v", a.FileID, purgeErr)
				}
			}
		}
		return err
	}
	return nil
}

// keepReplyAttachment keeps one file if it is an image, and says why not
// when it is not kept.
func (s *Server) keepReplyAttachment(reply DMReply, position int, a DMAttachment) DMReplyAttachment {
	out := DMReplyAttachment{Position: position, DiscordAttachmentID: a.ID, Filename: a.Filename,
		ContentType: a.ContentType, Bytes: a.Size}
	switch {
	case s.files == nil:
		out.NotKept = "file-store is not configured"
		return out
	case a.Size > replyImageMaximumBytes:
		out.NotKept = fmt.Sprintf("over %d MB", replyImageMaximumBytes>>20)
		return out
	}
	// The type Discord gives comes from the sender's file name; only the
	// bytes decide, but nothing that does not even claim to be an image is
	// downloaded.
	if _, claimed := messageImageExtensions[a.ContentType]; !claimed {
		out.NotKept = "not an image"
		return out
	}
	data, err := downloadDiscordAttachment(a.URL)
	if err != nil {
		log.Printf("[discord-signup] ERROR download attachment %s on reply %d: %v", a.ID, reply.ID, err)
		out.NotKept = "download failed: " + err.Error()
		return out
	}
	contentType := http.DetectContentType(data)
	if _, image := messageImageExtensions[contentType]; !image {
		out.NotKept = "not an image"
		return out
	}
	out.ContentType, out.Bytes = contentType, len(data)
	fileID, err := s.files.uploadPhoto("dm-reply:"+strconv.FormatInt(reply.ID, 10), a.Filename, contentType, data)
	if err != nil {
		log.Printf("[discord-signup] ERROR keep attachment %s on reply %d: %v", a.ID, reply.ID, err)
		out.NotKept = "file-store refused it: " + err.Error()
		return out
	}
	out.FileID = fileID
	return out
}

var attachmentDownloadClient = &http.Client{Timeout: 60 * time.Second}

// downloadDiscordAttachment fetches a file from Discord's CDN, and from
// nowhere else.
func downloadDiscordAttachment(link string) ([]byte, error) {
	parsed, err := url.Parse(link)
	if err != nil {
		return nil, fmt.Errorf("read attachment link: %w", err)
	}
	if !discordAttachmentHosts[parsed.Host] {
		return nil, fmt.Errorf("attachment link is not on Discord's CDN: %s", parsed.Host)
	}
	response, err := attachmentDownloadClient.Get(link)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discord cdn answered %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, replyImageMaximumBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > replyImageMaximumBytes {
		return nil, fmt.Errorf("over %d MB", replyImageMaximumBytes>>20)
	}
	return data, nil
}

// MessageAttachments reads one message's files from Discord, with links
// fresh enough to download.
func (c *DiscordClient) MessageAttachments(channelID, messageID string) ([]DMAttachment, error) {
	raw, err := c.do(http.MethodGet, "/channels/"+escapePathSegment(channelID)+"/messages/"+escapePathSegment(messageID), nil)
	if err != nil {
		return nil, err
	}
	var message struct {
		Attachments []DMAttachment `json:"attachments"`
	}
	if err := json.Unmarshal(raw, &message); err != nil {
		return nil, fmt.Errorf("decode message: %w", err)
	}
	return message.Attachments, nil
}

// handleKeepReplyAttachments keeps the files on every reply that says it
// carried some but has none recorded: from before replies kept images, or
// whose recording failed. Answers what it did.
func (s *Server) handleKeepReplyAttachments(w http.ResponseWriter, r *http.Request) {
	if s.discord == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no Discord client configured"})
		return
	}
	replies, channels, err := s.store.replyAttachmentsUnrecorded()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	type outcome struct {
		ReplyID int64  `json:"reply_id"`
		Kept    int    `json:"kept"`
		Noted   int    `json:"noted"`
		Error   string `json:"error,omitempty"`
	}
	outcomes := []outcome{}
	for _, reply := range replies {
		o := outcome{ReplyID: reply.ID}
		channelID := channels[reply.ID]
		if channelID == "" {
			o.Error = "no DM channel recorded for them"
			outcomes = append(outcomes, o)
			continue
		}
		attachments, err := s.discord.MessageAttachments(channelID, reply.MessageID)
		if err == nil {
			err = s.keepReplyAttachments(reply, attachments)
		}
		if err != nil {
			o.Error = err.Error()
			outcomes = append(outcomes, o)
			continue
		}
		recorded, err := s.store.replyAttachmentsByReply(reply.EventID)
		if err != nil {
			o.Error = err.Error()
		}
		for _, a := range recorded[reply.ID] {
			if a.Kept() {
				o.Kept++
			} else {
				o.Noted++
			}
		}
		outcomes = append(outcomes, o)
	}
	writeJSON(w, http.StatusOK, map[string]any{"replies": outcomes})
}

// handleWebReplyAttachment shows one image someone sent back, to whoever
// may edit the event, unless their DMs were handed off when they sent it.
func (s *Server) handleWebReplyAttachment(w http.ResponseWriter, r *http.Request) {
	session := s.requireSession(w, r)
	if session == nil {
		return
	}
	ev, canManage := s.webEvent(w, r, session)
	if ev == nil {
		return
	}
	if !canManage {
		http.Error(w, "you cannot edit this event", http.StatusForbidden)
		return
	}
	replyID, err := strconv.ParseInt(r.PathValue("replyID"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	position, err := strconv.Atoi(r.PathValue("position"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// The replies the page shows, so a reply hidden by a handoff is hidden
	// here too.
	replies, err := s.store.DMReplies(ev.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var fileID string
	for _, reply := range replies {
		if reply.ID != replyID {
			continue
		}
		for _, a := range reply.Files {
			if a.Position == position && a.Kept() {
				fileID = a.FileID
			}
		}
	}
	if fileID == "" || s.files == nil {
		http.NotFound(w, r)
		return
	}
	data, contentType, err := s.files.PhotoContent(fileID)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if _, image := messageImageExtensions[contentType]; !image {
		http.Error(w, "not an image", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Write(data)
}
