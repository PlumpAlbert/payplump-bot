//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/billing"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/lifecycle"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

// TestImportedPaidRenewalFromNewLedgerInvoice verifies the complete imported
// happy paths: actual admission, baseline payment suppression, normal invoice
// adoption, current-invoice cash proof, and native renewal, including recovery
// from a real overdue action and an ambiguous post-mutation schedule response.
func TestImportedPaidRenewalFromNewLedgerInvoice(t *testing.T) {
	t.Run("active renewal", func(t *testing.T) { runImportedPaidRenewalHappy(t, false, false) })
	t.Run("late paid invoice restores overdue account", func(t *testing.T) { runImportedPaidRenewalHappy(t, true, false) })
	t.Run("schedule mutation with unknown response is pinned", func(t *testing.T) { runImportedPaidRenewalHappy(t, false, true) })
}

func runImportedPaidRenewalHappy(t *testing.T, late, unknownSchedule bool) {
	var newPayment map[string]any
	var renewalPosts, providerOrder []string
	var afterResumeMutation func(context.Context) error
	trig := newImportedOverdueRigWithHandler(t, false, func(src *importedOverdueSource) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := strings.TrimPrefix(r.URL.Path, "/ninja/api/v1/")
			if strings.HasPrefix(r.URL.Path, "/ninja/api/v1/") {
				w.Header().Set("X-APP-VERSION", "5.13.47")
				if r.Header.Get("X-API-TOKEN") != importedOverdueNinjaToken || r.Header.Get("Token") != "" || r.Header.Get("Authorization") != "" {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
			}
			authorized := func(step string) bool {
				var valid bool
				ctx, cancel := context.WithTimeout(r.Context(), time.Second)
				defer cancel()
				err := src.store.pool.QueryRow(ctx, `SELECT g.operation_id=a.operation_id AND g.step=$1 AND g.epoch>0 AND o.kind='invoice_paid_renewal' AND o.status='running' AND o.locked_until>clock_timestamp() AND st.dispatch_attempt=o.attempts AND st.confirmed_at IS NULL AND a.outcome='pending' AND a.invoice_id='invoice-imported-new' AND i.dispatch_attempt=st.dispatch_attempt AND i.request_hash=sha256(convert_to(st.request::text,'UTF8')) FROM activation_write_guard g JOIN operations o ON o.id=g.operation_id JOIN invoice_renewal_actions a ON a.operation_id=g.operation_id JOIN invoice_renewal_steps st ON st.operation_id=a.operation_id AND st.step=g.step JOIN external_write_intents i ON i.operation_id=st.operation_id AND i.step=st.step AND i.origin='lifecycle' WHERE g.id=1 AND g.step=$1`, step).Scan(&valid)
				return err == nil && valid
			}
			if r.Method == http.MethodGet && p == "payments" {
				src.mu.Lock()
				items := []any{src.payment}
				if newPayment != nil {
					items = append(items, newPayment)
				}
				src.mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				admissionPage(w, items)
				return
			}
			if r.Method == http.MethodPost && p == "recurring_invoices/bulk" {
				var kind string
				_ = src.store.pool.QueryRow(r.Context(), `SELECT o.kind FROM activation_write_guard g JOIN operations o ON o.id=g.operation_id WHERE g.id=1 AND g.operation_id IS NOT NULL`).Scan(&kind)
				if kind == lifecycle.OverdueOperationKind {
					src.mu.Lock()
					providerOrder = append(providerOrder, "overdue_pause")
					src.mu.Unlock()
					src.ServeHTTP(w, r)
					return
				}
				if r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/json" {
					http.Error(w, "bad request", http.StatusBadRequest)
					return
				}
				var payload map[string]json.RawMessage
				decoder := json.NewDecoder(r.Body)
				var req struct {
					IDs    []string `json:"ids"`
					Action string   `json:"action"`
				}
				if decoder.Decode(&payload) != nil || len(payload) != 2 || payload["ids"] == nil || payload["action"] == nil || json.Unmarshal(payload["ids"], &req.IDs) != nil || json.Unmarshal(payload["action"], &req.Action) != nil || !reflect.DeepEqual(req.IDs, []string{admissionFixtureRec}) || (req.Action != "stop" && req.Action != "start") {
					http.Error(w, "unexpected recurring action", http.StatusBadRequest)
					return
				}
				step, status, label := string(lifecycle.RenewalPause), "3", "pause_recurring"
				if req.Action == "start" {
					step, status, label = string(lifecycle.RenewalResume), "2", "resume_recurring"
				}
				if !authorized(step) {
					http.Error(w, "renewal intent missing", http.StatusConflict)
					return
				}
				src.mu.Lock()
				src.recurring["status_id"] = status
				body, _ := json.Marshal(map[string]any{"data": []any{src.recurring}})
				renewalPosts = append(renewalPosts, label)
				providerOrder = append(providerOrder, label)
				src.mu.Unlock()
				if req.Action == "start" && afterResumeMutation != nil {
					if err := afterResumeMutation(r.Context()); err != nil {
						http.Error(w, "synthetic permission revocation failed", http.StatusInternalServerError)
						return
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(body)
				return
			}
			if strings.HasPrefix(r.URL.Path, "/panel/apiv2/") && r.Method == http.MethodPost && strings.TrimPrefix(r.URL.Path, "/panel/apiv2/") == "save" {
				if !late {
					src.ServeHTTP(w, r)
					return
				}
				var kind string
				_ = src.store.pool.QueryRow(r.Context(), `SELECT o.kind FROM activation_write_guard g JOIN operations o ON o.id=g.operation_id WHERE g.id=1 AND g.operation_id IS NOT NULL`).Scan(&kind)
				if kind == lifecycle.OverdueOperationKind {
					src.mu.Lock()
					providerOrder = append(providerOrder, "overdue_disable")
					src.mu.Unlock()
					src.ServeHTTP(w, r)
					return
				}
				if r.Header.Get("Token") != importedOverdueSUIToken || r.Header.Get("X-API-TOKEN") != "" || r.Header.Get("Authorization") != "" || r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.ParseForm() != nil || len(r.PostForm) != 3 || r.PostForm.Get("object") != "clients" || r.PostForm.Get("action") != "edit" {
					http.Error(w, "bad SUI enable", http.StatusBadRequest)
					return
				}
				updated, err := sui.ParseClientRecord([]byte(r.PostForm.Get("data")))
				if err != nil || updated.ID() != 42 || !updated.Enabled() || !importedOverdueSameSUIExceptEnable(src.suiRaw, updated) || !authorized(string(lifecycle.RenewalEnable)) {
					http.Error(w, "SUI enable intent/record invalid", http.StatusConflict)
					return
				}
				src.mu.Lock()
				src.panel = updated
				renewalPosts = append(renewalPosts, "enable_sui")
				providerOrder = append(providerOrder, "enable_sui")
				src.mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				admissionJSON(w, map[string]any{"success": true, "obj": map[string]any{"clients": []any{map[string]any{"id": 42, "name": "42"}}}})
				return
			}
			if r.Method == http.MethodPost && r.URL.Path == "/panel/apiv2/closeSessions" {
				src.mu.Lock()
				providerOrder = append(providerOrder, "overdue_close_sessions")
				src.mu.Unlock()
			}
			if r.Method == http.MethodPut && p == "recurring_invoices/"+admissionFixtureRec {
				if r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/json" || !authorized(string(lifecycle.RenewalSchedule)) {
					http.Error(w, "schedule intent missing", http.StatusConflict)
					return
				}
				var payload map[string]json.RawMessage
				decoder := json.NewDecoder(r.Body)
				var req struct {
					Date string      `json:"next_send_date"`
					Rate json.Number `json:"exchange_rate"`
				}
				if decoder.Decode(&payload) != nil || len(payload) != 2 || payload["next_send_date"] == nil || payload["exchange_rate"] == nil || json.Unmarshal(payload["next_send_date"], &req.Date) != nil || json.Unmarshal(payload["exchange_rate"], &req.Rate) != nil || req.Date == "" || req.Rate.String() != "1" {
					http.Error(w, "unexpected schedule", http.StatusBadRequest)
					return
				}
				src.mu.Lock()
				src.recurring["next_send_date"] = req.Date
				src.recurring["next_send_datetime"] = req.Date + "T00:00:00.000000Z"
				body, _ := json.Marshal(map[string]any{"data": src.recurring})
				renewalPosts = append(renewalPosts, "schedule_recurring")
				providerOrder = append(providerOrder, "schedule_recurring")
				src.mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if unknownSchedule {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = io.WriteString(w, `{"error":"synthetic post-mutation outage"}`)
					return
				}
				_, _ = w.Write(body)
				return
			}
			src.ServeHTTP(w, r)
		})
	})
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
		t.Fatalf("seed baseline proof: %d %v", n, e)
	}
	claim, snap := claimSinglePayment(t, trig.s, ctx, botID)
	decision, e := payments.Observe(ctx, trig.settlement, snap, "1", time.Now().In(time.Local).Format(time.DateOnly))
	if e != nil || decision.Kind != payments.Verified || decision.Proof == nil {
		t.Fatalf("baseline observation: %+v %v", decision, e)
	}
	if e = trig.s.FinishPaymentCheck(ctx, claim, snap, decision, payments.Notices{}, time.Minute); e != nil {
		t.Fatal(e)
	}
	if n, e := trig.s.SeedRenewalOperations(ctx, botID, []int64{42}, 10); e != nil || n != 0 {
		t.Fatalf("baseline seeded renewal: %d %v", n, e)
	}

	trig.source.setFreshOverdueInvoice()
	if e = trig.s.RecordInvoiceLifecycleWake(ctx, botID); e != nil {
		t.Fatal(e)
	}
	observed := &observedImportedLifecycleStore{Store: trig.s, finished: make(chan error, 2)}
	lcfg := lifecycle.Config{Enabled: true, BaseURL: trig.ninjaBase, Token: importedOverdueNinjaToken, WebhookSecret: "synthetic-renewal-lifecycle-secret-012345", AllowedTelegramIDs: []int64{42}, Workers: 1, PageSize: 10, MaxPages: 2, MaxAccounts: 10, PollInterval: 100 * time.Millisecond, RecheckInterval: 30 * time.Second, HTTPTimeout: time.Second}
	lw, e := lifecycle.New(observed, trig.cycle, botID, []int64{9001}, lcfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if e != nil {
		t.Fatal(e)
	}
	runLifecycleOnce(t, ctx, lw, observed.finished)
	paymentSQL(t, trig.s, ctx, `SELECT current_invoice_id='invoice-imported-new' AND first_invoice_id IS NULL AND state='active' AND desired_access FROM subscriptions WHERE client_id=(SELECT id FROM clients WHERE telegram_id=42)`)
	var sentDate string
	if e = trig.s.pool.QueryRow(ctx, `SELECT sent_date::text FROM invoice_lifecycle_invoices WHERE invoice_id='invoice-imported-new' AND status='sent'`).Scan(&sentDate); e != nil {
		t.Fatal(e)
	}
	if sentDate != time.Now().In(time.Local).AddDate(0, 0, -1).Format(time.DateOnly) {
		t.Fatalf("new invoice sent date=%q", sentDate)
	}
	if late {
		if e = trig.s.SyncImportedEffectPermission(ctx, botID, 1, prepared.PolicyHash(), prepared.SourceScopeHash(), true); e != nil {
			t.Fatal(e)
		}
		if n, e := trig.s.SeedOverdueOperations(ctx, botID, []int64{42}, time.Now().In(time.Local).Format(time.DateOnly), 10); e != nil || n != 1 {
			t.Fatalf("overdue seed=%d err=%v", n, e)
		}
		runImportedOverdueUntil(t, ctx, trig.newEnforcement(t, false), trig.s, trig.source, "completed")
		assertImportedOverdueComplete(t, trig)
		paymentSQL(t, trig.s, ctx, `SELECT revision=1 AND enabled=true FROM imported_effect_permissions WHERE bot_id=$1`, botID)
		if got := trig.source.postSnapshot(); !reflect.DeepEqual(got, []string{"pause_recurring", "disable_sui", "close_sessions"}) {
			t.Fatalf("overdue provider writes=%v", got)
		}
		trig.source.mu.Lock()
		order := append([]string(nil), providerOrder...)
		trig.source.mu.Unlock()
		if !reflect.DeepEqual(order, []string{"overdue_pause", "overdue_disable", "overdue_close_sessions"}) {
			t.Fatalf("overdue mutation order=%v", order)
		}
	}
	paidDate := time.Now().In(time.Local).Format(time.DateOnly)
	trig.source.mu.Lock()
	trig.source.freshInvoice["status_id"], trig.source.freshInvoice["balance"], trig.source.freshInvoice["paid_to_date"] = "4", 0, 100
	newPayment = map[string]any{"id": "payment-new-42", "client_id": admissionFixtureCust, "date": paidDate, "status_id": "4", "currency_id": "1", "exchange_currency_id": "", "amount": 100, "refunded": 0, "applied": 100, "exchange_rate": 1, "is_manual": true, "is_deleted": false, "archived_at": 0, "paymentables": []any{map[string]any{"id": "allocation-new-42", "invoice_id": "invoice-imported-new", "payment_id": "payment-new-42", "amount": 100, "refunded": 0, "archived_at": 0}}, "invoices": []any{}}
	trig.source.mu.Unlock()
	claim, snap = claimSinglePayment(t, trig.s, ctx, botID)
	decision, e = payments.Observe(ctx, trig.settlement, snap, "1", time.Now().In(time.Local).Format(time.DateOnly))
	if e != nil || decision.Kind != payments.Verified || decision.Proof == nil || decision.Proof.InvoiceID != "invoice-imported-new" {
		t.Fatalf("new invoice proof: %+v %v", decision, e)
	}
	if e = trig.s.FinishPaymentCheck(ctx, claim, snap, decision, payments.Notices{}, time.Minute); e != nil {
		t.Fatal(e)
	}
	if !late {
		if n, e := trig.s.SeedRenewalOperations(ctx, botID, []int64{42}, 10); e != nil || n != 0 {
			t.Fatalf("effects-absent seed=%d err=%v", n, e)
		}
		if e = trig.s.SyncImportedEffectPermission(ctx, botID, 1, prepared.PolicyHash(), prepared.SourceScopeHash(), true); e != nil {
			t.Fatal(e)
		}
	}
	if n, e := trig.s.SeedRenewalOperations(ctx, botID, []int64{42}, 10); e != nil || n != 1 {
		t.Fatalf("effects-on seed=%d err=%v", n, e)
	}
	if !unknownSchedule {
		afterResumeMutation = func(callbackCtx context.Context) error {
			bounded, cancel := context.WithTimeout(callbackCtx, 2*time.Second)
			defer cancel()
			return trig.s.SyncImportedEffectPermission(bounded, botID, 1, prepared.PolicyHash(), prepared.SourceScopeHash(), false)
		}
	}
	importedRecurring, e := invoiceninja.NewImportedRecurringClient(trig.ninjaBase, importedOverdueNinjaToken, time.Second, 10, 2)
	if e != nil {
		t.Fatal(e)
	}
	ownedRecurring, e := invoiceninja.NewRecurringClient(trig.ninjaBase, importedOverdueNinjaToken, time.Second, 10, 2)
	if e != nil {
		t.Fatal(e)
	}
	panel, e := sui.NewManagedClient(strings.TrimSuffix(trig.ninjaBase, "/ninja")+"/panel", importedOverdueSUIToken, "1.6.4", time.Second, 10)
	if e != nil {
		t.Fatal(e)
	}
	rcfg := lifecycle.RenewalConfig{Enabled: true, ExclusiveWriter: true, BaseURL: trig.ninjaBase, Token: importedOverdueNinjaToken, InvoiceNinjaVersion: "5.13.47", SUIBaseURL: strings.TrimSuffix(trig.ninjaBase, "/ninja") + "/panel", SUIToken: importedOverdueSUIToken, SUIVersion: "1.6.4", AllowedTelegramIDs: []int64{42}, Workers: 1, MaxAccounts: 10, PollInterval: 100 * time.Millisecond, HTTPTimeout: time.Second}
	renewal, e := lifecycle.NewRenewalWorkerWithImported(trig.s, trig.cycle, trig.settlement, ownedRecurring, importedRecurring, panel, botID, []int64{9001}, "1", rcfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if e != nil {
		t.Fatal(e)
	}
	if unknownSchedule {
		runRenewalUntilReview(t, ctx, renewal, trig.s)
		var frozenRevision, guardEpoch int64
		var intentHashValid bool
		e = trig.s.pool.QueryRow(ctx, `SELECT a.imported_effect_revision,g.epoch,i.request_hash=sha256(convert_to(st.request::text,'UTF8')) FROM invoice_renewal_actions a JOIN invoice_renewal_steps st ON st.operation_id=a.operation_id AND st.step='schedule_recurring' JOIN external_write_intents i ON i.operation_id=st.operation_id AND i.step=st.step AND i.origin='lifecycle' CROSS JOIN activation_write_guard g WHERE a.invoice_id='invoice-imported-new' AND g.id=1 AND g.operation_id=a.operation_id AND g.step='schedule_recurring' AND st.confirmed_at IS NULL`).Scan(&frozenRevision, &guardEpoch, &intentHashValid)
		if e != nil || frozenRevision != 1 || guardEpoch <= 0 || !intentHashValid {
			t.Fatalf("unknown schedule frozen state rev=%d epoch=%d hash=%v err=%v", frozenRevision, guardEpoch, intentHashValid, e)
		}
		if e = trig.s.SyncImportedEffectPermission(ctx, botID, 1, prepared.PolicyHash(), prepared.SourceScopeHash(), false); e != nil {
			t.Fatal(e)
		}
		if e = trig.s.SyncImportedEffectPermission(ctx, botID, 2, prepared.PolicyHash(), prepared.SourceScopeHash(), true); e != nil {
			t.Fatal(e)
		}
		assertRenewalQuietRestart(t, ctx, renewal)
		var finalEpoch int64
		if e = trig.s.pool.QueryRow(ctx, `SELECT epoch FROM activation_write_guard WHERE id=1`).Scan(&finalEpoch); e != nil || finalEpoch != guardEpoch {
			t.Fatalf("guard epoch changed %d/%d %v", guardEpoch, finalEpoch, e)
		}
		paymentSQL(t, trig.s, ctx, `SELECT imported_effect_revision=1 AND outcome='review' FROM invoice_renewal_actions WHERE invoice_id='invoice-imported-new'`)
		paymentSQL(t, trig.s, ctx, `SELECT count(*)=0 FROM invoice_renewal_steps WHERE operation_id=(SELECT operation_id FROM invoice_renewal_actions WHERE invoice_id='invoice-imported-new') AND step IN ('enable_sui','resume_recurring')`)
		paymentSQL(t, trig.s, ctx, `SELECT EXISTS(SELECT 1 FROM activation_write_guard g JOIN invoice_renewal_actions a ON a.operation_id=g.operation_id JOIN invoice_renewal_steps st ON st.operation_id=a.operation_id AND st.step=g.step JOIN external_write_intents i ON i.operation_id=st.operation_id AND i.step=st.step AND i.origin='lifecycle' WHERE g.id=1 AND g.step='schedule_recurring' AND st.confirmed_at IS NULL AND i.request_hash=sha256(convert_to(st.request::text,'UTF8')))`)
		trig.source.mu.Lock()
		got := append([]string(nil), renewalPosts...)
		order := append([]string(nil), providerOrder...)
		status, date := trig.source.recurring["status_id"], trig.source.recurring["next_send_date"]
		trig.source.mu.Unlock()
		if !reflect.DeepEqual(got, []string{"pause_recurring", "schedule_recurring"}) || !reflect.DeepEqual(order, []string{"pause_recurring", "schedule_recurring"}) || status != "3" || date == "" {
			t.Fatalf("schedule replay/state writes=%v status=%v date=%v", got, status, date)
		}
		paymentSQL(t, trig.s, ctx, `SELECT outcome='review' AND review_code=$1 FROM invoice_renewal_actions WHERE invoice_id='invoice-imported-new'`, lifecycle.ReviewRenewalUnknownWrite)
		return
	}
	waitRenewalComplete(t, ctx, renewal, trig.s, trig.source)
	wantEnd, e := billing.NextBillingDate(paidDate)
	if e != nil {
		t.Fatal(e)
	}
	paymentSQL(t, trig.s, ctx, `SELECT current_invoice_id='invoice-imported-new' AND first_invoice_id IS NULL AND state='active' AND desired_access AND payment_date=$1::date AND next_invoice_date=$2::date FROM subscriptions WHERE client_id=(SELECT id FROM clients WHERE telegram_id=42)`, paidDate, wantEnd)
	paymentSQL(t, trig.s, ctx, `SELECT count(*)=1 FROM invoice_renewal_actions a JOIN operations o ON o.id=a.operation_id WHERE a.invoice_id='invoice-imported-new' AND a.outcome='completed' AND o.status='completed'`)
	paymentSQL(t, trig.s, ctx, `SELECT count(*)=3 AND count(*) FILTER(WHERE step IN ('pause_recurring','schedule_recurring','resume_recurring','enable_sui') AND confirmed_at IS NOT NULL)=3 FROM invoice_renewal_steps WHERE operation_id=(SELECT operation_id FROM invoice_renewal_actions WHERE invoice_id='invoice-imported-new')`)
	paymentSQL(t, trig.s, ctx, `SELECT revision=1 AND enabled=false FROM imported_effect_permissions WHERE bot_id=$1`, botID)
	paymentSQL(t, trig.s, ctx, `SELECT operation_id IS NULL AND step IS NULL FROM activation_write_guard WHERE id=1`)
	trig.source.mu.Lock()
	got := append([]string(nil), renewalPosts...)
	order := append([]string(nil), providerOrder...)
	profileOK := trig.source.recurring["status_id"] == "2" && trig.source.recurring["private_notes"] == "historical legacy note" && trig.source.recurring["amount"] == 100
	line, _ := trig.source.recurring["line_items"].([]any)
	target, datetime := trig.source.recurring["next_send_date"], trig.source.recurring["next_send_datetime"]
	panelRecord := trig.source.panel
	trig.source.mu.Unlock()
	wantPosts := []string{"pause_recurring", "schedule_recurring", "resume_recurring"}
	wantOrder := wantPosts
	if late {
		wantPosts = []string{"schedule_recurring", "enable_sui", "resume_recurring"}
		wantOrder = []string{"overdue_pause", "overdue_disable", "overdue_close_sessions", "schedule_recurring", "enable_sui", "resume_recurring"}
		if panelRecord.ID() != 42 || !panelRecord.Enabled() || !importedOverdueSameSUIExceptEnable(trig.source.suiRaw, panelRecord) {
			profileOK = false
		}
	}
	if !reflect.DeepEqual(got, wantPosts) || !reflect.DeepEqual(order, wantOrder) || !profileOK || len(line) != 1 {
		t.Fatalf("renewal source post/profile: writes=%v profile=%v", got, profileOK)
	}
	if item, ok := line[0].(map[string]any); !ok || item["cost"] != 100 || item["product_key"] != "legacy" {
		t.Fatal("imported legacy line item was changed")
	}
	wantTarget, e := billing.NextBillingDate(paidDate)
	if e != nil || target != wantTarget || datetime != wantTarget+"T00:00:00.000000Z" {
		t.Fatalf("target=%v datetime=%v want=%s err=%v", target, datetime, wantTarget, e)
	}
	paymentSQL(t, trig.s, ctx, `SELECT count(*)=0 FROM operations WHERE client_id=(SELECT id FROM clients WHERE telegram_id=42) AND kind IN ('register_client','activate_first_subscription')`)
}

func waitRenewalComplete(t *testing.T, parent context.Context, worker *lifecycle.RenewalWorker, s *Store, source *importedOverdueSource) {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	timer := time.NewTimer(25 * time.Second)
	defer timer.Stop()
	for {
		var outcome string
		err := s.pool.QueryRow(parent, `SELECT COALESCE((SELECT outcome FROM invoice_renewal_actions WHERE invoice_id='invoice-imported-new'),'')`).Scan(&outcome)
		if err == nil && outcome == "completed" {
			cancel()
			if e := <-done; e != nil {
				t.Fatalf("renewal worker stopped: %v", e)
			}
			return
		}
		select {
		case <-tick.C:
		case <-timer.C:
			cancel()
			<-done
			source.mu.Lock()
			posts := append([]string(nil), source.posts...)
			source.mu.Unlock()
			t.Fatalf("renewal timed out outcome=%q overdue_posts=%v", outcome, posts)
		case <-parent.Done():
			cancel()
			<-done
			t.Fatal("test context ended during renewal")
		}
	}
}

func runRenewalUntilReview(t *testing.T, parent context.Context, worker *lifecycle.RenewalWorker, s *Store) {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	timer := time.NewTimer(25 * time.Second)
	defer timer.Stop()
	for {
		var outcome string
		err := s.pool.QueryRow(parent, `SELECT COALESCE((SELECT outcome FROM invoice_renewal_actions WHERE invoice_id='invoice-imported-new'),'')`).Scan(&outcome)
		if err == nil && outcome == "review" {
			cancel()
			if e := <-done; e != nil {
				t.Fatalf("renewal worker stopped: %v", e)
			}
			return
		}
		select {
		case <-tick.C:
		case <-timer.C:
			cancel()
			<-done
			t.Fatalf("timed out waiting for renewal review, outcome=%q", outcome)
		case <-parent.Done():
			cancel()
			<-done
			t.Fatal("context ended waiting for review")
		}
	}
}

func assertRenewalQuietRestart(t *testing.T, parent context.Context, worker *lifecycle.RenewalWorker) {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	timer := time.NewTimer(750 * time.Millisecond)
	defer timer.Stop()
	select {
	case e := <-done:
		cancel()
		t.Fatalf("renewal restart exited: %v", e)
	case <-timer.C:
		cancel()
		if e := <-done; e != nil {
			t.Fatalf("renewal restart stopped: %v", e)
		}
	}
}
