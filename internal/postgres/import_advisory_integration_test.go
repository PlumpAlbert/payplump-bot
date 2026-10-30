//go:build integration

package postgres

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/queue"
)

func TestImportedBillingPauseAdvisoryIsAtomicAndDeduplicated(t *testing.T) {
	for _, status := range []string{"2", "3"} {
		t.Run(status, func(t *testing.T) {
			rig := newImportedOverdueRigWithHandler(t, false, func(src *importedOverdueSource) http.Handler {
				src.recurring["status_id"] = status
				if status == "3" {
					src.recurring["next_send_date"], src.recurring["next_send_datetime"] = "", ""
				}
				return src
			})
			prepared, err := rig.prepare(time.Now().UTC().Add(-time.Second))
			if err != nil {
				t.Fatal(err)
			}
			bot := prepared.BotID()
			if _, err = rig.s.pool.Exec(rig.ctx, `INSERT INTO telegram_bots(bot_id) VALUES($1) ON CONFLICT DO NOTHING`, bot); err != nil {
				t.Fatal(err)
			}
			if err = rig.s.SyncImportAdmissionPolicy(rig.ctx, bot, prepared.Revision(), prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
				t.Fatal(err)
			}
			admins := []int64{42, 9001, 9002, 9001}
			result, err := rig.s.ApplyVerifiedImportWithAdvisories(rig.ctx, prepared, admins)
			if err != nil || result.Admitted != 1 {
				t.Fatalf("advisory blocked valid admission: result=%+v err=%v", result, err)
			}
			var wg sync.WaitGroup
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					replay, err := rig.s.ApplyVerifiedImportWithAdvisories(rig.ctx, prepared, admins)
					if err != nil || replay.AlreadyAdmitted != 1 || replay.Admitted != 0 {
						t.Errorf("advisory replay changed import: result=%+v err=%v", replay, err)
					}
				}()
			}
			wg.Wait()
			want := 0
			if status == "3" {
				want = 3
			}
			var notices int
			if err = rig.s.pool.QueryRow(rig.ctx, `SELECT count(*) FROM outbox WHERE bot_id=$1 AND chat_id IN (42,9001,9002) AND message_key LIKE 'import-advisory:%'`, bot).Scan(&notices); err != nil || notices != want {
				t.Fatalf("wrong private advisory count: got=%d want=%d err=%v", notices, want, err)
			}
			paymentSQL(t, rig.s, rig.ctx, `SELECT NOT EXISTS(SELECT 1 FROM automatic_import_issues WHERE NOT resolved)`)
			paymentSQL(t, rig.s, rig.ctx, `SELECT NOT EXISTS(SELECT 1 FROM operations)`)
			paymentSQL(t, rig.s, rig.ctx, `SELECT state='active' AND desired_access FROM subscriptions WHERE client_id=(SELECT id FROM clients WHERE telegram_id=42)`)
			if len(rig.source.postSnapshot()) != 0 {
				t.Fatal("admission/advisory changed provider state")
			}
			if _, err := rig.s.EnqueueOutbox(rig.ctx, queue.Draft{BotID: bot, ChatID: 42, Key: "synthetic-managed-workflow", Text: "synthetic workflow message"}); err != nil {
				t.Fatal(err)
			}
			if _, err := rig.s.ApplyVerifiedImportWithAdvisories(rig.ctx, prepared, admins); err == nil {
				t.Fatal("ordinary managed workflow message was wrongly exempted as an advisory")
			}
		})
	}
}

func TestImportedAdvisoryEnqueueFailureRollsBackAdmission(t *testing.T) {
	rig := newImportedOverdueRigWithHandler(t, false, func(src *importedOverdueSource) http.Handler {
		src.recurring["status_id"] = "3"
		return src
	})
	prepared, err := rig.prepare(time.Now().UTC().Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	bot := prepared.BotID()
	if _, err = rig.s.pool.Exec(rig.ctx, `INSERT INTO telegram_bots(bot_id) VALUES($1) ON CONFLICT DO NOTHING`, bot); err != nil {
		t.Fatal(err)
	}
	if err = rig.s.SyncImportAdmissionPolicy(rig.ctx, bot, prepared.Revision(), prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
		t.Fatal(err)
	}
	// A conflicting immutable outbox row forces enqueue to fail after origin
	// insertion. Both identity admission and its pending notice must roll back.
	key := fmt.Sprintf("import-advisory:%s:billing-paused:9001", prepared.Rows()[0].StableHash)
	if _, err = rig.s.EnqueueOutbox(rig.ctx, queue.Draft{BotID: bot, ChatID: 9001, Key: key, Text: "synthetic conflicting notice"}); err != nil {
		t.Fatal(err)
	}
	if _, err = rig.s.ApplyVerifiedImportWithAdvisories(rig.ctx, prepared, []int64{9001}); err == nil {
		t.Fatal("conflicting advisory did not abort its admission transaction")
	}
	paymentSQL(t, rig.s, rig.ctx, `SELECT NOT EXISTS(SELECT 1 FROM verified_import_origins)`)
	paymentSQL(t, rig.s, rig.ctx, `SELECT NOT EXISTS(SELECT 1 FROM clients WHERE telegram_id=42)`)
	paymentSQL(t, rig.s, rig.ctx, `SELECT count(*)=1 FROM outbox`)
}
