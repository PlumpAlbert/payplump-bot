package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
)

// ApplyTelegramUpdate acknowledges unsupported updates too, but registers only
// private /start commands. The event, client and operation commit atomically;
// HTTP success is sent only after this returns. No ordering of IDs is assumed.
func (s *Store) ApplyTelegramUpdate(ctx context.Context, botID int64, botUsername string, update telegram.Update) error {
	if botID <= 0 || botUsername == "" || update.ID < 0 {
		return errors.New("invalid Telegram update identity")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `INSERT INTO telegram_bots (bot_id) VALUES ($1) ON CONFLICT DO NOTHING`, botID); err != nil {
		return err
	}
	// Serialize deliveries without blocking foreign-key checks from outbox
	// inserts. FOR UPDATE would deadlock with a completing operation that
	// already owns its client row while this registration waits for that row.
	var lockedBotID int64
	if err := tx.QueryRow(ctx, `SELECT bot_id FROM telegram_bots WHERE bot_id = $1 FOR NO KEY UPDATE`, botID).Scan(&lockedBotID); err != nil {
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
		blocked, err := rejectAutomaticImportRegistration(ctx, tx, botID, identity.TelegramID, update.ID)
		if err != nil {
			return err
		}
		if blocked {
			return tx.Commit(ctx)
		}
		_, err = registerPrivateClient(ctx, tx, TelegramIdentity{TelegramID: identity.TelegramID, ChatID: identity.ChatID, DisplayName: identity.DisplayName})
		if err != nil {
			return fmt.Errorf("register Telegram client: %w", err)
		}
	}
	return tx.Commit(ctx)
}
