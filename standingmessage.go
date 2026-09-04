package discordsignup

import (
	"database/sql"
	"errors"
	"fmt"
)

// Standing messages: ones this service keeps written rather than posts once.
// The only kind there ever was, the pinned how-to, was retired 2026-09-04 —
// it told people to press a Create button that had moved to the management
// table, and carried a My events button retired before that — so nothing
// writes a row here today. The table and these methods stay for the next
// kind, and so that an old row means what it meant.

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
