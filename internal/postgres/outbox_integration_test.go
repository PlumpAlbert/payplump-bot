//go:build integration

package postgres

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/queue"
)

func TestOutboxDeduplicationAndChatOrdering(t *testing.T) {
	s, ctx := testStore(t)
	first := queue.Draft{BotID: 123, ChatID: 42, Key: "invoice:1", Text: "Первый счёт"}
	id, err := s.EnqueueOutbox(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if repeated, err := s.EnqueueOutbox(ctx, first); err != nil || repeated != id {
		t.Fatalf("idempotency lost: id=%d, err=%v", repeated, err)
	}
	for _, changed := range []queue.Draft{
		{BotID: 123, ChatID: 99, Key: first.Key, Text: first.Text},
		{BotID: 123, ChatID: 42, Key: first.Key, Text: "Другой счёт"},
	} {
		if _, err := s.EnqueueOutbox(ctx, changed); !errors.Is(err, queue.ErrConflict) {
			t.Fatalf("changed notification accepted: %v", err)
		}
	}
	for _, draft := range []queue.Draft{
		{BotID: 123, ChatID: 42, Key: "payment:1", Text: "Оплата"},
		{BotID: 123, ChatID: 43, Key: "invoice:2", Text: "Другой адресат"},
		{BotID: 456, ChatID: 42, Key: first.Key, Text: "Другой бот"},
	} {
		if _, err := s.EnqueueOutbox(ctx, draft); err != nil {
			t.Fatal(err)
		}
	}
	messages, err := s.ClaimOutbox(ctx, 123, 10, time.Minute)
	if err != nil || len(messages) != 2 {
		t.Fatalf("claim: len=%d, err=%v", len(messages), err)
	}
	var original queue.Message
	for _, message := range messages {
		if message.BotID != 123 || message.Attempt != 1 || message.Text == "Оплата" {
			t.Fatal("wrong bot, generation or per-chat order")
		}
		if message.ID == id {
			original = message
		}
	}
	if original.ID == 0 {
		t.Fatal("oldest message was not claimed")
	}
	if err := s.CompleteOutbox(ctx, original.Lease, 700); err != nil {
		t.Fatal(err)
	}
	messages, err = s.ClaimOutbox(ctx, 123, 10, time.Minute)
	if err != nil || len(messages) != 1 || messages[0].Text != "Оплата" {
		t.Fatalf("following message not released: %+v, %v", messages, err)
	}
	if repeated, err := s.EnqueueOutbox(ctx, first); err != nil || repeated != id {
		t.Fatal("delivered notification was recreated")
	}
	var sentID int64
	if err := s.pool.QueryRow(ctx, `SELECT telegram_message_id FROM outbox WHERE id = $1 AND status = 'sent'`, id).Scan(&sentID); err != nil || sentID != 700 {
		t.Fatal("Telegram acknowledgement was not saved")
	}
}

func TestOutboxLeaseRecoveryFencesStaleOwners(t *testing.T) {
	s, ctx := testStore(t)
	id, err := s.EnqueueOutbox(ctx, queue.Draft{BotID: 123, ChatID: 42, Key: "notice", Text: "Сообщение"})
	if err != nil {
		t.Fatal(err)
	}
	messages, err := s.ClaimOutbox(ctx, 123, 1, time.Minute)
	if err != nil || len(messages) != 1 {
		t.Fatalf("claim: %v", err)
	}
	old := messages[0].Lease
	if again, err := s.ClaimOutbox(ctx, 123, 1, time.Minute); err != nil || len(again) != 0 {
		t.Fatal("active lease was reclaimed")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE outbox SET locked_until = clock_timestamp() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteOutbox(ctx, old, 100); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("expired lease acknowledged: %v", err)
	}
	messages, err = s.ClaimOutbox(ctx, 123, 1, time.Minute)
	if err != nil || len(messages) != 1 || messages[0].Attempt <= old.Attempt {
		t.Fatalf("crashed delivery not recovered: %+v, %v", messages, err)
	}
	if err := s.RetryOutbox(ctx, old, time.Second, 500); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("stale retry accepted: %v", err)
	}
	if err := s.FailOutbox(ctx, old, 403); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("stale failure accepted: %v", err)
	}
	if err := s.FailOutbox(ctx, messages[0].Lease, 403); err != nil {
		t.Fatal(err)
	}
	if again, err := s.ClaimOutbox(ctx, 123, 1, time.Minute); err != nil || len(again) != 0 {
		t.Fatal("permanently failed delivery was retried")
	}
}

