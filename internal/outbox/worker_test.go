package outbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
)

type senderFunc func(context.Context, int64, string) (int64, error)

func (f senderFunc) SendMessage(ctx context.Context, chatID int64, text string) (int64, error) {
	return f(ctx, chatID, text)
}

type finalization struct {
	kind      string
	lease     queue.Lease
	messageID int64
	delay     time.Duration
	code      int
}

type fakeStore struct {
	claim    func(context.Context, int64, int, time.Duration) ([]queue.Message, error)
	finalize func(context.Context, finalization) error
}

func (s *fakeStore) ClaimOutbox(ctx context.Context, botID int64, limit int, lease time.Duration) ([]queue.Message, error) {
	if s.claim != nil {
		return s.claim(ctx, botID, limit, lease)
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (s *fakeStore) record(ctx context.Context, result finalization) error {
	if s.finalize == nil {
		return errors.New("unexpected finalization")
	}
	return s.finalize(ctx, result)
}

func (s *fakeStore) CompleteOutbox(ctx context.Context, lease queue.Lease, messageID int64) error {
	return s.record(ctx, finalization{kind: "complete", lease: lease, messageID: messageID})
}

func (s *fakeStore) RetryOutbox(ctx context.Context, lease queue.Lease, delay time.Duration, code int) error {
	return s.record(ctx, finalization{kind: "retry", lease: lease, delay: delay, code: code})
}

func (s *fakeStore) FailOutbox(ctx context.Context, lease queue.Lease, code int) error {
	return s.record(ctx, finalization{kind: "fail", lease: lease, code: code})
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for worker")
		var zero T
		return zero
	}
}

func checkDeadline(t *testing.T, ctx context.Context, maximum time.Duration) {
	t.Helper()
	deadline, ok := ctx.Deadline()
	remaining := time.Until(deadline)
	if !ok || ctx.Err() != nil || remaining <= 0 || remaining > maximum {
		t.Errorf("expected live context with deadline within %v", maximum)
	}
}

func testWorker(t *testing.T, store Store, sender Sender, workers int) *Worker {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w, err := New(store, sender, 7, Config{workers, 100 * time.Millisecond}, logger)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func startWorker(t *testing.T, w *Worker) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	return cancel, done
}

func checkRunning(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("Run returned before its tasks joined: %v", err)
	default:
	}
}

func TestNewValidation(t *testing.T) {
	store := &fakeStore{}
	sender := senderFunc(func(context.Context, int64, string) (int64, error) { return 1, nil })
	cfg := Config{2, time.Second}
	for _, tc := range []struct {
		store  Store
		sender Sender
		botID  int64
		cfg    Config
	}{
		{nil, sender, 7, cfg}, {store, nil, 7, cfg},
		{store, sender, 0, cfg}, {store, sender, -1, cfg},
		{store, sender, 7, Config{0, time.Second}},
		{store, sender, 7, Config{2, 0}},
	} {
		if _, err := New(tc.store, tc.sender, tc.botID, tc.cfg, nil); err == nil {
			t.Error("accepted invalid worker configuration")
		}
	}
	if w, err := New(store, sender, 7, cfg, nil); err != nil || w.logger == nil {
		t.Fatal("nil logger should use the default logger")
	}
}

