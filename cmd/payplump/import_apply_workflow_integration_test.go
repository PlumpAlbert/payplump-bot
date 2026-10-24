//go:build integration

package main

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/postgres"
	"github.com/jackc/pgx/v5"
)

type importApplyPostgresSchema struct {
	ctx     context.Context
	inspect *pgx.Conn
	store   *postgres.Store
	dbURL   string
}

// This helper accepts only CI's explicitly supplied disposable local PG17 URL.
// It creates a uniquely named schema, proves startup search_path is exactly
// that schema before opening the app Store or invoking Migrate, and drops only
// that schema during cleanup.
func newImportApplyPostgresSchema(t *testing.T) *importApplyPostgresSchema {
	t.Helper()
	adminURL := os.Getenv("TEST_DATABASE_URL")
	parsed, err := url.Parse(adminURL)
	if err != nil || parsed == nil {
		t.Fatal("invalid explicit TEST_DATABASE_URL")
	}
	query := parsed.Query()
	if adminURL == "" || !isLoopbackPostgresURL(parsed) || parsed.Fragment != "" || len(query) != 1 || len(query["sslmode"]) != 1 || query.Get("sslmode") != "disable" || os.Getenv("PGOPTIONS") != "" {
		t.Fatal("integration requires TEST_DATABASE_URL on loopback with only sslmode=disable and no PGOPTIONS")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect to disposable test PostgreSQL: %v", err)
	}
	var version int
	if err := admin.QueryRow(ctx, `SHOW server_version_num`).Scan(&version); err != nil || version < 170000 || version >= 180000 {
		_ = admin.Close(context.Background())
		t.Fatalf("integration requires PostgreSQL 17, got %d (err=%v)", version, err)
	}
	schema := fmt.Sprintf("test_cli_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
		_ = admin.Close(context.Background())
		t.Fatal(err)
	}
	q := parsed.Query()
	q.Set("search_path", schema)
	parsed.RawQuery = q.Encode()
	dbURL := parsed.String()
	inspect, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		_, dropErr := admin.Exec(cleanup, `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
		stop()
		_ = admin.Close(context.Background())
		if dropErr != nil {
			t.Errorf("drop isolated test schema after connect failure: %v", dropErr)
		}
		t.Fatal(err)
	}
	var currentSchema, searchPath string
	if err := inspect.QueryRow(ctx, `SELECT current_schema(), current_setting('search_path')`).Scan(&currentSchema, &searchPath); err != nil || currentSchema != schema || searchPath != schema {
		_ = inspect.Close(context.Background())
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		_, dropErr := admin.Exec(cleanup, `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
		stop()
		_ = admin.Close(context.Background())
		if dropErr != nil {
			t.Errorf("drop isolated test schema after scope check failure: %v", dropErr)
		}
		t.Fatalf("refusing migration outside exact isolated search_path: current_schema=%q search_path=%q err=%v", currentSchema, searchPath, err)
	}

	// No migration occurs until the direct scoped connection has passed both
	// current_schema and exact search_path checks above.
	store, err := postgres.Open(ctx, dbURL, 4)
	if err != nil {
		_ = inspect.Close(context.Background())
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		_, dropErr := admin.Exec(cleanup, `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
		stop()
		_ = admin.Close(context.Background())
		if dropErr != nil {
			t.Errorf("drop isolated test schema after app connection failure: %v", dropErr)
		}
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		store.Close()
		_ = inspect.Close(context.Background())
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		_, dropErr := admin.Exec(cleanup, `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
		stop()
		_ = admin.Close(context.Background())
		if dropErr != nil {
			t.Errorf("drop isolated test schema after migration failure: %v", dropErr)
		}
		t.Fatalf("migrate isolated test schema: %v", err)
	}
	t.Cleanup(func() {
		store.Close()
		closeCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := inspect.Close(closeCtx); err != nil {
			t.Errorf("close isolated schema inspector: %v", err)
		}
		if _, err := admin.Exec(closeCtx, `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`); err != nil {
			t.Errorf("drop isolated test schema: %v", err)
		}
		if err := admin.Close(closeCtx); err != nil {
			t.Errorf("close disposable PostgreSQL admin connection: %v", err)
		}
	})
	return &importApplyPostgresSchema{ctx: ctx, inspect: inspect, store: store, dbURL: dbURL}
}

func TestImportApplyWorkflowRealPostgres(t *testing.T) {
	t.Run("explicit_enable_and_idempotent_replay", func(t *testing.T) {
		f := newImportApplyFixture(t)
		db := newImportApplyPostgresSchema(t)
		f.env["DATABASE_URL"] = db.dbURL
		f.env["DB_MAX_CONNS"] = "4"
		if _, err := db.inspect.Exec(db.ctx, `INSERT INTO telegram_bots(bot_id) VALUES(9) ON CONFLICT DO NOTHING`); err != nil {
			t.Fatal(err)
		}
		var existingPolicies int
		if err := db.inspect.QueryRow(db.ctx, `SELECT count(*) FROM import_admission_policy`).Scan(&existingPolicies); err != nil || existingPolicies != 0 {
			t.Fatalf("expected empty admission policy before CLI, count=%d err=%v", existingPolicies, err)
		}
		getenv := func(key string) string { return f.env[key] }
		var opens int
		opener := func(ctx context.Context, raw string, max int32) (importApplyStore, error) {
			opens++
			return postgres.Open(ctx, raw, max)
		}
		args := []string{"--file", f.manifestPath, "--expected-hash", f.planHash, "--admission-revision", "1", "--apply", "--enable-admission"}
		var out bytes.Buffer
		if err := importApplyWith(args, &out, getenv, newVerifyProviders, func() time.Time { return f.now }, opener); err != nil {
			t.Fatalf("actual import-apply failed: %v", err)
		}
		if opens != 1 || f.GETs.Load() == 0 || !strings.Contains(out.String(), `"state":"applied"`) {
			t.Fatalf("command did not fresh-verify then apply through real PostgreSQL: opens=%d GETs=%d output=%s", opens, f.GETs.Load(), out.String())
		}
		var clients, subs, origins, batches int
		for table, target := range map[string]*int{"clients": &clients, "subscriptions": &subs, "verified_import_origins": &origins, "import_batches": &batches} {
			if err := db.inspect.QueryRow(db.ctx, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()).Scan(target); err != nil {
				t.Fatal(err)
			}
		}
		if clients != 1 || subs != 1 || origins != 1 || batches != 1 {
			t.Fatalf("admission rows clients=%d subscriptions=%d origins=%d batches=%d", clients, subs, origins, batches)
		}
		cfg, err := loadVerifyConfig(getenv)
		if err != nil {
			t.Fatal(err)
		}
		var originCustomer, originCurrent, paidAnchor, currency string
		var amount string
		var storedScope, evidence, proof []byte
		if err := db.inspect.QueryRow(db.ctx, `SELECT customer_id,current_invoice_id,paid_anchor_invoice_id,amount::text,currency,source_scope_hash,evidence,proof FROM verified_import_origins`).Scan(&originCustomer, &originCurrent, &paidAnchor, &amount, &currency, &storedScope, &evidence, &proof); err != nil {
			t.Fatal(err)
		}
		if originCustomer != "customer-42" || originCurrent != "invoice-42" || paidAnchor != "invoice-42" || amount != "100" || currency != "USD" || fmt.Sprintf("%x", storedScope) != cfg.policy.SourceScopeHash || len(evidence) == 0 || len(proof) == 0 {
			t.Fatalf("origin did not retain verified source proof/native settlement facts: customer=%q current=%q paid=%q amount=%q currency=%q", originCustomer, originCurrent, paidAnchor, amount, currency)
		}
		var chatID int64
		var customer, suiID *string
		if err := db.inspect.QueryRow(db.ctx, `SELECT chat_id,invoice_ninja_id,sui_id FROM clients WHERE telegram_id=42`).Scan(&chatID, &customer, &suiID); err != nil || chatID != 42 || customer == nil || *customer != "customer-42" || suiID == nil || *suiID != "42" {
			t.Fatalf("unexpected imported client identity: chat=%d customer=%v sui=%v err=%v", chatID, customer, suiID, err)
		}
		var state string
		var desired bool
		var currentInvoice, recurring, firstInvoice *string
		if err := db.inspect.QueryRow(db.ctx, `SELECT state,desired_access,current_invoice_id,recurring_id,first_invoice_id FROM subscriptions`).Scan(&state, &desired, &currentInvoice, &recurring, &firstInvoice); err != nil || state != "active" || !desired || currentInvoice == nil || *currentInvoice != "invoice-42" || recurring == nil || *recurring != "recurring-42" || firstInvoice != nil {
			t.Fatalf("unexpected imported subscription: %q %v %v %v %v err=%v", state, desired, currentInvoice, recurring, firstInvoice, err)
		}
		assertNoManagedSideEffects(t, db)

		beforeGETs := f.GETs.Load()
		out.Reset()
		if err := importApplyWith(args, &out, getenv, newVerifyProviders, func() time.Time { return f.now }, opener); err != nil {
			t.Fatalf("idempotent second real apply: %v", err)
		}
		if f.GETs.Load() <= beforeGETs || opens != 2 || !strings.Contains(out.String(), `"already_admitted":1`) || !strings.Contains(out.String(), `"admitted":0`) {
			t.Fatalf("second apply did not fresh-verify/idempotently admit: GETs %d->%d opens=%d output=%s", beforeGETs, f.GETs.Load(), opens, out.String())
		}
		if err := db.store.SyncImportAdmissionPolicy(db.ctx, 9, 2, func() string {
			var value []byte
			if err := db.inspect.QueryRow(db.ctx, `SELECT policy_hash FROM verified_import_origins LIMIT 1`).Scan(&value); err != nil {
				t.Fatal(err)
			}
			return fmt.Sprintf("%x", value)
		}(), func() string {
			var value []byte
			if err := db.inspect.QueryRow(db.ctx, `SELECT source_scope_hash FROM verified_import_origins LIMIT 1`).Scan(&value); err != nil {
				t.Fatal(err)
			}
			return fmt.Sprintf("%x", value)
		}(), false); err != nil {
			t.Fatal(err)
		}
		beforeGETs = f.GETs.Load()
		out.Reset()
		deniedArgs := []string{"--file", f.manifestPath, "--expected-hash", f.planHash, "--admission-revision", "2", "--apply"}
		if err := importApplyWith(deniedArgs, &out, getenv, newVerifyProviders, func() time.Time { return f.now }, opener); err == nil {
			t.Fatal("command bypassed disabled admission policy")
		}
		if f.GETs.Load() <= beforeGETs || opens != 3 {
			t.Fatalf("revoked-policy case skipped fresh verification or DB open: GETs=%d opens=%d", f.GETs.Load(), opens)
		}
	})

	t.Run("default_off_denies_empty_schema", func(t *testing.T) {
		f := newImportApplyFixture(t)
		db := newImportApplyPostgresSchema(t)
		f.env["DATABASE_URL"] = db.dbURL
		f.env["DB_MAX_CONNS"] = "4"
		// This explicit bot metadata is an API precondition for policy sync; no
		// admission policy or other application state is seeded by the test.
		if _, err := db.inspect.Exec(db.ctx, `INSERT INTO telegram_bots(bot_id) VALUES(9) ON CONFLICT DO NOTHING`); err != nil {
			t.Fatal(err)
		}
		getenv := func(key string) string { return f.env[key] }
		var opens int
		opener := func(ctx context.Context, raw string, max int32) (importApplyStore, error) {
			opens++
			return postgres.Open(ctx, raw, max)
		}
		args := []string{"--file", f.manifestPath, "--expected-hash", f.planHash, "--admission-revision", "1", "--apply"}
		var out bytes.Buffer
		beforeGETs := f.GETs.Load()
		err := importApplyWith(args, &out, getenv, newVerifyProviders, func() time.Time { return f.now }, opener)
		if err == nil {
			t.Fatal("default-off command applied import in a fresh schema with no admission policy")
		}
		if f.GETs.Load() <= beforeGETs || opens != 1 {
			t.Fatalf("default-off path did not fresh-verify then reach real PostgreSQL: GETs=%d opens=%d err=%v", f.GETs.Load(), opens, err)
		}
		assertNoAdmissionSideEffects(t, db)
	})
}

func assertNoAdmissionSideEffects(t *testing.T, db *importApplyPostgresSchema) {
	t.Helper()
	for _, table := range []string{"clients", "subscriptions", "verified_import_origins", "import_batches", "import_candidates_staging", "import_admission_policy", "imported_runtime_permissions", "imported_effect_permissions", "operations", "payment_checks", "payment_confirmations", "activation_sui_bindings", "invoice_lifecycle_checks", "invoice_lifecycle_invoices", "configuration_requests", "outbox"} {
		var count int
		if err := db.inspect.QueryRow(db.ctx, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()).Scan(&count); err != nil || count != 0 {
			t.Errorf("expected no rows in %s after denied admission, count=%d err=%v", table, count, err)
		}
	}
	assertActivationGuardFree(t, db)
}

func assertNoManagedSideEffects(t *testing.T, db *importApplyPostgresSchema) {
	t.Helper()
	for _, table := range []string{"operations", "outbox", "imported_runtime_permissions", "imported_effect_permissions", "payment_checks", "payment_confirmations", "activation_sui_bindings", "invoice_lifecycle_checks", "invoice_lifecycle_invoices", "configuration_requests"} {
		var count int
		if err := db.inspect.QueryRow(db.ctx, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()).Scan(&count); err != nil || count != 0 {
			t.Errorf("unexpected managed workflow rows in %s: count=%d err=%v", table, count, err)
		}
	}
	assertActivationGuardFree(t, db)
}

func assertActivationGuardFree(t *testing.T, db *importApplyPostgresSchema) {
	t.Helper()
	var occupied bool
	if err := db.inspect.QueryRow(db.ctx, `SELECT operation_id IS NOT NULL OR step IS NOT NULL FROM activation_write_guard WHERE id=1`).Scan(&occupied); err != nil || occupied {
		t.Errorf("unexpected activation write intent: occupied=%v err=%v", occupied, err)
	}
}

func isLoopbackPostgresURL(u *url.URL) bool {
	if u == nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return false
	}
	h := strings.Trim(u.Hostname(), "[]")
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}
