//go:build integration

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/postgres"
)

// This end-to-end scenario proves that imported admission, read-only runtime
// observation, and external-effect permission remain separate durable gates.
// It deliberately exercises no Telegram or source-provider write capability.
func TestImportedRuntimeReadOnlyStartupWorkflowRealPostgres(t *testing.T) {
	f := newImportApplyFixture(t)
	db := newImportApplyPostgresSchema(t)
	ctx := db.ctx
	var guardEpochBefore int64
	var guardFreeBefore bool
	if err := db.inspect.QueryRow(ctx, `SELECT epoch,operation_id IS NULL AND step IS NULL FROM activation_write_guard WHERE id=1`).Scan(&guardEpochBefore, &guardFreeBefore); err != nil || !guardFreeBefore {
		t.Fatalf("fresh schema activation guard is not free: epoch=%d free=%v err=%v", guardEpochBefore, guardFreeBefore, err)
	}
	getsBeforeInitialFence := f.GETs.Load()
	if getsBeforeInitialFence != 0 {
		t.Fatalf("fresh fixture unexpectedly performed source GETs before startup fence: %d", getsBeforeInitialFence)
	}
	if err := fenceImportedRuntimeStartup(ctx, db.store, importedRuntimeConfig{}); err != nil {
		t.Fatalf("default-off fresh-schema startup fence: %v", err)
	}
	if f.GETs.Load() != getsBeforeInitialFence {
		t.Fatal("fresh-schema startup fence performed source GETs")
	}
	assertImportRuntimeCounts(t, db, 0, 0, 0)
	for _, table := range []string{"clients", "verified_import_origins", "operations", "outbox"} {
		var count int
		if err := db.inspect.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("fresh-schema startup fence changed %s: count=%d err=%v", table, count, err)
		}
	}
	f.env["DATABASE_URL"], f.env["DB_MAX_CONNS"] = db.dbURL, "4"
	if _, err := db.inspect.Exec(ctx, `INSERT INTO telegram_bots(bot_id) VALUES(9) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	assertImportRuntimeCounts(t, db, 0, 0, 0)

	getenv := func(key string) string { return f.env[key] }
	args := []string{"--file", f.manifestPath, "--expected-hash", f.planHash, "--admission-revision", "1", "--apply", "--enable-admission"}
	if err := importApplyWith(args, io.Discard, getenv, newVerifyProviders, func() time.Time { return f.now }, func(ctx context.Context, raw string, max int32) (importApplyStore, error) {
		return postgres.Open(ctx, raw, max)
	}); err != nil {
		t.Fatalf("actual import-apply: %v", err)
	}
	verifyCfg, err := loadVerifyConfig(getenv)
	if err != nil {
		t.Fatal(err)
	}
	var originPolicy, originScope, originEvidence, originProof []byte
	if err := db.inspect.QueryRow(ctx, `SELECT o.policy_hash,o.source_scope_hash,o.evidence,o.proof FROM verified_import_origins o JOIN clients c ON c.id=o.client_id WHERE c.telegram_id=42`).Scan(&originPolicy, &originScope, &originEvidence, &originProof); err != nil {
		t.Fatal(err)
	}
	if len(originEvidence) == 0 || len(originProof) == 0 {
		t.Fatal("verified import origin lacks evidence or proof")
	}
	assertImportRuntimeCounts(t, db, 1, 0, 0)
	if n, err := db.store.SeedPaymentChecks(ctx, 9); err != nil || n != 0 {
		t.Fatalf("admission alone must not seed runtime reads: count=%d err=%v", n, err)
	}

	f.env["IMPORTED_RUNTIME_ENABLED"], f.env["IMPORTED_RUNTIME_REVISION"] = "true", "1"
	getsBeforeConfig := f.GETs.Load()
	runtimeCfg, err := loadImportedRuntimeConfig(getenv, true, true)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := verifyCfg.policy.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if f.GETs.Load() != getsBeforeConfig || runtimeCfg.BotID != 9 || runtimeCfg.PolicyHash != fmt.Sprintf("%x", originPolicy) || runtimeCfg.SourceScopeHash != fmt.Sprintf("%x", originScope) || runtimeCfg.PolicyHash != fingerprint {
		t.Fatalf("runtime config did not preserve exact verified source policy: cfg=%+v GETs=%d->%d", runtimeCfg, getsBeforeConfig, f.GETs.Load())
	}
	if err := fenceImportedRuntimeStartup(ctx, db.store, runtimeCfg); err != nil {
		t.Fatalf("startup must establish disabled effect gate: %v", err)
	}
	assertImportRuntimeRows(t, db, "admission", 1, true, runtimeCfg.PolicyHash, runtimeCfg.SourceScopeHash)
	assertImportRuntimeCounts(t, db, 1, 0, 0)
	if err := installImportedRuntimePermission(ctx, db.store, 42, runtimeCfg); err == nil {
		t.Fatal("runtime install accepted a bot other than the verified configured bot")
	}
	assertImportRuntimeCounts(t, db, 1, 0, 0)
	if err := installImportedRuntimePermission(ctx, db.store, 9, runtimeCfg); err != nil {
		t.Fatalf("install verified read-only runtime permission: %v", err)
	}
	assertImportRuntimeRows(t, db, "runtime", 1, true, runtimeCfg.PolicyHash, runtimeCfg.SourceScopeHash)
	assertImportRuntimeCounts(t, db, 1, 1, 0)
	if n, err := db.store.SeedPaymentChecks(ctx, 9); err != nil || n != 1 {
		t.Fatalf("runtime permission did not independently enable reads: count=%d err=%v", n, err)
	}

	// Run the real payment worker against the fixture's native GET-only Ninja
	// endpoint. Imported-baseline proof suppresses notification and has no
	// activation or provider-write side effects.
	paymentCfg := payments.Config{Enabled: true, TelegramEnabled: true,
		BaseURL: f.env["INVOICE_NINJA_BASE_URL"], Token: f.env["INVOICE_NINJA_API_TOKEN"],
		WebhookSecret: "synthetic-runtime-webhook-secret-0123456789", CurrencyID: "1",
		Workers: 1, PageSize: 50, MaxPages: 20, PollInterval: 100 * time.Millisecond,
		RecheckInterval: 30 * time.Second, HTTPTimeout: time.Second}
	worker, err := payments.New(db.store, verifyCfgProviders(t, verifyCfg), 9, []int64{9001}, paymentCfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	done := make(chan error, 1)
	go func() { done <- worker.Run(runCtx) }()
	deadline := time.Now().Add(7 * time.Second)
	for time.Now().Before(deadline) {
		var confirmations, checks int
		if err := db.inspect.QueryRow(ctx, `SELECT count(*) FROM payment_confirmations WHERE invoice_id='invoice-42'`).Scan(&confirmations); err != nil {
			cancel()
			t.Fatal(err)
		}
		if err := db.inspect.QueryRow(ctx, `SELECT count(*) FROM payment_checks WHERE bot_id=9 AND last_checked_at IS NOT NULL AND last_error_code=0`).Scan(&checks); err != nil {
			cancel()
			t.Fatal(err)
		}
		if confirmations == 1 && checks == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("payment worker: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("payment worker did not stop after cancellation")
	}
	if f.GETs.Load() <= getsBeforeConfig {
		t.Fatal("real runtime worker did not perform source GETs")
	}
	assertImportedBaselinePayment(t, db)
	assertImportedOriginUnchanged(t, db, originPolicy, originScope, originEvidence, originProof)
	getsAfterWorker := f.GETs.Load()

	// Only this test's explicit API call enables effects. Startup fences it on
	// the next run while leaving the exact runtime read permission unchanged.
	if err := db.store.SyncImportedEffectPermission(ctx, 9, 1, runtimeCfg.PolicyHash, runtimeCfg.SourceScopeHash, true); err != nil {
		t.Fatal(err)
	}
	if err := fenceImportedRuntimeStartup(ctx, db.store, runtimeCfg); err != nil {
		t.Fatal(err)
	}
	assertImportRuntimeRows(t, db, "effects", 2, false, runtimeCfg.PolicyHash, runtimeCfg.SourceScopeHash)
	assertImportRuntimeRows(t, db, "runtime", 1, true, runtimeCfg.PolicyHash, runtimeCfg.SourceScopeHash)

	// Replacement with the default-off configuration revokes runtime reads,
	// but does not change admission, effects, immutable origin, or history.
	f.env["IMPORTED_RUNTIME_ENABLED"], f.env["IMPORTED_RUNTIME_REVISION"] = "false", ""
	offCfg, err := loadImportedRuntimeConfig(getenv, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := fenceImportedRuntimeStartup(ctx, db.store, offCfg); err != nil {
		t.Fatal(err)
	}
	if err := fenceImportedRuntimeStartup(ctx, db.store, offCfg); err != nil {
		t.Fatal(err)
	}
	assertImportRuntimeRows(t, db, "runtime", 2, false, runtimeCfg.PolicyHash, runtimeCfg.SourceScopeHash)
	assertImportRuntimeRows(t, db, "effects", 2, false, runtimeCfg.PolicyHash, runtimeCfg.SourceScopeHash)
	assertImportRuntimeRows(t, db, "admission", 1, true, runtimeCfg.PolicyHash, runtimeCfg.SourceScopeHash)
	if err := installImportedRuntimePermission(ctx, db.store, 9, runtimeCfg); err == nil {
		t.Fatal("stale runtime revision unexpectedly re-enabled a revoked permission")
	}
	newCfg := runtimeCfg
	newCfg.Revision = 3
	if err := installImportedRuntimePermission(ctx, db.store, 9, newCfg); err != nil {
		t.Fatalf("explicit newer runtime request: %v", err)
	}
	assertImportRuntimeRows(t, db, "runtime", 3, true, runtimeCfg.PolicyHash, runtimeCfg.SourceScopeHash)
	assertImportRuntimeRows(t, db, "effects", 2, false, runtimeCfg.PolicyHash, runtimeCfg.SourceScopeHash)
	var guardEpochAfter int64
	var guardFreeAfter bool
	if err := db.inspect.QueryRow(ctx, `SELECT epoch,operation_id IS NULL AND step IS NULL FROM activation_write_guard WHERE id=1`).Scan(&guardEpochAfter, &guardFreeAfter); err != nil || guardEpochAfter != guardEpochBefore || !guardFreeAfter {
		t.Fatalf("permission changes altered unrelated activation guard: before=(%d,%v) after=(%d,%v) err=%v", guardEpochBefore, guardFreeBefore, guardEpochAfter, guardFreeAfter, err)
	}
	if f.GETs.Load() != getsAfterWorker {
		t.Fatalf("DB-only permission workflows performed source GETs: worker baseline=%d final=%d", getsAfterWorker, f.GETs.Load())
	}
	assertImportedOriginUnchanged(t, db, originPolicy, originScope, originEvidence, originProof)
	assertImportedBaselinePayment(t, db)
}

func verifyCfgProviders(t *testing.T, cfg verifyConfig) payments.Reader {
	t.Helper()
	providers, err := newVerifyProviders(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return providers.Payments
}

func assertImportRuntimeCounts(t *testing.T, db *importApplyPostgresSchema, admission, runtime, effects int) {
	t.Helper()
	for _, item := range []struct {
		table string
		want  int
	}{{"import_admission_policy", admission}, {"imported_runtime_permissions", runtime}, {"imported_effect_permissions", effects}} {
		var got int
		if err := db.inspect.QueryRow(db.ctx, `SELECT count(*) FROM `+item.table).Scan(&got); err != nil || got != item.want {
			t.Fatalf("%s rows=%d want=%d err=%v", item.table, got, item.want, err)
		}
	}
}

func assertImportRuntimeRows(t *testing.T, db *importApplyPostgresSchema, kind string, revision int64, enabled bool, policy, scope string) {
	t.Helper()
	table := map[string]string{"admission": "import_admission_policy", "runtime": "imported_runtime_permissions", "effects": "imported_effect_permissions"}[kind]
	if table == "" {
		t.Fatalf("unknown imported permission kind %q", kind)
	}
	var gotRevision int64
	var gotEnabled bool
	var gotPolicy, gotScope []byte
	if err := db.inspect.QueryRow(db.ctx, `SELECT revision,enabled,policy_hash,source_scope_hash FROM `+table+` WHERE bot_id=9`).Scan(&gotRevision, &gotEnabled, &gotPolicy, &gotScope); err != nil {
		t.Fatalf("read %s permission: %v", kind, err)
	}
	if gotRevision != revision || gotEnabled != enabled || fmt.Sprintf("%x", gotPolicy) != policy || fmt.Sprintf("%x", gotScope) != scope {
		t.Fatalf("%s permission=(%d,%t,%x,%x), want=(%d,%t,%s,%s)", kind, gotRevision, gotEnabled, gotPolicy, gotScope, revision, enabled, policy, scope)
	}
}

func assertImportedOriginUnchanged(t *testing.T, db *importApplyPostgresSchema, policy, scope, evidence, proof []byte) {
	t.Helper()
	var actualPolicy, actualScope, actualEvidence, actualProof []byte
	if err := db.inspect.QueryRow(db.ctx, `SELECT o.policy_hash,o.source_scope_hash,o.evidence,o.proof FROM verified_import_origins o JOIN clients c ON c.id=o.client_id WHERE c.telegram_id=42`).Scan(&actualPolicy, &actualScope, &actualEvidence, &actualProof); err != nil {
		t.Fatal(err)
	}
	if string(actualPolicy) != string(policy) || string(actualScope) != string(scope) || string(actualEvidence) != string(evidence) || string(actualProof) != string(proof) {
		t.Fatal("read-only runtime workflow mutated immutable imported origin evidence")
	}
}

func assertImportedBaselinePayment(t *testing.T, db *importApplyPostgresSchema) {
	t.Helper()
	var valid bool
	if err := db.inspect.QueryRow(db.ctx, `SELECT (SELECT count(*)=1 FROM payment_confirmations pc JOIN verified_import_origins io ON io.client_id=pc.client_id
		WHERE pc.invoice_id='invoice-42' AND pc.client_id=(SELECT id FROM clients WHERE telegram_id=42)
		AND pc.proof_hash=decode(io.evidence->>'proof_hash','hex') AND NOT pc.review_required AND pc.review_code=0)
		AND (SELECT count(*)=1 FROM payment_checks WHERE bot_id=9 AND last_error_code=0 AND review_code=0)
		AND (SELECT count(*)=0 FROM outbox) AND (SELECT count(*)=0 FROM operations)
		AND (SELECT count(*)=0 FROM activation_sui_bindings) AND (SELECT count(*)=0 FROM configuration_requests)
		AND (SELECT first_invoice_id IS NULL AND current_invoice_id='invoice-42' FROM subscriptions)
		AND (SELECT count(*)=0 FROM payment_confirmations WHERE invoice_id<>'invoice-42')`).Scan(&valid); err != nil || !valid {
		t.Fatalf("imported payment baseline produced unexpected effects or lost proof: valid=%v err=%v", valid, err)
	}
}