func TestOutboxRetryAfterPersistsBotCooldown(t *testing.T) {
	s, ctx := testStore(t)
	for i, draft := range []queue.Draft{
		{BotID: 123, ChatID: 42, Key: "first", Text: "Первый"},
		{BotID: 123, ChatID: 43, Key: "second", Text: "Второй"},
		{BotID: 456, ChatID: 44, Key: "other", Text: "Другой бот"},
	} {
		if _, err := s.EnqueueOutbox(ctx, draft); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	messages, err := s.ClaimOutbox(ctx, 123, 1, time.Minute)
	if err != nil || len(messages) != 1 {
		t.Fatalf("claim: %v", err)
	}
	if err := s.RetryOutbox(ctx, messages[0].Lease, 17*time.Second, 429); err != nil {
		t.Fatal(err)
	}
	s.pool.Reset() // No in-memory cooldown may be required after a restart.
	if again, err := s.ClaimOutbox(ctx, 123, 10, time.Minute); err != nil || len(again) != 0 {
		t.Fatal("bot cooldown was ignored")
	}
	if other, err := s.ClaimOutbox(ctx, 456, 10, time.Minute); err != nil || len(other) != 1 {
		t.Fatal("one bot's rate limit blocked another")
	}
	var deferred bool
	if err := s.pool.QueryRow(ctx, `SELECT next_attempt_at > clock_timestamp() + interval '10 seconds' AND last_error_code = 429 FROM outbox WHERE id = $1`, messages[0].ID).Scan(&deferred); err != nil || !deferred {
		t.Fatal("retry_after was not durably scheduled")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE telegram_bots SET outbox_not_before = '-infinity'; UPDATE outbox SET next_attempt_at = clock_timestamp() - interval '1 second' WHERE status = 'pending'`); err != nil {
		t.Fatal(err)
	}
	if again, err := s.ClaimOutbox(ctx, 123, 10, time.Minute); err != nil || len(again) != 2 {
		t.Fatalf("deferred work not released: %d, %v", len(again), err)
	}
}

func TestConcurrentOutboxClaimsAreDisjoint(t *testing.T) {
	s, ctx := testStore(t)
	for i := range 4 {
		if _, err := s.EnqueueOutbox(ctx, queue.Draft{BotID: 123, ChatID: int64(42 + i), Key: fmt.Sprint(i), Text: "Сообщение"}); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	results := make(chan []queue.Message, 4)
	for range 4 {
		wg.Go(func() {
			messages, err := s.ClaimOutbox(ctx, 123, 1, time.Minute)
			if err != nil {
				t.Error(err)
			}
			results <- messages
		})
	}
	wg.Wait()
	close(results)
	seen := make(map[int64]bool)
	for batch := range results {
		for _, message := range batch {
			if seen[message.ID] {
				t.Fatal("duplicate concurrent claim")
			}
			seen[message.ID] = true
		}
	}
	// SKIP LOCKED may give some claimers an empty batch; drain the remainder.
	rest, err := s.ClaimOutbox(ctx, 123, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range rest {
		if seen[message.ID] {
			t.Fatal("active delivery reclaimed")
		}
		seen[message.ID] = true
	}
	if len(seen) != 4 {
		t.Fatalf("claimed %d of 4 messages", len(seen))
	}
}
