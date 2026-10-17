//go:build integration

package postgres

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/jackc/pgx/v5"
)

func seedOperationWork(t *testing.T, s *Store, ctx context.Context, clients, perClient int) []queue.Operation {
	t.Helper()
	rows, err := s.pool.Query(ctx, `
        WITH inserted AS (
            INSERT INTO clients (telegram_id, chat_id, display_name)
            SELECT n, n, 'operation test' FROM generate_series(1, $1::integer) n
            RETURNING id
        )
        INSERT INTO operations (client_id, operation_key, kind)
        SELECT id, format('test:%s:%s', id, ordinal), 'test_notify'
        FROM inserted CROSS JOIN generate_series(1, $2::integer) ordinal
        ORDER BY ordinal, id
        RETURNING id, client_id, kind`, clients, perClient)
	if err != nil {
		t.Fatal(err)
	}
	operations, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (queue.Operation, error) {
		var operation queue.Operation
		err := row.Scan(&operation.ID, &operation.ClientID, &operation.Kind)
		return operation, err
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(operations, func(i, j int) bool { return operations[i].ID < operations[j].ID })
	return operations
}

func claimOperationWork(t *testing.T, s *Store, ctx context.Context, kinds []string, limit int) []queue.Operation {
	t.Helper()
	operations, err := s.ClaimOperations(ctx, kinds, limit, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return operations
}

func TestOperationsRejectInvalidClaimsAndDelay(t *testing.T) {
	// Validation must happen before opening a database transaction.
	s := &Store{}
	for _, tc := range []struct {
		kinds []string
		limit int
		lease time.Duration
	}{
		{nil, 1, time.Minute},
		{[]string{}, 1, time.Minute},
		{[]string{""}, 1, time.Minute},
		{[]string{"test_notify", ""}, 1, time.Minute},
		{[]string{"test_notify"}, 0, time.Minute},
		{[]string{"test_notify"}, 101, time.Minute},
		{[]string{"test_notify"}, 1, time.Second - time.Nanosecond},
		{[]string{"test_notify"}, 1, 5*time.Minute + time.Nanosecond},
	} {
		if _, err := s.ClaimOperations(context.Background(), tc.kinds, tc.limit, tc.lease); err == nil {
			t.Errorf("accepted invalid claim: %+v", tc)
		}
	}
	if err := s.RetryOperation(context.Background(), queue.Lease{ID: 1, Attempt: 1}, -time.Nanosecond, 503); err == nil {
		t.Fatal("accepted negative retry delay")
	}
}

func TestOperationCompetingClaimsPreserveClientFIFO(t *testing.T) {
	s, ctx := testStore(t)
	seeded := seedOperationWork(t, s, ctx, 8, 2)
	heads := make(map[int64]int64)
	for _, operation := range seeded {
		if head, ok := heads[operation.ClientID]; !ok || operation.ID < head {
			heads[operation.ClientID] = operation.ID
		}
	}

	type result struct {
		operations []queue.Operation
		err        error
	}
	results := make(chan result, 4)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			<-start
			operations, err := s.ClaimOperations(ctx, []string{"test_notify"}, 2, 5*time.Minute)
			results <- result{operations, err}
		})
	}
	close(start)
	workers.Wait()
	close(results)

	claimedClients := make(map[int64]bool)
	check := func(operations []queue.Operation) {
		t.Helper()
		for _, operation := range operations {
			if operation.ID != heads[operation.ClientID] || operation.Attempt != 1 || operation.Kind != "test_notify" {
				t.Fatalf("claimed non-head or unexpected generation: %+v", operation)
			}
			if claimedClients[operation.ClientID] {
				t.Fatalf("competing workers claimed client %d twice", operation.ClientID)
			}
			claimedClients[operation.ClientID] = true
		}
	}
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if len(result.operations) > 2 {
			t.Fatal("claim exceeded batch limit")
		}
		check(result.operations)
	}
	// A claimant can lose eligibility between its two statements; a later poll
	// must find any clients that were not claimed, without duplicating leases.
	check(claimOperationWork(t, s, ctx, []string{"test_notify"}, 8))
	if len(claimedClients) != 8 {
		t.Fatalf("claimed %d of 8 eligible clients", len(claimedClients))
	}
	if operations := claimOperationWork(t, s, ctx, []string{"test_notify"}, 8); len(operations) != 0 {
		t.Fatalf("active client leases did not block later work: %+v", operations)
	}
}