func TestWorkerIndependentOutcomes(t *testing.T) {
	messages := []queue.Message{
		{Lease: queue.Lease{ID: 1, Attempt: 3}, BotID: 7, ChatID: 987654321001, Text: "private message contents"},
		{Lease: queue.Lease{ID: 2, Attempt: 4}, BotID: 7, ChatID: 987654321002, Text: "private message contents"},
		{Lease: queue.Lease{ID: 3, Attempt: 5}, BotID: 7, ChatID: 987654321003, Text: "private message contents"},
	}
	results := make(chan finalization, 3)
	claimed := false
	store := &fakeStore{
		claim: func(ctx context.Context, botID int64, limit int, lease time.Duration) ([]queue.Message, error) {
			checkDeadline(t, ctx, 5*time.Second)
			if botID != 7 || limit != 3 || lease != 45*time.Second {
				t.Error("incorrect claim parameters")
			}
			if claimed {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			claimed = true
			return messages, nil
		},
		finalize: func(ctx context.Context, result finalization) error {
			checkDeadline(t, ctx, 5*time.Second)
			results <- result
			return nil
		},
	}
	sender := senderFunc(func(ctx context.Context, chatID int64, text string) (int64, error) {
		checkDeadline(t, ctx, 10*time.Second)
		if text != "private message contents" {
			t.Error("message text changed")
		}
		switch chatID {
		case messages[0].ChatID:
			return 987, nil
		case messages[1].ChatID:
			return 0, &telegram.APIError{Code: 403}
		default:
			return 0, fmt.Errorf("sensitive-token-value: %w", &telegram.APIError{Code: 429, RetryAfter: 10 * time.Minute})
		}
	})
	var logs bytes.Buffer
	w := testWorker(t, store, sender, 3)
	w.logger = slog.New(slog.NewTextHandler(&logs, nil))
	cancel, done := startWorker(t, w)
	got := make(map[int64]finalization)
	for range messages {
		result := receive(t, results)
		got[result.lease.ID] = result
	}
	cancel()
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if got[message.ID].lease != message.Lease {
			t.Error("finalization lost its fencing token")
		}
	}
	if got[1].kind != "complete" || got[1].messageID != 987 {
		t.Errorf("success not persisted: %+v", got[1])
	}
	if got[2].kind != "fail" || got[2].code != 403 {
		t.Errorf("permanent recipient failure not persisted: %+v", got[2])
	}
	if got[3].kind != "retry" || got[3].code != 429 || got[3].delay != 10*time.Minute {
		t.Errorf("retry_after minimum not preserved: %+v", got[3])
	}
	for _, secret := range []string{"sensitive-token-value", "private message contents", "987654321001", "987654321002", "987654321003"} {
		if strings.Contains(logs.String(), secret) {
			t.Error("logs exposed private data")
		}
	}
}

func TestWorkerErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		kind string
		code int
	}{
		{"transport", errors.New("private transport details"), "retry", 0},
		{"timeout", context.DeadlineExceeded, "retry", 0},
		{"server", &telegram.APIError{Code: 503}, "retry", 503},
		{"request timeout", &telegram.APIError{Code: 408}, "retry", 408},
		{"credentials", fmt.Errorf("private credentials: %w", &telegram.APIError{Code: 401}), "retry", 401},
		{"bad recipient", &telegram.APIError{Code: 400}, "fail", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got finalization
			store := &fakeStore{finalize: func(_ context.Context, result finalization) error {
				got = result
				return nil
			}}
			sender := senderFunc(func(context.Context, int64, string) (int64, error) { return 0, tc.err })
			w := testWorker(t, store, sender, 1)
			lease := queue.Lease{ID: 1, Attempt: 2}
			w.deliver(context.Background(), queue.Message{Lease: lease, ChatID: 42, Text: "message"})
			if got.kind != tc.kind || got.code != tc.code || got.lease != lease {
				t.Fatalf("incorrect error classification: %+v", got)
			}
			if got.kind == "retry" && (got.delay < 2*time.Second || got.delay > 2*time.Second+retryJitter) {
				t.Errorf("incorrect exponential retry: %v", got.delay)
			}
		})
	}
}

func TestWorkerPersistsCooldownDuringShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var got finalization
	store := &fakeStore{finalize: func(ctx context.Context, result finalization) error {
		checkDeadline(t, ctx, 5*time.Second)
		got = result
		return nil
	}}
	sender := senderFunc(func(context.Context, int64, string) (int64, error) {
		cancel()
		return 0, &telegram.APIError{Code: 429, RetryAfter: 10 * time.Minute}
	})
	w := testWorker(t, store, sender, 1)
	w.deliver(ctx, queue.Message{Lease: queue.Lease{ID: 1, Attempt: 1}, ChatID: 42, Text: "message"})
	if got.kind != "retry" || got.code != 429 || got.delay != 10*time.Minute {
		t.Errorf("known cooldown lost during shutdown: %+v", got)
	}
}

