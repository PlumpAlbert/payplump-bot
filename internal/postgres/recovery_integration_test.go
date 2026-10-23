//go:build integration

package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/lifecycle"
)

func recoveryCount(report RecoveryReport, kind, outcome string) int64 {
	for _, count := range report.Counts {
		if count.Kind == kind && count.Outcome == outcome {
			return count.Count
		}
	}
	return 0
}

func TestRecoverySnapshotReportsInvoiceOverdueWithoutLeakingReceipts(t *testing.T) {
	s, ctx := invoiceLifecycleTestStore(t)
	claim, snapshot, _, botID := seedOverdueOperation(t, s, ctx, []int64{101})

	pending, err := s.RecoverySnapshot(ctx, claim.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Operation == nil || pending.Operation.Kind != lifecycle.OverdueOperationKind || pending.Operation.ReviewCode != 0 || recoveryCount(pending, lifecycle.OverdueOperationKind, "running") != 1 {
		t.Fatalf("pending overdue operation missing from recovery report: %+v", pending)
	}
	if pending.Guard.OperationID != nil || pending.Guard.Step != nil {
		t.Fatalf("free guard should have no owner or step: %+v", pending.Guard)
	}

	request := json.RawMessage(`{"before":{"id":"recurring-synthetic"},"private":"SYNTHETIC_OVERDUE_PAYLOAD"}`)
	today := time.Now().In(time.Local).Format(time.DateOnly)
	if authorized, err := s.AuthorizeOverdueWrite(ctx, claim, botID, snapshot, lifecycle.OverduePauseRecurring, request, today, []int64{101}); err != nil || !authorized {
		t.Fatalf("prepare known overdue receipt: authorized=%v err=%v", authorized, err)
	}
	guarded, err := s.RecoverySnapshot(ctx, claim.ID)
	if err != nil || guarded.Guard.OperationID == nil || *guarded.Guard.OperationID != claim.ID || guarded.Guard.Step == nil || *guarded.Guard.Step != string(lifecycle.OverduePauseRecurring) {
		t.Fatalf("known overdue guard step missing from recovery report: guard=%+v err=%v", guarded.Guard, err)
	}
	externalID := snapshot.RecurringID
	if externalID == "" {
		t.Fatal("overdue fixture omitted the recurring identity")
	}
	if err := s.ConfirmOverdueStep(ctx, claim, lifecycle.OverduePauseRecurring, externalID); err != nil {
		t.Fatal(err)
	}
	withReceipt, err := s.RecoverySnapshot(ctx, claim.ID)
	if err != nil || withReceipt.UnresolvedKnownReceipts != 1 {
		t.Fatalf("overdue receipt missing from recovery report: count=%d err=%v", withReceipt.UnresolvedKnownReceipts, err)
	}
	encoded, err := json.Marshal(withReceipt)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"invoice-current", externalID, "SYNTHETIC_OVERDUE_PAYLOAD", "recurring-synthetic"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("recovery report leaked private overdue data %q", private)
		}
	}

	const reviewCode = 731
	if _, err := s.pool.Exec(ctx, `UPDATE invoice_overdue_actions SET outcome='review',review_code=$2,finished_at=clock_timestamp() WHERE operation_id=$1`, claim.ID, reviewCode); err != nil {
		t.Fatal(err)
	}
	unknownHash := sha256.Sum256([]byte("synthetic unknown guard step"))
	if _, err := s.pool.Exec(ctx, `INSERT INTO external_write_intents(operation_id,step,origin,request_hash,dispatch_attempt) VALUES($1,'future_guard_step','lifecycle',$2,1)`, claim.ID, unknownHash[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE activation_write_guard SET epoch=epoch+1,operation_id=$1,step='future_guard_step' WHERE id=1`, claim.ID); err != nil {
		t.Fatal(err)
	}
	review, err := s.RecoverySnapshot(ctx, claim.ID)
	if err != nil {
		t.Fatal(err)
	}
	if review.Operation == nil || review.Operation.Kind != lifecycle.OverdueOperationKind || review.Operation.ReviewCode != reviewCode || recoveryCount(review, lifecycle.OverdueOperationKind, "running") != 1 {
		t.Fatalf("overdue review not reported: %+v", review.Operation)
	}
	if review.UnresolvedKnownReceipts != 1 || review.Guard.Step == nil || *review.Guard.Step != "other" {
		t.Fatalf("overdue receipt or unknown guard missing: receipts=%d guard=%+v", review.UnresolvedKnownReceipts, review.Guard)
	}
}

func recoveryStoreThrough(t *testing.T, last string) (*Store, context.Context) {
	t.Helper()
	s, ctx := unmigratedStore(t)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `CREATE TABLE schema_migrations (version text PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if name > last {
			break
		}
		data, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, string(data)); err != nil {
			t.Fatalf("apply legacy migration %s: %v", name, err)
		}
		checksum := fmt.Sprintf("%x", sha256.Sum256(data))
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version,checksum) VALUES($1,$2)`, name, checksum); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return s, ctx
}

func TestRecoverySnapshotSupportsSchemasWithoutOverdueMigration(t *testing.T) {
	for _, last := range []string{"009_protected_configuration.sql", "010_shared_external_dispatch.sql"} {
		t.Run(last, func(t *testing.T) {
			s, ctx := recoveryStoreThrough(t, last)
			report, err := s.RecoverySnapshot(ctx, 0)
			if err != nil {
				t.Fatalf("legacy schema %s was not reported safely: %v", last, err)
			}
			if report.FeatureVersion != "009_protected_configuration.sql" || report.Operation != nil || report.Guard.OperationID != nil || report.Guard.Step != nil {
				t.Fatalf("unexpected legacy recovery report for %s: %+v", last, report)
			}
		})
	}
}
