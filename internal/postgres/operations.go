package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/jackc/pgx/v5"
)

// ClaimOperations leases at most one operation per client. An unsupported or
// delayed head blocks later work, even when that later work is otherwise due.
func (s *Store) ClaimOperations(ctx context.Context, kinds []string, limit int, lease time.Duration) ([]queue.Operation, error) {
	if err := validateClaim(limit, lease); err != nil {
		return nil, err
	}
	if len(kinds) == 0 || len(kinds) > 32 {
		return nil, errors.New("operation claims require 1-32 explicit supported kinds")
	}
	for _, kind := range kinds {
		if kind == "" || len(kind) > 64 {
			return nil, errors.New("operation kinds must contain 1-64 bytes")
		}
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin operation claim: %w", err)
	}
	defer rollback(tx)

	// Lock clients, not only operations: completion can expose a new head while
	// another claimant's statement is still using its original snapshot.
	rows, err := tx.Query(ctx, `
        SELECT c.id
        FROM clients c
        JOIN LATERAL (
            SELECT id, kind, status, next_attempt_at, locked_until
            FROM operations
            WHERE client_id = c.id AND status IN ('pending', 'running')
            ORDER BY id
            LIMIT 1
        ) head ON true
        WHERE head.kind = ANY($1::text[])
          AND ((head.status = 'pending' AND head.next_attempt_at <= clock_timestamp())
               OR (head.status = 'running' AND (head.locked_until IS NULL
                   OR head.locked_until <= clock_timestamp())))
        ORDER BY head.id
        LIMIT $2
        FOR NO KEY UPDATE OF c SKIP LOCKED`, kinds, limit)
	if err != nil {
		return nil, fmt.Errorf("lock operation clients: %w", err)
	}
	clientIDs, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("read operation clients: %w", err)
	}

	claimed := make([]queue.Operation, 0, len(clientIDs))
	if len(clientIDs) != 0 {
		// This separate statement gets a fresh Read Committed snapshot after all
		// client locks were acquired. Recheck both the head and its eligibility.
		rows, err := tx.Query(ctx, `
            WITH heads AS (
                SELECT DISTINCT ON (client_id)
                    id, kind, status, next_attempt_at, locked_until
                FROM operations
                WHERE client_id = ANY($1::bigint[])
                  AND status IN ('pending', 'running')
                ORDER BY client_id, id
            )
            UPDATE operations o
            SET status = 'running', attempts = o.attempts + 1,
                locked_until = clock_timestamp() + $3::bigint * interval '1 microsecond'
            FROM heads head
            WHERE o.id = head.id
              AND head.kind = ANY($2::text[])
              AND ((head.status = 'pending' AND head.next_attempt_at <= clock_timestamp())
                   OR (head.status = 'running' AND (head.locked_until IS NULL
                       OR head.locked_until <= clock_timestamp())))
            RETURNING o.id, o.attempts, o.client_id, o.kind`, clientIDs, kinds, lease.Microseconds())
		if err != nil {
			return nil, fmt.Errorf("claim operations: %w", err)
		}
		claimed, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (queue.Operation, error) {
			var operation queue.Operation
			err := row.Scan(&operation.ID, &operation.Attempt, &operation.ClientID, &operation.Kind)
			return operation, err
		})
		if err != nil {
			return nil, fmt.Errorf("read claimed operations: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit operation claim: %w", err)
	}
	return claimed, nil
}

// RetryOperation releases owned work and schedules another attempt using the
// database clock. Only a numeric error classification is persisted.
func (s *Store) RetryOperation(ctx context.Context, lease queue.Lease, delay time.Duration, errorCode int) error {
	if err := validateDelay(delay); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin operation retry: %w", err)
	}
	defer rollback(tx)
	if err := lockOperationClient(ctx, tx, lease.ID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `
        UPDATE operations
        SET status = 'pending', locked_until = NULL,
            next_attempt_at = clock_timestamp() + $3::bigint * interval '1 microsecond',
            last_error_code = $4
        WHERE id = $1 AND attempts = $2 AND status = 'running'
          AND locked_until > clock_timestamp()`, lease.ID, lease.Attempt, delay.Microseconds(), errorCode)
	if err != nil {
		return fmt.Errorf("retry operation: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrLeaseLost
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit operation retry: %w", err)
	}
	return nil
}

// CompleteOperation commits the owned operation and its notifications together.
// Any invalid draft or idempotency conflict also rolls back the completion.
func (s *Store) CompleteOperation(ctx context.Context, lease queue.Lease, messages []queue.Draft) error {
	if len(messages) > 100 {
		return errors.New("operation completion supports at most 100 notifications")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin operation completion: %w", err)
	}
	defer rollback(tx)
	if err := prepareOutboxBots(ctx, tx, messages); err != nil {
		return fmt.Errorf("prepare notification bots: %w", err)
	}
	if err := lockOperationClient(ctx, tx, lease.ID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `
        UPDATE operations
        SET status = 'completed', locked_until = NULL,
            completed_at = clock_timestamp(), last_error_code = 0
        WHERE id = $1 AND attempts = $2 AND status = 'running'
          AND kind NOT IN ('register_client', 'activate_first_subscription') AND locked_until > clock_timestamp()`, lease.ID, lease.Attempt)
	if err != nil {
		return fmt.Errorf("complete operation: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrLeaseLost
	}
	for _, message := range messages {
		if _, err := enqueueOutbox(ctx, tx, message); err != nil {
			return fmt.Errorf("enqueue operation notification: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit operation completion: %w", err)
	}
	return nil
}

// All operation state transitions take the client lock before updating work,
// matching claim lock order. Ownership is rechecked after acquiring the lock.
func lockOperationClient(ctx context.Context, tx pgx.Tx, operationID int64) error {
	var clientID int64
	err := tx.QueryRow(ctx, `
        SELECT c.id
        FROM clients c JOIN operations o ON o.client_id = c.id
        WHERE o.id = $1
        FOR NO KEY UPDATE OF c`, operationID).Scan(&clientID)
	if errors.Is(err, pgx.ErrNoRows) {
		return queue.ErrLeaseLost
	}
	if err != nil {
		return fmt.Errorf("lock operation client: %w", err)
	}
	return nil
}
