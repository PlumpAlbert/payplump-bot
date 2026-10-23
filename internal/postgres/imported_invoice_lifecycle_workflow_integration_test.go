//go:build integration

package postgres

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/lifecycle"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
)

func TestImportedInvoiceLifecycleSnapshotIncludesFrozenHistory(t *testing.T) {
	s, ctx := activationTestStore(t)
	prepare, _, _ := importedRuntimeAdmissionFixture(t, ctx)
	prepared, err := prepare(time.Now().UTC().Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	botID := prepared.BotID()
	if _, err := s.pool.Exec(ctx, `INSERT INTO telegram_bots(bot_id) VALUES($1) ON CONFLICT DO NOTHING`, botID); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncImportAdmissionPolicy(ctx, botID, prepared.Revision(), prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyVerifiedImport(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncImportedRuntimePermission(ctx, botID, 1, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
		t.Fatal(err)
	}
	if count, err := s.SeedInvoiceLifecycleChecks(ctx, botID, []int64{42}, 10); err != nil || count != 1 {
		t.Fatalf("imported lifecycle seed count=%d err=%v", count, err)
	}
	claims, err := s.ClaimInvoiceLifecycleChecks(ctx, botID, []int64{42}, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("imported lifecycle claims=%d err=%v", len(claims), err)
	}
	snapshot, err := s.LoadInvoiceLifecycleSnapshot(ctx, claims[0])
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.ImportedOrigin.Valid() || snapshot.ImportedOrigin.ClientID != snapshot.ClientID || snapshot.ImportedOrigin.BotID != botID || snapshot.ImportedOrigin.RuntimeRevision != 1 {
		t.Fatalf("invalid imported lifecycle reference: %+v", snapshot.ImportedOrigin)
	}
	if snapshot.FirstInvoiceID != "" || snapshot.RegistrationToken != "" || snapshot.ActivationToken != "" || !snapshot.ManagedProvenance {
		t.Fatalf("imported lifecycle snapshot fabricated owned provenance: %+v", snapshot)
	}
	if !containsLifecycleID(snapshot.SeenInvoiceIDs, admissionFixtureInv) || !containsLifecycleID(snapshot.SeenInvoiceIDs, "invoice-older-42") {
		t.Fatalf("frozen historical invoices missing from seen set: %v", snapshot.SeenInvoiceIDs)
	}
}

func TestImportedInvoiceLifecycleCannotFinalizeFrozenHistoricalInvoice(t *testing.T) {
	s, ctx := activationTestStore(t)
	prepare, _, _ := importedRuntimeAdmissionFixture(t, ctx)
	prepared, err := prepare(time.Now().UTC().Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	botID := prepared.BotID()
	if _, err := s.pool.Exec(ctx, `INSERT INTO telegram_bots(bot_id) VALUES($1) ON CONFLICT DO NOTHING`, botID); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncImportAdmissionPolicy(ctx, botID, prepared.Revision(), prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyVerifiedImport(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncImportedRuntimePermission(ctx, botID, 1, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SeedInvoiceLifecycleChecks(ctx, botID, []int64{42}, 10); err != nil {
		t.Fatal(err)
	}
	claims, err := s.ClaimInvoiceLifecycleChecks(ctx, botID, []int64{42}, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("imported lifecycle claims=%d err=%v", len(claims), err)
	}
	snapshot, err := s.LoadInvoiceLifecycleSnapshot(ctx, claims[0])
	if err != nil {
		t.Fatal(err)
	}
	invoice := &lifecycle.Invoice{ID: "invoice-older-42", Number: "N-41", Status: "sent", SentDate: time.Now().Format(time.DateOnly), Amount: "100", Balance: "100", PaidToDate: "0"}
	notices := lifecycle.Notices{
		Client: &queue.Draft{BotID: botID, ChatID: snapshot.ChatID, Key: "historical-guard:client", Text: "synthetic notice"},
		Admins: []queue.Draft{{BotID: botID, ChatID: 9001, Key: "historical-guard:admin", Text: "synthetic notice"}},
	}
	if err := s.FinishInvoiceLifecycleCheck(ctx, claims[0], snapshot, invoice, 0, notices, time.Minute); !errors.Is(err, queue.ErrConflict) {
		t.Fatalf("finalizing frozen historical invoice error=%v, want conflict", err)
	}
	var current string
	if err := s.pool.QueryRow(ctx, `SELECT current_invoice_id FROM subscriptions WHERE client_id=$1`, snapshot.ClientID).Scan(&current); err != nil || current != admissionFixtureInv {
		t.Fatalf("historical finalization changed current invoice to %q: %v", current, err)
	}
	paymentSQL(t, s, ctx, `SELECT count(*)=0 FROM invoice_lifecycle_invoices WHERE client_id=$1`, snapshot.ClientID)
	paymentSQL(t, s, ctx, `SELECT count(*)=0 FROM outbox WHERE bot_id=$1 AND message_key LIKE 'historical-guard:%'`, botID)
}

type observedImportedLifecycleStore struct {
	lifecycle.Store
	finished chan error
}

func (s *observedImportedLifecycleStore) FinishInvoiceLifecycleCheck(ctx context.Context, c lifecycle.Claim, snapshot lifecycle.Snapshot, invoice *lifecycle.Invoice, code int, notices lifecycle.Notices, interval time.Duration) error {
	err := s.Store.FinishInvoiceLifecycleCheck(ctx, c, snapshot, invoice, code, notices, interval)
	select {
	case s.finished <- err:
	default:
	}
	return err
}

func TestImportedInvoiceLifecycleWorkerAdoptsNewInvoiceFromGET(t *testing.T) {
	s, ctx := activationTestStore(t)
	prepare, settlement, _, cycle, setNewInvoice, ninjaBase := importedRuntimeAdmissionFixtureWithCycle(t, ctx)
	prepared, err := prepare(time.Now().UTC().Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	botID := prepared.BotID()
	if _, err := s.pool.Exec(ctx, `INSERT INTO telegram_bots(bot_id) VALUES($1) ON CONFLICT DO NOTHING`, botID); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncImportAdmissionPolicy(ctx, botID, prepared.Revision(), prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyVerifiedImport(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncImportedRuntimePermission(ctx, botID, 1, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
		t.Fatal(err)
	}
	if n, err := s.SeedPaymentChecks(ctx, botID); err != nil || n != 1 {
		t.Fatalf("seed imported payment check=%d err=%v", n, err)
	}
	paymentClaim, paymentSnapshot := claimSinglePayment(t, s, ctx, botID)
	decision, err := payments.Observe(ctx, settlement, paymentSnapshot, "1", time.Now().In(time.Local).Format(time.DateOnly))
	if err != nil || decision.Kind != payments.Verified || decision.Proof == nil {
		t.Fatalf("observe imported baseline payment=%+v err=%v", decision, err)
	}
	if err := s.FinishPaymentCheck(ctx, paymentClaim, paymentSnapshot, decision, payments.Notices{}, time.Minute); err != nil {
		t.Fatal(err)
	}

	if err := s.RecordInvoiceLifecycleWake(ctx, botID); err != nil {
		t.Fatal(err)
	}
	observed := &observedImportedLifecycleStore{Store: s, finished: make(chan error, 4)}
	cfg := lifecycle.Config{Enabled: true, BaseURL: ninjaBase, Token: "synthetic-ninja-token", WebhookSecret: "synthetic-lifecycle-secret-0123456789", AllowedTelegramIDs: []int64{42}, Workers: 1, PageSize: 10, MaxPages: 2, MaxAccounts: 10, PollInterval: 100 * time.Millisecond, RecheckInterval: 30 * time.Second, HTTPTimeout: time.Second}
	worker, err := lifecycle.New(observed, cycle, botID, []int64{9001}, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	waitRun := func() {
		t.Helper()
		workerCtx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- worker.Run(workerCtx) }()
		select {
		case err := <-observed.finished:
			cancel()
			if runErr := <-done; runErr != nil {
				t.Fatalf("lifecycle worker stopped: %v", runErr)
			}
			if err != nil {
				t.Fatalf("lifecycle finalize: %v", err)
			}
		case <-time.After(15 * time.Second):
			cancel()
			<-done
			t.Fatal("timed out waiting for actual lifecycle worker finalization")
		case <-ctx.Done():
			cancel()
			<-done
			t.Fatal("test context ended while waiting for lifecycle")
		}
	}
	waitRun()
	paymentSQL(t, s, ctx, `SELECT count(*)=0 FROM invoice_lifecycle_invoices WHERE client_id=(SELECT id FROM clients WHERE telegram_id=42)`)
	paymentSQL(t, s, ctx, `SELECT count(*)=0 FROM outbox WHERE bot_id=$1`, botID)
	paymentSQL(t, s, ctx, `SELECT current_invoice_id=$1 AND first_invoice_id IS NULL AND state='active' AND desired_access FROM subscriptions WHERE client_id=(SELECT id FROM clients WHERE telegram_id=42)`, admissionFixtureInv)

	yesterday := time.Now().In(time.Local).AddDate(0, 0, -1).Format(time.DateOnly)
	tomorrow := time.Now().In(time.Local).AddDate(0, 0, 3).Format(time.DateOnly)
	setNewInvoice(map[string]any{"id": "invoice-new-42", "client_id": admissionFixtureCust, "recurring_id": admissionFixtureRec, "number": "N-43", "private_notes": "historical legacy note", "status_id": "2", "date": yesterday, "due_date": tomorrow, "amount": 100, "balance": 100, "paid_to_date": 0, "exchange_rate": 1, "is_deleted": false, "archived_at": 0, "updated_at": 0, "invitations": []any{map[string]any{"id": "invitation-new-42", "sent_date": yesterday + " 10:30:00"}}})
	if err := s.RecordInvoiceLifecycleWake(ctx, botID); err != nil {
		t.Fatal(err)
	}
	waitRun()
	paymentSQL(t, s, ctx, `SELECT current_invoice_id='invoice-new-42' FROM subscriptions WHERE client_id=(SELECT id FROM clients WHERE telegram_id=42)`)
	paymentSQL(t, s, ctx, `SELECT count(*)=1 FROM invoice_lifecycle_invoices WHERE invoice_id='invoice-new-42' AND status='sent' AND sent_date=$1::date`, yesterday)
	paymentSQL(t, s, ctx, `SELECT count(*)=2 FROM outbox WHERE bot_id=$1`, botID)
	paymentSQL(t, s, ctx, `SELECT count(*)=0 FROM invoice_lifecycle_invoices WHERE invoice_id=$1`, admissionFixtureInv)
	paymentSQL(t, s, ctx, `SELECT count(*)=0 FROM invoice_lifecycle_invoices WHERE invoice_id=$1`, "invoice-older-42")

	if err := s.RecordInvoiceLifecycleWake(ctx, botID); err != nil {
		t.Fatal(err)
	}
	waitRun()
	paymentSQL(t, s, ctx, `SELECT current_invoice_id='invoice-new-42' FROM subscriptions WHERE client_id=(SELECT id FROM clients WHERE telegram_id=42)`)
	paymentSQL(t, s, ctx, `SELECT count(*)=1 FROM invoice_lifecycle_invoices WHERE invoice_id='invoice-new-42' AND status='sent'`)
	paymentSQL(t, s, ctx, `SELECT count(*)=2 FROM outbox WHERE bot_id=$1`, botID)
	paymentSQL(t, s, ctx, `SELECT count(*)=0 FROM invoice_lifecycle_invoices WHERE invoice_id IN ($1, 'invoice-older-42')`, admissionFixtureInv)

	if n, err := s.SeedOverdueOperations(ctx, botID, []int64{42}, time.Now().In(time.Local).Format(time.DateOnly), 10); err != nil || n != 0 {
		t.Fatalf("imported invoice seeded owned overdue operations: %d %v", n, err)
	}
	if n, err := s.SeedRenewalOperations(ctx, botID, []int64{42}, 10); err != nil || n != 0 {
		t.Fatalf("imported invoice seeded owned renewal operations: %d %v", n, err)
	}
}

func containsLifecycleID(ids []string, wanted string) bool {
	for _, id := range ids {
		if id == wanted {
			return true
		}
	}
	return false
}
