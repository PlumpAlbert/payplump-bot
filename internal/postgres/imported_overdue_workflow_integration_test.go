//go:build integration

package postgres

import (
	"context"
	"encoding/json"
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

// This test exercises the real imported admission, payment proof, invoice
// lifecycle adoption, and overdue enforcement paths against finite loopback
// provider fakes. It never starts PostgreSQL itself; activationTestStore uses
// the repository's integration-test database fixture.
func TestImportedOverdueWorkflowLoopback(t *testing.T) {
	t.Run("effect gate defaults off then imported overdue completes", func(t *testing.T) {
		trig := newImportedOverdueRig(t, false)
		ctx := trig.ctx
		prepared, err := trig.prepare(time.Now().UTC().Add(-time.Second))
		if err != nil {
			t.Fatal(err)
		}
		botID := prepared.BotID()
		if _, err = trig.s.pool.Exec(ctx, `INSERT INTO telegram_bots(bot_id) VALUES($1) ON CONFLICT DO NOTHING`, botID); err != nil {
			t.Fatal(err)
		}
		if err = trig.s.SyncImportAdmissionPolicy(ctx, botID, prepared.Revision(), prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
			t.Fatal(err)
		}
		if _, err = trig.s.ApplyVerifiedImport(ctx, prepared); err != nil {
			t.Fatal(err)
		}
		if err = trig.s.SyncImportedRuntimePermission(ctx, botID, 1, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
			t.Fatal(err)
		}

		if n, e := trig.s.SeedPaymentChecks(ctx, botID); e != nil || n != 1 {
			t.Fatalf("seed baseline payment proof: %d %v", n, e)
		}
		claim, snap := claimSinglePayment(t, trig.s, ctx, botID)
		decision, e := payments.Observe(ctx, trig.settlement, snap, "1", time.Now().In(time.Local).Format(time.DateOnly))
		if e != nil || decision.Kind != payments.Verified || decision.Proof == nil {
			t.Fatalf("baseline payment observation: %+v %v", decision, e)
		}
		if e = trig.s.FinishPaymentCheck(ctx, claim, snap, decision, payments.Notices{}, time.Minute); e != nil {
			t.Fatal(e)
		}

		trig.source.setFreshOverdueInvoice()
		if e = trig.s.RecordInvoiceLifecycleWake(ctx, botID); e != nil {
			t.Fatal(e)
		}
		observed := &observedImportedLifecycleStore{Store: trig.s, finished: make(chan error, 2)}
		lcfg := lifecycle.Config{Enabled: true, BaseURL: trig.ninjaBase, Token: importedOverdueNinjaToken, WebhookSecret: "synthetic-imported-overdue-secret-012345", AllowedTelegramIDs: []int64{42}, Workers: 1, PageSize: 10, MaxPages: 2, MaxAccounts: 10, PollInterval: 50 * time.Millisecond, RecheckInterval: 30 * time.Second, HTTPTimeout: time.Second}
		lw, e := lifecycle.New(observed, trig.cycle, botID, []int64{9001}, lcfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if e != nil {
			t.Fatal(e)
		}
		runLifecycleOnce(t, ctx, lw, observed.finished)
		yesterday := time.Now().In(time.Local).AddDate(0, 0, -1).Format(time.DateOnly)
		paymentSQL(t, trig.s, ctx, `SELECT current_invoice_id='invoice-imported-new' AND first_invoice_id IS NULL AND state='active' AND desired_access FROM subscriptions WHERE client_id=(SELECT id FROM clients WHERE telegram_id=42)`)
		paymentSQL(t, trig.s, ctx, `SELECT count(*)=1 FROM invoice_lifecycle_invoices WHERE invoice_id='invoice-imported-new' AND status='sent' AND sent_date=$1::date`, yesterday)
		paymentSQL(t, trig.s, ctx, `SELECT count(*)=0 FROM invoice_lifecycle_invoices WHERE invoice_id IN ($1,'invoice-older-42')`, admissionFixtureInv)
		if n, e := trig.s.SeedOverdueOperations(ctx, botID, []int64{42}, time.Now().In(time.Local).Format(time.DateOnly), 10); e != nil || n != 0 {
			t.Fatalf("runtime permission alone seeded imported overdue work: %d %v", n, e)
		}
		if err = trig.s.SyncImportedEffectPermission(ctx, botID, 1, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
			t.Fatal(err)
		}
		if n, e := trig.s.SeedOverdueOperations(ctx, botID, []int64{42}, time.Now().In(time.Local).Format(time.DateOnly), 10); e != nil || n != 1 {
			t.Fatalf("seed imported overdue action: %d %v", n, e)
		}
		trig.source.onCloseBeforeResponse = func(callbackCtx context.Context) error {
			bounded, cancel := context.WithTimeout(callbackCtx, 2*time.Second)
			defer cancel()
			return trig.s.SyncImportedEffectPermission(bounded, botID, 1, prepared.PolicyHash(), prepared.SourceScopeHash(), false)
		}

		worker := trig.newEnforcement(t, false)
		runImportedOverdueUntil(t, ctx, worker, trig.s, trig.source, "completed")
		assertImportedOverdueComplete(t, trig)
		posts := trig.source.postSnapshot()
		if !reflect.DeepEqual(posts, []string{"pause_recurring", "disable_sui", "close_sessions"}) {
			t.Fatalf("provider mutation order=%v", posts)
		}
		trig.source.mu.Lock()
		paused := trig.source.recurring["status_id"] == "3" && trig.source.recurring["private_notes"] == "historical legacy note" && trig.source.recurring["amount"] == 100
		line, _ := trig.source.recurring["line_items"].([]any)
		if len(line) != 1 {
			paused = false
		} else if item, ok := line[0].(map[string]any); !ok || item["cost"] != 100 || item["notes"] != "legacy monthly" {
			paused = false
		}
		panel := trig.source.panel
		trig.source.mu.Unlock()
		if !paused || panel.ID() != 42 || panel.Enabled() || !importedOverdueSameSUIExceptEnable(trig.source.suiRaw, panel) {
			t.Fatalf("source recurring/SUI identity changed beyond pause and disable")
		}
		paymentSQL(t, trig.s, ctx, `SELECT revision=1 AND enabled=false FROM imported_effect_permissions WHERE bot_id=$1`, admissionFixtureBotID)
	})

	t.Run("mutate then 503 pins guard and restart never replays", func(t *testing.T) {
		trig := newImportedOverdueRig(t, true)
		ctx := trig.ctx
		prepared, err := trig.prepare(time.Now().UTC().Add(-time.Second))
		if err != nil {
			t.Fatal(err)
		}
		botID := prepared.BotID()
		if _, err = trig.s.pool.Exec(ctx, `INSERT INTO telegram_bots(bot_id) VALUES($1) ON CONFLICT DO NOTHING`, botID); err != nil {
			t.Fatal(err)
		}
		if err = trig.s.SyncImportAdmissionPolicy(ctx, botID, prepared.Revision(), prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
			t.Fatal(err)
		}
		if _, err = trig.s.ApplyVerifiedImport(ctx, prepared); err != nil {
			t.Fatal(err)
		}
		if err = trig.s.SyncImportedRuntimePermission(ctx, botID, 1, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
			t.Fatal(err)
		}
		if err = trig.s.SyncImportedEffectPermission(ctx, botID, 1, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
			t.Fatal(err)
		}
		if n, e := trig.s.SeedPaymentChecks(ctx, botID); e != nil || n != 1 {
			t.Fatalf("payment seed=%d %v", n, e)
		}
		claim, snap := claimSinglePayment(t, trig.s, ctx, botID)
		decision, e := payments.Observe(ctx, trig.settlement, snap, "1", time.Now().In(time.Local).Format(time.DateOnly))
		if e != nil || decision.Kind != payments.Verified || decision.Proof == nil {
			t.Fatalf("payment proof=%+v %v", decision, e)
		}
		if e = trig.s.FinishPaymentCheck(ctx, claim, snap, decision, payments.Notices{}, time.Minute); e != nil {
			t.Fatal(e)
		}
		trig.source.setFreshOverdueInvoice()
		if e = trig.s.RecordInvoiceLifecycleWake(ctx, botID); e != nil {
			t.Fatal(e)
		}
		observed := &observedImportedLifecycleStore{Store: trig.s, finished: make(chan error, 2)}
		lcfg := lifecycle.Config{Enabled: true, BaseURL: trig.ninjaBase, Token: importedOverdueNinjaToken, WebhookSecret: "synthetic-imported-overdue-secret-012345", AllowedTelegramIDs: []int64{42}, Workers: 1, PageSize: 10, MaxPages: 2, MaxAccounts: 10, PollInterval: 50 * time.Millisecond, RecheckInterval: 30 * time.Second, HTTPTimeout: time.Second}
		lw, e := lifecycle.New(observed, trig.cycle, botID, []int64{9001}, lcfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if e != nil {
			t.Fatal(e)
		}
		runLifecycleOnce(t, ctx, lw, observed.finished)
		if n, e := trig.s.SeedOverdueOperations(ctx, botID, []int64{42}, time.Now().In(time.Local).Format(time.DateOnly), 10); e != nil || n != 1 {
			t.Fatalf("overdue seed=%d %v", n, e)
		}
		worker := trig.newEnforcement(t, true)
		runImportedOverdueUntil(t, ctx, worker, trig.s, trig.source, "review")
		assertImportedOverdueUnknown(t, trig)
		var guardEpochBefore int64
		if err := trig.s.pool.QueryRow(ctx, `SELECT g.epoch FROM activation_write_guard g JOIN invoice_overdue_actions a ON a.operation_id=g.operation_id WHERE a.invoice_id='invoice-imported-new' AND g.id=1`).Scan(&guardEpochBefore); err != nil {
			t.Fatal(err)
		}
		if err := trig.s.SyncImportedEffectPermission(ctx, botID, 1, prepared.PolicyHash(), prepared.SourceScopeHash(), false); err != nil {
			t.Fatal(err)
		}
		if err := trig.s.SyncImportedEffectPermission(ctx, botID, 2, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
			t.Fatal(err)
		}
		paymentSQL(t, trig.s, ctx, `SELECT revision=2 AND enabled=true FROM imported_effect_permissions WHERE bot_id=$1`, botID)
		var guardEpochAfter int64
		if err := trig.s.pool.QueryRow(ctx, `SELECT g.epoch FROM activation_write_guard g JOIN invoice_overdue_actions a ON a.operation_id=g.operation_id WHERE a.invoice_id='invoice-imported-new' AND g.id=1`).Scan(&guardEpochAfter); err != nil || guardEpochAfter != guardEpochBefore {
			t.Fatalf("permission epoch changed unknown global guard: before=%d after=%d err=%v", guardEpochBefore, guardEpochAfter, err)
		}
		var frozenRevision int64
		if err := trig.s.pool.QueryRow(ctx, `SELECT imported_effect_revision FROM invoice_overdue_actions WHERE invoice_id='invoice-imported-new'`).Scan(&frozenRevision); err != nil || frozenRevision != 1 {
			t.Fatalf("permission epoch rewrote frozen action revision: %d %v", frozenRevision, err)
		}
		worker = trig.newEnforcement(t, true)
		runWorkerQuiet(t, ctx, worker)
		assertImportedOverdueUnknown(t, trig)
		if got := trig.source.postSnapshot(); !reflect.DeepEqual(got, []string{"pause_recurring"}) {
			t.Fatalf("unknown pause was replayed or later writes dispatched: %v", got)
		}
		trig.source.mu.Lock()
		mutated := trig.source.recurring["status_id"] == "3"
		trig.source.mu.Unlock()
		if !mutated {
			t.Fatal("fake source did not preserve mutation behind the 503")
		}
	})
}

const importedOverdueNinjaToken = "synthetic-ninja-token"
const importedOverdueSUIToken = "synthetic-sui-token"

func TestImportedPermissionRevisionFences(t *testing.T) {
	s, ctx := activationTestStore(t)
	const botA, botB int64 = 989001, 989002
	if _, err := s.pool.Exec(ctx, `INSERT INTO telegram_bots(bot_id) VALUES($1),($2) ON CONFLICT DO NOTHING`, botA, botB); err != nil {
		t.Fatal(err)
	}
	policy, scope := strings.Repeat("a", 64), strings.Repeat("b", 64)
	apis := []struct {
		name, table string
		sync        func(context.Context, int64, int64, string, string, bool) error
	}{
		{"admission", "import_admission_policy", s.SyncImportAdmissionPolicy},
		{"runtime", "imported_runtime_permissions", s.SyncImportedRuntimePermission},
		{"effects", "imported_effect_permissions", s.SyncImportedEffectPermission},
	}
	for _, api := range apis {
		t.Run(api.name, func(t *testing.T) {
			if err := api.sync(ctx, botA, 1, policy, scope, false); err != nil {
				t.Fatalf("create default-off revision 1: %v", err)
			}
			if err := api.sync(ctx, botA, 1, policy, scope, true); err == nil {
				t.Fatal("same-revision re-enable after revocation was accepted")
			}
			if err := api.sync(ctx, botA, 2, policy, scope, true); err != nil {
				t.Fatalf("revision 2 enable: %v", err)
			}
			if err := api.sync(ctx, botA, 2, policy, scope, false); err != nil {
				t.Fatalf("revision 2 disable: %v", err)
			}
			if err := api.sync(ctx, botA, 2, policy, scope, true); err == nil {
				t.Fatal("same-revision re-enable after second revocation was accepted")
			}
			if err := api.sync(ctx, botA, 3, policy, scope, true); err != nil {
				t.Fatalf("revision 3 enable: %v", err)
			}
			if err := api.sync(ctx, botA, 2, policy, scope, false); err == nil {
				t.Fatal("lower permission revision was accepted")
			}
			if err := api.sync(ctx, botA, 3, strings.Repeat("c", 64), scope, true); err == nil {
				t.Fatal("same-revision policy hash drift was accepted")
			}
			if err := api.sync(ctx, botA, 3, policy, strings.Repeat("d", 64), true); err == nil {
				t.Fatal("same-revision source-scope drift was accepted")
			}
			if _, err := s.pool.Exec(ctx, `DELETE FROM `+api.table+` WHERE bot_id=$1`, botA); err == nil {
				t.Fatal("raw permission deletion was accepted")
			}
			if _, err := s.pool.Exec(ctx, `UPDATE `+api.table+` SET bot_id=$2 WHERE bot_id=$1`, botA, botB); err == nil {
				t.Fatal("raw permission bot identity change was accepted")
			}
			var preserved bool
			if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+api.table+` WHERE bot_id=$1)`, botA).Scan(&preserved); err != nil || !preserved {
				t.Fatalf("failed raw mutation changed permission identity: preserved=%v err=%v", preserved, err)
			}
		})
	}
}

type importedOverdueRig struct {
	s          *Store
	ctx        context.Context
	prepare    func(time.Time) (importadmission.PreparedPlan, error)
	settlement *invoiceninja.SettlementClient
	cycle      *invoiceninja.CycleReader
	ninjaBase  string
	source     *importedOverdueSource
	unknown    bool
}

func newImportedOverdueRig(t *testing.T, unknown bool) *importedOverdueRig {
	t.Helper()
	s, ctx := activationTestStore(t)
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
	older := map[string]any{"id": "invoice-older-42", "client_id": admissionFixtureCust, "recurring_id": admissionFixtureRec, "number": "N-41", "private_notes": "older historic private note", "status_id": "4", "date": olderDate, "due_date": olderDate, "amount": 100, "balance": 0, "paid_to_date": 100, "exchange_rate": 1, "is_deleted": false, "archived_at": 0, "updated_at": 0, "invitations": []any{}}
	customer := map[string]any{"id": admissionFixtureCust, "id_number": admissionFixtureTG, "private_notes": "legacy customer", "is_deleted": false, "archived_at": 0}
	settlementCustomer := map[string]any{"id": admissionFixtureCust, "id_number": admissionFixtureTG, "group_settings_id": "", "settings": map[string]any{"currency_id": "1"}, "currency_id": "ignored", "is_deleted": false, "archived_at": 0}
	allocation := map[string]any{"id": "allocation-42", "invoice_id": admissionFixtureInv, "payment_id": "payment-42", "amount": 100, "refunded": 0, "archived_at": 0}
	payment := map[string]any{"id": "payment-42", "client_id": admissionFixtureCust, "date": start, "status_id": "4", "currency_id": "1", "exchange_currency_id": "", "amount": 100, "refunded": 0, "applied": 100, "exchange_rate": 1, "is_manual": true, "is_deleted": false, "archived_at": 0, "paymentables": []any{allocation}, "invoices": []any{}}
	f := &importedOverdueSource{t: t, store: s, botID: admissionFixtureBotID, recurring: recurring, invoices: []any{older, invoice}, current: invoice, older: older, payment: payment, customer: customer, settlementCustomer: settlementCustomer, suiRaw: suiRaw, unknown: unknown}
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	imp, err := invoiceninja.NewImportReader(server.URL+"/ninja", importedOverdueNinjaToken, time.Second, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := invoiceninja.NewRecurringReader(server.URL+"/ninja", importedOverdueNinjaToken, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cycle, err := invoiceninja.NewCycleReader(server.URL+"/ninja", importedOverdueNinjaToken, time.Second, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	settle, err := invoiceninja.NewSettlementClient(server.URL+"/ninja", importedOverdueNinjaToken, time.Second, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := sui.NewIdentityReader(server.URL+"/panel", importedOverdueSUIToken, "1.6.4", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	records, err := sui.NewRecordReader(server.URL+"/panel", importedOverdueSUIToken, "1.6.4", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := sui.New(server.URL+"/panel", importedOverdueSUIToken, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := importverify.SourceScope(server.URL+"/ninja", importedOverdueNinjaToken, "5.13.47", server.URL+"/panel", importedOverdueSUIToken, "1.6.4")
	if err != nil {
		t.Fatal(err)
	}
	policy := importverify.Policy{BotID: admissionFixtureBotID, SourceScopeHash: scope, PilotIDs: []string{admissionFixtureTG}, NinjaVersion: "5.13.47", SUIVersion: "1.6.4", NativeCurrencyID: "1", NativeCurrencyCode: "USD", ProductKey: "legacy", PublicSubscriptionBase: "https://subs.invalid/", ExpectedInboundIDs: []int64{1}, MaxRecords: 10, MaxInventoryRecords: 20, MaxPages: 2, MaxHistoricalAnchors: 5, EvidenceTTL: 5 * time.Minute}
	service, err := importverify.New(policy, importverify.Providers{Customers: imp, Recurring: rec, Invoices: cycle, Payments: settle, Identities: ids, Records: records, Settings: settings})
	if err != nil {
		t.Fatal(err)
	}
	claim := importplan.Record{TelegramID: admissionFixtureTG, InvoiceCustomerID: admissionFixtureCust, InvoiceNumber: admissionFixtureTG, CurrentInvoiceID: admissionFixtureInv, InvoiceStatus: "paid", RecurringID: admissionFixtureRec, SUIID: admissionFixtureTG, SUIName: admissionFixtureTG, ConfigHash: configHash, PeriodStart: start, PeriodEnd: end, Currency: "USD", Lifecycle: "active", SourceVersion: "5.13.47", PinnedProvenance: true, CompleteProof: true, Amount: "100"}
	plan := importplan.Analyze(importplan.Manifest{Records: []importplan.Record{claim}})
	if plan.Imported != 1 {
		t.Fatalf("fixture is not admissible: %v", plan.Reasons)
	}
	prepare := func(at time.Time) (importadmission.PreparedPlan, error) {
		return importadmission.Prepare(ctx, service, plan, plan.Hash, 1, at)
	}
	return &importedOverdueRig{s: s, ctx: ctx, prepare: prepare, settlement: settle, cycle: cycle, ninjaBase: server.URL + "/ninja", source: f}
}

type importedOverdueSource struct {
	t                                                                   *testing.T
	store                                                               *Store
	botID                                                               int64
	mu                                                                  sync.Mutex
	recurring                                                           map[string]any
	invoices                                                            []any
	current, freshInvoice, older, payment, customer, settlementCustomer map[string]any
	suiRaw                                                              json.RawMessage
	unknown                                                             bool
	onCloseBeforeResponse                                               func(context.Context) error
	panel                                                               sui.ClientRecord
	posts                                                               []string
}

func (f *importedOverdueSource) setFreshOverdueInvoice() {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := time.Now().In(time.Local).AddDate(0, 0, -1).Format(time.DateOnly)
	tomorrow := time.Now().In(time.Local).AddDate(0, 0, 3).Format(time.DateOnly)
	v := map[string]any{"id": "invoice-imported-new", "client_id": admissionFixtureCust, "recurring_id": admissionFixtureRec, "number": "N-43", "private_notes": "historical legacy note", "status_id": "2", "date": d, "due_date": tomorrow, "amount": 100, "balance": 100, "paid_to_date": 0, "exchange_rate": 1, "is_deleted": false, "archived_at": 0, "updated_at": 0, "invitations": []any{map[string]any{"id": "invitation-new-42", "sent_date": d + " 10:30:00"}}}
	f.freshInvoice = v
	f.invoices = append(f.invoices, v)
}
func (f *importedOverdueSource) postSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.posts...)
}
func (f *importedOverdueSource) visibleIntent(ctx context.Context, step lifecycle.EnforcementStep, external string, confirmed int, state string, access bool) bool {
	var valid bool
	err := f.store.pool.QueryRow(ctx, `SELECT g.operation_id=o.id AND g.step=st.step AND g.epoch>0 AND o.kind=$1 AND o.status='running' AND o.locked_until>clock_timestamp() AND a.outcome='pending' AND a.review_code=0 AND st.dispatch_attempt=o.attempts AND st.external_id IS NULL AND st.confirmed_at IS NULL AND st.request->'before'->>'id'=$5 AND sub.state=$6 AND sub.desired_access=$7 AND (SELECT count(*) FROM invoice_overdue_steps x WHERE x.operation_id=o.id AND x.confirmed_at IS NOT NULL)=$8 AND (SELECT count(*) FROM invoice_overdue_steps x WHERE x.operation_id=o.id)=$8+1 AND (SELECT count(*) FROM external_write_intents e WHERE e.operation_id=o.id AND e.origin='lifecycle')=$8+1 AND EXISTS(SELECT 1 FROM external_write_intents i WHERE i.operation_id=st.operation_id AND i.step=st.step AND i.origin='lifecycle' AND i.dispatch_attempt=st.dispatch_attempt AND i.request_hash=sha256(convert_to(st.request::text,'UTF8'))) AND ($3<>'pause_recurring' OR COALESCE(st.request->'profile'->>'hash','')<>'') FROM activation_write_guard g JOIN operations o ON o.id=g.operation_id JOIN invoice_overdue_actions a ON a.operation_id=o.id JOIN invoice_overdue_steps st ON st.operation_id=o.id AND st.step=g.step JOIN subscriptions sub ON sub.client_id=a.client_id WHERE g.id=1 AND a.bot_id=$2 AND a.invoice_id='invoice-imported-new' AND g.step=$3 AND st.request->'before'->>'id'=$4`, lifecycle.OverdueOperationKind, f.botID, string(step), external, external, state, access, confirmed).Scan(&valid)
	return err == nil && valid
}
func (f *importedOverdueSource) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if strings.HasPrefix(r.URL.Path, "/ninja/api/v1/") {
		if r.Header.Get("X-API-TOKEN") != importedOverdueNinjaToken || r.Header.Get("Token") != "" || r.Header.Get("Authorization") != "" {
			f.t.Errorf("Invoice Ninja credentials crossed source boundary")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("X-APP-VERSION", "5.13.47")
		f.serveNinja(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/panel/apiv2/") {
		if r.Header.Get("Token") != importedOverdueSUIToken || r.Header.Get("X-API-TOKEN") != "" || r.Header.Get("Authorization") != "" {
			f.t.Errorf("S-UI credentials crossed source boundary")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		f.servePanel(w, r)
		return
	}
	http.NotFound(w, r)
}
func (f *importedOverdueSource) serveNinja(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/ninja/api/v1/")
	if r.Method == http.MethodGet {
		switch {
		case p == "clients":
			admissionPage(w, []any{f.customer})
		case p == "clients/"+admissionFixtureCust:
			admissionJSON(w, map[string]any{"data": f.settlementCustomer})
		case p == "recurring_invoices":
			admissionPage(w, []any{f.recurring})
		case p == "recurring_invoices/"+admissionFixtureRec:
			admissionJSON(w, map[string]any{"data": f.recurring})
		case p == "invoices":
			admissionPage(w, f.invoices)
		case p == "invoices/"+admissionFixtureInv:
			admissionJSON(w, map[string]any{"data": f.current})
		case p == "invoices/invoice-older-42":
			admissionJSON(w, map[string]any{"data": f.older})
		case p == "invoices/invoice-imported-new":
			admissionJSON(w, map[string]any{"data": f.freshInvoice})
		case p == "payments":
			admissionPage(w, []any{f.payment})
		case p == "ping":
			admissionJSON(w, map[string]any{"data": map[string]any{}})
		default:
			f.t.Errorf("unexpected Ninja GET %s?%s", r.URL.Path, r.URL.RawQuery)
			http.NotFound(w, r)
		}
		return
	}
	if r.Method == http.MethodPost && p == "recurring_invoices/bulk" {
		if r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/json" {
			f.t.Errorf("unexpected imported pause request format")
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var x struct {
			IDs    []string `json:"ids"`
			Action string   `json:"action"`
		}
		_ = json.NewDecoder(r.Body).Decode(&x)
		if !reflect.DeepEqual(x.IDs, []string{admissionFixtureRec}) || x.Action != "stop" {
			f.t.Errorf("unexpected imported pause request: %+v", x)
			http.Error(w, "bad request", 400)
			return
		}
		if !f.visibleIntent(r.Context(), lifecycle.OverduePauseRecurring, admissionFixtureRec, 0, "active", true) {
			f.t.Errorf("imported pause preceded committed intent and global writer guard")
			http.Error(w, "intent missing", http.StatusConflict)
			return
		}
		f.posts = append(f.posts, "pause_recurring")
		f.recurring["status_id"] = "3"
		if f.unknown {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":"synthetic temporary response"}`)
			return
		}
		admissionJSON(w, map[string]any{"data": []any{f.recurring}})
		return
	}
	f.t.Errorf("unexpected Ninja %s %s", r.Method, r.URL.Path)
	http.NotFound(w, r)
}
func importedOverdueSameSUIExceptEnable(raw json.RawMessage, updated sui.ClientRecord) bool {
	before, err := sui.ParseClientRecord(raw)
	if err != nil {
		return false
	}
	var a, b map[string]json.RawMessage
	if json.Unmarshal(before.Raw(), &a) != nil || json.Unmarshal(updated.Raw(), &b) != nil {
		return false
	}
	a["enable"], b["enable"] = json.RawMessage("false"), json.RawMessage("false")
	return reflect.DeepEqual(a, b)
}

func (f *importedOverdueSource) servePanel(w http.ResponseWriter, r *http.Request) {
	reply := func(obj any) { admissionJSON(w, map[string]any{"success": true, "obj": obj}) }
	p := strings.TrimPrefix(r.URL.Path, "/panel/apiv2/")
	if r.Method == http.MethodGet {
		switch p {
		case "status":
			if r.URL.RawQuery != "r=sys" {
				f.t.Errorf("unexpected SUI version query %q", r.URL.RawQuery)
				http.Error(w, "bad query", http.StatusBadRequest)
				return
			}
			reply(map[string]any{"sys": map[string]any{"appVersion": "1.6.4"}})
		case "clients":
			if r.URL.Query().Get("id") == "42" {
				record := f.suiRaw
				if f.panel.ID() == 42 {
					record = f.panel.Raw()
				}
				reply(map[string]any{"clients": []json.RawMessage{record}})
			} else if r.URL.Query().Get("id") == "" {
				reply(map[string]any{"clients": []any{map[string]any{"id": 42, "name": "42", "enable": true, "desc": "", "remark": "", "inbounds": []int{1}, "expiry": 0, "volume": 0}}})
			} else {
				http.NotFound(w, r)
			}
		case "settings":
			reply(map[string]any{"subURI": "https://subs.invalid/", "subPath": "private-path", "subPort": "443", "subDomain": ""})
		case "inbounds":
			reply(map[string]any{"inbounds": []any{map[string]any{"id": 1, "type": "vless"}}})
		default:
			http.NotFound(w, r)
		}
		return
	}
	if p == "save" && r.Method == http.MethodPost {
		if r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.ParseForm() != nil || len(r.PostForm) != 3 || r.PostForm.Get("object") != "clients" || r.PostForm.Get("action") != "edit" {
			f.t.Errorf("unexpected SUI disable request format")
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var raw json.RawMessage
		raw = []byte(r.PostForm.Get("data"))
		updated, e := sui.ParseClientRecord(raw)
		if e != nil || updated.ID() != 42 || updated.Name() != "42" || updated.Enabled() || !importedOverdueSameSUIExceptEnable(f.suiRaw, updated) {
			f.t.Errorf("unexpected SUI edit: %v", e)
			http.Error(w, "bad", 400)
			return
		}
		if !f.visibleIntent(r.Context(), lifecycle.OverdueDisableSUI, "42", 1, "active", true) {
			f.t.Errorf("SUI disable preceded committed intent and global writer guard")
			http.Error(w, "intent missing", http.StatusConflict)
			return
		}
		f.panel = updated
		f.posts = append(f.posts, "disable_sui")
		reply(map[string]any{"clients": []any{map[string]any{"id": 42, "name": "42"}}})
		return
	}
	if p == "closeSessions" && r.Method == http.MethodPost {
		if r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.ParseForm() != nil || len(r.PostForm) != 1 || r.PostForm.Get("u") != "42" || f.panel.Enabled() || !f.visibleIntent(r.Context(), lifecycle.OverdueCloseSessions, "42", 2, "overdue", false) {
			f.t.Errorf("closeSessions was not bound to disabled SUI with committed intent")
			http.Error(w, "intent missing", http.StatusConflict)
			return
		}
		f.posts = append(f.posts, "close_sessions")
		if f.onCloseBeforeResponse != nil {
			if err := f.onCloseBeforeResponse(r.Context()); err != nil {
				f.t.Errorf("disable imported effect permission after known close mutation: %v", err)
				http.Error(w, "synthetic permission update failed", http.StatusInternalServerError)
				return
			}
		}
		admissionJSON(w, map[string]any{"success": true, "msg": "ok"})
		return
	}
	f.t.Errorf("unexpected SUI %s %s", r.Method, r.URL.Path)
	http.NotFound(w, r)
}

func (r *importedOverdueRig) newEnforcement(t *testing.T, unknown bool) *lifecycle.EnforcementWorker {
	t.Helper()
	rc, e := invoiceninja.NewImportedRecurringClient(r.ninjaBase, importedOverdueNinjaToken, time.Second, 10, 2)
	if e != nil {
		t.Fatal(e)
	}
	owned, e := invoiceninja.NewRecurringClient(r.ninjaBase, importedOverdueNinjaToken, time.Second, 10, 2)
	if e != nil {
		t.Fatal(e)
	}
	panel, e := sui.NewManagedClient(strings.TrimSuffix(r.ninjaBase, "/ninja")+"/panel", importedOverdueSUIToken, "1.6.4", time.Second, 10)
	if e != nil {
		t.Fatal(e)
	}
	r.source.unknown = unknown
	cfg := lifecycle.EnforcementConfig{Enabled: true, ExclusiveWriter: true, BaseURL: r.ninjaBase, Token: importedOverdueNinjaToken, InvoiceNinjaVersion: "5.13.47", SUIBaseURL: strings.TrimSuffix(r.ninjaBase, "/ninja") + "/panel", SUIToken: importedOverdueSUIToken, SUIVersion: "1.6.4", AllowedTelegramIDs: []int64{42}, Workers: 1, MaxAccounts: 10, PollInterval: 50 * time.Millisecond, HTTPTimeout: time.Second}
	w, e := lifecycle.NewEnforcementWorkerWithImported(r.s, r.cycle, owned, rc, panel, admissionFixtureBotID, []int64{9001}, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if e != nil {
		t.Fatal(e)
	}
	return w
}

func runLifecycleOnce(t *testing.T, parent context.Context, worker *lifecycle.Worker, finished <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	select {
	case err := <-finished:
		cancel()
		if runErr := <-done; runErr != nil {
			t.Fatalf("lifecycle worker stopped: %v", runErr)
		}
		if err != nil {
			t.Fatalf("lifecycle finalization: %v", err)
		}
	case <-time.After(15 * time.Second):
		cancel()
		<-done
		t.Fatal("timed out waiting for imported invoice lifecycle adoption")
	case <-parent.Done():
		cancel()
		<-done
		t.Fatal("context ended waiting for imported invoice lifecycle adoption")
	}
}

func runImportedOverdueUntil(t *testing.T, parent context.Context, worker *lifecycle.EnforcementWorker, s *Store, source *importedOverdueSource, want string) {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		var outcome string
		err := s.pool.QueryRow(parent, `SELECT COALESCE((SELECT outcome FROM invoice_overdue_actions WHERE invoice_id='invoice-imported-new'),'')`).Scan(&outcome)
		if err != nil {
			cancel()
			<-done
			t.Fatal("read imported overdue outcome")
		}
		if outcome == want {
			cancel()
			if e := <-done; e != nil {
				t.Fatalf("enforcement worker stopped: %v", e)
			}
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			cancel()
			<-done
			t.Fatalf("timed out waiting for imported overdue outcome %q (got %q, posts=%v)", want, outcome, source.postSnapshot())
		case <-parent.Done():
			cancel()
			<-done
			t.Fatal("context ended waiting for overdue action")
		}
	}
}

func runWorkerQuiet(t *testing.T, parent context.Context, worker *lifecycle.EnforcementWorker) {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	timer := time.NewTimer(750 * time.Millisecond)
	defer timer.Stop()
	select {
	case e := <-done:
		cancel()
		t.Fatalf("restart worker exited unexpectedly: %v", e)
	case <-timer.C:
		cancel()
		if e := <-done; e != nil {
			t.Fatalf("restart worker stopped: %v", e)
		}
	}
}

func assertImportedOverdueComplete(t *testing.T, r *importedOverdueRig) {
	t.Helper()
	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()
	var outcome, opStatus, state string
	var review, steps, confirmed, intents int
	var access, busy bool
	err := r.s.pool.QueryRow(ctx, `SELECT a.outcome,a.review_code,o.status,s.state,s.desired_access,(SELECT count(*) FROM invoice_overdue_steps x WHERE x.operation_id=a.operation_id),(SELECT count(*) FROM invoice_overdue_steps x WHERE x.operation_id=a.operation_id AND x.confirmed_at IS NOT NULL),(SELECT count(*) FROM external_write_intents e WHERE e.operation_id=a.operation_id AND e.origin='lifecycle'),EXISTS(SELECT 1 FROM activation_write_guard g WHERE g.id=1 AND g.operation_id IS NOT NULL) FROM invoice_overdue_actions a JOIN operations o ON o.id=a.operation_id JOIN subscriptions s ON s.client_id=a.client_id WHERE a.invoice_id='invoice-imported-new'`).Scan(&outcome, &review, &opStatus, &state, &access, &steps, &confirmed, &intents, &busy)
	if err != nil || outcome != "completed" || review != 0 || opStatus != "completed" || state != "overdue" || access || steps != 3 || confirmed != 3 || intents != 3 || busy {
		t.Fatalf("imported completion outcome=%q review=%d op=%q state=%q access=%v steps=%d confirmed=%d intents=%d busy=%v err=%v", outcome, review, opStatus, state, access, steps, confirmed, intents, busy, err)
	}
	paymentSQL(t, r.s, ctx, `SELECT count(*)=0 FROM operations WHERE client_id=(SELECT id FROM clients WHERE telegram_id=42) AND kind IN ('register_client','activate_first_subscription')`)
	paymentSQL(t, r.s, ctx, `SELECT count(*)=0 FROM registration_flows f JOIN operations o ON o.id=f.operation_id WHERE o.client_id=(SELECT id FROM clients WHERE telegram_id=42)`)
	paymentSQL(t, r.s, ctx, `SELECT count(*)=0 FROM activation_flows f JOIN operations o ON o.id=f.operation_id WHERE o.client_id=(SELECT id FROM clients WHERE telegram_id=42)`)
	paymentSQL(t, r.s, ctx, `SELECT count(*)=0 FROM activation_sui_bindings WHERE client_id=(SELECT id FROM clients WHERE telegram_id=42)`)
	paymentSQL(t, r.s, ctx, `SELECT current_invoice_id='invoice-imported-new' AND first_invoice_id IS NULL FROM subscriptions WHERE client_id=(SELECT id FROM clients WHERE telegram_id=42)`)
	paymentSQL(t, r.s, ctx, `SELECT count(*)=3 AND count(*) FILTER (WHERE step='pause_recurring' AND external_id=$1 AND confirmed_at IS NOT NULL)=1 AND count(*) FILTER (WHERE step='disable_sui' AND external_id='42' AND confirmed_at IS NOT NULL)=1 AND count(*) FILTER (WHERE step='close_sessions' AND external_id='42' AND confirmed_at IS NOT NULL)=1 FROM invoice_overdue_steps WHERE operation_id=(SELECT operation_id FROM invoice_overdue_actions WHERE invoice_id='invoice-imported-new')`, admissionFixtureRec)
	paymentSQL(t, r.s, ctx, `SELECT revision=1 AND enabled=false FROM imported_effect_permissions WHERE bot_id=$1`, admissionFixtureBotID)
	paymentSQL(t, r.s, ctx, `SELECT count(*)=2 FROM outbox WHERE bot_id=$1 AND message_key LIKE 'invoice-overdue:%:overdue:%'`, admissionFixtureBotID)
	var first string
	_ = r.s.pool.QueryRow(ctx, `SELECT first_invoice_id FROM subscriptions WHERE client_id=(SELECT id FROM clients WHERE telegram_id=42)`).Scan(&first)
	if first != "" {
		t.Fatalf("import fabricated first invoice: %q", first)
	}
}

func assertImportedOverdueUnknown(t *testing.T, r *importedOverdueRig) {
	t.Helper()
	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()
	var outcome, opStatus, state, guardStep string
	var review, intents, unconfirmed int
	var epoch, frozenRevision int64
	var access, guard, journalValid bool
	err := r.s.pool.QueryRow(ctx, `SELECT a.outcome,a.review_code,o.status,s.state,s.desired_access,(g.operation_id=a.operation_id),COALESCE(g.step,''),g.epoch,a.imported_effect_revision,(SELECT count(*) FROM external_write_intents e WHERE e.operation_id=a.operation_id AND e.origin='lifecycle'),(SELECT count(*) FROM invoice_overdue_steps x WHERE x.operation_id=a.operation_id AND x.confirmed_at IS NULL),EXISTS(SELECT 1 FROM invoice_overdue_steps st JOIN external_write_intents i USING(operation_id,step) WHERE st.operation_id=a.operation_id AND st.step='pause_recurring' AND i.origin='lifecycle' AND i.request_hash=sha256(convert_to(st.request::text,'UTF8'))) FROM invoice_overdue_actions a JOIN operations o ON o.id=a.operation_id JOIN subscriptions s ON s.client_id=a.client_id CROSS JOIN activation_write_guard g WHERE a.invoice_id='invoice-imported-new' AND g.id=1`).Scan(&outcome, &review, &opStatus, &state, &access, &guard, &guardStep, &epoch, &frozenRevision, &intents, &unconfirmed, &journalValid)
	if err != nil || outcome != "review" || review != lifecycle.ReviewOverdueUnknownWrite || opStatus != "completed" || state != "active" || !access || !guard || guardStep != "pause_recurring" || epoch <= 0 || frozenRevision != 1 || intents != 1 || unconfirmed != 1 || !journalValid {
		t.Fatalf("unknown imported pause outcome=%q review=%d op=%q state=%q access=%v guard=%v/%q epoch=%d frozen_revision=%d intents=%d unconfirmed=%d journal_valid=%v err=%v", outcome, review, opStatus, state, access, guard, guardStep, epoch, frozenRevision, intents, unconfirmed, journalValid, err)
	}
	paymentSQL(t, r.s, ctx, `SELECT count(*)=1 FROM outbox WHERE bot_id=$1 AND message_key LIKE 'invoice-overdue:%:review:%' AND chat_id=9001`, admissionFixtureBotID)
}