func TestRetryDelayBounds(t *testing.T) {
	for _, tc := range []struct {
		attempt int
		base    time.Duration
	}{
		{-1, time.Second}, {0, time.Second}, {1, time.Second},
		{2, 2 * time.Second}, {9, 256 * time.Second},
		{10, 5 * time.Minute}, {1 << 30, 5 * time.Minute},
	} {
		got := retryDelay(tc.attempt, 0)
		if got < tc.base || got > min(tc.base+retryJitter, 5*time.Minute) {
			t.Errorf("attempt %d: invalid backoff %v", tc.attempt, got)
		}
	}
	for _, minimum := range []time.Duration{time.Minute, 10 * time.Minute} {
		if got := retryDelay(1, minimum); got != minimum {
			t.Errorf("minimum %v was not honored: %v", minimum, got)
		}
	}
}

func TestWorkerBoundedParallelBatches(t *testing.T) {
	claims := make(chan int, 2)
	started := make(chan int64, 4)
	results := make(chan finalization, 4)
	release := make(chan struct{})
	var active atomic.Int32
	batch := 0
	store := &fakeStore{
		claim: func(ctx context.Context, _ int64, limit int, _ time.Duration) ([]queue.Message, error) {
			if batch == 2 {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			batch++
			claims <- limit
			messages := make([]queue.Message, limit)
			for i := range messages {
				id := int64((batch-1)*limit + i + 1)
				messages[i] = queue.Message{Lease: queue.Lease{ID: id, Attempt: 1}, ChatID: id, Text: "message"}
			}
			return messages, nil
		},
		finalize: func(_ context.Context, result finalization) error {
			results <- result
			return nil
		},
	}
	sender := senderFunc(func(ctx context.Context, chatID int64, _ string) (int64, error) {
		if count := active.Add(1); count > 2 {
			t.Errorf("concurrency exceeded workers: %d", count)
		}
		defer active.Add(-1)
		started <- chatID
		select {
		case <-release:
			return chatID + 100, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	})
	cancel, done := startWorker(t, testWorker(t, store, sender, 2))
	if limit := receive(t, claims); limit != 2 {
		t.Fatalf("claim limit = %d", limit)
	}
	receive(t, started)
	receive(t, started)
	if active.Load() != 2 {
		t.Fatal("sends did not run in parallel")
	}
	release <- struct{}{}
	receive(t, results)
	select {
	case <-claims:
		t.Fatal("claimed a new batch while previous send was still running")
	default:
	}
	release <- struct{}{}
	receive(t, results)
	if limit := receive(t, claims); limit != 2 {
		t.Fatalf("claim limit = %d", limit)
	}
	receive(t, started)
	receive(t, started)
	close(release)
	receive(t, results)
	receive(t, results)
	cancel()
	if err := receive(t, done); err != nil || active.Load() != 0 {
		t.Fatal("Run did not join all sends")
	}
}

func TestWorkerShutdownJoinsAndPersistsKnownSuccess(t *testing.T) {
	for _, success := range []bool{false, true} {
		t.Run(fmt.Sprintf("success=%v", success), func(t *testing.T) {
			started := make(chan struct{})
			canceled := make(chan struct{})
			releaseSend := make(chan struct{})
			finalizing := make(chan finalization, 1)
			releaseFinalize := make(chan struct{})
			claimed := false
			store := &fakeStore{
				claim: func(ctx context.Context, _ int64, _ int, _ time.Duration) ([]queue.Message, error) {
					if claimed {
						<-ctx.Done()
						return nil, ctx.Err()
					}
					claimed = true
					return []queue.Message{{Lease: queue.Lease{ID: 1, Attempt: 1}, ChatID: 42, Text: "message"}}, nil
				},
				finalize: func(ctx context.Context, result finalization) error {
					checkDeadline(t, ctx, 5*time.Second)
					finalizing <- result
					select {
					case <-releaseFinalize:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				},
			}
			sender := senderFunc(func(ctx context.Context, _ int64, _ string) (int64, error) {
				close(started)
				<-ctx.Done()
				close(canceled)
				<-releaseSend
				if success {
					return 123, nil
				}
				return 0, ctx.Err()
			})
			cancel, done := startWorker(t, testWorker(t, store, sender, 1))
			receive(t, started)
			cancel()
			receive(t, canceled)
			checkRunning(t, done)
			close(releaseSend)
			if success {
				result := receive(t, finalizing)
				if result.kind != "complete" || result.messageID != 123 {
					t.Error("known success lost during shutdown")
				}
				checkRunning(t, done)
				close(releaseFinalize)
			}
			if err := receive(t, done); err != nil {
				t.Fatal(err)
			}
			if !success && len(finalizing) != 0 {
				t.Error("canceled send should leave its lease recoverable")
			}
		})
	}
}

func TestWorkerDoesNotSendAfterCanceledClaim(t *testing.T) {
	claiming := make(chan struct{})
	store := &fakeStore{claim: func(ctx context.Context, _ int64, _ int, _ time.Duration) ([]queue.Message, error) {
		close(claiming)
		<-ctx.Done()
		return []queue.Message{{Lease: queue.Lease{ID: 1, Attempt: 1}, ChatID: 42, Text: "message"}}, nil
	}}
	sender := senderFunc(func(context.Context, int64, string) (int64, error) {
		t.Error("started a send after cancellation")
		return 1, nil
	})
	w := testWorker(t, store, sender, 1)
	cancel, done := startWorker(t, w)
	receive(t, claiming)
	cancel()
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
	// Also cover a task scheduled before cancellation but entered afterward.
	ctx, stop := context.WithCancel(context.Background())
	stop()
	w.deliver(ctx, queue.Message{ChatID: 42, Text: "message"})
}

func TestWorkerRecoversAfterDatabaseFailures(t *testing.T) {
	results := make(chan finalization, 2)
	var claimTimes []time.Time
	finalizations := 0
	store := &fakeStore{
		claim: func(ctx context.Context, _ int64, _ int, _ time.Duration) ([]queue.Message, error) {
			claimTimes = append(claimTimes, time.Now())
			switch len(claimTimes) {
			case 1:
				return nil, errors.New("private-database-details")
			case 2, 3:
				return []queue.Message{{Lease: queue.Lease{ID: int64(len(claimTimes)), Attempt: 7}, ChatID: 42, Text: "message"}}, nil
			default:
				<-ctx.Done()
				return nil, ctx.Err()
			}
		},
		finalize: func(_ context.Context, result finalization) error {
			finalizations++
			results <- result
			if finalizations == 1 {
				return errors.New("private-database-details")
			}
			return nil
		},
	}
	sender := senderFunc(func(context.Context, int64, string) (int64, error) { return 321, nil })
	var logs bytes.Buffer
	w := testWorker(t, store, sender, 1)
	w.logger = slog.New(slog.NewTextHandler(&logs, nil))
	cancel, done := startWorker(t, w)
	for range 2 {
		result := receive(t, results)
		if result.kind != "complete" || result.messageID != 321 || result.lease.Attempt != 7 {
			t.Errorf("unexpected finalization: %+v", result)
		}
	}
	cancel()
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(claimTimes); i++ {
		if claimTimes[i].Sub(claimTimes[i-1]) < w.cfg.PollInterval {
			t.Error("database failure caused a tight polling loop")
		}
	}
	if !strings.Contains(logs.String(), "outbox claim failed") || !strings.Contains(logs.String(), "outbox completion failed") {
		t.Error("database failures were not logged")
	}
	if strings.Contains(logs.String(), "private-database-details") {
		t.Error("database error leaked into logs")
	}
}
