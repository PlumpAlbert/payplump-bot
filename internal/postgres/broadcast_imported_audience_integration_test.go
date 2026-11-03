//go:build integration

package postgres

import (
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/broadcast"
	"github.com/PlumpAlbert/payplump-bot/templates"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestBroadcastAuthorityAudienceSurvivesRestartAndRechecksFrozenIdentity(t *testing.T) {
	s, ctx := authorityFixture(t)
	const bot = int64(9123)
	p := broadcastTestPolicy()
	if err := s.InstallBroadcastPolicy(ctx, bot, p); err != nil {
		t.Fatal(err)
	}
	paymentExec(t, s, ctx, `INSERT INTO telegram_bots(bot_id) VALUES(9124)`)
	for i := range 12 {
		o := authorityObservation(int64(70000 + i))
		o.ObservedAt = time.Now().UTC().AddDate(-2, 0, 0)
		if i == 1 {
			o.Enabled = false
		}
		if i == 2 {
			o.StateKnown, o.Enabled = false, false
		}
		sourceBot := bot
		if i == 11 {
			sourceBot++
		}
		if err := s.SaveAuthorityObservation(ctx, sourceBot, o); err != nil {
			t.Fatal(err)
		}
	}
	// A second provenance path must not duplicate a client.
	paymentExec(t, s, ctx, `INSERT INTO broadcast_memberships(bot_id,client_id,actor_id) SELECT $1,id,telegram_id FROM clients WHERE telegram_id=70003`, bot)
	paymentExec(t, s, ctx, `UPDATE authority_observations SET actor_id=80008 WHERE actor_id=70008`)
	paymentExec(t, s, ctx, `UPDATE clients SET chat_id=80009 WHERE telegram_id=70009`)
	paymentExec(t, s, ctx, `UPDATE clients SET invoice_ninja_id='changed-before-freeze' WHERE telegram_id=70010`)
	paymentExec(t, s, ctx, `INSERT INTO clients(telegram_id,chat_id,display_name) VALUES(70012,70012,'bare global client')`)
	paymentSQL(t, s, ctx, `SELECT NOT EXISTS(SELECT 1 FROM outbox) AND (SELECT count(*)=1 FROM broadcast_memberships) AND NOT EXISTS(SELECT 1 FROM clients WHERE sui_id IS NOT NULL)`)
	// A fresh pool and Store must use persisted provenance, not process memory.
	cfg := s.pool.Config()
	s.Close()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s = &Store{pool: pool}
	t.Cleanup(s.Close)
	job := createCleanupTestJob(t, s, ctx, bot, p, 1, "Imported ordinary announcement")
	paymentSQL(t, s, ctx, `SELECT array_agg(actor_id ORDER BY actor_id)=ARRAY[70000,70001,70002,70003,70004,70005,70006,70007]::bigint[] FROM broadcast_deliveries WHERE job_id=$1`, job)
	// Revoke provenance, change actor (even with new matching provenance), chat,
	// and customer after freezing. All four must be skipped at dispatch.
	paymentExec(t, s, ctx, `DELETE FROM authority_observations WHERE actor_id=70004`)
	paymentExec(t, s, ctx, `UPDATE clients SET telegram_id=80005,chat_id=80005 WHERE telegram_id=70005`)
	paymentExec(t, s, ctx, `UPDATE authority_observations SET actor_id=80005 WHERE actor_id=70005`)
	paymentExec(t, s, ctx, `UPDATE clients SET chat_id=80006 WHERE telegram_id=70006`)
	paymentExec(t, s, ctx, `UPDATE clients SET invoice_ninja_id='changed-after-freeze' WHERE telegram_id=70007`)
	if err := s.SaveAuthorityObservation(ctx, bot, authorityObservation(70013)); err != nil {
		t.Fatal(err)
	}
	progress, err := s.ProcessBroadcastBatch(ctx, bot, p, 50)
	if err != nil || progress.Enqueued != 4 || progress.Skipped != 4 {
		t.Fatalf("progress=%+v err=%v", progress, err)
	}
	paymentSQL(t, s, ctx, `SELECT count(*)=4 AND bool_and(NOT o.protected_configuration AND NOT o.client_menu) FROM broadcast_deliveries d JOIN outbox o ON o.id=d.outbox_id WHERE d.job_id=$1`, job)
	paymentSQL(t, s, ctx, `SELECT NOT EXISTS(SELECT 1 FROM broadcast_deliveries WHERE actor_id=70013) AND (SELECT count(*)=1 FROM broadcast_memberships) AND NOT EXISTS(SELECT 1 FROM outbox WHERE client_menu OR protected_configuration)`)
	// Complete the old job, then import another client. Completed jobs cannot rearm.
	paymentExec(t, s, ctx, `UPDATE outbox o SET status='sent',telegram_message_id=1,finished_at=clock_timestamp() FROM broadcast_deliveries d WHERE d.job_id=$1 AND o.id=d.outbox_id`, job)
	progress, err = s.ProcessBroadcastBatch(ctx, bot, p, 50)
	if err != nil || progress.Completed != 1 {
		t.Fatalf("completion=%+v err=%v", progress, err)
	}
	if err := s.SaveAuthorityObservation(ctx, bot, authorityObservation(70014)); err != nil {
		t.Fatal(err)
	}
	progress, err = s.ProcessBroadcastBatch(ctx, bot, p, 50)
	if err != nil || progress != (broadcast.Progress{}) {
		t.Fatalf("rearmed=%+v err=%v", progress, err)
	}
	paymentSQL(t, s, ctx, `SELECT count(*)=8 FROM broadcast_deliveries WHERE job_id=$1`, job)
}

func TestBroadcastVerifiedImportAudienceUsesImmutableActorAndCurrentCustomer(t *testing.T) {
	s, ctx := activationTestStore(t)
	const bot = admissionFixtureBotID
	paymentExec(t, s, ctx, `INSERT INTO telegram_bots(bot_id) VALUES($1)`, bot)
	prepared := admissionPreparedFixture(t, ctx)
	if err := s.SyncImportAdmissionPolicy(ctx, bot, 1, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyVerifiedImport(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	p := broadcastTestPolicy()
	for _, b := range []int64{bot, bot + 1} {
		if err := s.InstallBroadcastPolicy(ctx, b, p); err != nil {
			t.Fatal(err)
		}
	}
	job := createCleanupTestJob(t, s, ctx, bot, p, 1, "Legacy imported announcement")
	paymentSQL(t, s, ctx, `SELECT count(*)=1 AND min(actor_id)=42 FROM broadcast_deliveries WHERE job_id=$1`, job)
	progress, err := s.ProcessBroadcastBatch(ctx, bot, p, 50)
	if err != nil || progress.Enqueued != 1 {
		t.Fatalf("legacy progress=%+v err=%v", progress, err)
	}
	paymentSQL(t, s, ctx, `SELECT NOT EXISTS(SELECT 1 FROM broadcast_memberships) AND NOT EXISTS(SELECT 1 FROM outbox WHERE client_menu OR protected_configuration)`)
	// Freeze one more job before provenance no longer matches the current client.
	changedJob := createCleanupTestJob(t, s, ctx, bot, p, 10, "Recheck legacy identity")
	for i, tc := range []struct {
		name, change, restore string
		targetBot             int64
	}{
		{"other bot", "", "", bot + 1},
		{"customer", `UPDATE clients SET invoice_ninja_id='unrelated' WHERE telegram_id=42`, `UPDATE clients SET invoice_ninja_id='customer-42' WHERE telegram_id=42`, bot},
		{"actor", `UPDATE clients SET telegram_id=43,chat_id=43 WHERE telegram_id=42`, `UPDATE clients SET telegram_id=42,chat_id=42 WHERE telegram_id=43`, bot},
		{"chat", `UPDATE clients SET chat_id=43 WHERE telegram_id=42`, `UPDATE clients SET chat_id=42 WHERE telegram_id=42`, bot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.change != "" {
				paymentExec(t, s, ctx, tc.change)
			}
			id := int64(20 + i*2)
			preview := broadcastTestAction(t, s, ctx, tc.targetBot, p, id, "/broadcast_text excluded legacy identity")
			tok := broadcastResponseToken(t, preview, "/broadcast_all ")
			response := broadcastTestAction(t, s, ctx, tc.targetBot, p, id+1, "/broadcast_all "+tok)
			if response != templates.Render("broadcast/audience_empty", templates.Data{}) {
				t.Fatalf("unexpected audience: %q", response)
			}
			if tc.restore != "" {
				paymentExec(t, s, ctx, tc.restore)
			}
		})
	}
	paymentExec(t, s, ctx, `UPDATE clients SET invoice_ninja_id='unrelated' WHERE telegram_id=42`)
	progress, err = s.ProcessBroadcastBatch(ctx, bot, p, 50)
	if err != nil || progress.Skipped != 1 || progress.Enqueued != 0 {
		t.Fatalf("legacy recheck=%+v err=%v", progress, err)
	}
	paymentSQL(t, s, ctx, `SELECT status='skipped' FROM broadcast_deliveries WHERE job_id=$1`, changedJob)
}

func TestBroadcastEmptyAudienceKeepsTextTokenAndGuardsLegacyConfirm(t *testing.T) {
	s, ctx := testStore(t)
	const bot = int64(550120)
	p := broadcastTestPolicy()
	if err := s.InstallBroadcastPolicy(ctx, bot, p); err != nil {
		t.Fatal(err)
	}
	preview := broadcastTestAction(t, s, ctx, bot, p, 1, "/broadcast_text empty audience")
	tok := broadcastResponseToken(t, preview, "/broadcast_all ")
	h, _ := tokenHash(tok)
	for _, id := range []int64{2, 3} {
		response := broadcastTestAction(t, s, ctx, bot, p, id, "/broadcast_all "+tok)
		if response != templates.Render("broadcast/audience_empty", templates.Data{}) {
			t.Fatalf("unexpected empty response: %q", response)
		}
	}
	paymentSQL(t, s, ctx, `SELECT state='text' AND token_hash=$2 FROM broadcast_drafts WHERE bot_id=$1`, bot, h[:])
	paymentSQL(t, s, ctx, `SELECT NOT EXISTS(SELECT 1 FROM broadcasts) AND NOT EXISTS(SELECT 1 FROM broadcast_recipients)`)
	// Persist the state produced by the old empty-freeze implementation.
	paymentExec(t, s, ctx, `UPDATE broadcast_drafts SET state='audience' WHERE bot_id=$1`, bot)
	response := broadcastTestAction(t, s, ctx, bot, p, 4, "/broadcast_confirm "+tok)
	if response != templates.Render("broadcast/audience_empty", templates.Data{}) {
		t.Fatalf("legacy empty confirm: %q", response)
	}
	paymentSQL(t, s, ctx, `SELECT NOT EXISTS(SELECT 1 FROM broadcasts)`)
	// A historical confirmed empty job must still report its existing status.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	if _, err = tx.Exec(ctx, `INSERT INTO broadcasts(draft_id,bot_id,creator_id,policy_revision,policy_hash,state) SELECT id,bot_id,actor_id,policy_revision,policy_hash,'pending' FROM broadcast_drafts WHERE bot_id=$1`, bot); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE broadcast_drafts SET state='confirmed' WHERE bot_id=$1`, bot); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	expected := templates.Render("broadcast/status", templates.Data{Pending: "0", Enqueued: "0", Sent: "0", Failed: "0", Skipped: "0"})
	if response := broadcastTestAction(t, s, ctx, bot, p, 5, "/broadcast_confirm "+tok); response != expected {
		t.Fatalf("historical status: %q", response)
	}
	paymentSQL(t, s, ctx, `SELECT count(*)=1 FROM broadcasts`)
}

func TestBroadcastEmptyAudienceCanRetryOriginalTokenAfterImport(t *testing.T) {
	s, ctx := authorityFixture(t)
	const bot = int64(9123)
	p := broadcastTestPolicy()
	if err := s.InstallBroadcastPolicy(ctx, bot, p); err != nil {
		t.Fatal(err)
	}
	preview := broadcastTestAction(t, s, ctx, bot, p, 1, "/broadcast_text retry after import")
	tok := broadcastResponseToken(t, preview, "/broadcast_all ")
	if response := broadcastTestAction(t, s, ctx, bot, p, 2, "/broadcast_all "+tok); response != templates.Render("broadcast/audience_empty", templates.Data{}) {
		t.Fatalf("unexpected empty response: %q", response)
	}
	if err := s.SaveAuthorityObservation(ctx, bot, authorityObservation(71000)); err != nil {
		t.Fatal(err)
	}
	response := broadcastTestAction(t, s, ctx, bot, p, 3, "/broadcast_all "+tok)
	confirm := broadcastResponseToken(t, response, "/broadcast_confirm ")
	if response != templates.Render("broadcast/confirm_audience", templates.Data{Count: "1", Token: confirm}) {
		t.Fatalf("retry response: %q", response)
	}
	broadcastTestAction(t, s, ctx, bot, p, 4, "/broadcast_confirm "+confirm)
	paymentSQL(t, s, ctx, `SELECT count(*)=1 AND min(actor_id)=71000 FROM broadcast_deliveries`)
	paymentSQL(t, s, ctx, `SELECT NOT EXISTS(SELECT 1 FROM broadcast_memberships)`)
}
