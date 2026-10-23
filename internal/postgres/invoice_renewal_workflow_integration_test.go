//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/billing"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/lifecycle"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

type renewalLoopback struct {
	mu                                                  sync.Mutex
	recurring                                           invoiceninja.RecurringRecord
	panel                                               json.RawMessage
	invoiceDate, invoiceNext                            string
	pauseCalls, scheduleCalls, resumeCalls, panelWrites int
	unknownPause                                        bool
}

func TestInvoicePaidRenewalRealStoreAdaptersLoopbackSuccessAndUnknown(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed schedule and resume", true: "unknown pause is pinned"}[unknown], func(t *testing.T) {
			s, ctx := invoiceLifecycleTestStore(t)
			clientID, botID := seedInvoiceLifecycleAccount(t, s, ctx, false)
			state, target := prepareRenewalDatabaseFixture(t, s, ctx, clientID, botID)
			state.unknownPause = unknown
			ninja := httptest.NewServer(state.ninjaHandler())
			defer ninja.Close()
			panelServer := httptest.NewServer(state.panelHandler())
			defer panelServer.Close()
			cycle, err := invoiceninja.NewCycleReader(ninja.URL, "synthetic-ninja-token", 2*time.Second, 50, 20)
			if err != nil {
				t.Fatal(err)
			}
			settlements, err := invoiceninja.NewSettlementClient(ninja.URL, "synthetic-ninja-token", 2*time.Second, 50, 20)
			if err != nil {
				t.Fatal(err)
			}
			recurring, err := invoiceninja.NewRecurringClient(ninja.URL, "synthetic-ninja-token", 2*time.Second, 50, 20)
			if err != nil {
				t.Fatal(err)
			}
			panel, err := sui.NewManagedClient(panelServer.URL, "synthetic-panel-token", "1.6.4", 2*time.Second, 1000)
			if err != nil {
				t.Fatal(err)
			}
			cfg := lifecycle.RenewalConfig{Enabled: true, ExclusiveWriter: true, BaseURL: ninja.URL, Token: "synthetic-ninja-token", InvoiceNinjaVersion: "5.13.47", SUIBaseURL: panelServer.URL, SUIToken: "synthetic-panel-token", SUIVersion: "1.6.4", AllowedTelegramIDs: []int64{101}, Workers: 1, MaxAccounts: 1000, PollInterval: 100 * time.Millisecond, HTTPTimeout: 2 * time.Second}
			worker, err := lifecycle.NewRenewalWorker(s, cycle, settlements, recurring, panel, botID, []int64{9001}, "1", cfg, nil)
			if err != nil {
				t.Fatal(err)
			}
			service, cancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- worker.Run(service) }()
			wantOutcome := "completed"
			if unknown {
				wantOutcome = "review"
			}
			deadline := time.Now().Add(15 * time.Second)
			var outcome string
			for time.Now().Before(deadline) {
				if err := s.pool.QueryRow(ctx, `SELECT outcome FROM invoice_renewal_actions WHERE client_id=$1 AND invoice_id='invoice-current'`, clientID).Scan(&outcome); err == nil && outcome == wantOutcome {
					break
				}
				time.Sleep(25 * time.Millisecond)
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("renewal worker stopped: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("renewal worker did not join")
			}
			if outcome != wantOutcome {
				t.Fatalf("renewal outcome=%q want=%q", outcome, wantOutcome)
			}
			var guardOwner int64
			var guardStep string
			if err := s.pool.QueryRow(ctx, `SELECT COALESCE(operation_id,0),COALESCE(step,'') FROM activation_write_guard WHERE id=1`).Scan(&guardOwner, &guardStep); err != nil {
				t.Fatal(err)
			}
			state.mu.Lock()
			pause, schedule, resume, panelWrites := state.pauseCalls, state.scheduleCalls, state.resumeCalls, state.panelWrites
			state.mu.Unlock()
			if unknown {
				if pause != 1 || schedule != 0 || resume != 0 || panelWrites != 0 || guardOwner == 0 || guardStep != "pause_recurring" {
					t.Fatalf("unknown action replay or guard release: writes=%d/%d/%d panel=%d owner=%d step=%q", pause, schedule, resume, panelWrites, guardOwner, guardStep)
				}
			} else {
				if pause != 1 || schedule != 1 || resume != 1 || panelWrites != 0 || guardOwner != 0 || guardStep != "" {
					t.Fatalf("success actions/guard: writes=%d/%d/%d panel=%d owner=%d step=%q", pause, schedule, resume, panelWrites, guardOwner, guardStep)
				}
				var status, next string
				var desired bool
				if err := s.pool.QueryRow(ctx, `SELECT state,desired_access,next_invoice_date::text FROM subscriptions WHERE client_id=$1`, clientID).Scan(&status, &desired, &next); err != nil {
					t.Fatal(err)
				}
				if status != "active" || !desired || next != target {
					t.Fatalf("subscription=%s desired=%v next=%s want=%s", status, desired, next, target)
				}
				var intents int
				if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM external_write_intents i JOIN operations o ON o.id=i.operation_id WHERE o.client_id=$1 AND o.kind=$2 AND i.origin='lifecycle'`, clientID, lifecycle.RenewalOperationKind).Scan(&intents); err != nil || intents != 3 {
					t.Fatalf("lifecycle intents=%d err=%v", intents, err)
				}
			}
		})
	}
}

func prepareRenewalDatabaseFixture(t *testing.T, s *Store, ctx context.Context, clientID, botID int64) (*renewalLoopback, string) {
	t.Helper()
	today := time.Now().In(time.Local).Format(time.DateOnly)
	todayTime, _ := time.Parse(time.DateOnly, today)
	paidDate := todayTime.AddDate(0, -2, 0).Format(time.DateOnly)
	target, err := billing.NextBillingDate(today)
	if err != nil {
		t.Fatal(err)
	}
	token := "synthetic_activation_token_123456"
	marker := activation.Marker(token)
	product := invoiceninja.Product{ID: "product-synthetic", ProductKey: "monthly", Notes: "subscription", Price: "100", TaxRate1: "0", TaxRate2: "0", TaxRate3: "0", TaxID: "0"}
	firstDraft, err := invoiceninja.BuildRecurringDraft("customer-synthetic", paidDate, paidDate, product, false, marker)
	if err != nil {
		t.Fatal(err)
	}
	draftJSON, err := json.Marshal(firstDraft)
	if err != nil {
		t.Fatal(err)
	}
	customer := invoiceninja.SettlementCustomer{ID: "customer-synthetic", IDNumber: "101", CurrencyID: "1"}
	firstInvoice := invoiceninja.SettlementInvoice{ID: "invoice-first", ClientID: "customer-synthetic", Number: "SYNTHETIC-FIRST", Date: paidDate, DueDate: firstDraft.NextSendDate, Amount: "100", Balance: "0", PaidToDate: "100", ExchangeRate: "1", StatusID: "4"}
	firstPay := renewalProofPayment("payment-first", "allocation-first", "invoice-first", paidDate)
	firstLocal := payments.Snapshot{ClientID: clientID, TelegramID: 101, ChatID: 101, BotID: botID, CustomerID: "customer-synthetic", InvoiceID: "invoice-first", FirstInvoiceID: "invoice-first", RecurringID: "recurring-synthetic"}
	firstDecision := payments.Evaluate(firstLocal, customer, firstInvoice, []invoiceninja.SettlementPayment{firstPay}, "1", today)
	if firstDecision.Kind != payments.Verified {
		t.Fatalf("first activation proof fixture invalid: %+v", firstDecision)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE payment_confirmations SET invoice_number=$1,amount=$2::numeric,currency_id=$3,payment_date=$4::date,next_invoice_date=$5::date,proof_hash=$6,evidence=$7::jsonb,review_required=false,review_code=0 WHERE invoice_id='invoice-first'`, firstDecision.Proof.InvoiceNumber, firstDecision.Proof.Amount, firstDecision.Proof.CurrencyID, firstDecision.Proof.PaymentDate, firstDecision.Proof.NextInvoiceDate, firstDecision.Proof.Hash[:], string(firstDecision.Proof.JSON)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE activation_flows SET proof_hash=$1 WHERE invoice_id='invoice-first'`, firstDecision.Proof.Hash[:]); err != nil {
		t.Fatal(err)
	}
	firstDatetime := firstDraft.NextSendDate + "T08:00:00Z"
	for _, step := range []string{"recurring_create", "recurring_start"} {
		if _, err = s.pool.Exec(ctx, `UPDATE activation_steps SET request=$2::jsonb,recurring_amount=100,recurring_datetime=$3 WHERE step=$1 AND operation_id=(SELECT operation_id FROM activation_flows WHERE invoice_id='invoice-first')`, step, string(draftJSON), firstDatetime); err != nil {
			t.Fatal(err)
		}
	}
	currentInvoice := invoiceninja.SettlementInvoice{ID: "invoice-current", ClientID: "customer-synthetic", RecurringID: "recurring-synthetic", Number: "SYNTHETIC-CURRENT", StatusID: "4", Date: today, DueDate: target, Amount: "100", Balance: "0", PaidToDate: "100", ExchangeRate: "1"}
	currentPay := renewalProofPayment("payment-current", "allocation-current", "invoice-current", today)
	currentLocal := payments.Snapshot{ClientID: clientID, TelegramID: 101, ChatID: 101, BotID: botID, CustomerID: "customer-synthetic", InvoiceID: "invoice-current", FirstInvoiceID: "invoice-first", RecurringID: "recurring-synthetic", State: "active", DesiredAccess: true}
	currentDecision := payments.Evaluate(currentLocal, customer, currentInvoice, []invoiceninja.SettlementPayment{currentPay}, "1", today)
	if currentDecision.Kind != payments.Verified {
		t.Fatalf("current renewal proof fixture invalid: %+v", currentDecision)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO payment_confirmations(invoice_id,client_id,bot_id,customer_id,invoice_number,amount,currency_id,payment_date,next_invoice_date,proof_hash,evidence)
        VALUES('invoice-current',$1,$2,$3,$4,$5::numeric,$6,$7::date,$8::date,$9,$10::jsonb)`, clientID, botID, currentDecision.Proof.CustomerID, currentDecision.Proof.InvoiceNumber, currentDecision.Proof.Amount, currentDecision.Proof.CurrencyID, currentDecision.Proof.PaymentDate, currentDecision.Proof.NextInvoiceDate, currentDecision.Proof.Hash[:], string(currentDecision.Proof.JSON)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE invoice_lifecycle_invoices SET invoice_number='SYNTHETIC-CURRENT',status='sent',sent_date=$1::date,amount=100,balance=100,paid_to_date=0,notice_state='issued' WHERE invoice_id='invoice-current'`, todayTime.AddDate(0, 0, -1).Format(time.DateOnly)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE subscriptions SET state='active',desired_access=true,payment_date=$2::date,next_invoice_date=$3::date WHERE client_id=$1`, clientID, today, currentDecision.Proof.NextInvoiceDate); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO invoice_lifecycle_checks(client_id,bot_id) VALUES($1,$2) ON CONFLICT(client_id) DO UPDATE SET review_code=0`, clientID, botID); err != nil {
		t.Fatal(err)
	}
	panelJSON := json.RawMessage(`{"id":71,"enable":true,"name":"pp-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","config":{},"inbounds":[1],"links":[],"volume":0,"expiry":0,"down":1,"up":1,"desc":"","group":"","remark":"","createdAt":1,"onlineAt":0,"delayStart":false,"autoReset":false,"resetDays":0,"nextReset":0,"totalUp":0,"totalDown":0}`)
	panelRecord, err := sui.ParseClientRecord(panelJSON)
	if err != nil {
		t.Fatal(err)
	}
	panelHash := panelRecord.CredentialsHash()
	if _, err = s.pool.Exec(ctx, `UPDATE activation_sui_bindings SET credentials_hash=$2 WHERE client_id=$1`, clientID, panelHash[:]); err != nil {
		t.Fatal(err)
	}
	line := firstDraft.LineItems[0]
	record := invoiceninja.RecurringRecord{ID: "recurring-synthetic", ClientID: "customer-synthetic", StatusID: "2", PrivateNotes: marker, FrequencyID: "5", NextSendDate: todayTime.AddDate(0, 0, 2).Format(time.DateOnly), NextSendDatetime: target + "T08:00:00Z", LastSentDate: paidDate + "T08:00:00Z", DueDateDays: "on_receipt", AutoBill: "off", RemainingCycles: -1, Amount: "100", Balance: "100", ExchangeRate: "1", LineItems: []invoiceninja.InvoiceLine{line}, Discount: "0", TaxRate1: "0", TaxRate2: "0", TaxRate3: "0", CustomSurcharge1: "0", CustomSurcharge2: "0", CustomSurcharge3: "0", CustomSurcharge4: "0", Partial: "0", PaidToDate: "0"}
	return &renewalLoopback{recurring: record, panel: panelJSON, invoiceDate: today, invoiceNext: target}, target
}

func renewalProofPayment(paymentID, allocationID, invoiceID, date string) invoiceninja.SettlementPayment {
	return invoiceninja.SettlementPayment{ID: paymentID, ClientID: "customer-synthetic", Date: date, StatusID: "4", CurrencyID: "1", Amount: "100", Refunded: "0", Applied: "100", ExchangeRate: "1", IsManual: true, Allocations: []invoiceninja.SettlementAllocation{{ID: allocationID, InvoiceID: invoiceID, PaymentID: paymentID, Amount: "100", Refunded: "0"}}}
}

func (r *renewalLoopback) ninjaHandler() http.Handler {
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("X-APP-VERSION", "5.13.47")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/api/v1/ping", func(w http.ResponseWriter, q *http.Request) { write(w, 200, map[string]any{"data": map[string]any{}}) })
	mux.HandleFunc("/api/v1/clients/customer-synthetic", func(w http.ResponseWriter, q *http.Request) {
		write(w, 200, map[string]any{"data": map[string]any{"id": "customer-synthetic", "id_number": "101", "group_settings_id": "", "settings": map[string]any{"currency_id": "1"}, "is_deleted": false, "archived_at": 0}})
	})
	mux.HandleFunc("/api/v1/invoices/invoice-current", func(w http.ResponseWriter, q *http.Request) {
		r.mu.Lock()
		date, next := r.invoiceDate, r.invoiceNext
		r.mu.Unlock()
		write(w, 200, map[string]any{"data": map[string]any{"id": "invoice-current", "client_id": "customer-synthetic", "recurring_id": "recurring-synthetic", "number": "SYNTHETIC-CURRENT", "private_notes": "payplump:synthetic_activation_token_123456:recurring_create", "status_id": "4", "date": date, "due_date": next, "amount": 100, "balance": 0, "paid_to_date": 100, "exchange_rate": 1, "is_deleted": false, "archived_at": 0, "updated_at": 1, "invitations": []any{map[string]any{"id": "invitation-current", "sent_date": date + " 08:00:00"}}}})
	})
	mux.HandleFunc("/api/v1/payments", func(w http.ResponseWriter, q *http.Request) {
		p := map[string]any{"id": "payment-current", "client_id": "customer-synthetic", "date": r.invoiceDate, "status_id": "4", "currency_id": "1", "exchange_currency_id": "", "amount": 100, "refunded": 0, "applied": 100, "exchange_rate": 1, "is_manual": true, "is_deleted": false, "archived_at": 0, "paymentables": []any{map[string]any{"id": "allocation-current", "invoice_id": "invoice-current", "payment_id": "payment-current", "amount": 100, "refunded": 0, "archived_at": 0}}}
		write(w, 200, map[string]any{"data": []any{p}, "meta": map[string]any{"pagination": map[string]any{"total": 1, "count": 1, "per_page": 50, "current_page": 1, "total_pages": 1}}})
	})
	mux.HandleFunc("/api/v1/recurring_invoices/recurring-synthetic", func(w http.ResponseWriter, q *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if q.Method == http.MethodPut {
			var body struct {
				Next string `json:"next_send_date"`
			}
			_ = json.NewDecoder(q.Body).Decode(&body)
			r.recurring.NextSendDate = body.Next
			r.recurring.NextSendDatetime = body.Next + "T08:00:00Z"
			r.scheduleCalls++
		}
		write(w, 200, map[string]any{"data": r.recurring})
	})
	mux.HandleFunc("/api/v1/recurring_invoices/bulk", func(w http.ResponseWriter, q *http.Request) {
		var body struct {
			IDs    []string `json:"ids"`
			Action string   `json:"action"`
		}
		_ = json.NewDecoder(q.Body).Decode(&body)
		r.mu.Lock()
		defer r.mu.Unlock()
		switch body.Action {
		case "stop":
			r.pauseCalls++
			r.recurring.StatusID = "3"
			if r.unknownPause {
				write(w, 500, map[string]any{"error": "synthetic unknown"})
				return
			}
		case "start":
			r.resumeCalls++
			r.recurring.StatusID = "2"
		}
		write(w, 200, map[string]any{"data": []invoiceninja.RecurringRecord{r.recurring}})
	})
	return mux
}
func (r *renewalLoopback) panelHandler() http.Handler {
	mux := http.NewServeMux()
	envelope := func(w http.ResponseWriter, obj map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "obj": obj})
	}
	mux.HandleFunc("/apiv2/clients", func(w http.ResponseWriter, q *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		var record map[string]any
		_ = json.Unmarshal(r.panel, &record)
		if q.URL.Query().Get("id") != "" {
			envelope(w, map[string]any{"clients": []any{record}})
		} else {
			envelope(w, map[string]any{"clients": []any{map[string]any{"id": 71, "name": "pp-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}})
		}
	})
	mux.HandleFunc("/apiv2/inbounds", func(w http.ResponseWriter, q *http.Request) {
		envelope(w, map[string]any{"inbounds": []any{map[string]any{"id": 1, "type": "vmess"}}})
	})
	mux.HandleFunc("/apiv2/status", func(w http.ResponseWriter, q *http.Request) {
		envelope(w, map[string]any{"sys": map[string]any{"appVersion": "1.6.4"}})
	})
	mux.HandleFunc("/apiv2/save", func(w http.ResponseWriter, q *http.Request) {
		_ = q.ParseForm()
		var record map[string]any
		_ = json.Unmarshal([]byte(q.Form.Get("data")), &record)
		raw, _ := json.Marshal(record)
		r.mu.Lock()
		r.panel = raw
		r.panelWrites++
		r.mu.Unlock()
		envelope(w, map[string]any{"ok": true})
	})
	return mux
}