func TestOperationClaimsSkipLockedClients(t *testing.T) {
	s, ctx := testStore(t)
	seeded := seedOperationWork(t, s, ctx, 2, 1)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `SELECT id FROM clients WHERE id = $1 FOR NO KEY UPDATE`, seeded[0].ClientID); err != nil {
		t.Fatal(err)
	}
	claimCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	operations := claimOperationWork(t, s, claimCtx, []string{"test_notify"}, 2)
	if len(operations) != 1 || operations[0].ID != seeded[1].ID {
		t.Fatalf("claim did not skip the locked client: %+v", operations)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	operations = claimOperationWork(t, s, ctx, []string{"test_notify"}, 2)
	if len(operations) != 1 || operations[0].ID != seeded[0].ID {
		t.Fatalf("unlocked client was not claimable: %+v", operations)
	}
}

func TestOperationHeadsBlockDelayedAndUnsupportedWork(t *testing.T) {
	s, ctx := testStore(t)
	seeded := seedOperationWork(t, s, ctx, 3, 2)
	if _, err := s.pool.Exec(ctx, `
        UPDATE operations
        SET next_attempt_at = CASE WHEN id = $1 THEN clock_timestamp() + interval '1 hour' ELSE next_attempt_at END,
            kind = CASE WHEN id = $2 THEN 'unsupported_test' ELSE kind END
        WHERE id IN ($1, $2)`, seeded[0].ID, seeded[1].ID); err != nil {
		t.Fatal(err)
	}
	operations := claimOperationWork(t, s, ctx, []string{"test_notify"}, 10)
	if len(operations) != 1 || operations[0].ID != seeded[2].ID {
		t.Fatalf("skipped delayed or unsupported head: %+v", operations)
	}
	if more := claimOperationWork(t, s, ctx, []string{"test_notify"}, 10); len(more) != 0 {
		t.Fatalf("later work escaped a blocked head: %+v", more)
	}
	if err := s.CompleteOperation(ctx, operations[0].Lease, nil); err != nil {
		t.Fatal(err)
	}
	operations = claimOperationWork(t, s, ctx, []string{"test_notify"}, 10)
	if len(operations) != 1 || operations[0].ID != seeded[5].ID {
		t.Fatalf("completion did not expose the next operation: %+v", operations)
	}
	operations = claimOperationWork(t, s, ctx, []string{"unsupported_test"}, 10)
	if len(operations) != 1 || operations[0].ID != seeded[1].ID {
		t.Fatalf("explicitly supported head was not claimed: %+v", operations)
	}
}

