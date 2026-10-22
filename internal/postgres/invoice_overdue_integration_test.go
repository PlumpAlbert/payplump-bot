//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/lifecycle"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
)

func seedOverdueOperation(t *testing.T, s *Store, ctx context.Context, allowed []int64) (queue.Operation, lifecycle.EnforcementSnapshot, int64, int64) {
	t.Helper()
	clientID, botID := seedInvoiceLifecycleAccount(t, s, ctx, false)
	yesterday := time.Now().In(time.Local).AddDate(0, 0, -1).Format(time.DateOnly)
	if _, err := s.pool.Exec(ctx, `UPDATE invoice_lifecycle_invoices SET sent_date = $1::date WHERE invoice_id = 'invoice-current'`, yesterday); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO invoice_lifecycle_checks (client_id, bot_id) VALUES ($1, $2)`, clientID, botID); err != nil {
		t.Fatal(err)
	}
	count, err := s.SeedOverdueOperations(ctx, botID, allowed, time.Now().In(time.Local).Format(time.DateOnly), 10)
	if err != nil || count != 1 {
		t.Fatalf("seed overdue count=%d err=%v", count, err)
	}
	claims, err := s.ClaimOperations(ctx, []string{lifecycle.OverdueOperationKind}, 1, 5*time.Minute)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim overdue count=%d err=%v", len(claims), err)
	}
	snapshot, err := s.LoadOverdueSnapshot(ctx, claims[0], botID)
	if err != nil {
		t.Fatal(err)
	}
	return claims[0], snapshot, clientID, botID
}

func TestInvoiceOverdueSeedsOnlyCurrentAllowlistedSentInvoice(t *testing.T) {
	s, ctx := invoiceLifecycleTestStore(t)
	clientID, botID := seedInvoiceLifecycleAccount(t, s, ctx, false)
	yesterday := time.Now().In(time.Local).AddDate(0, 0, -1).Format(time.DateOnly)
	if _, err := s.pool.Exec(ctx, `UPDATE invoice_lifecycle_invoices SET sent_date = $1::date WHERE invoice_id = 'invoice-current'`, yesterday); err != nil {
		t.Fatal(err)
	}
	if count, err := s.SeedOverdueOperations(ctx, botID, []int64{999}, time.Now().In(time.Local).Format(time.DateOnly), 10); err != nil || count != 0 {
		t.Fatalf("out-of-scope account seeded: count=%d err=%v", count, err)
	}
	if count, err := s.SeedOverdueOperations(ctx, botID, []int64{101}, time.Now().In(time.Local).Format(time.DateOnly), 10); err != nil || count != 1 {
		t.Fatalf("current overdue invoice not seeded: count=%d err=%v", count, err)
	}
	if count, err := s.SeedOverdueOperations(ctx, botID, []int64{101}, time.Now().In(time.Local).Format(time.DateOnly), 10); err != nil || count != 0 {
		t.Fatalf("overdue operation was not idempotent: count=%d err=%v", count, err)
	}
	var actions, operations int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM invoice_overdue_actions WHERE client_id = $1`, clientID).Scan(&actions); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM operations WHERE client_id = $1 AND kind = $2`, clientID, lifecycle.OverdueOperationKind).Scan(&operations); err != nil {
		t.Fatal(err)
	}
	if actions != 1 || operations != 1 {
		t.Fatalf("actions=%d operations=%d", actions, operations)
	}
}

func TestInvoiceOverdueWriteIntentUsesSharedGuardAndKnownReceiptReleasesIt(t *testing.T) {
	s, ctx := invoiceLifecycleTestStore(t)
	claim, snapshot, _, botID := seedOverdueOperation(t, s, ctx, []int64{101})
	if !snapshot.ManagedProvenance || snapshot.Invoice.Status != "sent" || snapshot.Invoice.PaidToDate != "0" || snapshot.WriterBusy {
		t.Fatalf("unexpected overdue snapshot: %#v", snapshot)
	}
	request := json.RawMessage(`{"before":{"id":"recurring-synthetic"},"template":{"client_id":"customer-synthetic"}}`)
	today := time.Now().In(time.Local).Format(time.DateOnly)
	authorized, err := s.AuthorizeOverdueWrite(ctx, claim, botID, snapshot, lifecycle.OverduePauseRecurring, request, today, []int64{101})
	if err != nil || !authorized {
		t.Fatalf("pause authorization failed: authorized=%v err=%v", authorized, err)
	}
	if _, err := s.AuthorizeOverdueWrite(ctx, claim, botID, snapshot, lifecycle.OverduePauseRecurring, request, today, []int64{101}); err == nil {
		t.Fatal("same immutable intent was dispatched twice")
	}
	var epoch int64
	var owner int64
	var ownerStep string
	if err := s.pool.QueryRow(ctx, `SELECT epoch, operation_id, step FROM activation_write_guard WHERE id = 1`).Scan(&epoch, &owner, &ownerStep); err != nil {
		t.Fatal(err)
	}
	if epoch != 1 || owner != claim.ID || ownerStep != string(lifecycle.OverduePauseRecurring) {
		t.Fatalf("shared writer guard epoch=%d owner=%d step=%q", epoch, owner, ownerStep)
	}
	if err := s.ConfirmOverdueStep(ctx, claim, lifecycle.OverduePauseRecurring, "recurring-synthetic"); err != nil {
		t.Fatal(err)
	}
	var released bool
	if err := s.pool.QueryRow(ctx, `SELECT epoch, operation_id IS NULL FROM activation_write_guard WHERE id = 1`).Scan(&epoch, &released); err != nil {
		t.Fatal(err)
	}
	if epoch != 1 || !released {
		t.Fatalf("confirmed write did not release shared guard: epoch=%d released=%v", epoch, released)
	}
	var intents int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM external_write_intents WHERE operation_id = $1 AND origin = 'lifecycle'`, claim.ID).Scan(&intents); err != nil || intents != 1 {
		t.Fatalf("durable external intent count=%d err=%v", intents, err)
	}
}

