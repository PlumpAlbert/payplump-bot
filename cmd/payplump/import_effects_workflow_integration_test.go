//go:build integration

package main

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/postgres"
)

// Proves the explicit effects gate using the real CLI and a disposable schema;
// the synthetic source server remains strictly GET-only throughout.
func TestImportedEffectsStartupWorkflowRealPostgres(t *testing.T) {
	f := newImportApplyFixture(t)
	db := newImportApplyPostgresSchema(t)
	ctx := db.ctx
	f.env["DATABASE_URL"], f.env["DB_MAX_CONNS"] = db.dbURL, "4"
	if _, err := db.inspect.Exec(ctx, `INSERT INTO telegram_bots(bot_id) VALUES(9) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	var epoch int64
	var guardFree bool
	if err := db.inspect.QueryRow(ctx, `SELECT epoch,operation_id IS NULL AND step IS NULL FROM activation_write_guard WHERE id=1`).Scan(&epoch, &guardFree); err != nil || !guardFree {
		t.Fatalf("activation guard not free: epoch=%d free=%v err=%v", epoch, guardFree, err)
	}

	getenv := func(k string) string { return f.env[k] }
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
		t.Fatal("admission did not preserve verified source evidence")
	}
	assertImportRuntimeCounts(t, db, 1, 0, 0)
	gets := f.GETs.Load()
	if gets == 0 {
		t.Fatal("CLI did not verify source using GETs")
	}

	f.env["IMPORTED_RUNTIME_ENABLED"], f.env["IMPORTED_RUNTIME_REVISION"] = "true", "1"
	readCfg, err := loadImportedRuntimeConfig(getenv, true, true)
	if err != nil {
		t.Fatal(err)
	}
	policyHash, scopeHash := fmt.Sprintf("%x", originPolicy), fmt.Sprintf("%x", originScope)
	fingerprint, err := verifyCfg.policy.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if readCfg.PolicyHash != policyHash || readCfg.PolicyHash != fingerprint || readCfg.SourceScopeHash != scopeHash || f.GETs.Load() != gets {
		t.Fatalf("read config changed verified policy or made source calls: cfg=%+v", readCfg)
	}
	if err := fenceImportedRuntimeStartup(ctx, db.store, readCfg); err != nil {
		t.Fatal(err)
	}
	if err := installImportedRuntimePermission(ctx, db.store, 9, readCfg); err != nil {
		t.Fatal(err)
	}
	if err := installImportedEffectPermission(ctx, db.store, 9, readCfg); err != nil {
		t.Fatalf("effects-off install must be a no-op: %v", err)
	}
	assertImportRuntimeRows(t, db, "runtime", 1, true, policyHash, scopeHash)
	assertImportRuntimeCounts(t, db, 1, 1, 0)

	f.env["IMPORTED_EFFECTS_ENABLED"], f.env["IMPORTED_EFFECTS_REVISION"] = "true", "1"
	effectsCfg, err := loadImportedRuntimeConfig(getenv, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if !effectsCfg.Enabled || !effectsCfg.EffectsEnabled || effectsCfg.EffectsRevision != 1 || effectsCfg.PolicyHash != readCfg.PolicyHash || effectsCfg.SourceScopeHash != readCfg.SourceScopeHash {
		t.Fatalf("effects config changed source policy: read=%+v effects=%+v", readCfg, effectsCfg)
	}
	if err := fenceImportedRuntimeStartup(ctx, db.store, effectsCfg); err != nil {
		t.Fatal(err)
	}
	if err := installImportedEffectPermission(ctx, db.store, 42, effectsCfg); err == nil {
		t.Fatal("effects permission accepted mismatched bot")
	}
	assertImportRuntimeCounts(t, db, 1, 1, 0)
	assertImportRuntimeRows(t, db, "runtime", 1, true, policyHash, scopeHash)
	if err := installImportedRuntimePermission(ctx, db.store, 9, effectsCfg); err != nil {
		t.Fatal(err)
	}
	if err := installImportedEffectPermission(ctx, db.store, 9, effectsCfg); err != nil {
		t.Fatal(err)
	}
	assertImportRuntimeRows(t, db, "runtime", 1, true, policyHash, scopeHash)
	assertImportRuntimeRows(t, db, "effects", 1, true, policyHash, scopeHash)
	if err := fenceImportedRuntimeStartup(ctx, db.store, effectsCfg); err != nil {
		t.Fatal(err)
	}
	if err := installImportedRuntimePermission(ctx, db.store, 9, effectsCfg); err != nil {
		t.Fatal(err)
	}
	if err := installImportedEffectPermission(ctx, db.store, 9, effectsCfg); err != nil {
		t.Fatal(err)
	}
	assertImportRuntimeRows(t, db, "runtime", 1, true, policyHash, scopeHash)
	assertImportRuntimeRows(t, db, "effects", 1, true, policyHash, scopeHash)

	// Read-only replacement fences only effects; an old revision cannot re-enable it.
	f.env["IMPORTED_EFFECTS_ENABLED"], f.env["IMPORTED_EFFECTS_REVISION"] = "false", ""
	offCfg, err := loadImportedRuntimeConfig(getenv, true, true)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := fenceImportedRuntimeStartup(ctx, db.store, offCfg); err != nil {
			t.Fatal(err)
		}
	}
	assertImportRuntimeRows(t, db, "runtime", 1, true, policyHash, scopeHash)
	assertImportRuntimeRows(t, db, "effects", 2, false, policyHash, scopeHash)
	if err := fenceImportedRuntimeStartup(ctx, db.store, effectsCfg); err != nil {
		t.Fatal(err)
	}
	if err := installImportedEffectPermission(ctx, db.store, 9, effectsCfg); err == nil {
		t.Fatal("stale effects revision unexpectedly re-enabled permission")
	}
	assertImportRuntimeRows(t, db, "effects", 2, false, policyHash, scopeHash)

	f.env["IMPORTED_EFFECTS_ENABLED"], f.env["IMPORTED_EFFECTS_REVISION"] = "true", "3"
	nextEffectsCfg, err := loadImportedRuntimeConfig(getenv, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := fenceImportedRuntimeStartup(ctx, db.store, nextEffectsCfg); err != nil {
		t.Fatal(err)
	}
	if err := installImportedRuntimePermission(ctx, db.store, 9, nextEffectsCfg); err != nil {
		t.Fatal(err)
	}
	if err := installImportedEffectPermission(ctx, db.store, 9, nextEffectsCfg); err != nil {
		t.Fatal(err)
	}
	assertImportRuntimeRows(t, db, "runtime", 1, true, policyHash, scopeHash)
	assertImportRuntimeRows(t, db, "effects", 3, true, policyHash, scopeHash)

	// Both default-off gates require separate newer revisions to rearm.
	f.env["IMPORTED_RUNTIME_ENABLED"], f.env["IMPORTED_RUNTIME_REVISION"] = "false", ""
	f.env["IMPORTED_EFFECTS_ENABLED"], f.env["IMPORTED_EFFECTS_REVISION"] = "false", ""
	disabledCfg, err := loadImportedRuntimeConfig(getenv, true, true)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := fenceImportedRuntimeStartup(ctx, db.store, disabledCfg); err != nil {
			t.Fatal(err)
		}
	}
	assertImportRuntimeRows(t, db, "runtime", 2, false, policyHash, scopeHash)
	assertImportRuntimeRows(t, db, "effects", 4, false, policyHash, scopeHash)
	if err := installImportedRuntimePermission(ctx, db.store, 9, readCfg); err == nil {
		t.Fatal("stale runtime revision unexpectedly re-enabled permission")
	}
	if err := installImportedEffectPermission(ctx, db.store, 9, nextEffectsCfg); err == nil {
		t.Fatal("stale effects revision unexpectedly re-enabled permission")
	}
	assertImportRuntimeRows(t, db, "runtime", 2, false, policyHash, scopeHash)
	assertImportRuntimeRows(t, db, "effects", 4, false, policyHash, scopeHash)

	f.env["IMPORTED_RUNTIME_ENABLED"], f.env["IMPORTED_RUNTIME_REVISION"] = "true", "3"
	f.env["IMPORTED_EFFECTS_ENABLED"], f.env["IMPORTED_EFFECTS_REVISION"] = "true", "5"
	rearmedCfg, err := loadImportedRuntimeConfig(getenv, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := fenceImportedRuntimeStartup(ctx, db.store, rearmedCfg); err != nil {
		t.Fatal(err)
	}
	if err := installImportedRuntimePermission(ctx, db.store, 9, rearmedCfg); err != nil {
		t.Fatal(err)
	}
	if err := installImportedEffectPermission(ctx, db.store, 9, rearmedCfg); err != nil {
		t.Fatal(err)
	}
	assertImportRuntimeRows(t, db, "runtime", 3, true, policyHash, scopeHash)
	assertImportRuntimeRows(t, db, "effects", 5, true, policyHash, scopeHash)
	assertImportRuntimeRows(t, db, "admission", 1, true, policyHash, scopeHash)
	assertImportRuntimeCounts(t, db, 1, 1, 1)
	assertImportedOriginUnchanged(t, db, originPolicy, originScope, originEvidence, originProof)
	assertNoEffects(t, db)
	var finalEpoch int64
	var finalFree bool
	if err := db.inspect.QueryRow(ctx, `SELECT epoch,operation_id IS NULL AND step IS NULL FROM activation_write_guard WHERE id=1`).Scan(&finalEpoch, &finalFree); err != nil || finalEpoch != epoch || !finalFree {
		t.Fatalf("permission workflow changed activation guard: before=%d after=(%d,%v) err=%v", epoch, finalEpoch, finalFree, err)
	}
	if f.GETs.Load() != gets {
		t.Fatalf("DB-only workflow made source calls: GETs=%d->%d", gets, f.GETs.Load())
	}
}

func assertNoEffects(t *testing.T, db *importApplyPostgresSchema) {
	t.Helper()
	var importedSubscription bool
	if err := db.inspect.QueryRow(db.ctx, `SELECT count(*)=1 AND bool_and(first_invoice_id IS NULL AND current_invoice_id='invoice-42') FROM subscriptions`).Scan(&importedSubscription); err != nil || !importedSubscription {
		t.Errorf("effects permission changed imported subscription baseline: valid=%v err=%v", importedSubscription, err)
	}
	for _, table := range []string{"operations", "outbox", "payment_checks", "payment_confirmations", "activation_sui_bindings", "invoice_lifecycle_checks", "invoice_lifecycle_invoices", "invoice_overdue_actions", "invoice_renewal_actions", "configuration_requests", "configuration_policies"} {
		var count int
		if err := db.inspect.QueryRow(db.ctx, `SELECT count(*) FROM `+table).Scan(&count); err != nil || count != 0 {
			t.Errorf("unexpected managed rows in %s: count=%d err=%v", table, count, err)
		}
	}
}
