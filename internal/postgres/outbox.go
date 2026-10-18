package postgres

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/jackc/pgx/v5"
)

func (s *Store) EnqueueOutbox(ctx context.Context, message queue.Draft) (int64, error) {
	if message.ProtectedConfiguration || message.Text == queue.ConfigurationIntentBody {
		return 0, errors.New("protected configuration requires a private request")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer rollback(tx)
	id, err := enqueueOutbox(ctx, tx, message)
	if err != nil {
		return 0, err
	}
	return id, tx.Commit(ctx)
}

// Prepare bot rows before taking client locks. A bot's first webhook may have
// inserted its identity and be waiting for that client in the same transaction.
// Waiting for its insert after locking the client would form a deadlock.
func prepareOutboxBots(ctx context.Context, tx pgx.Tx, messages []queue.Draft) error {
	seen := make(map[int64]bool)
	var botIDs []int64
	for _, message := range messages {
		if err := message.Validate(); err != nil {
			return err
		}
		if !seen[message.BotID] {
			seen[message.BotID] = true
			botIDs = append(botIDs, message.BotID)
		}
	}
	if len(botIDs) == 0 {
		return nil
	}
	slices.Sort(botIDs)
	_, err := tx.Exec(ctx, `INSERT INTO telegram_bots (bot_id)
        SELECT bot_id FROM unnest($1::bigint[]) AS ids(bot_id) ORDER BY bot_id
        ON CONFLICT DO NOTHING`, botIDs)
	return err
}

// The same helper is used when completing an operation, so notification intent
// and business progress cannot commit separately. Conflicts never replace text.
func enqueueOutbox(ctx context.Context, tx pgx.Tx, message queue.Draft) (int64, error) {
	if message.ProtectedConfiguration || message.Text == queue.ConfigurationIntentBody {
		return 0, errors.New("protected configuration requires a private request")
	}
	return insertOutbox(ctx, tx, message)
}

// Private configuration requests are the sole protected caller; their metadata
// must be inserted in the same transaction (also enforced at COMMIT).
func insertOutbox(ctx context.Context, tx pgx.Tx, message queue.Draft) (int64, error) {
	if err := message.Validate(); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO telegram_bots (bot_id) VALUES ($1) ON CONFLICT DO NOTHING`, message.BotID); err != nil {
		return 0, err
	}
	var id int64
	err := tx.QueryRow(ctx, `
        INSERT INTO outbox (bot_id, chat_id, message_key, body, client_menu, protected_configuration) VALUES ($1, $2, $3, $4, $5, $6)
        ON CONFLICT (bot_id, message_key) DO UPDATE SET message_key = outbox.message_key
            WHERE outbox.chat_id = EXCLUDED.chat_id AND outbox.body = EXCLUDED.body
              AND outbox.client_menu = EXCLUDED.client_menu
              AND outbox.protected_configuration = EXCLUDED.protected_configuration
        RETURNING id`, message.BotID, message.ChatID, message.Key, message.Text, message.ClientMenu, message.ProtectedConfiguration).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, queue.ErrConflict
	}
	return id, err
}

func (s *Store) ClaimOutbox(ctx context.Context, botID int64, limit int, lease time.Duration) ([]queue.Message, error) {
	if err := validateClaim(limit, lease); err != nil {
		return nil, err
	}
	if botID <= 0 {
		return nil, errors.New("outbox requires a positive bot identity")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer rollback(tx)
	var locked int64
	// Serialize short claim transactions, not network deliveries. A fresh
	// snapshot below sees claims committed by the preceding transaction.
	err = tx.QueryRow(ctx, `SELECT bot_id FROM telegram_bots
        WHERE bot_id = $1 AND outbox_not_before <= clock_timestamp()
        FOR NO KEY UPDATE SKIP LOCKED`, botID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
        WITH heads AS (
            SELECT DISTINCT ON (chat_id) id, status, next_attempt_at, locked_until
            FROM outbox WHERE bot_id = $1 AND status IN ('pending', 'running')
            ORDER BY chat_id, id
        ), due AS (
            SELECT id FROM heads
            WHERE (status = 'pending' AND next_attempt_at <= clock_timestamp())
               OR (status = 'running' AND locked_until <= clock_timestamp())
            ORDER BY next_attempt_at, id LIMIT $2
        )
        UPDATE outbox o SET status = 'running', attempts = attempts + 1,
            locked_until = clock_timestamp() + $3 * interval '1 millisecond'
        FROM due WHERE o.id = due.id
            AND ((o.status = 'pending' AND o.next_attempt_at <= clock_timestamp())
                OR (o.status = 'running' AND o.locked_until <= clock_timestamp()))
        RETURNING o.id, o.attempts, o.bot_id, o.chat_id, o.body, o.client_menu, o.protected_configuration`, botID, limit, lease.Milliseconds())
	if err != nil {
		return nil, err
	}
	var messages []queue.Message
	for rows.Next() {
		var message queue.Message
		if err := rows.Scan(&message.ID, &message.Attempt, &message.BotID, &message.ChatID, &message.Text, &message.ClientMenu, &message.ProtectedConfiguration); err != nil {
			rows.Close()
			return nil, err
		}
		messages = append(messages, message)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return messages, nil
}

func (s *Store) CompleteOutbox(ctx context.Context, lease queue.Lease, telegramMessageID int64) error {
	if telegramMessageID <= 0 {
		return errors.New("invalid delivered Telegram message identity")
	}
	var protected bool
	if err := s.pool.QueryRow(ctx, `SELECT protected_configuration FROM outbox WHERE id=$1`, lease.ID).Scan(&protected); err != nil {
		return configurationError(queue.ErrLeaseLost)
	}
	if protected {
		return s.completeConfigurationOutbox(ctx, lease, telegramMessageID)
	}
	tag, err := s.pool.Exec(ctx, `UPDATE outbox
        SET status = 'sent', telegram_message_id = $3, finished_at = clock_timestamp(),
            locked_until = NULL, last_error_code = 0
        WHERE id = $1 AND attempts = $2 AND status = 'running' AND locked_until > clock_timestamp() AND NOT protected_configuration`,
		lease.ID, lease.Attempt, telegramMessageID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrLeaseLost
	}
	return nil
}

func (s *Store) RetryOutbox(ctx context.Context, lease queue.Lease, delay time.Duration, errorCode int) error {
	if err := validateDelay(delay); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	var botID int64
	var protected bool
	if err := tx.QueryRow(ctx, `SELECT bot_id, protected_configuration FROM outbox WHERE id = $1`, lease.ID).Scan(&botID, &protected); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return queue.ErrLeaseLost
		}
		return err
	}
	if protected {
		return errors.New("protected configuration cannot be retried")
	}
	// Match the claim lock order before touching either the lease or cooldown.
	if _, err := tx.Exec(ctx, `SELECT bot_id FROM telegram_bots WHERE bot_id = $1 FOR NO KEY UPDATE`, botID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE outbox SET status = 'pending', locked_until = NULL,
        next_attempt_at = clock_timestamp() + $3 * interval '1 millisecond', last_error_code = $4
        WHERE id = $1 AND attempts = $2 AND status = 'running' AND locked_until > clock_timestamp()`,
		lease.ID, lease.Attempt, delay.Milliseconds(), errorCode)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrLeaseLost
	}
	if errorCode == 429 {
		if _, err := tx.Exec(ctx, `UPDATE telegram_bots
            SET outbox_not_before = GREATEST(outbox_not_before, clock_timestamp() + $2 * interval '1 millisecond')
            WHERE bot_id = $1`, botID, delay.Milliseconds()); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) FailOutbox(ctx context.Context, lease queue.Lease, errorCode int) error {
	var protected bool
	if err := s.pool.QueryRow(ctx, `SELECT protected_configuration FROM outbox WHERE id=$1`, lease.ID).Scan(&protected); err != nil {
		return configurationError(queue.ErrLeaseLost)
	}
	if protected {
		return s.FailConfigurationOutbox(ctx, lease, errorCode, 0, false)
	}
	tag, err := s.pool.Exec(ctx, `UPDATE outbox SET status = 'failed', locked_until = NULL,
        finished_at = clock_timestamp(), last_error_code = $3
        WHERE id = $1 AND attempts = $2 AND status = 'running' AND locked_until > clock_timestamp() AND NOT protected_configuration`,
		lease.ID, lease.Attempt, errorCode)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrLeaseLost
	}
	return nil
}
