//go:build integration

package postgres

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type botInsertTrace struct {
	started chan struct{}
	once    sync.Once
}

func (t *botInsertTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "INSERT INTO telegram_bots") {
		t.once.Do(func() { close(t.started) })
	}
	return ctx
}

func (*botInsertTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestCompletionPreparesNewBotBeforeLockingClient(t *testing.T) {
	s, ctx := testStore(t)
	seedOperationWork(t, s, ctx, 1, 1)
	operations := claimOperationWork(t, s, ctx, []string{"test_notify"}, 1)
	if len(operations) != 1 {
		t.Fatal("operation not claimed")
	}
	trace := &botInsertTrace{started: make(chan struct{})}
	cfg := s.pool.Config()
	cfg.MaxConns = 1
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	completion := &Store{pool: pool}

	// Model the first webhook: insert its new bot identity, then register an
	// existing client. The completion must wait for the bot before locking that
	// client, or the two transactions would wait for each other indefinitely.
	webhook, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(webhook)
	if _, err := webhook.Exec(ctx, `INSERT INTO telegram_bots (bot_id) VALUES (123)`); err != nil {
		t.Fatal(err)
	}
	completeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- completion.CompleteOperation(completeCtx, operations[0].Lease, []queue.Draft{
			{BotID: 123, ChatID: 42, Key: "notification", Text: "Сообщение"},
		})
	}()
	select {
	case <-trace.started:
	case <-completeCtx.Done():
		t.Fatal("completion never reached the bot insertion")
	}
	if _, err := webhook.Exec(ctx, `SELECT id FROM clients WHERE id = $1 FOR NO KEY UPDATE NOWAIT`, operations[0].ClientID); err != nil {
		t.Fatalf("completion locked client before its notification bot: %v", err)
	}
	if err := webhook.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-completeCtx.Done():
		t.Fatal("completion did not resume after the first webhook committed")
	}
}
