//go:build integration

package postgres

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
)

type pollingAPI struct {
	fetch func(context.Context, int64) ([]telegram.Update, error)
}

func (a pollingAPI) GetMe(context.Context) (telegram.User, error) {
	return telegram.User{ID: 123, IsBot: true, Username: "PayplumpBot"}, nil
}
func (a pollingAPI) GetUpdates(ctx context.Context, offset int64) ([]telegram.Update, error) {
	return a.fetch(ctx, offset)
}

type stopAfterCommit struct {
	*Store
	cancel context.CancelFunc
}

func (s stopAfterCommit) ApplyTelegramUpdate(ctx context.Context, botID int64, name string, u telegram.Update) error {
	err := s.Store.ApplyTelegramUpdate(ctx, botID, name, u)
	if err == nil {
		s.cancel()
	}
	return err
}

func TestPollingRestartAfterCommitBeforeAcknowledgement(t *testing.T) {
	s, ctx := testStore(t)
	firstCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	requests := 0
	api := pollingAPI{fetch: func(_ context.Context, offset int64) ([]telegram.Update, error) {
		requests++
		if offset != 0 {
			t.Error("first request must start at zero")
		}
		return []telegram.Update{privateUpdate(100)}, nil
	}}
	if err := telegram.NewPoller(api, stopAfterCommit{Store: s, cancel: cancel}, logger).Run(firstCtx); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatal("old process confirmed an update after cancellation")
	}
	s.pool.Reset()
	secondCtx, cancelSecond := context.WithCancel(ctx)
	defer cancelSecond()
	api.fetch = func(ctx context.Context, offset int64) ([]telegram.Update, error) {
		if offset != 101 {
			t.Error("restart did not confirm the committed update")
		}
		cancelSecond()
		return nil, ctx.Err()
	}
	if err := telegram.NewPoller(api, s, logger).Run(secondCtx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM clients`).Scan(&count); err != nil || count != 1 {
		t.Fatal("restart lost or duplicated registration")
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM operations`).Scan(&count); err != nil || count != 1 {
		t.Fatal("restart lost or duplicated registration operation")
	}
}

func privateUpdate(id int64) telegram.Update {
	return telegram.Update{ID: id, Message: &telegram.Message{
		From: &telegram.User{ID: 4503599627370497, FirstName: "Клиент"},
		Chat: telegram.Chat{ID: 4503599627370497, Type: "private"}, Text: "/start",
		Entities: []telegram.Entity{{Type: "bot_command", Offset: 0, Length: 6}},
	}}
}

func TestTelegramUpdateReplayAndCheckpoint(t *testing.T) {
	s, ctx := testStore(t)
	if offset, err := s.TelegramOffset(ctx, 123); err != nil || offset != 0 {
		t.Fatalf("initial offset: %d, %v", offset, err)
	}
	u := privateUpdate(100)
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			if err := s.ApplyTelegramUpdate(ctx, 123, "PayplumpBot", u); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	s.pool.Reset() // Simulate a new process reading the committed checkpoint.
	if offset, err := s.TelegramOffset(ctx, 123); err != nil || offset != 101 {
		t.Fatalf("persisted offset: %d, %v", offset, err)
	}
	// Duplicate delivery must not replay business changes, even with a changed body.
	u.Message.From.FirstName = "Повтор"
	if err := s.ApplyTelegramUpdate(ctx, 123, "PayplumpBot", u); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := s.pool.QueryRow(ctx, `SELECT display_name FROM clients`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "Клиент" {
		t.Fatal("duplicate update replayed business processing")
	}
	// A genuinely new /start returns to the existing client and updates the profile.
	u.ID = 101
	if err := s.ApplyTelegramUpdate(ctx, 123, "PayplumpBot", u); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"clients", "subscriptions", "operations"} {
		var count int
		if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("duplicate records in %s", table)
		}
	}
	if offset, err := s.TelegramOffset(ctx, 123); err != nil || offset != 102 {
		t.Fatalf("next offset: %d, %v", offset, err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT display_name FROM clients`).Scan(&name); err != nil || name != "Повтор" {
		t.Fatal("new /start did not load/update the existing client")
	}
	// Replaying an older processed update cannot move the cursor backwards.
	if err := s.ApplyTelegramUpdate(ctx, 123, "PayplumpBot", privateUpdate(100)); err != nil {
		t.Fatal(err)
	}
	if offset, err := s.TelegramOffset(ctx, 123); err != nil || offset != 102 {
		t.Fatal("duplicate update regressed the cursor")
	}
	if offset, err := s.TelegramOffset(ctx, 456); err != nil || offset != 0 {
		t.Fatal("checkpoint leaked between bots")
	}
}

func TestTelegramFailureRollsBackCheckpointAndRegistration(t *testing.T) {
	s, ctx := testStore(t)
	if err := s.ApplyTelegramUpdate(ctx, 123, "PayplumpBot", telegram.Update{ID: 9}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE operations ADD CONSTRAINT fail_enqueue CHECK (false)`); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyTelegramUpdate(ctx, 123, "PayplumpBot", privateUpdate(10)); err == nil {
		t.Fatal("failed operation acknowledged")
	}
	if offset, err := s.TelegramOffset(ctx, 123); err != nil || offset != 10 {
		t.Fatal("failed update advanced the checkpoint")
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM clients`).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed update left a client")
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_processed_updates WHERE update_id=10`).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed update was marked processed")
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE operations DROP CONSTRAINT fail_enqueue`); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyTelegramUpdate(ctx, 123, "PayplumpBot", privateUpdate(10)); err != nil {
		t.Fatal(err)
	}
	if offset, err := s.TelegramOffset(ctx, 123); err != nil || offset != 11 {
		t.Fatal("replayed update did not commit")
	}
}

func TestTelegramIgnoresNonPrivateCommandsButAdvancesCheckpoint(t *testing.T) {
	s, ctx := testStore(t)
	group := privateUpdate(1)
	group.Message.Chat.Type = "group"
	group.Message.Chat.ID = -42
	other := privateUpdate(2)
	other.Message.Text = "/help"
	other.Message.Entities[0].Length = 5
	for _, u := range []telegram.Update{group, other, {ID: 3}} {
		if err := s.ApplyTelegramUpdate(ctx, 123, "PayplumpBot", u); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM clients`).Scan(&count); err != nil || count != 0 {
		t.Fatal("unsupported update registered a client")
	}
	if offset, err := s.TelegramOffset(ctx, 123); err != nil || offset != 4 {
		t.Fatal("unsupported updates stuck in the queue")
	}
}

func TestTelegramCheckpointExpiresBeforeIDReseed(t *testing.T) {
	s, ctx := testStore(t)
	if err := s.ApplyTelegramUpdate(ctx, 123, "PayplumpBot", telegram.Update{ID: 1000}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE telegram_polling_state SET last_update_at = now() - interval '8 days'`); err != nil {
		t.Fatal(err)
	}
	if offset, err := s.TelegramOffset(ctx, 123); err != nil || offset != 0 {
		t.Fatal("stale offset did not expire")
	}
	if err := s.ApplyTelegramUpdate(ctx, 123, "PayplumpBot", privateUpdate(5)); err != nil {
		t.Fatal(err)
	}
	if offset, err := s.TelegramOffset(ctx, 123); err != nil || offset != 6 {
		t.Fatal("new update ID range was skipped")
	}
}
