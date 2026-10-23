//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/billing"
	"github.com/PlumpAlbert/payplump-bot/internal/importadmission"
	"github.com/PlumpAlbert/payplump-bot/internal/importplan"
	"github.com/PlumpAlbert/payplump-bot/internal/importverify"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/lifecycle"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

// TestImportedInitialRestorationPaymentProofTargetsCurrentDebt ensures the
// imported historical paid anchor is never payment proof for current debt.
func TestImportedInitialRestorationPaymentProofTargetsCurrentDebt(t *testing.T) {
	s, ctx := activationTestStore(t)
	if _, err := s.pool.Exec(ctx, `INSERT INTO telegram_bots(bot_id) VALUES($1) ON CONFLICT DO NOTHING`, admissionFixtureBotID); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	periodStart := monthStart.AddDate(0, -2, 0).Format(time.DateOnly)
	periodEnd := monthStart.AddDate(0, -1, 0).Format(time.DateOnly)
	prepared, err := admissionPausedPrepared(t, ctx, periodStart, periodEnd, now.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SyncImportAdmissionPolicy(ctx, admissionFixtureBotID, 1, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
		t.Fatal(err)
	}
	if result, applyErr := s.ApplyVerifiedImport(ctx, prepared); applyErr != nil || result.Admitted != 1 {
		t.Fatalf("apply paused import: result=%+v err=%v", result, applyErr)
	}
	if err = s.SyncImportedRuntimePermission(ctx, admissionFixtureBotID, 1, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
		t.Fatal(err)
	}

	paymentSQL(t, s, ctx, `SELECT state='overdue' AND desired_access=false AND current_invoice_id='debt-current' AND first_invoice_id IS NULL AND payment_date=$1::date AND next_invoice_date=$2::date FROM subscriptions WHERE client_id=(SELECT id FROM clients WHERE telegram_id=42)`, periodStart, periodEnd)
	paymentSQL(t, s, ctx, `SELECT current_invoice_id='debt-current' AND paid_anchor_invoice_id='paid-historical' AND current_invoice_id<>paid_anchor_invoice_id FROM verified_import_origins WHERE client_id=(SELECT id FROM clients WHERE telegram_id=42)`)
	paymentSQL(t, s, ctx, `SELECT count(*)=0 FROM invoice_lifecycle_invoices WHERE client_id=(SELECT id FROM clients WHERE telegram_id=42)`)
	paymentSQL(t, s, ctx, `SELECT count(*)=0 FROM invoice_overdue_actions WHERE client_id=(SELECT id FROM clients WHERE telegram_id=42)`)
	paymentSQL(t, s, ctx, `SELECT count(*)=0 FROM invoice_renewal_actions WHERE client_id=(SELECT id FROM clients WHERE telegram_id=42)`)

	if n, seedErr := s.SeedPaymentChecks(ctx, admissionFixtureBotID); seedErr != nil || n != 1 {
		t.Fatalf("seed current-debt payment check: count=%d err=%v", n, seedErr)
	}
	claim, snapshot := claimSinglePayment(t, s, ctx, admissionFixtureBotID)
	if snapshot.InvoiceID != "debt-current" {
		t.Fatalf("payment snapshot selected %q, want current debt", snapshot.InvoiceID)
	}
	decision, err := payments.Observe(ctx, importedRuntimeSettlementReader(t, snapshot, false, false), snapshot, "1", now.Format(time.DateOnly))
	if err != nil || decision.Kind != payments.Pending || decision.Proof != nil {
		t.Fatalf("historical anchor paid the unpaid current debt: decision=%+v err=%v", decision, err)
	}
	if err = s.FinishPaymentCheck(ctx, claim, snapshot, decision, payments.Notices{}, time.Minute); err != nil {
		t.Fatal(err)
	}
	paymentSQL(t, s, ctx, `SELECT count(*)=0 FROM payment_confirmations WHERE client_id=$1`, snapshot.ClientID)
	paymentSQL(t, s, ctx, `SELECT state='overdue' AND desired_access=false AND first_invoice_id IS NULL FROM subscriptions WHERE client_id=$1`, snapshot.ClientID)

	paidDate := time.Now().UTC().Format(time.DateOnly)
	duePayment(t, s, ctx, snapshot.ClientID)
	claim, snapshot = claimSinglePayment(t, s, ctx, admissionFixtureBotID)
	decision, err = payments.Observe(ctx, importedRuntimeSettlementReader(t, snapshot, true, false), snapshot, "1", paidDate)
	if err != nil || decision.Kind != payments.Verified || decision.Proof == nil || decision.Proof.InvoiceID != "debt-current" {
		t.Fatalf("current debt payment was not independently verified: decision=%+v err=%v", decision, err)
	}
	if err = s.FinishPaymentCheck(ctx, claim, snapshot, decision, payments.Notices{}, time.Minute); err != nil {
		t.Fatal(err)
	}
	// Payment proof is necessary but does not itself authorize access. Access
	// restoration must be performed by the effect-gated renewal worker.
	paymentSQL(t, s, ctx, `SELECT state='overdue' AND desired_access=false AND current_invoice_id='debt-current' AND first_invoice_id IS NULL FROM subscriptions WHERE client_id=$1`, snapshot.ClientID)
}

const initialRestorationNinjaToken = "synthetic-ninja-token"
const initialRestorationSUIToken = "synthetic-sui-token"

// initialRestorationSource is one mutable, finite source system shared by
// verification, settlement, and the eventual lifecycle writers.
type initialRestorationSource struct {
	t                            *testing.T
	store                        *Store
	baseNinja, baseSUI           string
	steps                        []string
	unknownResume                bool
	onResumeSuccess              func(context.Context) error
	mu                           sync.Mutex
	customer, settlementCustomer map[string]any
	recurring, debt, paidAnchor  map[string]any
	invoices                     []any
	nextCycle                    map[string]any
	payments                     []any
	suiRaw                       json.RawMessage
	suiRecord                    sui.ClientRecord
	settings                     map[string]any
}

func (src *initialRestorationSource) MarkDebtPaid(date string) {
	src.mu.Lock()
	defer src.mu.Unlock()
	src.debt["status_id"], src.debt["balance"], src.debt["paid_to_date"] = "4", 0, 100
	paymentID := "payment-current-debt"
	src.payments = append(src.payments, map[string]any{
		"id": paymentID, "client_id": "customer-42", "date": date, "status_id": "4", "currency_id": "1",
		"exchange_currency_id": "", "amount": 100, "refunded": 0, "applied": 100, "exchange_rate": 1,
		"is_manual": true, "is_deleted": false, "archived_at": 0,
		"paymentables": []any{map[string]any{"id": "allocation-current-debt", "invoice_id": "debt-current", "payment_id": paymentID, "amount": 100, "refunded": 0, "archived_at": 0}},
		"invoices":     []any{},
	})
}

func (src *initialRestorationSource) EmitNextCycle(date string) {
	src.mu.Lock()
	defer src.mu.Unlock()
	due, err := time.Parse(time.DateOnly, date)
	if err != nil {
		src.t.Fatalf("invalid next-cycle date %q: %v", date, err)
	}
	src.nextCycle = map[string]any{"id": "invoice-after-restoration", "client_id": "customer-42", "recurring_id": admissionFixtureRec, "number": "N-44", "private_notes": "historical legacy note", "status_id": "2", "date": date, "due_date": due.AddDate(0, 0, 10).Format(time.DateOnly), "amount": 100, "balance": 100, "paid_to_date": 0, "exchange_rate": 1, "is_deleted": false, "archived_at": 0, "updated_at": 0, "invitations": []any{map[string]any{"id": "invitation-after-restoration", "sent_date": date + " 10:30:00"}}}
	src.invoices = append(src.invoices, src.nextCycle)
}

func (src *initialRestorationSource) MarkNextCyclePaid(date string) {
	src.mu.Lock()
	defer src.mu.Unlock()
	src.nextCycle["status_id"], src.nextCycle["balance"], src.nextCycle["paid_to_date"] = "4", 0, 100
	paymentID := "payment-after-restoration"
	src.payments = append(src.payments, map[string]any{"id": paymentID, "client_id": "customer-42", "date": date, "status_id": "4", "currency_id": "1", "exchange_currency_id": "", "amount": 100, "refunded": 0, "applied": 100, "exchange_rate": 1, "is_manual": true, "is_deleted": false, "archived_at": 0, "paymentables": []any{map[string]any{"id": "allocation-after-restoration", "invoice_id": "invoice-after-restoration", "payment_id": paymentID, "amount": 100, "refunded": 0, "archived_at": 0}}, "invoices": []any{}})
}

func (src *initialRestorationSource) SetSUIRecord(record sui.ClientRecord) {
	src.mu.Lock()
	defer src.mu.Unlock()
	src.suiRecord = record
}

func (src *initialRestorationSource) SetRecurringState(status, nextDate, nextDateTime string) {
	src.mu.Lock()
	defer src.mu.Unlock()
	src.recurring["status_id"], src.recurring["next_send_date"], src.recurring["next_send_datetime"] = status, nextDate, nextDateTime
}

// newInitialRestorationRig builds verification and runtime clients against one
// source server. Prepare performs fresh GET verification before admission.
func newInitialRestorationRig(t *testing.T, emptySchedule bool) (*Store, context.Context, importadmission.PreparedPlan, *invoiceninja.SettlementClient, *invoiceninja.CycleReader, *invoiceninja.ImportedRecurringClient, *sui.ManagedClient, *initialRestorationSource) {
	t.Helper()
	s, ctx := activationTestStore(t)
	if _, err := s.pool.Exec(ctx, `INSERT INTO telegram_bots(bot_id) VALUES($1) ON CONFLICT DO NOTHING`, admissionFixtureBotID); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -2, 0).Format(time.DateOnly)
	end := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 0).Format(time.DateOnly)
	suiRaw, _ := admissionSUIFixture(t)
	var suiFields map[string]json.RawMessage
	if err := json.Unmarshal(suiRaw, &suiFields); err != nil {
		t.Fatal(err)
	}
	suiFields["enable"] = json.RawMessage(`false`)
	suiRaw, err := json.Marshal(suiFields)
	if err != nil {
		t.Fatal(err)
	}
	suiRecord, err := sui.ParseClientRecord(suiRaw)
	if err != nil {
		t.Fatal(err)
	}

	line := map[string]any{"product_key": "legacy", "notes": "historical legacy note", "cost": 100, "quantity": 1, "discount": 0, "is_amount_discount": false, "type_id": "1", "tax_name1": "", "tax_name2": "", "tax_name3": "", "tax_rate1": 0, "tax_rate2": 0, "tax_rate3": 0, "tax_id": ""}
	nextDate, nextDateTime := end, end+"T00:00:00.000000Z"
	if emptySchedule {
		nextDate, nextDateTime = "", ""
	}
	recurring := map[string]any{"id": admissionFixtureRec, "client_id": "customer-42", "status_id": "3", "private_notes": "historical legacy note", "frequency_id": "5", "next_send_date": nextDate, "next_send_datetime": nextDateTime, "last_sent_date": end, "due_date": end, "due_date_days": "on_receipt", "auto_bill": "off", "auto_bill_enabled": false, "remaining_cycles": -1, "amount": 100, "balance": 100, "uses_inclusive_taxes": false, "line_items": []any{line}, "is_deleted": false, "archived_at": 0, "discount": 0, "is_amount_discount": true, "tax_rate1": 0, "tax_rate2": 0, "tax_rate3": 0, "custom_surcharge1": 0, "custom_surcharge2": 0, "custom_surcharge3": 0, "custom_surcharge4": 0, "partial": 0, "paid_to_date": 0, "exchange_rate": 1}
	debt := map[string]any{"id": "debt-current", "client_id": "customer-42", "recurring_id": admissionFixtureRec, "number": "D-42", "private_notes": "current debt note", "status_id": "2", "date": end, "due_date": end, "amount": 100, "balance": 100, "paid_to_date": 0, "exchange_rate": 1, "is_deleted": false, "archived_at": 0, "updated_at": 0, "invitations": []any{}}
	anchor := map[string]any{"id": "paid-historical", "client_id": "customer-42", "recurring_id": admissionFixtureRec, "number": "P-42", "private_notes": "paid historical invoice", "status_id": "4", "date": start, "due_date": start, "amount": 100, "balance": 0, "paid_to_date": 100, "exchange_rate": 1, "is_deleted": false, "archived_at": 0, "updated_at": 0, "invitations": []any{}}
	customer := map[string]any{"id": "customer-42", "id_number": "42", "private_notes": "legacy customer", "is_deleted": false, "archived_at": 0}
	settlementCustomer := map[string]any{"id": "customer-42", "id_number": "42", "group_settings_id": "", "settings": map[string]any{"currency_id": "1"}, "currency_id": "ignored", "is_deleted": false, "archived_at": 0}
	allocation := map[string]any{"id": "allocation-paid-anchor", "invoice_id": "paid-historical", "payment_id": "payment-paid-anchor", "amount": 100, "refunded": 0, "archived_at": 0}
	oldPayment := map[string]any{"id": "payment-paid-anchor", "client_id": "customer-42", "date": start, "status_id": "4", "currency_id": "1", "exchange_currency_id": "", "amount": 100, "refunded": 0, "applied": 100, "exchange_rate": 1, "is_manual": true, "is_deleted": false, "archived_at": 0, "paymentables": []any{allocation}, "invoices": []any{}}
	src := &initialRestorationSource{t: t, store: s, customer: customer, settlementCustomer: settlementCustomer, recurring: recurring, debt: debt, paidAnchor: anchor, invoices: []any{debt, anchor}, payments: []any{oldPayment}, suiRaw: suiRaw, suiRecord: suiRecord, settings: map[string]any{"subURI": "https://subs.invalid/", "subPath": "private-path", "subPort": "443", "subDomain": ""}}
	server := httptest.NewServer(http.HandlerFunc(src.serveHTTP))
	src.baseNinja, src.baseSUI = server.URL+"/ninja", server.URL+"/panel"
	t.Cleanup(server.Close)
	imp, err := invoiceninja.NewImportReader(server.URL+"/ninja", initialRestorationNinjaToken, time.Second, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := invoiceninja.NewRecurringReader(server.URL+"/ninja", initialRestorationNinjaToken, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cycle, err := invoiceninja.NewCycleReader(server.URL+"/ninja", initialRestorationNinjaToken, time.Second, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	settlement, err := invoiceninja.NewSettlementClient(server.URL+"/ninja", initialRestorationNinjaToken, time.Second, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := sui.NewIdentityReader(server.URL+"/panel", initialRestorationSUIToken, "1.6.4", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	records, err := sui.NewRecordReader(server.URL+"/panel", initialRestorationSUIToken, "1.6.4", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := sui.New(server.URL+"/panel", initialRestorationSUIToken, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := importverify.SourceScope(server.URL+"/ninja", initialRestorationNinjaToken, "5.13.47", server.URL+"/panel", initialRestorationSUIToken, "1.6.4")
	if err != nil {
		t.Fatal(err)
	}
	policy := importverify.Policy{BotID: admissionFixtureBotID, SourceScopeHash: scope, PilotIDs: []string{"42"}, NinjaVersion: "5.13.47", SUIVersion: "1.6.4", NativeCurrencyID: "1", NativeCurrencyCode: "USD", ProductKey: "legacy", PublicSubscriptionBase: "https://subs.invalid/", ExpectedInboundIDs: []int64{1}, MaxRecords: 10, MaxInventoryRecords: 20, MaxPages: 2, MaxHistoricalAnchors: 5, EvidenceTTL: 5 * time.Minute}
	service, err := importverify.New(policy, importverify.Providers{Customers: imp, Recurring: rec, Invoices: cycle, Payments: settlement, Identities: ids, Records: records, Settings: settings})
	if err != nil {
		t.Fatal(err)
	}
	claim := importplan.Record{TelegramID: "42", InvoiceCustomerID: "customer-42", InvoiceNumber: "42", CurrentInvoiceID: "debt-current", InvoiceStatus: "sent", RecurringID: admissionFixtureRec, SUIID: "42", SUIName: "42", ConfigHash: credentialsHash(suiRecord), PeriodStart: start, PeriodEnd: end, Currency: "USD", Lifecycle: "paused", SourceVersion: "5.13.47", PinnedProvenance: true, CompleteProof: true, UnpaidInvoices: 1, Amount: "100"}
	plan := importplan.Analyze(importplan.Manifest{Records: []importplan.Record{claim}})
	if plan.Imported != 1 {
		t.Fatalf("paused fixture claim inadmissible: %v", plan.Reasons)
	}
	prepared, err := importadmission.Prepare(ctx, service, plan, plan.Hash, 1, time.Now().UTC().Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	managed, err := sui.NewManagedClient(server.URL+"/panel", initialRestorationSUIToken, "1.6.4", time.Second, 10)
	if err != nil {
		t.Fatal(err)
	}
	importedRecurring, err := invoiceninja.NewImportedRecurringClient(server.URL+"/ninja", initialRestorationNinjaToken, time.Second, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	return s, ctx, prepared, settlement, cycle, importedRecurring, managed, src
}

func credentialsHash(record sui.ClientRecord) string {
	sum := record.CredentialsHash()
	return fmt.Sprintf("%x", sum[:])
}

func (src *initialRestorationSource) authorized(ctx context.Context, step lifecycle.RenewalStep) bool {
	var valid bool
	err := src.store.pool.QueryRow(ctx, `SELECT g.operation_id=o.id AND g.step=$1 AND g.epoch>0
		AND o.kind='invoice_paid_renewal' AND o.status='running' AND o.locked_until>clock_timestamp()
		AND o.attempts=st.dispatch_attempt AND st.dispatch_attempt=i.dispatch_attempt AND st.confirmed_at IS NULL
		AND a.outcome='pending' AND a.origin_mode='imported_initial_paused' AND a.ledger_invoice_id IS NULL
		AND a.invoice_id='debt-current' AND i.origin='lifecycle'
		AND i.request_hash=sha256(convert_to(st.request::text,'UTF8'))
		FROM activation_write_guard g JOIN operations o ON o.id=g.operation_id
		JOIN invoice_renewal_actions a ON a.operation_id=o.id
		JOIN invoice_renewal_steps st ON st.operation_id=o.id AND st.step=g.step
		JOIN external_write_intents i ON i.operation_id=st.operation_id AND i.step=st.step AND i.origin='lifecycle'
		WHERE g.id=1 AND g.step=$1`, string(step)).Scan(&valid)
	return err == nil && valid
}

func (src *initialRestorationSource) handleSchedule(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/json" {
		src.t.Errorf("schedule request shape mismatch")
		http.Error(w, "bad request", 400)
		return
	}
	var body map[string]json.RawMessage
	var payload struct {
		Date string      `json:"next_send_date"`
		Rate json.Number `json:"exchange_rate"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 2 || body["next_send_date"] == nil || body["exchange_rate"] == nil || json.Unmarshal(body["next_send_date"], &payload.Date) != nil || json.Unmarshal(body["exchange_rate"], &payload.Rate) != nil || payload.Date == "" || payload.Rate.String() != "1" || !src.authorized(r.Context(), lifecycle.RenewalSchedule) {
		src.t.Errorf("schedule payload or committed intent invalid: %v", body)
		http.Error(w, "invalid schedule intent", 409)
		return
	}
	if _, err := time.Parse(time.DateOnly, payload.Date); err != nil {
		src.t.Errorf("schedule date not a date: %v", err)
		http.Error(w, "bad date", 400)
		return
	}
	src.mu.Lock()
	defer src.mu.Unlock()
	src.recurring["next_send_date"], src.recurring["next_send_datetime"] = payload.Date, payload.Date+"T00:00:00.000000Z"
	src.steps = append(src.steps, "schedule_recurring")
	w.Header().Set("Content-Type", "application/json")
	admissionJSON(w, map[string]any{"data": src.recurring})
}

func (src *initialRestorationSource) handleEnable(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.ParseForm() != nil || len(r.PostForm) != 3 || r.PostForm.Get("object") != "clients" || r.PostForm.Get("action") != "edit" {
		src.t.Errorf("SUI edit form shape mismatch")
		http.Error(w, "bad request", 400)
		return
	}
	updated, err := sui.ParseClientRecord([]byte(r.PostForm.Get("data")))
	if err != nil || updated.ID() != 42 || updated.Name() != "42" || !updated.Enabled() || !importedOverdueSameSUIExceptEnable(src.suiRaw, updated) || !src.authorized(r.Context(), lifecycle.RenewalEnable) {
		src.t.Errorf("SUI edit or committed intent invalid: %v", err)
		http.Error(w, "invalid SUI intent", 409)
		return
	}
	src.mu.Lock()
	defer src.mu.Unlock()
	src.suiRecord = updated
	src.steps = append(src.steps, "enable_sui")
	w.Header().Set("Content-Type", "application/json")
	admissionJSON(w, map[string]any{"success": true, "obj": map[string]any{"clients": []any{map[string]any{"id": 42, "name": "42"}}}})
}

func (src *initialRestorationSource) handleResume(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/json" {
		src.t.Errorf("resume request shape mismatch")
		http.Error(w, "bad request", 400)
		return
	}
	var body map[string]json.RawMessage
	var payload struct {
		IDs    []string `json:"ids"`
		Action string   `json:"action"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 2 || json.Unmarshal(body["ids"], &payload.IDs) != nil || json.Unmarshal(body["action"], &payload.Action) != nil || !reflect.DeepEqual(payload.IDs, []string{admissionFixtureRec}) || payload.Action != "start" || !src.authorized(r.Context(), lifecycle.RenewalResume) {
		src.t.Errorf("resume payload or committed intent invalid: %v", body)
		http.Error(w, "invalid resume intent", 409)
		return
	}
	src.mu.Lock()
	src.recurring["status_id"] = "2"
	src.steps = append(src.steps, "resume_recurring")
	callback, unknown := src.onResumeSuccess, src.unknownResume
	src.mu.Unlock()
	if unknown {
		http.Error(w, `{"error":"synthetic post-mutation outage"}`, http.StatusServiceUnavailable)
		return
	}
	if callback != nil {
		if err := callback(r.Context()); err != nil {
			src.t.Errorf("resume callback failed: %v", err)
			http.Error(w, "callback failed", 500)
			return
		}
	}
	src.mu.Lock()
	defer src.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	admissionJSON(w, map[string]any{"data": []any{src.recurring}})
}

func TestImportedInitialPausedDebtRestorationWorker(t *testing.T) {
	t.Run("expired original schedule", func(t *testing.T) { runInitialRestorationWorkflow(t, false, false) })
	t.Run("empty original schedule", func(t *testing.T) { runInitialRestorationWorkflow(t, true, false) })
	t.Run("resume mutation with unknown response", func(t *testing.T) { runInitialRestorationWorkflow(t, false, true) })
}

type initialRestorationOutcomeHook func(*Store, context.Context, importadmission.PreparedPlan, *initialRestorationSource, int64, payments.Snapshot)

func runInitialRestorationWorkflow(t *testing.T, emptySchedule, unknownResume bool, hooks ...initialRestorationOutcomeHook) {
	s, ctx, prepared, settlement, cycle, importedRecurring, panel, src := newInitialRestorationRig(t, emptySchedule)
	src.unknownResume = unknownResume
	botID := prepared.BotID()
	if err := s.SyncImportAdmissionPolicy(ctx, botID, prepared.Revision(), prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
		t.Fatal(err)
	}
	if result, err := s.ApplyVerifiedImport(ctx, prepared); err != nil || result.Admitted != 1 {
		t.Fatalf("apply verified paused import: %+v %v", result, err)
	}
	if err := s.SyncImportedRuntimePermission(ctx, botID, 1, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
		t.Fatal(err)
	}
	if n, err := s.SeedPaymentChecks(ctx, botID); err != nil || n != 1 {
		t.Fatalf("seed debt payment check: %d %v", n, err)
	}
	claim, snap := claimSinglePayment(t, s, ctx, botID)
	if snap.InvoiceID != "debt-current" {
		t.Fatalf("payment snapshot invoice=%s", snap.InvoiceID)
	}
	decision, err := payments.Observe(ctx, settlement, snap, "1", time.Now().UTC().Format(time.DateOnly))
	if err != nil || decision.Kind != payments.Pending || decision.Proof != nil {
		t.Fatalf("historical anchor became debt proof: %+v %v", decision, err)
	}
	if err := s.FinishPaymentCheck(ctx, claim, snap, decision, payments.Notices{}, time.Minute); err != nil {
		t.Fatal(err)
	}
	paymentSQL(t, s, ctx, `SELECT state='overdue' AND desired_access=false FROM subscriptions WHERE client_id=$1`, snap.ClientID)
	if n, err := s.SeedRenewalOperations(ctx, botID, []int64{42}, 10); err != nil || n != 0 {
		t.Fatalf("unpaid baseline renewal seed=%d %v", n, err)
	}

	paidDate := time.Now().UTC().Format(time.DateOnly)
	src.MarkDebtPaid(paidDate)
	duePayment(t, s, ctx, snap.ClientID)
	claim, snap = claimSinglePayment(t, s, ctx, botID)
	decision, err = payments.Observe(ctx, settlement, snap, "1", paidDate)
	if err != nil || decision.Kind != payments.Verified || decision.Proof == nil || decision.Proof.InvoiceID != "debt-current" {
		t.Fatalf("current debt payment proof=%+v %v", decision, err)
	}
	if err = s.FinishPaymentCheck(ctx, claim, snap, decision, payments.Notices{}, time.Minute); err != nil {
		t.Fatal(err)
	}
	paymentSQL(t, s, ctx, `SELECT state='overdue' AND desired_access=false AND current_invoice_id='debt-current' AND first_invoice_id IS NULL FROM subscriptions WHERE client_id=$1`, snap.ClientID)
	if n, err := s.SeedRenewalOperations(ctx, botID, []int64{42}, 10); err != nil || n != 0 {
		t.Fatalf("effects-off renewal seed=%d %v", n, err)
	}
	if err = s.SyncImportedEffectPermission(ctx, botID, 1, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
		t.Fatal(err)
	}
	if n, err := s.SeedRenewalOperations(ctx, botID, []int64{42}, 10); err != nil || n != 1 {
		t.Fatalf("initial restoration seed=%d %v", n, err)
	}
	if !unknownResume {
		src.onResumeSuccess = func(callbackCtx context.Context) error {
			bounded, cancel := context.WithTimeout(callbackCtx, time.Second)
			defer cancel()
			return s.SyncImportedEffectPermission(bounded, botID, 1, prepared.PolicyHash(), prepared.SourceScopeHash(), false)
		}
	}
	ownedRecurring, err := invoiceninja.NewRecurringClient(src.baseNinja, initialRestorationNinjaToken, time.Second, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	rcfg := lifecycle.RenewalConfig{Enabled: true, ExclusiveWriter: true, BaseURL: src.baseNinja, Token: initialRestorationNinjaToken, InvoiceNinjaVersion: "5.13.47", SUIBaseURL: src.baseSUI, SUIToken: initialRestorationSUIToken, SUIVersion: "1.6.4", AllowedTelegramIDs: []int64{42}, Workers: 1, MaxAccounts: 10, PollInterval: 100 * time.Millisecond, HTTPTimeout: time.Second}
	worker, err := lifecycle.NewRenewalWorkerWithImported(s, cycle, settlement, ownedRecurring, importedRecurring, panel, botID, []int64{9001}, "1", rcfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	wantOutcome := "completed"
	if unknownResume {
		wantOutcome = "review"
	}
	waitInitialRestorationOutcome(t, ctx, worker, s, wantOutcome)
	if unknownResume {
		paymentSQL(t, s, ctx, `SELECT outcome='review' AND review_code=$1 AND imported_effect_revision=1 AND origin_mode='imported_initial_paused' AND ledger_invoice_id IS NULL FROM invoice_renewal_actions WHERE invoice_id='debt-current'`, lifecycle.ReviewRenewalUnknownWrite)
		paymentSQL(t, s, ctx, `SELECT state='overdue' AND desired_access=false AND current_invoice_id='debt-current' AND first_invoice_id IS NULL FROM subscriptions WHERE client_id=$1`, snap.ClientID)
		paymentSQL(t, s, ctx, `SELECT count(*)=3 AND count(*) FILTER (WHERE step IN ('schedule_recurring','enable_sui') AND confirmed_at IS NOT NULL)=2 AND count(*) FILTER (WHERE step='resume_recurring' AND confirmed_at IS NULL)=1 FROM invoice_renewal_steps WHERE operation_id=(SELECT operation_id FROM invoice_renewal_actions WHERE invoice_id='debt-current')`)
		var epoch int64
		var intentHashValid bool
		if err := s.pool.QueryRow(ctx, `SELECT g.epoch,i.request_hash=sha256(convert_to(st.request::text,'UTF8')) FROM activation_write_guard g JOIN invoice_renewal_actions a ON a.operation_id=g.operation_id JOIN invoice_renewal_steps st ON st.operation_id=a.operation_id AND st.step='resume_recurring' JOIN external_write_intents i ON i.operation_id=st.operation_id AND i.step=st.step AND i.origin='lifecycle' WHERE g.id=1 AND g.step='resume_recurring' AND st.confirmed_at IS NULL AND a.invoice_id='debt-current' AND a.imported_effect_revision=1`).Scan(&epoch, &intentHashValid); err != nil || epoch <= 0 || !intentHashValid {
			t.Fatalf("resume unknown intent not pinned: epoch=%d hash=%v err=%v", epoch, intentHashValid, err)
		}
		if err := s.SyncImportedEffectPermission(ctx, botID, 1, prepared.PolicyHash(), prepared.SourceScopeHash(), false); err != nil {
			t.Fatal(err)
		}
		if err := s.SyncImportedEffectPermission(ctx, botID, 2, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
			t.Fatal(err)
		}
		assertInitialRestorationQuietRestart(t, ctx, worker)
		var after int64
		if err := s.pool.QueryRow(ctx, `SELECT epoch FROM activation_write_guard WHERE id=1`).Scan(&after); err != nil || after != epoch {
			t.Fatalf("unknown guard epoch changed: before=%d after=%d err=%v", epoch, after, err)
		}
		paymentSQL(t, s, ctx, `SELECT imported_effect_revision=1 AND outcome='review' AND review_code=$1 FROM invoice_renewal_actions WHERE invoice_id='debt-current'`, lifecycle.ReviewRenewalUnknownWrite)
		paymentSQL(t, s, ctx, `SELECT revision=2 AND enabled=true FROM imported_effect_permissions WHERE bot_id=$1`, botID)
		paymentSQL(t, s, ctx, `SELECT count(*)=0 FROM invoice_lifecycle_invoices WHERE client_id=$1`, snap.ClientID)
		paymentSQL(t, s, ctx, `SELECT count(*)=0 FROM invoice_overdue_actions WHERE client_id=$1`, snap.ClientID)
		paymentSQL(t, s, ctx, `SELECT count(*)=0 FROM operations WHERE client_id=$1 AND kind IN ('register_client','activate_first_subscription')`, snap.ClientID)
		paymentSQL(t, s, ctx, `SELECT count(*)=0 FROM activation_sui_bindings WHERE client_id=$1`, snap.ClientID)
		src.mu.Lock()
		posts := append([]string(nil), src.steps...)
		recurring := src.recurring
		panelRecord := src.suiRecord
		src.mu.Unlock()
		if !reflect.DeepEqual(posts, []string{"schedule_recurring", "enable_sui", "resume_recurring"}) || recurring["status_id"] != "2" || !panelRecord.Enabled() {
			t.Fatalf("ambiguous resume source state/writes: steps=%v status=%v enabled=%v", posts, recurring["status_id"], panelRecord.Enabled())
		}
		for _, hook := range hooks {
			if hook != nil {
				hook(s, ctx, prepared, src, botID, snap)
			}
		}
		return
	}
	wantNext, err := billing.NextBillingDate(paidDate)
	if err != nil {
		t.Fatal(err)
	}
	paymentSQL(t, s, ctx, `SELECT state='active' AND desired_access=true AND current_invoice_id='debt-current' AND first_invoice_id IS NULL AND payment_date=$1::date AND next_invoice_date=$2::date FROM subscriptions WHERE client_id=$3`, paidDate, wantNext, snap.ClientID)
	paymentSQL(t, s, ctx, `SELECT count(*)=1 FROM invoice_renewal_actions a JOIN operations o ON o.id=a.operation_id WHERE a.invoice_id='debt-current' AND a.origin_mode='imported_initial_paused' AND a.ledger_invoice_id IS NULL AND a.baseline_origin_client_id=a.client_id AND a.outcome='completed' AND o.status='completed'`)
	paymentSQL(t, s, ctx, `SELECT count(*)=3 AND count(*) FILTER (WHERE confirmed_at IS NOT NULL)=3 FROM invoice_renewal_steps WHERE operation_id=(SELECT operation_id FROM invoice_renewal_actions WHERE invoice_id='debt-current')`)
	paymentSQL(t, s, ctx, `SELECT operation_id IS NULL AND step IS NULL FROM activation_write_guard WHERE id=1`)
	paymentSQL(t, s, ctx, `SELECT revision=1 AND enabled=false FROM imported_effect_permissions WHERE bot_id=$1`, botID)
	paymentSQL(t, s, ctx, `SELECT count(*)=0 FROM invoice_lifecycle_invoices WHERE client_id=$1`, snap.ClientID)
	paymentSQL(t, s, ctx, `SELECT count(*)=0 FROM invoice_overdue_actions WHERE client_id=$1`, snap.ClientID)
	paymentSQL(t, s, ctx, `SELECT count(*)=0 FROM operations WHERE client_id=$1 AND kind IN ('register_client','activate_first_subscription')`, snap.ClientID)
	paymentSQL(t, s, ctx, `SELECT count(*)=0 FROM activation_sui_bindings WHERE client_id=$1`, snap.ClientID)
	src.mu.Lock()
	posts := append([]string(nil), src.steps...)
	recurring := src.recurring
	panelRecord := src.suiRecord
	src.mu.Unlock()
	if !reflect.DeepEqual(posts, []string{"schedule_recurring", "enable_sui", "resume_recurring"}) {
		t.Fatalf("native writes=%v", posts)
	}
	if recurring["status_id"] != "2" || recurring["private_notes"] != "historical legacy note" || recurring["amount"] != 100 || recurring["next_send_date"] != wantNext || recurring["next_send_datetime"] != wantNext+"T00:00:00.000000Z" {
		t.Fatalf("legacy recurring profile mutated unexpectedly: %+v", recurring)
	}
	line, _ := recurring["line_items"].([]any)
	if len(line) != 1 || line[0].(map[string]any)["product_key"] != "legacy" || line[0].(map[string]any)["cost"] != 100 {
		t.Fatalf("legacy line item changed: %+v", line)
	}
	if !panelRecord.Enabled() || !importedOverdueSameSUIExceptEnable(src.suiRaw, panelRecord) {
		t.Fatal("SUI client identity/config changed beyond enable")
	}
	for _, hook := range hooks {
		if hook != nil {
			hook(s, ctx, prepared, src, botID, snap)
		}
	}
	if !emptySchedule {
		continueInitialRestorationWithNormalLedgerCycle(t, s, ctx, prepared, settlement, cycle, snap, src, paidDate)
	}
}

func continueInitialRestorationWithNormalLedgerCycle(t *testing.T, s *Store, ctx context.Context, prepared importadmission.PreparedPlan, settlement *invoiceninja.SettlementClient, cycle *invoiceninja.CycleReader, prior payments.Snapshot, src *initialRestorationSource, paidDate string) {
	t.Helper()
	botID := prepared.BotID()
	if err := s.SyncImportedEffectPermission(ctx, botID, 2, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
		t.Fatal(err)
	}
	src.EmitNextCycle(paidDate)
	if err := s.RecordInvoiceLifecycleWake(ctx, botID); err != nil {
		t.Fatal(err)
	}
	observed := &observedImportedLifecycleStore{Store: s, finished: make(chan error, 2)}
	cfg := lifecycle.Config{Enabled: true, BaseURL: src.baseNinja, Token: initialRestorationNinjaToken, WebhookSecret: "synthetic-initial-restoration-lifecycle-secret", AllowedTelegramIDs: []int64{42}, Workers: 1, PageSize: 10, MaxPages: 2, MaxAccounts: 10, PollInterval: 100 * time.Millisecond, RecheckInterval: 30 * time.Second, HTTPTimeout: time.Second}
	worker, err := lifecycle.New(observed, cycle, botID, []int64{9001}, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	runLifecycleOnce(t, ctx, worker, observed.finished)
	paymentSQL(t, s, ctx, `SELECT current_invoice_id='invoice-after-restoration' AND first_invoice_id IS NULL AND state='active' AND desired_access FROM subscriptions WHERE client_id=$1`, prior.ClientID)
	paymentSQL(t, s, ctx, `SELECT count(*)=1 FROM invoice_lifecycle_invoices WHERE client_id=$1 AND invoice_id='invoice-after-restoration' AND status='sent' AND sent_date=$2::date`, prior.ClientID, paidDate)
	paymentSQL(t, s, ctx, `SELECT count(*)=0 FROM invoice_lifecycle_invoices WHERE client_id=$1 AND invoice_id IN ('debt-current','paid-historical')`, prior.ClientID)
	if n, err := s.SeedPaymentChecks(ctx, botID); err != nil || n != 0 {
		t.Fatalf("new-cycle payment check seed=%d %v", n, err)
	}
	paymentSQL(t, s, ctx, `SELECT count(*)=1 FROM payment_checks WHERE client_id=$1 AND bot_id=$2`, prior.ClientID, botID)
	duePayment(t, s, ctx, prior.ClientID)
	claim, snap := claimSinglePayment(t, s, ctx, botID)
	if snap.InvoiceID != "invoice-after-restoration" {
		t.Fatalf("new-cycle payment snapshot selected %q", snap.InvoiceID)
	}
	src.MarkNextCyclePaid(paidDate)
	decision, err := payments.Observe(ctx, settlement, snap, "1", paidDate)
	if err != nil || decision.Kind != payments.Verified || decision.Proof == nil || decision.Proof.InvoiceID != "invoice-after-restoration" {
		t.Fatalf("new normal-ledger payment proof=%+v %v", decision, err)
	}
	if err = s.FinishPaymentCheck(ctx, claim, snap, decision, payments.Notices{}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if n, err := s.SeedRenewalOperations(ctx, botID, []int64{42}, 10); err != nil || n != 1 {
		t.Fatalf("normal renewal after initial restore seeded=%d %v", n, err)
	}
	paymentSQL(t, s, ctx, `SELECT count(*)=1 FROM invoice_renewal_actions WHERE invoice_id='invoice-after-restoration' AND origin_mode='ledger' AND ledger_invoice_id='invoice-after-restoration' AND baseline_origin_client_id IS NULL`)
	paymentSQL(t, s, ctx, `SELECT count(*)=1 FROM invoice_renewal_actions WHERE invoice_id='debt-current' AND origin_mode='imported_initial_paused' AND ledger_invoice_id IS NULL AND outcome='completed'`)
	paymentSQL(t, s, ctx, `SELECT count(*)=3 AND count(*) FILTER (WHERE confirmed_at IS NOT NULL)=3 FROM invoice_renewal_steps WHERE operation_id=(SELECT operation_id FROM invoice_renewal_actions WHERE invoice_id='debt-current')`)
	ops, err := s.ClaimOperations(ctx, []string{lifecycle.RenewalOperationKind}, 1, time.Minute)
	if err != nil || len(ops) != 1 {
		t.Fatalf("claim normal renewal snapshot: count=%d err=%v", len(ops), err)
	}
	normal, err := s.LoadRenewalSnapshot(ctx, ops[0], botID)
	if err != nil {
		t.Fatal(err)
	}
	if !normal.ManagedProvenance || normal.ImportedInitialPaused || normal.InvoiceID != "invoice-after-restoration" || normal.CurrentInvoiceID != "invoice-after-restoration" || !normal.ImportedOrigin.Valid() || normal.ImportedOrigin.ClientID != prior.ClientID {
		t.Fatalf("new invoice did not load as managed normal ledger renewal: %+v", normal)
	}
}

func waitInitialRestorationOutcome(t *testing.T, parent context.Context, worker *lifecycle.RenewalWorker, s *Store, want string) {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(25 * time.Second)
	defer timer.Stop()
	for {
		var outcome string
		err := s.pool.QueryRow(parent, `SELECT COALESCE((SELECT outcome FROM invoice_renewal_actions WHERE invoice_id='debt-current'),'')`).Scan(&outcome)
		if err == nil && outcome == want {
			cancel()
			if e := <-done; e != nil {
				t.Fatalf("renewal worker stopped: %v", e)
			}
			return
		}
		select {
		case <-ticker.C:
		case <-timer.C:
			cancel()
			<-done
			t.Fatalf("initial restoration timed out want=%q outcome=%q", want, outcome)
		case <-parent.Done():
			cancel()
			<-done
			t.Fatal("parent context ended during restoration")
		}
	}
}

func assertInitialRestorationQuietRestart(t *testing.T, parent context.Context, worker *lifecycle.RenewalWorker) {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	timer := time.NewTimer(750 * time.Millisecond)
	defer timer.Stop()
	select {
	case err := <-done:
		cancel()
		t.Fatalf("unknown-resume restart exited: %v", err)
	case <-timer.C:
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("unknown-resume restart stopped: %v", err)
		}
	}
}

func (src *initialRestorationSource) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/ninja/api/v1/") {
		if r.Header.Get("X-API-TOKEN") != initialRestorationNinjaToken || r.Header.Get("Token") != "" || r.Header.Get("Authorization") != "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("X-APP-VERSION", "5.13.47")
		if r.Method != http.MethodGet {
			if r.Method == http.MethodPut && r.URL.Path == "/ninja/api/v1/recurring_invoices/"+admissionFixtureRec {
				src.handleSchedule(w, r)
				return
			}
			if r.Method == http.MethodPost && r.URL.Path == "/ninja/api/v1/recurring_invoices/bulk" {
				src.handleResume(w, r)
				return
			}
			src.t.Errorf("unexpected Ninja mutation %s %s", r.Method, r.URL.Path)
			http.Error(w, "unsupported mutation", http.StatusMethodNotAllowed)
			return
		}
		src.mu.Lock()
		defer src.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/ninja/api/v1/ping":
			admissionJSON(w, map[string]any{"data": map[string]any{}})
		case "/ninja/api/v1/clients":
			admissionPage(w, []any{src.customer})
		case "/ninja/api/v1/clients/customer-42":
			admissionJSON(w, map[string]any{"data": src.settlementCustomer})
		case "/ninja/api/v1/recurring_invoices":
			admissionPage(w, []any{src.recurring})
		case "/ninja/api/v1/recurring_invoices/" + admissionFixtureRec:
			admissionJSON(w, map[string]any{"data": src.recurring})
		case "/ninja/api/v1/invoices":
			admissionPage(w, src.invoices)
		case "/ninja/api/v1/invoices/debt-current":
			admissionJSON(w, map[string]any{"data": src.debt})
		case "/ninja/api/v1/invoices/paid-historical":
			admissionJSON(w, map[string]any{"data": src.paidAnchor})
		case "/ninja/api/v1/invoices/invoice-after-restoration":
			admissionJSON(w, map[string]any{"data": src.nextCycle})
		case "/ninja/api/v1/payments":
			admissionPage(w, src.payments)
		default:
			src.t.Errorf("unexpected Ninja GET %s?%s", r.URL.Path, r.URL.RawQuery)
			http.NotFound(w, r)
		}
		return
	}
	if strings.HasPrefix(r.URL.Path, "/panel/apiv2/") {
		if r.Header.Get("Token") != initialRestorationSUIToken || r.Header.Get("X-API-TOKEN") != "" || r.Header.Get("Authorization") != "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet {
			if r.Method == http.MethodPost && r.URL.Path == "/panel/apiv2/save" {
				src.handleEnable(w, r)
				return
			}
			src.t.Errorf("unexpected SUI mutation %s %s", r.Method, r.URL.Path)
			http.Error(w, "unsupported mutation", http.StatusMethodNotAllowed)
			return
		}
		src.mu.Lock()
		defer src.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		reply := func(obj any) { admissionJSON(w, map[string]any{"success": true, "obj": obj}) }
		switch strings.TrimPrefix(r.URL.Path, "/panel/apiv2/") {
		case "status":
			reply(map[string]any{"sys": map[string]any{"appVersion": "1.6.4"}})
		case "clients":
			if r.URL.Query().Get("id") == "42" {
				reply(map[string]any{"clients": []json.RawMessage{src.suiRecord.Raw()}})
			} else if r.URL.Query().Get("id") == "" {
				reply(map[string]any{"clients": []any{map[string]any{"id": 42, "name": "42", "enable": false, "desc": "", "remark": "", "inbounds": []int{1}, "expiry": 0, "volume": 0}}})
			} else {
				http.NotFound(w, r)
			}
		case "settings":
			reply(src.settings)
		case "inbounds":
			reply(map[string]any{"inbounds": []any{map[string]any{"id": 1, "type": "vless"}}})
		default:
			src.t.Errorf("unexpected SUI GET %s?%s", r.URL.Path, r.URL.RawQuery)
			http.NotFound(w, r)
		}
		return
	}
	http.NotFound(w, r)
}
