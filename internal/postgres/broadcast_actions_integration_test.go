//go:build integration

package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/broadcast"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
)

func broadcastUpdate(id int64, text string) telegram.Update {
	head := text
	for i, c := range text {
		if c == ' ' || c == '\n' || c == '\t' {
			head = text[:i]
			break
		}
	}
	return telegram.Update{ID: id, Message: &telegram.Message{From: &telegram.User{ID: 7, FirstName: "Admin"}, Chat: telegram.Chat{ID: 7, Type: "private"}, Text: text, Entities: []telegram.Entity{{Type: "bot_command", Offset: 0, Length: len(head)}}}}
}
func TestBroadcastAdminActionsAreBoundToPrivateDraft(t *testing.T) {
	s, ctx := testStore(t)
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	cfg := broadcast.Config{Enabled: true, Revision: 1, MaxRecipients: 10, AdminIDs: map[int64]struct{}{7: {}}}
	p := cfg.Policy()
	if err := s.InstallBroadcastPolicy(ctx, 123, p); err != nil {
		t.Fatal("install policy")
	}
	for _, id := range []int64{7, 8} {
		_, err := s.RegisterPrivateClient(ctx, TelegramIdentity{TelegramID: id, ChatID: id, DisplayName: "synthetic"})
		if err != nil {
			t.Fatal(err)
		}
		tx, e := s.pool.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		if e = recordBroadcastMembership(ctx, tx, 123, id); e != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(e)
		}
		if e = tx.Commit(ctx); e != nil {
			t.Fatal(e)
		}
	}
	apply := func(n int64, text string) {
		t.Helper()
		handled, err := s.ApplyBroadcastUpdate(ctx, 123, "mybot", broadcastUpdate(n, text), p)
		if err != nil || !handled {
			t.Fatalf("command failed: handled=%v", handled)
		}
	}
	apply(1, "/broadcast")
	apply(2, "/broadcast_text first line\nsecond line")
	var preview string
	if err := s.pool.QueryRow(ctx, `SELECT body FROM outbox WHERE bot_id=123 AND message_key='broadcast-admin:123:2'`).Scan(&preview); err != nil {
		t.Fatal("missing preview")
	}
	tokenA := strings.Split(strings.Split(preview, "Токен: ")[1], "\n")[0]
	apply(3, "/broadcast_all "+tokenA)
	var freeze string
	if err := s.pool.QueryRow(ctx, `SELECT body FROM outbox WHERE bot_id=123 AND message_key='broadcast-admin:123:3'`).Scan(&freeze); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(freeze, "Получателей: 2") {
		t.Fatalf("unexpected audience summary")
	}
	tokenB := strings.TrimPrefix(strings.Split(freeze, "/broadcast_confirm ")[1], "")
	apply(4, "/broadcast_confirm "+tokenB)
	var jobs int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM broadcasts WHERE bot_id=123`).Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("job count %d", jobs)
	}
	apply(5, "/broadcast_status "+tokenB)
	apply(6, "/broadcast_cancel "+tokenB)
}
