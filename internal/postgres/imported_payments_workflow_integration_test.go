//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/billing"
	"github.com/PlumpAlbert/payplump-bot/internal/importadmission"
	"github.com/PlumpAlbert/payplump-bot/internal/importplan"
	"github.com/PlumpAlbert/payplump-bot/internal/importverify"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

func importedRuntimeSettlementReader(t *testing.T, snapshot payments.Snapshot, paid, changed bool) *invoiceninja.SettlementClient {
	t.Helper()
	date := snapshot.PaymentDate
	if changed {
		parsed, err := time.Parse(time.DateOnly, date)
		if err != nil {
			t.Fatal(err)
		}
		date = parsed.AddDate(0, 0, 1).Format(time.DateOnly)
	}
	invoiceDate := snapshot.PaymentDate
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("runtime settlement used non-GET method %s", r.Method)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("X-APP-VERSION", "5.13.47")
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/clients/" + snapshot.CustomerID:
			fmt.Fprintf(w, `{"data":{"id":%q,"id_number":%q,"group_settings_id":"","settings":{"currency_id":"1"},"currency_id":"ignored","is_deleted":false,"archived_at":0}}`, snapshot.CustomerID, fmt.Sprint(snapshot.TelegramID))
		case "/api/v1/invoices/" + snapshot.InvoiceID:
			status, balance, paidToDate := "2", 100, 0
			if paid {
				status, balance, paidToDate = "4", 0, 100
			}
			fmt.Fprintf(w, `{"data":{"id":%q,"client_id":%q,"recurring_id":%q,"number":"N-42","status_id":%q,"date":%q,"due_date":%q,"amount":100,"balance":%d,"paid_to_date":%d,"exchange_rate":1,"is_deleted":false,"archived_at":0}}`, snapshot.InvoiceID, snapshot.CustomerID, snapshot.RecurringID, status, invoiceDate, invoiceDate, balance, paidToDate)
		case "/api/v1/payments":
			fmt.Fprintf(w, `{"data":[{"id":"payment-42","client_id":%q,"date":%q,"status_id":"4","currency_id":"1","exchange_currency_id":"","amount":100,"refunded":0,"applied":100,"exchange_rate":1,"is_manual":true,"is_deleted":false,"archived_at":0,"paymentables":[{"id":"allocation-42","invoice_id":%q,"payment_id":"payment-42","amount":100,"refunded":0,"archived_at":0}]}],"meta":{"pagination":{"total":1,"count":1,"per_page":10,"current_page":1,"total_pages":1}}}`, snapshot.CustomerID, date, snapshot.InvoiceID)
		default:
			t.Errorf("unexpected settlement GET %s?%s", r.URL.Path, r.URL.RawQuery)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	reader, err := invoiceninja.NewSettlementClient(server.URL, "synthetic-runtime-token", time.Second, 10, 1)
	if err != nil {
		t.Fatal(err)
	}
	return reader
}

func importedRuntimeAdmissionFixture(t *testing.T, ctx context.Context) (func(time.Time) (importadmission.PreparedPlan, error), *invoiceninja.SettlementClient, func(bool)) {
	prepare, settle, setPayment, _, _, _ := importedRuntimeAdmissionFixtureWithCycle(t, ctx)
	return prepare, settle, setPayment
}

