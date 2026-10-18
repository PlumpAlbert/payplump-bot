//go:build integration

package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/broadcast"
	"github.com/PlumpAlbert/payplump-bot/internal/clientmenu"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
)

func broadcastTestPolicy() broadcast.Policy {
	return broadcast.Config{Enabled: true, Revision: 1, MaxRecipients: 100, AdminIDs: map[int64]struct{}{7: {}}}.Policy()
}
func broadcastTestAction(t *testing.T, s *Store, ctx context.Context, bot int64, p broadcast.Policy, id int64, text string) string {
	t.Helper()
	handled, err := s.ApplyBroadcastUpdate(ctx, bot, "mybot", broadcastUpdate(id, text), p)
	if err != nil || !handled {
		t.Fatal("broadcast action failed")
	}
	var body string
	if err = s.pool.QueryRow(ctx, `SELECT body FROM outbox WHERE bot_id=$1 AND message_key=$2`, bot, "broadcast-admin:"+formatIDs(bot, id)).Scan(&body); err != nil {
		t.Fatal("broadcast response missing")
	}
	return body
}
func broadcastTestMembers(t *testing.T, s *Store, ctx context.Context, bot int64, n int) {
	t.Helper()
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer rollback(tx)
	if _, e = tx.Exec(ctx, `INSERT INTO clients(telegram_id,chat_id,display_name) SELECT n,n,'synthetic' FROM generate_series(1000::bigint,1000::bigint+$1::bigint-1) n`, n); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, `INSERT INTO subscriptions(client_id) SELECT id FROM clients WHERE telegram_id>=1000 AND telegram_id<1000+$1`, n); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, `INSERT INTO broadcast_memberships(bot_id,client_id,actor_id) SELECT $1,id,telegram_id FROM clients WHERE telegram_id>=1000 AND telegram_id<1000+$2`, bot, n); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
}
func TestBroadcastFrozenDeliveryAndTerminalSummaryCannotBeRearmed(t *testing.T) {
	s, _ := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	const bot = int64(550001)
	p := broadcastTestPolicy()
	if e := s.InstallBroadcastPolicy(ctx, bot, p); e != nil {
		t.Fatal(e)
	}
	broadcastTestMembers(t, s, ctx, bot, 51)
	a := broadcastResponseToken(t, broadcastTestAction(t, s, ctx, bot, p, 1, "/broadcast_text synthetic immutable body"), "Токен: ")
	b := broadcastResponseToken(t, broadcastTestAction(t, s, ctx, bot, p, 2, "/broadcast_all "+a), "/broadcast_confirm ")
	var draft int64
	if e := s.pool.QueryRow(ctx, `SELECT id FROM broadcast_drafts WHERE bot_id=$1 AND state='audience'`, bot).Scan(&draft); e != nil {
		t.Fatal(e)
	}
	for _, sql := range []string{`UPDATE broadcast_drafts SET body='changed' WHERE id=$1`, `DELETE FROM broadcast_recipients WHERE draft_id=$1`, `UPDATE broadcast_recipients SET actor_id=actor_id+5000,chat_id=chat_id+5000 WHERE draft_id=$1`} {
		if _, e := s.pool.Exec(ctx, sql, draft); e == nil {
			t.Fatal("frozen preview could be modified")
		}
	}
	if body := broadcastTestAction(t, s, ctx, bot, p, 3, "/broadcast_confirm "+a); !strings.Contains(body, "не найден") {
		t.Fatal("old preview token survived nonce rotation")
	}
	broadcastTestAction(t, s, ctx, bot, p, 4, "/broadcast_confirm "+b)
	var job int64
	if e := s.pool.QueryRow(ctx, `SELECT id FROM broadcasts WHERE draft_id=$1`, draft).Scan(&job); e != nil {
		t.Fatal(e)
	}
	for _, sql := range []string{`DELETE FROM broadcasts WHERE id=$1`, `UPDATE broadcast_deliveries SET actor_id=actor_id+5000,chat_id=chat_id+5000 WHERE job_id=$1`, `DELETE FROM broadcast_deliveries WHERE job_id=$1`} {
		if _, e := s.pool.Exec(ctx, sql, job); e == nil {
			t.Fatal("confirmed job could be reassigned or removed")
		}
	}
	first, e := s.ProcessBroadcastBatch(ctx, bot, p, 50)
	if e != nil || first.Enqueued != 50 || first.Completed != 0 {
		t.Fatal("first batch was unbounded or falsely complete")
	}
	second, e := s.ProcessBroadcastBatch(ctx, bot, p, 50)
	if e != nil || second.Enqueued != 1 || second.Completed != 0 {
		t.Fatal("remaining batch was lost or falsely complete")
	}
	third, e := s.ProcessBroadcastBatch(ctx, bot, p, 50)
	if e != nil || third.Enqueued != 0 || third.Completed != 0 {
		t.Fatal("queued messages were counted as delivered")
	}
	var count int
	if e = s.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE message_key LIKE 'broadcast-summary:%'`).Scan(&count); e != nil || count != 0 {
		t.Fatal("summary emitted before terminal delivery")
	}
	// Synthetic transport receipt injection: this test isolates SQL completion.
	// The separate workflow test exercises real worker and Telegram HTTP parsing.
	if _, e = s.pool.Exec(ctx, `UPDATE outbox o SET status='sent',telegram_message_id=1,finished_at=clock_timestamp() FROM broadcast_deliveries d WHERE d.job_id=$1 AND o.id=d.outbox_id`, job); e != nil {
		t.Fatal(e)
	}
	done, e := s.ProcessBroadcastBatch(ctx, bot, p, 50)
	if e != nil || done.Completed != 1 {
		t.Fatal("terminal broadcast did not finalize")
	}
	again, e := s.ProcessBroadcastBatch(ctx, bot, p, 50)
	if e != nil || again.Completed != 0 {
		t.Fatal("completion was replayed")
	}
	if e = s.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE message_key LIKE 'broadcast-summary:%'`).Scan(&count); e != nil || count != 1 {
		t.Fatal("summary was duplicated")
	}
	if _, e = s.pool.Exec(ctx, `UPDATE broadcast_deliveries SET status='pending',outbox_id=NULL WHERE job_id=$1`, job); e == nil {
		t.Fatal("known delivery could be rearmed")
	}
}
func TestBroadcastMembershipRequiresSameBotPrivateMenuInteraction(t *testing.T) {
	s, _ := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	const bot = int64(550002)
	p := broadcastTestPolicy()
	if e := s.InstallBroadcastPolicy(ctx, bot, p); e != nil {
		t.Fatal(e)
	}
	start := func(bot, actor, id int64) {
		t.Helper()
		u := telegram.Update{ID: id, Message: &telegram.Message{From: &telegram.User{ID: actor, FirstName: "synthetic"}, Chat: telegram.Chat{ID: actor, Type: "private"}, Text: "/start", Entities: []telegram.Entity{{Type: "bot_command", Offset: 0, Length: 6}}}}
		prepared := clientmenu.Prepared{Request: clientmenu.Request{BotID: bot, UpdateID: id, TelegramID: actor, ChatID: actor, Action: clientmenu.Start}, Text: "synthetic hello"}
		if e := s.ApplyMenuUpdate(ctx, bot, "mybot", u, prepared); e != nil {
			t.Fatal(e)
		}
	}
	start(bot, 100, 1)
	start(bot+1, 102, 1)
	if _, e := s.RegisterPrivateClient(ctx, TelegramIdentity{TelegramID: 101, ChatID: 101, DisplayName: "unrelated global client"}); e != nil {
		t.Fatal(e)
	}
	a := broadcastResponseToken(t, broadcastTestAction(t, s, ctx, bot, p, 2, "/broadcast_text private members only"), "Токен: ")
	preview := broadcastTestAction(t, s, ctx, bot, p, 3, "/broadcast_all "+a)
	if !strings.HasPrefix(preview, "Получателей: 1.") {
		t.Fatal("foreign-bot or arbitrary global clients were adopted")
	}
	var members int
	if e := s.pool.QueryRow(ctx, `SELECT count(*) FROM broadcast_memberships WHERE bot_id=$1`, bot).Scan(&members); e != nil || members != 1 {
		t.Fatal("private menu membership was not atomic")
	}
}
