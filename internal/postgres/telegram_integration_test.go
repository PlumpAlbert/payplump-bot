//go:build integration

package postgres

import (
	"sync"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
)

func privateUpdate(id int64) telegram.Update {
	return telegram.Update{ID: id, Message: &telegram.Message{
		From: &telegram.User{ID: 4503599627370497, FirstName: "Клиент"},
		Chat: telegram.Chat{ID: 4503599627370497, Type: "private"}, Text: "/start",
		Entities: []telegram.Entity{{Type: "bot_command", Offset: 0, Length: 6}},
	}}
}

func TestTelegramUpdateReplayAndRegistration(t *testing.T) {
	s, ctx := testStore(t)
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
	s.pool.Reset() // Replay after reconnecting must retain deduplication metadata.
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
	if err := s.pool.QueryRow(ctx, `SELECT display_name FROM clients`).Scan(&name); err != nil || name != "Повтор" {
		t.Fatal("new /start did not load/update the existing client")
	}
	// Replaying an older processed update remains idempotent.
	if err := s.ApplyTelegramUpdate(ctx, 123, "PayplumpBot", privateUpdate(100)); err != nil {
		t.Fatal(err)
	}
}

func TestTelegramFailureRollsBackEventAndRegistration(t *testing.T) {
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
}

func TestTelegramIgnoresNonPrivateCommands(t *testing.T) {
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
}

func TestTelegramWebhookAcceptsOutOfOrderIDs(t *testing.T) {
	s, ctx := testStore(t)
	for _, id := range []int64{1000, 5, 999} {
		if err := s.ApplyTelegramUpdate(ctx, 123, "PayplumpBot", privateUpdate(id)); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_processed_updates`).Scan(&count); err != nil || count != 3 {
		t.Fatal("out-of-order deliveries were skipped")
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM clients`).Scan(&count); err != nil || count != 1 {
		t.Fatal("out-of-order deliveries duplicated registration")
	}
	// An equal update ID from a different bot is a distinct event.
	if err := s.ApplyTelegramUpdate(ctx, 456, "OtherBot", privateUpdate(5)); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_processed_updates`).Scan(&count); err != nil || count != 4 {
		t.Fatal("deduplication leaked between bots")
	}
}