func importedRuntimeAdmissionFixtureWithCycle(t *testing.T, ctx context.Context) (func(time.Time) (importadmission.PreparedPlan, error), *invoiceninja.SettlementClient, func(bool), *invoiceninja.CycleReader, func(map[string]any), string) {
	t.Helper()
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -10).Format(time.DateOnly)
	end, err := billing.NextBillingDate(start)
	if err != nil {
		t.Fatal(err)
	}
	suiRaw, configHash := admissionSUIFixture(t)
	line := map[string]any{"product_key": "legacy", "notes": "legacy monthly", "cost": 100, "quantity": 1, "discount": 0, "is_amount_discount": false, "type_id": "1", "tax_name1": "", "tax_name2": "", "tax_name3": "", "tax_rate1": 0, "tax_rate2": 0, "tax_rate3": 0, "tax_id": ""}
	recurring := map[string]any{"id": admissionFixtureRec, "client_id": admissionFixtureCust, "status_id": "2", "private_notes": "historical legacy note", "frequency_id": "5", "next_send_date": end, "next_send_datetime": end + "T00:00:00.000000Z", "last_sent_date": start, "due_date": end, "due_date_days": "on_receipt", "auto_bill": "off", "auto_bill_enabled": false, "remaining_cycles": -1, "amount": 100, "balance": 100, "uses_inclusive_taxes": false, "line_items": []any{line}, "is_deleted": false, "archived_at": 0, "discount": 0, "is_amount_discount": true, "tax_rate1": 0, "tax_rate2": 0, "tax_rate3": 0, "custom_surcharge1": 0, "custom_surcharge2": 0, "custom_surcharge3": 0, "custom_surcharge4": 0, "partial": 0, "paid_to_date": 0, "exchange_rate": 1}
	invoice := map[string]any{"id": admissionFixtureInv, "client_id": admissionFixtureCust, "recurring_id": admissionFixtureRec, "number": "N-42", "private_notes": "paid current cycle", "status_id": "4", "date": start, "due_date": start, "amount": 100, "balance": 0, "paid_to_date": 100, "exchange_rate": 1, "is_deleted": false, "archived_at": 0, "updated_at": 0, "invitations": []any{}}
	olderDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -40).Format(time.DateOnly)
	olderInvoice := map[string]any{"id": "invoice-older-42", "client_id": admissionFixtureCust, "recurring_id": admissionFixtureRec, "number": "N-41", "private_notes": "older historic private note", "status_id": "4", "date": olderDate, "due_date": olderDate, "amount": 100, "balance": 0, "paid_to_date": 100, "exchange_rate": 1, "is_deleted": false, "archived_at": 0, "updated_at": 0, "invitations": []any{}}
	customer := map[string]any{"id": admissionFixtureCust, "id_number": admissionFixtureTG, "private_notes": "legacy customer", "is_deleted": false, "archived_at": 0}
	settlementCustomer := map[string]any{"id": admissionFixtureCust, "id_number": admissionFixtureTG, "group_settings_id": "", "settings": map[string]any{"currency_id": "1"}, "currency_id": "ignored", "is_deleted": false, "archived_at": 0}
	allocation := map[string]any{"id": "allocation-42", "invoice_id": admissionFixtureInv, "payment_id": "payment-42", "amount": 100, "refunded": 0, "archived_at": 0}
	payment := map[string]any{"id": "payment-42", "client_id": admissionFixtureCust, "date": start, "status_id": "4", "currency_id": "1", "exchange_currency_id": "", "amount": 100, "refunded": 0, "applied": 100, "exchange_rate": 1, "is_manual": true, "is_deleted": false, "archived_at": 0, "paymentables": []any{allocation}, "invoices": []any{}}
	var cycleMu sync.RWMutex
	cycleInvoices := []any{olderInvoice, invoice}
	var cycleInvoice map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("non-GET source request: %s", r.Method)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/ninja/api/v1/") {
			w.Header().Set("X-APP-VERSION", "5.13.47")
		}
		switch r.URL.Path {
		case "/ninja/api/v1/clients":
			admissionPage(w, []any{customer})
		case "/ninja/api/v1/recurring_invoices":
			admissionPage(w, []any{recurring})
		case "/ninja/api/v1/recurring_invoices/" + admissionFixtureRec:
			admissionJSON(w, map[string]any{"data": recurring})
		case "/ninja/api/v1/invoices":
			cycleMu.RLock()
			page := append([]any(nil), cycleInvoices...)
			cycleMu.RUnlock()
			admissionPage(w, page)
		case "/ninja/api/v1/invoices/" + admissionFixtureInv:
			admissionJSON(w, map[string]any{"data": invoice})
		case "/ninja/api/v1/invoices/invoice-older-42":
			admissionJSON(w, map[string]any{"data": olderInvoice})
		case "/ninja/api/v1/invoices/invoice-new-42":
			cycleMu.RLock()
			fresh := cycleInvoice
			cycleMu.RUnlock()
			if fresh == nil {
				http.NotFound(w, r)
				return
			}
			admissionJSON(w, map[string]any{"data": fresh})
		case "/ninja/api/v1/clients/" + admissionFixtureCust:
			admissionJSON(w, map[string]any{"data": settlementCustomer})
		case "/ninja/api/v1/payments":
			admissionPage(w, []any{payment})
		case "/panel/apiv2/status":
			admissionJSON(w, map[string]any{"success": true, "obj": map[string]any{"sys": map[string]any{"appVersion": "1.6.4"}}})
		case "/panel/apiv2/clients":
			if r.URL.Query().Get("id") == admissionFixtureTG {
				admissionJSON(w, map[string]any{"success": true, "obj": map[string]any{"clients": []json.RawMessage{suiRaw}}})
			} else if r.URL.Query().Get("id") == "" {
				admissionJSON(w, map[string]any{"success": true, "obj": map[string]any{"clients": []any{map[string]any{"id": 42, "name": "42", "enable": true, "desc": "", "remark": "", "inbounds": []int{1}, "expiry": 0, "volume": 0}}}})
			} else {
				http.NotFound(w, r)
			}
		case "/panel/apiv2/settings":
			admissionJSON(w, map[string]any{"success": true, "obj": map[string]any{"subURI": "https://subs.invalid/", "subPath": "private-path", "subPort": "443", "subDomain": ""}})
		default:
			t.Errorf("unexpected source GET %s?%s", r.URL.Path, r.URL.RawQuery)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	const ninjaToken, suiToken = "synthetic-ninja-token", "synthetic-sui-token"
	imp, err := invoiceninja.NewImportReader(server.URL+"/ninja", ninjaToken, time.Second, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := invoiceninja.NewRecurringReader(server.URL+"/ninja", ninjaToken, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cyc, err := invoiceninja.NewCycleReader(server.URL+"/ninja", ninjaToken, time.Second, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	settle, err := invoiceninja.NewSettlementClient(server.URL+"/ninja", ninjaToken, time.Second, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := sui.NewIdentityReader(server.URL+"/panel", suiToken, "1.6.4", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	records, err := sui.NewRecordReader(server.URL+"/panel", suiToken, "1.6.4", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	suiClient, err := sui.New(server.URL+"/panel", suiToken, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := importverify.SourceScope(server.URL+"/ninja", ninjaToken, "5.13.47", server.URL+"/panel", suiToken, "1.6.4")
	if err != nil {
		t.Fatal(err)
	}
	policy := importverify.Policy{BotID: admissionFixtureBotID, SourceScopeHash: scope, PilotIDs: []string{admissionFixtureTG}, NinjaVersion: "5.13.47", SUIVersion: "1.6.4", NativeCurrencyID: "1", NativeCurrencyCode: "USD", ProductKey: "legacy", PublicSubscriptionBase: "https://subs.invalid/", ExpectedInboundIDs: []int64{1}, MaxRecords: 10, MaxInventoryRecords: 20, MaxPages: 2, MaxHistoricalAnchors: 5, EvidenceTTL: 5 * time.Minute}
	service, err := importverify.New(policy, importverify.Providers{Customers: imp, Recurring: rec, Invoices: cyc, Payments: settle, Identities: ids, Records: records, Settings: suiClient})
	if err != nil {
		t.Fatal(err)
	}
	claim := importplan.Record{TelegramID: admissionFixtureTG, InvoiceCustomerID: admissionFixtureCust, InvoiceNumber: admissionFixtureTG, CurrentInvoiceID: admissionFixtureInv, InvoiceStatus: "paid", RecurringID: admissionFixtureRec, SUIID: admissionFixtureTG, SUIName: admissionFixtureTG, ConfigHash: configHash, PeriodStart: start, PeriodEnd: end, Currency: "USD", Lifecycle: "active", SourceVersion: "5.13.47", PinnedProvenance: true, CompleteProof: true, Amount: "100"}
	plan := importplan.Analyze(importplan.Manifest{Records: []importplan.Record{claim}})
	if plan.Imported != 1 {
		t.Fatalf("fixture claim not admissible: %v", plan.Reasons)
	}
	prepare := func(at time.Time) (importadmission.PreparedPlan, error) {
		return importadmission.Prepare(ctx, service, plan, plan.Hash, 1, at)
	}
	setChangedPayment := func(changed bool) {
		paymentDate := start
		if changed {
			parsed, err := time.Parse(time.DateOnly, start)
			if err != nil {
				t.Fatal(err)
			}
			paymentDate = parsed.AddDate(0, 0, 1).Format(time.DateOnly)
		}
		payment["date"] = paymentDate
	}
	setNewInvoice := func(fresh map[string]any) {
		cycleMu.Lock()
		cycleInvoice = fresh
		cycleInvoices = append(append([]any(nil), cycleInvoices...), fresh)
		cycleMu.Unlock()
	}
	return prepare, settle, setChangedPayment, cyc, setNewInvoice, server.URL + "/ninja"
}

func TestImportedPausedRuntimeUsesCurrentDebtNotHistoricalAnchor(t *testing.T) {
	s, ctx := activationTestStore(t)
	if _, err := s.pool.Exec(ctx, `INSERT INTO telegram_bots(bot_id) VALUES($1) ON CONFLICT DO NOTHING`, admissionFixtureBotID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	periodStart, periodEnd := monthStart.AddDate(0, -1, 0).Format(time.DateOnly), monthStart.Format(time.DateOnly)
	prepared, err := admissionPausedPrepared(t, ctx, periodStart, periodEnd, now.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SyncImportAdmissionPolicy(ctx, admissionFixtureBotID, 1, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyVerifiedImport(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncImportedRuntimePermission(ctx, admissionFixtureBotID, 1, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
		t.Fatal(err)
	}
	if n, err := s.SeedPaymentChecks(ctx, admissionFixtureBotID); err != nil || n != 1 {
		t.Fatalf("paused imported payment discovery count=%d err=%v", n, err)
	}
	claim, snapshot := claimSinglePayment(t, s, ctx, admissionFixtureBotID)
	decision, err := payments.Observe(ctx, importedRuntimeSettlementReader(t, snapshot, false, false), snapshot, "1", time.Now().UTC().Format(time.DateOnly))
	if err != nil || decision.Kind != payments.Pending || decision.Proof != nil {
		t.Fatalf("historical anchor was treated as current payment: decision=%+v err=%v", decision, err)
	}
	if err := s.FinishPaymentCheck(ctx, claim, snapshot, decision, payments.Notices{}, time.Minute); err != nil {
		t.Fatal(err)
	}
	paymentSQL(t, s, ctx, `SELECT count(*)=0 FROM payment_confirmations WHERE client_id=$1`, snapshot.ClientID)
	paymentSQL(t, s, ctx, `SELECT state='overdue' AND desired_access=false AND first_invoice_id IS NULL FROM subscriptions WHERE client_id=$1`, snapshot.ClientID)
}

func TestImportedBaselineProofChangeBeforeConfirmationIsSticky(t *testing.T) {
	s, ctx := activationTestStore(t)
	prepare, reader, setChangedPayment := importedRuntimeAdmissionFixture(t, ctx)
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
		t.Fatalf("seed imported baseline: count=%d err=%v", n, err)
	}
	claim, snapshot := claimSinglePayment(t, s, ctx, botID)
	if snapshot.ImportedBaselineProofHash == ([32]byte{}) {
		t.Fatal("missing imported baseline proof hash")
	}
	var originProofHash string
	if err := s.pool.QueryRow(ctx, `SELECT evidence->>'proof_hash' FROM verified_import_origins WHERE client_id=$1`, snapshot.ClientID).Scan(&originProofHash); err != nil {
		t.Fatal(err)
	}
	if originProofHash != fmt.Sprintf("%x", snapshot.ImportedBaselineProofHash[:]) {
		t.Fatal("snapshot baseline hash differs from immutable origin evidence")
	}

	setChangedPayment(true)
	decision, err := payments.Observe(ctx, reader, snapshot, "1", time.Now().UTC().Format(time.DateOnly))
	if err != nil || decision.Kind != payments.Verified || decision.Proof == nil || decision.Proof.Hash == snapshot.ImportedBaselineProofHash {
		t.Fatalf("changed baseline GET was not independently verified: decision=%+v err=%v", decision, err)
	}
	notices := payments.Notices{
		Confirmed: []queue.Draft{{BotID: botID, ChatID: snapshot.ChatID, Key: "before-confirmation-confirmed", Text: "synthetic confirmation"}},
		Review:    []queue.Draft{{BotID: botID, ChatID: snapshot.ChatID, Key: "before-confirmation-review", Text: "synthetic review"}},
	}
	if err := s.FinishPaymentCheck(ctx, claim, snapshot, decision, notices, time.Minute); err != nil {
		t.Fatal(err)
	}
	paymentSQL(t, s, ctx, `SELECT count(*)=0 FROM payment_confirmations WHERE client_id=$1 AND invoice_id=$2`, snapshot.ClientID, snapshot.InvoiceID)
	paymentSQL(t, s, ctx, `SELECT review_code=2007 FROM payment_checks WHERE client_id=$1 AND bot_id=$2`, snapshot.ClientID, botID)
	paymentSQL(t, s, ctx, `SELECT count(*)=1 FROM outbox WHERE bot_id=$1 AND message_key='before-confirmation-review'`, botID)

	// Restoring the original source proof cannot clear the first contradiction.
	setChangedPayment(false)
	duePayment(t, s, ctx, snapshot.ClientID)
	claims, err := s.ClaimPaymentChecks(ctx, botID, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim restored baseline: count=%d err=%v", len(claims), err)
	}
	restoredSnapshot, err := s.LoadPaymentSnapshot(ctx, claims[0])
	if err != nil {
		t.Fatal(err)
	}
	restored, err := payments.Observe(ctx, reader, restoredSnapshot, "1", time.Now().UTC().Format(time.DateOnly))
	if err != nil || restored.Kind != payments.Verified || restored.Proof == nil || restored.Proof.Hash != snapshot.ImportedBaselineProofHash {
		t.Fatalf("restored source did not reproduce original baseline: decision=%+v err=%v", restored, err)
	}
	if err := s.FinishPaymentCheck(ctx, claims[0], restoredSnapshot, restored, notices, time.Minute); err != nil {
		t.Fatal(err)
	}
	paymentSQL(t, s, ctx, `SELECT count(*)=0 FROM payment_confirmations WHERE client_id=$1 AND invoice_id=$2`, snapshot.ClientID, snapshot.InvoiceID)
	paymentSQL(t, s, ctx, `SELECT review_code=2007 FROM payment_checks WHERE client_id=$1 AND bot_id=$2`, snapshot.ClientID, botID)
	paymentSQL(t, s, ctx, `SELECT evidence->>'proof_hash'=$2 FROM verified_import_origins WHERE client_id=$1`, snapshot.ClientID, originProofHash)
}

func TestImportedPaymentTrackingRequiresIndependentRuntimePermission(t *testing.T) {
	s, ctx := activationTestStore(t)
	prepare, reader, setChangedPayment := importedRuntimeAdmissionFixture(t, ctx)
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
	if n, err := s.SeedPaymentChecks(ctx, botID); err != nil || n != 0 {
		t.Fatalf("default-absent runtime gate seeded imported account: %d %v", n, err)
	}

	if err := s.SyncImportedRuntimePermission(ctx, botID, 1, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
		t.Fatal(err)
	}
	if n, err := s.SeedPaymentChecks(ctx, botID); err != nil || n != 1 {
		t.Fatalf("enabled runtime gate did not seed imported account: %d %v", n, err)
	}
	claim, snapshot := claimSinglePayment(t, s, ctx, botID)
	if !snapshot.ImportedOrigin.Valid() || snapshot.ImportedOrigin.ClientID != snapshot.ClientID || snapshot.ImportedOrigin.BotID != botID || snapshot.ImportedOrigin.AdmissionRevision != prepared.Revision() || snapshot.ImportedOrigin.RuntimeRevision != 1 {
		t.Fatalf("snapshot did not contain DB-validated imported origin: %+v", snapshot.ImportedOrigin)
	}
	var currentConfigRevision int64
	if err := s.pool.QueryRow(ctx, `SELECT configuration_revision FROM clients WHERE id=$1`, snapshot.ClientID).Scan(&currentConfigRevision); err != nil {
		t.Fatal(err)
	}
	if snapshot.ImportedOrigin.ConfigurationRevision != currentConfigRevision {
		t.Fatalf("origin captured configuration revision %d, current database revision is %d", snapshot.ImportedOrigin.ConfigurationRevision, currentConfigRevision)
	}
	if snapshot.ImportedBaselineProofHash == ([32]byte{}) {
		t.Fatal("active imported account did not expose its immutable baseline proof hash")
	}

	// A disable revokes an in-flight claim before either snapshot loading or
	// final proof recording. Re-enable at the same revision is forbidden.
	if err := s.SyncImportedRuntimePermission(ctx, botID, 2, prepared.PolicyHash(), prepared.SourceScopeHash(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadPaymentSnapshot(ctx, claim); err == nil {
		t.Fatal("loaded payment snapshot after runtime disable")
	}
	if err := s.FinishPaymentCheck(ctx, claim, snapshot, payments.Decision{Kind: payments.Verified, Proof: paymentProof(t, snapshot, snapshot.PaymentDate, "100")}, payments.Notices{}, time.Minute); err == nil {
		t.Fatal("finished stale imported claim after runtime disable")
	}
	if err := s.SyncImportedRuntimePermission(ctx, botID, 2, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err == nil {
		t.Fatal("runtime permission re-enabled at same revision")
	}
	if err := s.SyncImportedRuntimePermission(ctx, botID, 3, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
		t.Fatal(err)
	}
	// Higher revision makes old snapshots stale even though the permission is
	// enabled again. The failed finalization leaves its lease intact.
	if err := s.FinishPaymentCheck(ctx, claim, snapshot, payments.Decision{Kind: payments.Verified, Proof: paymentProof(t, snapshot, snapshot.PaymentDate, "100")}, payments.Notices{}, time.Minute); err == nil {
		t.Fatal("accepted snapshot from prior runtime revision")
	}
	paymentExec(t, s, ctx, `UPDATE payment_checks SET locked_until=NULL WHERE client_id=$1`, snapshot.ClientID)

	claim, snapshot = claimSinglePayment(t, s, ctx, botID)
	if snapshot.ImportedOrigin.RuntimeRevision != 3 {
		t.Fatalf("snapshot did not observe current runtime revision: %+v", snapshot.ImportedOrigin)
	}
	decision, err := payments.Observe(ctx, reader, snapshot, "1", time.Now().UTC().Format(time.DateOnly))
	if err != nil || decision.Kind != payments.Verified || decision.Proof == nil {
		t.Fatalf("real settlement GET did not verify current invoice: decision=%+v err=%v", decision, err)
	}
	if snapshot.ImportedBaselineProofHash == ([32]byte{}) || decision.Proof.Hash != snapshot.ImportedBaselineProofHash {
		t.Fatalf("GET proof did not match immutable imported baseline")
	}
	notices := payments.Notices{
		Confirmed: []queue.Draft{{BotID: botID, ChatID: snapshot.ChatID, Key: "baseline-confirmed", Text: "synthetic confirmation"}},
		Review:    []queue.Draft{{BotID: botID, ChatID: snapshot.ChatID, Key: "baseline-review", Text: "synthetic review"}},
	}
	if err := s.FinishPaymentCheck(ctx, claim, snapshot, decision, notices, time.Minute); err != nil {
		t.Fatal(err)
	}
	paymentSQL(t, s, ctx, `SELECT count(*)=1 FROM payment_confirmations WHERE client_id=$1 AND invoice_id=$2`, snapshot.ClientID, snapshot.InvoiceID)
	paymentSQL(t, s, ctx, `SELECT first_invoice_id IS NULL AND state='active' AND desired_access FROM subscriptions WHERE client_id=$1`, snapshot.ClientID)
	paymentSQL(t, s, ctx, `SELECT count(*)=0 FROM operations WHERE client_id=$1`, snapshot.ClientID)
	paymentSQL(t, s, ctx, `SELECT count(*)=0 FROM activation_sui_bindings WHERE client_id=$1`, snapshot.ClientID)
	paymentSQL(t, s, ctx, `SELECT count(*)=0 FROM outbox WHERE bot_id=$1`, botID)

	// A later, fully valid but changed GET proof for the immutable imported
	// baseline is sticky review, not a replacement confirmation or notice.
	duePayment(t, s, ctx, snapshot.ClientID)
	claims, err := s.ClaimPaymentChecks(ctx, botID, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim changed imported payment: count=%d err=%v", len(claims), err)
	}
	changedSnapshot, err := s.LoadPaymentSnapshot(ctx, claims[0])
	if err != nil {
		t.Fatal(err)
	}
	setChangedPayment(true)
	changedDecision, err := payments.Observe(ctx, reader, changedSnapshot, "1", time.Now().UTC().Format(time.DateOnly))
	if err != nil || changedDecision.Kind != payments.Verified || changedDecision.Proof == nil {
		t.Fatalf("changed current financial GET was not independently verified: decision=%+v err=%v", changedDecision, err)
	}
	paymentSQL(t, s, ctx, `SELECT proof_hash<>$2 FROM payment_confirmations WHERE client_id=$1 AND invoice_id=$3`, snapshot.ClientID, changedDecision.Proof.Hash[:], snapshot.InvoiceID)
	if err := s.FinishPaymentCheck(ctx, claims[0], changedSnapshot, changedDecision, notices, time.Minute); err != nil {
		t.Fatal(err)
	}
	paymentSQL(t, s, ctx, `SELECT review_required AND review_code=2007 FROM payment_confirmations WHERE client_id=$1 AND invoice_id=$2`, snapshot.ClientID, snapshot.InvoiceID)
	paymentSQL(t, s, ctx, `SELECT count(*)=1 FROM outbox WHERE bot_id=$1 AND message_key='baseline-review'`, botID)

	// Future-due OWN rows must not leak through the OR branch when an
	// imported row is eligible for this same bot.
	sameBotOwnIDs := seedPaymentAccounts(t, s, ctx, botID, 99501, 1)
	if n, err := s.SeedPaymentChecks(ctx, botID); err != nil || n != 1 {
		t.Fatalf("same-bot OWN discovery count=%d err=%v", n, err)
	}
	paymentExec(t, s, ctx, `UPDATE payment_checks SET next_check_at=clock_timestamp()+interval '1 day', last_error_code=1 WHERE client_id=$1`, sameBotOwnIDs[0])
	paymentExec(t, s, ctx, `UPDATE payment_checks SET next_check_at=clock_timestamp()-interval '1 second', last_error_code=1, locked_until=NULL WHERE client_id=$1`, snapshot.ClientID)
	scopedClaims, err := s.ClaimPaymentChecks(ctx, botID, 4)
	if err != nil || len(scopedClaims) != 1 || scopedClaims[0].ID != snapshot.ClientID {
		t.Fatalf("due/provenance claim scope: claims=%+v err=%v", scopedClaims, err)
	}
	paymentSQL(t, s, ctx, `SELECT attempts=0 AND locked_until IS NULL FROM payment_checks WHERE client_id=$1`, sameBotOwnIDs[0])

	// Existing OWN journal eligibility is still accepted with a zero ref.
	ownIDs := seedPaymentAccounts(t, s, ctx, 777, 99001, 1)
	if _, err := s.SeedPaymentChecks(ctx, 777); err != nil {
		t.Fatal(err)
	}
	paymentSQL(t, s, ctx, `SELECT count(*)=0 FROM payment_checks WHERE client_id=$1 AND bot_id=777`, snapshot.ClientID)
	var importedAttempts int
	var importedLease bool
	if err := s.pool.QueryRow(ctx, `SELECT attempts, locked_until IS NOT NULL FROM payment_checks WHERE client_id=$1`, snapshot.ClientID).Scan(&importedAttempts, &importedLease); err != nil {
		t.Fatal(err)
	}
	ownClaims, err := s.ClaimPaymentChecks(ctx, 777, 1)
	if err != nil || len(ownClaims) != 1 {
		t.Fatalf("own payment claim: %d %v", len(ownClaims), err)
	}
	ownSnapshot, err := s.LoadPaymentSnapshot(ctx, ownClaims[0])
	if err != nil || ownSnapshot.ClientID != ownIDs[0] || ownSnapshot.ImportedOrigin.Valid() {
		t.Fatalf("OWN snapshot was altered: %+v %v", ownSnapshot, err)
	}
	var attemptsAfterWrongBot int
	var leaseAfterWrongBot bool
	if err := s.pool.QueryRow(ctx, `SELECT attempts, locked_until IS NOT NULL FROM payment_checks WHERE client_id=$1`, snapshot.ClientID).Scan(&attemptsAfterWrongBot, &leaseAfterWrongBot); err != nil {
		t.Fatal(err)
	}
	if attemptsAfterWrongBot != importedAttempts || leaseAfterWrongBot != importedLease {
		t.Fatalf("bot 777 claim mutated imported bot %d work: attempts %d->%d lease %v->%v", botID, importedAttempts, attemptsAfterWrongBot, importedLease, leaseAfterWrongBot)
	}

	// The API rejects invalid and non-monotonic permission updates.
	if err := s.SyncImportedRuntimePermission(ctx, botID, 2, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err == nil {
		t.Fatal("accepted lower runtime permission revision")
	}
	if err := s.SyncImportedRuntimePermission(ctx, botID, 3, strings.Repeat("f", 64), prepared.SourceScopeHash(), true); err == nil {
		t.Fatal("accepted same-revision policy change")
	}
	if err := s.SyncImportedRuntimePermission(ctx, botID, 4, prepared.PolicyHash(), "not-a-hash", false); err == nil {
		t.Fatal("accepted malformed runtime scope")
	}
}
