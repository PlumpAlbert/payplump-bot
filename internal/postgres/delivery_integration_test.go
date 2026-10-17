//go:build integration

package postgres

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/outbox"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
	"github.com/jackc/pgx/v5/pgxpool"
)

type observedDeliveryStore struct {
	*Store
	finalized chan error
}

func (s observedDeliveryStore) CompleteOutbox(ctx context.Context, lease queue.Lease, id int64) error {
	err := s.Store.CompleteOutbox(ctx, lease, id)
	s.finalized <- err
	return err
}

func (s observedDeliveryStore) RetryOutbox(ctx context.Context, lease queue.Lease, delay time.Duration, code int) error {
	err := s.Store.RetryOutbox(ctx, lease, delay, code)
	s.finalized <- err
	return err
}

func (s observedDeliveryStore) FailOutbox(ctx context.Context, lease queue.Lease, code int) error {
	err := s.Store.FailOutbox(ctx, lease, code)
	s.finalized <- err
	return err
}

type scriptedDeliverySender struct{}

func (scriptedDeliverySender) SendMessage(_ context.Context, chatID int64, _ string) (int64, error) {
	switch chatID {
	case 42:
		return 0, &telegram.APIError{Code: 403, HTTPStatus: 403}
	case 44:
		return 0, &telegram.APIError{Code: 429, HTTPStatus: 429, RetryAfter: 17 * time.Second}
	default:
		return 700, nil
	}
}

func TestOutboxWorkerPersistsDeliveryResults(t *testing.T) {
	s, ctx := testStore(t)
	for _, draft := range []queue.Draft{
		{BotID: 123, ChatID: 42, Key: "blocked", Text: "Первый"},
		{BotID: 123, ChatID: 43, Key: "delivered", Text: "Второй"},
		{BotID: 123, ChatID: 44, Key: "rate-limited", Text: "Третий"},
	} {
		if _, err := s.EnqueueOutbox(ctx, draft); err != nil {
			t.Fatal(err)
		}
	}
	// This test checks durable outcomes, not the hosted database's TLS setup
	// latency. Open all concurrent connections before timed finalization starts.
	var warm []*pgxpool.Conn
	for range 3 {
		conn, err := s.pool.Acquire(ctx)
		if err != nil {
			for _, held := range warm {
				held.Release()
			}
			t.Fatal(err)
		}
		warm = append(warm, conn)
	}
	for _, conn := range warm {
		conn.Release()
	}
	observed := observedDeliveryStore{Store: s, finalized: make(chan error, 3)}
	worker, err := outbox.New(observed, scriptedDeliverySender{}, 123, outbox.Config{Workers: 3, PollInterval: time.Second}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- worker.Run(workerCtx) }()
	for range 3 {
		select {
		case err := <-observed.finalized:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("worker did not persist all results")
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("delivery worker did not stop")
	}
	var correct int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE
        (chat_id = 42 AND status = 'failed' AND last_error_code = 403) OR
        (chat_id = 43 AND status = 'sent' AND telegram_message_id = 700) OR
        (chat_id = 44 AND status = 'pending' AND last_error_code = 429 AND next_attempt_at > clock_timestamp())`).Scan(&correct); err != nil || correct != 3 {
		t.Fatalf("delivery results not durable: %d, %v", correct, err)
	}
}