func TestOperationExpiryReclaimsAndFencesStaleOwners(t *testing.T) {
	s, ctx := testStore(t)
	seedOperationWork(t, s, ctx, 2, 1)
	original := claimOperationWork(t, s, ctx, []string{"test_notify"}, 2)
	if len(original) != 2 {
		t.Fatalf("initial claims: %+v", original)
	}
	if _, err := s.pool.Exec(ctx, `
        UPDATE operations
        SET locked_until = CASE WHEN id = $1 THEN NULL ELSE clock_timestamp() - interval '1 second' END,
            next_attempt_at = clock_timestamp() + interval '1 hour'
        WHERE status = 'running'`, original[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteOperation(ctx, original[0].Lease, nil); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("legacy null lease accepted completion: %v", err)
	}
	if err := s.RetryOperation(ctx, original[1].Lease, 0, 500); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("expired lease accepted retry: %v", err)
	}
	current := claimOperationWork(t, s, ctx, []string{"test_notify"}, 2)
	if len(current) != 2 {
		t.Fatalf("expired and legacy null leases were not reclaimed: %+v", current)
	}
	for _, operation := range current {
		if operation.Attempt != 2 {
			t.Fatalf("reclaim did not increment fencing token: %+v", operation)
		}
	}
	if err := s.CompleteOperation(ctx, queue.Lease{ID: current[0].ID, Attempt: 1}, nil); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("stale completion: %v", err)
	}
	if err := s.RetryOperation(ctx, queue.Lease{ID: current[1].ID, Attempt: 1}, 0, 503); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("stale retry: %v", err)
	}
	if err := s.RetryOperation(ctx, current[0].Lease, 2*time.Minute, 429); err != nil {
		t.Fatal(err)
	}
	var retryOK bool
	if err := s.pool.QueryRow(ctx, `
        SELECT status = 'pending' AND attempts = 2 AND locked_until IS NULL
            AND completed_at IS NULL AND last_error_code = 429
            AND next_attempt_at > clock_timestamp() + interval '1 minute'
            AND next_attempt_at <= clock_timestamp() + interval '2 minutes'
        FROM operations WHERE id = $1`, current[0].ID).Scan(&retryOK); err != nil || !retryOK {
		t.Fatalf("retry did not persist a delayed numeric error: ok=%v err=%v", retryOK, err)
	}
	if err := s.CompleteOperation(ctx, current[1].Lease, nil); err != nil {
		t.Fatal(err)
	}
	if operations := claimOperationWork(t, s, ctx, []string{"test_notify"}, 2); len(operations) != 0 {
		t.Fatalf("retry ignored its delay: %+v", operations)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE operations SET next_attempt_at = clock_timestamp() - interval '1 second' WHERE id = $1`, current[0].ID); err != nil {
		t.Fatal(err)
	}
	operations := claimOperationWork(t, s, ctx, []string{"test_notify"}, 2)
	if len(operations) != 1 || operations[0].ID != current[0].ID || operations[0].Attempt != 3 {
		t.Fatalf("retry reset the fencing token: %+v", operations)
	}
}

func TestOperationCompletionAndNotificationsAreAtomic(t *testing.T) {
	s, ctx := testStore(t)
	seedOperationWork(t, s, ctx, 1, 1)
	if _, err := s.pool.Exec(ctx, `
        WITH bot AS (INSERT INTO telegram_bots (bot_id) VALUES (123) RETURNING bot_id)
        INSERT INTO outbox (bot_id, chat_id, message_key, body)
        SELECT bot_id, 42, 'existing', 'unchanged' FROM bot`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE outbox ADD CONSTRAINT reject_operation_notification CHECK (message_key <> 'reject')`); err != nil {
		t.Fatal(err)
	}
	operations := claimOperationWork(t, s, ctx, []string{"test_notify"}, 1)
	if len(operations) != 1 {
		t.Fatalf("initial claim: %+v", operations)
	}
	operation := operations[0]
	first := queue.Draft{BotID: 123, ChatID: 42, Key: "first", Text: "Первое уведомление"}
	existing := queue.Draft{BotID: 123, ChatID: 42, Key: "existing", Text: "unchanged"}
	conflict := existing
	conflict.Text = "changed"
	invalid := queue.Draft{BotID: 123, ChatID: 42, Key: "invalid", Text: ""}
	rejected := queue.Draft{BotID: 999, ChatID: 42, Key: "reject", Text: "notification"}

	assertRolledBack := func() {
		t.Helper()
		var owned, onlyExisting bool
		if err := s.pool.QueryRow(ctx, `
            SELECT status = 'running' AND attempts = $2 AND locked_until > clock_timestamp()
                AND completed_at IS NULL,
                (SELECT count(*) = 1 AND bool_and(message_key = 'existing' AND body = 'unchanged') FROM outbox)
            FROM operations WHERE id = $1`, operation.ID, operation.Attempt).Scan(&owned, &onlyExisting); err != nil {
			t.Fatal(err)
		}
		if !owned || !onlyExisting {
			t.Fatalf("partial completion or outbox insert survived: owned=%v onlyExisting=%v", owned, onlyExisting)
		}
	}
	for _, tc := range []struct {
		name     string
		message  queue.Draft
		conflict bool
	}{
		{name: "idempotency conflict", message: conflict, conflict: true},
		{name: "invalid draft", message: invalid},
		{name: "database constraint failure", message: rejected},
	} {
		err := s.CompleteOperation(ctx, operation.Lease, []queue.Draft{first, tc.message})
		if err == nil || (tc.conflict && !errors.Is(err, queue.ErrConflict)) {
			t.Fatalf("%s: %v", tc.name, err)
		}
		assertRolledBack()
	}
	second := queue.Draft{BotID: 123, ChatID: 43, Key: "second", Text: "Второе уведомление"}
	if err := s.CompleteOperation(ctx, operation.Lease, []queue.Draft{first, second, existing}); err != nil {
		t.Fatal(err)
	}
	var completed, notificationsOK bool
	if err := s.pool.QueryRow(ctx, `
        SELECT status = 'completed' AND attempts = $2 AND locked_until IS NULL
            AND completed_at IS NOT NULL AND last_error_code = 0,
            (SELECT count(*) = 3 AND bool_and(status = 'pending' AND attempts = 0
                AND locked_until IS NULL AND finished_at IS NULL) FROM outbox)
        FROM operations WHERE id = $1`, operation.ID, operation.Attempt).Scan(&completed, &notificationsOK); err != nil {
		t.Fatal(err)
	}
	if !completed || !notificationsOK {
		t.Fatalf("completion and notifications did not commit: completed=%v notifications=%v", completed, notificationsOK)
	}
	if err := s.CompleteOperation(ctx, operation.Lease, []queue.Draft{conflict}); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("completed operation accepted another notification: %v", err)
	}
}