func TestInvoiceOverdueUnknownIntentIsNeverRearmed(t *testing.T) {
	s, ctx := invoiceLifecycleTestStore(t)
	claim, snapshot, _, botID := seedOverdueOperation(t, s, ctx, []int64{101})
	request := json.RawMessage(`{"before":{"id":"recurring-synthetic"},"template":{"client_id":"customer-synthetic"}}`)
	today := time.Now().In(time.Local).Format(time.DateOnly)
	if ok, err := s.AuthorizeOverdueWrite(ctx, claim, botID, snapshot, lifecycle.OverduePauseRecurring, request, today, []int64{101}); err != nil || !ok {
		t.Fatalf("initial intent authorization: ok=%v err=%v", ok, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE operations SET locked_until = clock_timestamp() - interval '1 second' WHERE id = $1`, claim.ID); err != nil {
		t.Fatal(err)
	}
	claims, err := s.ClaimOperations(ctx, []string{lifecycle.OverdueOperationKind}, 1, 5*time.Minute)
	if err != nil || len(claims) != 1 || claims[0].Attempt != claim.Attempt+1 {
		t.Fatalf("reclaimed overdue operation=%#v err=%v", claims, err)
	}
	reclaimed, err := s.LoadOverdueSnapshot(ctx, claims[0], botID)
	if err != nil || !reclaimed.WriterBusy || len(reclaimed.Steps) != 1 || reclaimed.Steps[lifecycle.OverduePauseRecurring].Confirmed {
		t.Fatalf("unknown write snapshot=%#v err=%v", reclaimed, err)
	}
	if ok, err := s.AuthorizeOverdueWrite(ctx, claims[0], botID, reclaimed, lifecycle.OverduePauseRecurring, request, today, []int64{101}); err == nil || ok {
		t.Fatalf("unknown intent was rearmed: ok=%v err=%v", ok, err)
	}
	var intents int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM external_write_intents WHERE operation_id = $1`, claim.ID).Scan(&intents); err != nil || intents != 1 {
		t.Fatalf("intent anchor count=%d err=%v", intents, err)
	}
	adminNotice := queue.Draft{BotID: botID, ChatID: 9001, Key: "invoice-overdue:synthetic:review:unknown", Text: "synthetic manual review"}
	if err := s.HoldOverdue(ctx, claims[0], botID, reclaimed, lifecycle.ReviewOverdueUnknownWrite, []queue.Draft{adminNotice}); err != nil {
		t.Fatal(err)
	}
	var held bool
	if err := s.pool.QueryRow(ctx, `SELECT operation_id = $1 AND step = 'pause_recurring' FROM activation_write_guard WHERE id = 1`, claim.ID).Scan(&held); err != nil || !held {
		t.Fatalf("unknown writer guard was released: held=%v err=%v", held, err)
	}
}
