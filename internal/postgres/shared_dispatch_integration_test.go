//go:build integration

package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
)

// Install the exact pre-010 schema with the same immutable checksum ledger used
// by Store.Migrate, so the migration test exercises real legacy rows.
func applyMigrationsThrough009(t *testing.T, s *Store, ctx context.Context) {
	t.Helper()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	if _, err = tx.Exec(ctx, `CREATE TABLE schema_migrations (version text PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if name >= "010" {
			continue
		}
		data, e := migrations.ReadFile("migrations/" + name)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = tx.Exec(ctx, string(data)); e != nil {
			t.Fatalf("apply %s: %v", name, e)
		}
		if _, e = tx.Exec(ctx, `INSERT INTO schema_migrations(version,checksum) VALUES($1,$2)`, name, fmt.Sprintf("%x", sha256.Sum256(data))); e != nil {
			t.Fatal(e)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// seedLegacyActivationAccounts constructs payment state using only the schema
// available through migration 009. In particular it avoids Store.SeedPaymentChecks,
// whose current implementation also consults post-009 provenance tables.
func seedLegacyActivationAccounts(t *testing.T, s *Store, ctx context.Context, botID int64, start, count int) {
	t.Helper()
	ids := seedPaymentAccounts(t, s, ctx, botID, start, count)
	proofs := make([]*payments.Proof, count)
	for i := range count {
		proofs[i] = paymentProof(t, payments.Snapshot{
			InvoiceID:  fmt.Sprintf("invoice-%d", start+i),
			CustomerID: fmt.Sprintf("customer-%d", start+i),
		}, "2039-01-31", "125.25")
	}
	for _, id := range ids {
		paymentExec(t, s, ctx, `INSERT INTO payment_checks (client_id, bot_id) VALUES ($1, $2)`, id, botID)
	}
	hashes, evidence := make([][]byte, count), make([]string, count)
	for i, proof := range proofs {
		hashes[i], evidence[i] = proof.Hash[:], string(proof.JSON)
	}
	paymentExec(t, s, ctx, `WITH facts AS (
        SELECT * FROM unnest($1::bigint[], $2::bytea[], $3::text[]) AS f(client_id, hash, evidence)
    ), confirmed AS (
        INSERT INTO payment_confirmations (invoice_id, client_id, bot_id, customer_id, invoice_number,
            amount, currency_id, payment_date, next_invoice_date, proof_hash, evidence)
        SELECT s.current_invoice_id, c.id, $4, c.invoice_ninja_id, 'SYNTHETIC', 125.25, '1',
            '2039-01-31', '2039-02-28', f.hash, f.evidence::jsonb
        FROM facts f JOIN clients c ON c.id=f.client_id JOIN subscriptions s ON s.client_id=c.id
        RETURNING client_id, payment_date, next_invoice_date
    ) UPDATE subscriptions s SET payment_date=pc.payment_date, next_invoice_date=pc.next_invoice_date
        FROM confirmed pc WHERE s.client_id=pc.client_id`, ids, hashes, evidence, botID)
}

func TestSharedDispatchMigrationBackfillsUnknownActivationGuard(t *testing.T) {
	if !integrationDatabaseConfigured() {
		t.Skip("requires disposable PostgreSQL integration database")
	}
	s, setup := unmigratedStore(t)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(setup), 4*time.Minute)
	t.Cleanup(cancel)
	applyMigrationsThrough009(t, s, ctx)
	for _, table := range []string{"verified_import_origins", "imported_runtime_permissions"} {
		var absent bool
		if err := s.pool.QueryRow(ctx, `SELECT to_regclass($1) IS NULL`, table).Scan(&absent); err != nil || !absent {
			t.Fatalf("pre-010 schema unexpectedly contains %s: absent=%v err=%v", table, absent, err)
		}
	}
	seedLegacyActivationAccounts(t, s, ctx, 34200, 34200, 1)
	seedActivationBatch(t, s, ctx, 34200, activationAllow(34200, 1), 1)
	op, snapshot := claimOneActivation(t, s, ctx, 34200)
	payload := activationPayload(t, snapshot, activation.SUICreate)
	if granted, err := s.PrepareActivationWrite(ctx, op.Lease, snapshot, activation.SUICreate, payload, activationToday); err != nil || !granted {
		t.Fatalf("reservation granted=%v err=%v", granted, err)
	}
	var oldOp int64
	var oldStep string
	var oldEpoch int64
	if err := s.pool.QueryRow(ctx, `SELECT operation_id,step,epoch FROM activation_write_guard`).Scan(&oldOp, &oldStep, &oldEpoch); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT g.operation_id=$1 AND g.step=$2 AND g.epoch=$3 AND i.origin='activation' AND i.dispatch_attempt=1 AND i.request_hash=sha256(convert_to(st.request::text,'UTF8')) AND (SELECT count(*)=1 FROM external_write_intents) FROM activation_write_guard g JOIN external_write_intents i ON (i.operation_id,i.step)=(g.operation_id,g.step) JOIN activation_steps st ON (st.operation_id,st.step)=(g.operation_id,g.step)`, oldOp, oldStep, oldEpoch).Scan(&ok)
	if err != nil || !ok {
		t.Fatalf("legacy unknown guard/anchor was not preserved: ok=%v err=%v", ok, err)
	}
}

