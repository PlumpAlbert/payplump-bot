package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
	"github.com/jackc/pgx/v5"
)

// TelegramOffset expires old checkpoints after the queue's 24-hour retention.
// Telegram can randomly reseed update IDs after a week without new updates;
// re-reading from zero after prolonged inactivity avoids skipping that new range.
func (s *Store) TelegramOffset(ctx context.Context, botID int64) (int64, error) {
	if botID <= 0 {
		return 0, errors.New("invalid Telegram bot ID")
	}
	var offset int64
	err := s.pool.QueryRow(ctx, `SELECT CASE
        WHEN last_update_at <= now() - interval '24 hours' THEN 0
        ELSE next_offset END FROM telegram_polling_state WHERE bot_id = $1`, botID).Scan(&offset)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return offset, err
}

// ApplyTelegramUpdate acknowledges unsupported updates too, but registers only
// private /start commands. The event, client, operation and next offset commit
// in one transaction; an error leaves the update available for replay.
func (s *Store) ApplyTelegramUpdate(ctx context.Context, botID int64, botUsername string, update telegram.Update) error {
	if botID <= 0 || botUsername == "" || update.ID < 0 || update.ID == math.MaxInt64 {
		return errors.New("invalid Telegram update identity")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `INSERT INTO telegram_polling_state (bot_id) VALUES ($1) ON CONFLICT DO NOTHING`, botID); err != nil {
		return err
	}
	// Serialize checkpoint changes, including overlapping deliveries on restart.
	var lockedBotID int64
	if err := tx.QueryRow(ctx, `SELECT bot_id FROM telegram_polling_state WHERE bot_id = $1 FOR UPDATE`, botID).Scan(&lockedBotID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO telegram_processed_updates (bot_id, update_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, botID, update.ID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return tx.Commit(ctx)
	}
	if identity, ok := update.PrivateStart(botUsername); ok {
		_, err := registerPrivateClient(ctx, tx, TelegramIdentity{TelegramID: identity.TelegramID, ChatID: identity.ChatID, DisplayName: identity.DisplayName})
		if err != nil {
			return fmt.Errorf("register Telegram client: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE telegram_polling_state SET next_offset = $2, last_update_at = now() WHERE bot_id = $1`, botID, update.ID+1); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
