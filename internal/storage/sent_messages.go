package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/pod32g/omni-notify/internal/models"
)

// sentMessageRetention is how long a sent message stays addressable by ref.
const sentMessageRetention = 30 * 24 * time.Hour

// PutSentMessage remembers a sent message under its ref, replacing an earlier
// message with the same ref, and prunes rows past the retention.
func (s *Store) PutSentMessage(ctx context.Context, m models.SentMessage) error {
	now := s.now()
	if m.SentAt.IsZero() {
		m.SentAt = now
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sent_messages (channel, account, chat, ref, message_id, kind, sent_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(channel, account, chat, ref) DO UPDATE SET
			message_id = excluded.message_id,
			kind       = excluded.kind,
			sent_at    = excluded.sent_at`,
		m.Channel, m.Account, m.Chat, m.Ref, m.MessageID, m.Kind, fmtTime(m.SentAt))
	if err != nil {
		return fmt.Errorf("put sent message: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sent_messages WHERE sent_at < ?`,
		fmtTime(now.Add(-sentMessageRetention))); err != nil {
		return fmt.Errorf("prune sent messages: %w", err)
	}
	return nil
}

// GetSentMessage returns the message remembered under ref, or ErrNotFound.
func (s *Store) GetSentMessage(ctx context.Context, channel, account, chat, ref string) (models.SentMessage, error) {
	m := models.SentMessage{Channel: channel, Account: account, Chat: chat, Ref: ref}
	var sentAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT message_id, kind, sent_at FROM sent_messages
		WHERE channel = ? AND account = ? AND chat = ? AND ref = ? AND sent_at >= ?`,
		channel, account, chat, ref, fmtTime(s.now().Add(-sentMessageRetention)),
	).Scan(&m.MessageID, &m.Kind, &sentAt)
	if errors.Is(err, sql.ErrNoRows) {
		return models.SentMessage{}, ErrNotFound
	}
	if err != nil {
		return models.SentMessage{}, fmt.Errorf("get sent message: %w", err)
	}
	m.SentAt = parseTime(sentAt)
	return m, nil
}

// DeleteSentMessage forgets ref (after its message was deleted).
func (s *Store) DeleteSentMessage(ctx context.Context, channel, account, chat, ref string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM sent_messages WHERE channel = ? AND account = ? AND chat = ? AND ref = ?`,
		channel, account, chat, ref)
	if err != nil {
		return fmt.Errorf("delete sent message: %w", err)
	}
	return nil
}
