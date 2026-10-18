//go:build integration

package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/broadcast"
)

func TestBroadcastRecipientLimitRollsBackPartialAudience(t *testing.T) {
	s, ctx := testStore(t)
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	const bot = int64(912346)
	cfg := broadcast.Config{Enabled: true, Revision: 1, MaxRecipients: 1, AdminIDs: map[int64]struct{}{7: {}}}
	p := cfg.Policy()
	if err := s.InstallBroadcastPolicy(ctx, bot, p); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{7, 8} {
		if _, err := s.RegisterPrivateClient(ctx, TelegramIdentity{TelegramID: id, ChatID: id, DisplayName: "synthetic"}); err != nil {
			t.Fatal(err)
		}
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = recordBroadcastMembership(ctx, tx, bot, id); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	apply := func(id int64, text string) {
		t.Helper()
		handled, err := s.ApplyBroadcastUpdate(ctx, bot, "mybot", broadcastUpdate(id, text), p)
		if err != nil || !handled {
			t.Fatalf("update: handled=%v err=%v", handled, err)
		}
	}
	apply(1, "/broadcast_text overflow")
	var preview string
	if err := s.pool.QueryRow(ctx, `SELECT body FROM outbox WHERE bot_id=$1 AND message_key=$2`, bot, "broadcast-admin:912346:1").Scan(&preview); err != nil {
		t.Fatal(err)
	}
	token := strings.Split(strings.Split(preview, "Токен: ")[1], "\n")[0]
	apply(2, "/broadcast_all "+token)
	var denial string
	if err := s.pool.QueryRow(ctx, `SELECT body FROM outbox WHERE bot_id=$1 AND message_key=$2`, bot, "broadcast-admin:912346:2").Scan(&denial); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(denial, "превышает разрешённый предел") {
		t.Fatalf("unexpected response: %q", denial)
	}
	var draftState string
	var recipients, clients, jobs int
	if err := s.pool.QueryRow(ctx, `SELECT state FROM broadcast_drafts WHERE bot_id=$1`, bot).Scan(&draftState); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM broadcast_recipients r JOIN broadcast_drafts d ON d.id=r.draft_id WHERE d.bot_id=$1`, bot).Scan(&recipients); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM clients WHERE telegram_id IN (7,8)`).Scan(&clients); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM broadcasts WHERE bot_id=$1`, bot).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if draftState != "text" || recipients != 0 || clients != 2 || jobs != 0 {
		t.Fatalf("state=%s recipients=%d clients=%d jobs=%d", draftState, recipients, clients, jobs)
	}
}

func TestBroadcastPolicyRotationRejectsPreparedDraftAndOldActor(t *testing.T) {
	s, ctx := testStore(t)
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	const bot = int64(912347)
	old := broadcast.Config{Enabled: true, Revision: 1, MaxRecipients: 10, AdminIDs: map[int64]struct{}{7: {}}}.Policy()
	if err := s.InstallBroadcastPolicy(ctx, bot, old); err != nil {
		t.Fatal(err)
	}
	apply := func(id int64, text string, p broadcast.Policy) {
		t.Helper()
		handled, err := s.ApplyBroadcastUpdate(ctx, bot, "mybot", broadcastUpdate(id, text), p)
		if err != nil || !handled {
			t.Fatalf("update: handled=%v err=%v", handled, err)
		}
	}
	apply(1, "/broadcast_text prepared before rotation", old)
	newPolicy := broadcast.Config{Enabled: true, Revision: 2, MaxRecipients: 10, AdminIDs: map[int64]struct{}{8: {}}}.Policy()
	if err := s.InstallBroadcastPolicy(ctx, bot, newPolicy); err != nil {
		t.Fatal(err)
	}
	// An old process snapshot cannot act for the former administrator after rotation.
	apply(2, "/broadcast_text unauthorized", old)
	var deny string
	if err := s.pool.QueryRow(ctx, `SELECT body FROM outbox WHERE bot_id=$1 AND message_key=$2`, bot, "broadcast-admin:912347:2").Scan(&deny); err != nil {
		t.Fatal(err)
	}
	if deny != "Доступ к команде рассылки изменился или команда неверна." {
		t.Fatal("former administrator was not denied by current database policy")
	}
	var drafts, jobs int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM broadcast_drafts WHERE bot_id=$1`, bot).Scan(&drafts); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM broadcasts WHERE bot_id=$1`, bot).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if drafts != 1 || jobs != 0 {
		t.Fatalf("drafts=%d jobs=%d", drafts, jobs)
	}
}