func TestSharedDispatchActivationAnchorSurvivesConfirmationAndCannotRearm(t *testing.T) {
	s, ctx := activationTestStore(t)
	seedActivationAccounts(t, s, ctx, 123, 34300, 1)
	seedActivationBatch(t, s, ctx, 123, activationAllow(34300, 1), 1)
	op, snapshot := claimOneActivation(t, s, ctx, 123)
	payload := activationPayload(t, snapshot, activation.SUICreate)
	if granted, err := s.PrepareActivationWrite(ctx, op.Lease, snapshot, activation.SUICreate, payload, activationToday); err != nil || !granted {
		t.Fatalf("reservation granted=%v err=%v", granted, err)
	}
	var anchored bool
	if err := s.pool.QueryRow(ctx, `SELECT i.origin='activation' AND i.dispatch_attempt=1 AND i.request_hash=sha256(convert_to(st.request::text,'UTF8')) AND g.operation_id=i.operation_id AND g.step=i.step FROM external_write_intents i JOIN activation_steps st USING(operation_id,step) CROSS JOIN activation_write_guard g`).Scan(&anchored); err != nil || !anchored {
		t.Fatalf("atomic anchor missing: %v", err)
	}
	if err := s.ConfirmActivationStep(ctx, op.Lease, activation.SUICreate, "77"); err != nil {
		t.Fatal(err)
	}
	var released, remains bool
	if err := s.pool.QueryRow(ctx, `SELECT operation_id IS NULL FROM activation_write_guard`).Scan(&released); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*)=1 FROM external_write_intents`).Scan(&remains); err != nil {
		t.Fatal(err)
	}
	if !released || !remains {
		t.Fatal("known confirmation must release the guard but retain its permanent anchor")
	}
	for _, sql := range []string{`UPDATE external_write_intents SET dispatch_attempt=dispatch_attempt+1 WHERE operation_id=$1`, `DELETE FROM external_write_intents WHERE operation_id=$1`} {
		if _, err := s.pool.Exec(ctx, sql, op.ID); err == nil {
			t.Fatal("permanent dispatch anchor could be edited or removed")
		}
	}
	// Fault injection only: remove the leaf, then prove its strict trigger cannot
	// recreate an already anchored transmission. No external call is made.
	if _, err := s.pool.Exec(ctx, `DELETE FROM activation_steps WHERE operation_id=$1 AND step=$2`, op.ID, activation.SUICreate); err != nil {
		t.Fatal(err)
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO activation_steps(operation_id,step,dispatch_attempt,request) VALUES($1,$2,1,$3)`, op.ID, activation.SUICreate, payload)
	if err == nil {
		t.Fatal("reinserting a deleted leaf unexpectedly rearmed an anchored dispatch")
	}
	var still bool
	if e := s.pool.QueryRow(ctx, `SELECT count(*)=1 FROM external_write_intents`).Scan(&still); e != nil || !still {
		t.Fatalf("anchor lost: %v", e)
	}
}

func TestSharedDispatchLifecycleOwnerBlocksActivation(t *testing.T) {
	s, ctx := activationTestStore(t)
	seedActivationAccounts(t, s, ctx, 123, 34400, 1)
	seedActivationBatch(t, s, ctx, 123, activationAllow(34400, 1), 1)
	op, snapshot := claimOneActivation(t, s, ctx, 123)
	var lifecycleID int64
	if err := s.pool.QueryRow(ctx, `INSERT INTO operations(client_id,operation_key,kind) SELECT id,'synthetic-lifecycle','subscription_overdue' FROM clients WHERE telegram_id=34400 RETURNING id`).Scan(&lifecycleID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO external_write_intents(operation_id,step,origin,request_hash,dispatch_attempt) VALUES($1,'notify','lifecycle',sha256(convert_to('{}','UTF8')),1)`, lifecycleID); err != nil {
		t.Fatal(err)
	}
	var epoch int64
	if err := s.pool.QueryRow(ctx, `UPDATE activation_write_guard SET operation_id=$1,step='notify',epoch=epoch+1 WHERE id=1 RETURNING epoch`, lifecycleID).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	fresh := loadActivation(t, s, ctx, op.Lease, 123)
	if !fresh.WriterBusy || fresh.WriteEpoch != epoch {
		t.Fatal("activation reader did not observe lifecycle owner and epoch")
	}
	if granted, err := s.PrepareActivationWrite(ctx, op.Lease, snapshot, activation.SUICreate, activationPayload(t, snapshot, activation.SUICreate), activationToday); granted || !errors.Is(err, activation.ErrGuardBusy) {
		t.Fatalf("stale activation permission granted=%v err=%v", granted, err)
	}
	var one bool
	if err := s.pool.QueryRow(ctx, `SELECT count(*)=1 AND bool_and(origin='lifecycle') FROM external_write_intents`).Scan(&one); err != nil || !one {
		t.Fatalf("unexpected intent set: %v", err)
	}
}

func integrationDatabaseConfigured() bool {
	return os.Getenv("TEST_DATABASE_URL") != "" || os.Getenv("PGHOST") != "" && os.Getenv("PGUSER") != "" && os.Getenv("PGDATABASE") != ""
}
