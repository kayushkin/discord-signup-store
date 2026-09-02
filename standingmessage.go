package discordsignup

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
)

// standingMessageHowTo is the pinned "press this button" message.
const standingMessageHowTo = "how-to"

// StandingMessage is a message this service keeps written rather than posts
// once.
type StandingMessage struct {
	Kind      string `json:"kind"`
	ChannelID string `json:"channel_id"`
	MessageID string `json:"message_id"`
	UpdatedAt int64  `json:"updated_at"`
}

// StandingMessageID returns the message this service last wrote of that kind in
// that channel, or "" if there is not one.
func (s *Store) StandingMessageID(kind, channelID string) (string, error) {
	var messageID string
	err := s.db.QueryRow(
		`SELECT message_id FROM standing_messages WHERE kind = ? AND channel_id = ?`,
		kind, channelID).Scan(&messageID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read standing message: %w", err)
	}
	return messageID, nil
}

// RememberStandingMessage records where a standing message ended up.
func (s *Store) RememberStandingMessage(kind, channelID, messageID string) error {
	_, err := s.db.Exec(`
		INSERT INTO standing_messages (kind, channel_id, message_id, updated_at)
		VALUES (?,?,?,?)
		ON CONFLICT(kind, channel_id) DO UPDATE SET message_id = excluded.message_id,
		                                            updated_at = excluded.updated_at`,
		kind, channelID, messageID, now())
	if err != nil {
		return fmt.Errorf("remember standing message: %w", err)
	}
	return nil
}

// ForgetStandingMessage drops the record, so the next publish posts a new one.
func (s *Store) ForgetStandingMessage(kind, channelID string) error {
	_, err := s.db.Exec(`DELETE FROM standing_messages WHERE kind = ? AND channel_id = ?`,
		kind, channelID)
	if err != nil {
		return fmt.Errorf("forget standing message: %w", err)
	}
	return nil
}

// PublishHowToMessage writes the standing how-to into a channel: editing the
// one already there, or posting and pinning a new one.
//
// It edits rather than re-posts because the wording changes and the message
// does not. Re-posting was the only option before there was anywhere to record
// the id, so every improvement to the text meant a second pinned how-to and
// somebody deleting the first by hand.
//
// adoptMessageID lets an existing message be taken over — the one already
// pinned in the channel, posted before any of this was recorded. Without it
// the first publish after this change would post the duplicate it exists to
// prevent.
func (s *Server) PublishHowToMessage(channelID, adoptMessageID string) (string, error) {
	if s.discord == nil {
		return "", errors.New("no discord client configured")
	}
	messageID, err := s.store.StandingMessageID(standingMessageHowTo, channelID)
	if err != nil {
		return "", err
	}
	if messageID == "" && adoptMessageID != "" {
		messageID = adoptMessageID
	}
	body := RenderHowToMessage(s.BoardChannelID(), s.DefaultTimezone())

	if messageID != "" {
		err := s.discord.EditMessage(channelID, messageID, body)
		if err == nil {
			return messageID, s.store.RememberStandingMessage(standingMessageHowTo, channelID, messageID)
		}
		// Gone means somebody deleted it, which is a fine way to ask for a
		// fresh one. Anything else is a real failure and must not be papered
		// over by quietly posting a second copy.
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
			return "", fmt.Errorf("edit how-to: %w", err)
		}
		log.Printf("[discord-signup] the how-to in %s is gone; posting a new one", channelID)
		if err := s.store.ForgetStandingMessage(standingMessageHowTo, channelID); err != nil {
			return "", err
		}
	}

	messageID, err = s.discord.CreateMessage(channelID, body)
	if err != nil {
		return "", fmt.Errorf("post how-to: %w", err)
	}
	if err := s.discord.PinMessage(channelID, messageID); err != nil {
		// Almost always a 403 for want of PIN_MESSAGES. Discord split that out
		// of MANAGE_MESSAGES into its own permission (bit 51), so a bot invited
		// before the split — or with an invite link that predates it — can
		// delete messages and still not pin one. Logged rather than fatal: an
		// unpinned how-to in an otherwise empty channel is still the first
		// thing anyone reads.
		log.Printf("[discord-signup] could not pin the how-to (needs PIN_MESSAGES, "+
			"which is separate from MANAGE_MESSAGES): %v", err)
	}
	return messageID, s.store.RememberStandingMessage(standingMessageHowTo, channelID, messageID)
}
