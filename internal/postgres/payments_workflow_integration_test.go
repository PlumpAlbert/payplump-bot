//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
)

type observedPaymentStore struct {
	*Store
	result chan error
}

func (s *observedPaymentStore) FinishPaymentCheck(ctx context.Context, c payments.Claim, snapshot payments.Snapshot, d payments.Decision, n payments.Notices, interval time.Duration) error {
	err := s.Store.FinishPaymentCheck(ctx, c, snapshot, d, n, interval)
	select {
	case s.result <- err:
	default:
	}
	return err
}

func (s *observedPaymentStore) DeferPaymentCheck(ctx context.Context, c payments.Claim, delay time.Duration, code int) error {
	err := s.Store.DeferPaymentCheck(ctx, c, delay, code)
	select {
	case s.result <- err:
	default:
	}
	return err
}

// Real durable storage + real GET adapter + synthetic provider. The forged
// webhook body deliberately disagrees with the GET evidence and selects no ID.
func TestPaymentsWorkflowWithSyntheticHTTP(t *testing.T) {
	s, ctx := testStore(t)
	ids := seedPaymentAccounts(t, s, ctx, 123, 47001, 1)
	clientID := ids[0]
	var gets, mutations atomic.Int32
	var outage atomic.Bool
	customerID, invoiceID := "customer-47001", "invoice-47001"
	invoice := invoiceninja.SettlementInvoice{ID: invoiceID, ClientID: customerID, Number: "SYNTHETIC-PAID", Date: "2020-01-01", DueDate: "2020-01-01", StatusID: "4", Amount: "200.01", Balance: "0", PaidToDate: "200.01", ExchangeRate: "1"}
	cash := func(id, amount, date string) invoiceninja.SettlementPayment {
		return invoiceninja.SettlementPayment{ID: id, ClientID: customerID, Date: date, StatusID: "4", CurrencyID: "1", Amount: json.Number(amount), Applied: json.Number(amount), Refunded: "0", ExchangeRate: "1", IsManual: true, Allocations: []invoiceninja.SettlementAllocation{{ID: "allocation-" + id, PaymentID: id, InvoiceID: invoiceID, Amount: json.Number(amount), Refunded: "0"}}}
	}
	records := []invoiceninja.SettlementPayment{cash("p1", "100", "2020-01-30"), cash("p2", "100.01", "2020-01-31")}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			mutations.Add(1)
			w.WriteHeader(405)
			return
		}
		gets.Add(1)
		w.Header().Set("X-APP-VERSION", invoiceninja.SupportedSettlementVersion)
		response := map[string]any{}
		switch r.URL.Path {
		case "/api/v1/clients/" + customerID:
			response["data"] = map[string]any{"id": customerID, "id_number": "47001", "settings": map[string]any{}, "group_settings_id": "", "is_deleted": false, "archived_at": 0}
		case "/api/v1/invoices/" + invoiceID:
			response["data"] = invoice
		case "/api/v1/payments":
			if outage.Load() {
				w.WriteHeader(503)
				return
			}
			if r.URL.Query().Get("client_id") != customerID || r.URL.Query().Get("invoice_id") != "" {
				t.Error("payment filter was not exact client scope")
			}
			limit, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
			response["data"] = records
			response["meta"] = map[string]any{"pagination": map[string]int{"total": 2, "count": 2, "per_page": limit, "current_page": 1, "total_pages": 1}}
		default:
			t.Error("webhook-provided foreign ID was followed or unsupported API used")
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	api, err := invoiceninja.NewSettlementClient(server.URL, "synthetic-token", time.Second, 50, 20)
	if err != nil {
		t.Fatal(err)
	}
	cfg := payments.Config{Enabled: true, TelegramEnabled: true, BaseURL: server.URL, Token: "synthetic-token", WebhookSecret: strings.Repeat("s", 32), CurrencyID: "1", Workers: 1, PageSize: 50, MaxPages: 20, PollInterval: 100 * time.Millisecond, RecheckInterval: 30 * time.Second, HTTPTimeout: time.Second}
	observed := &observedPaymentStore{Store: s, result: make(chan error, 1)}
	worker, err := payments.New(observed, api, 123, []int64{9001, 9002}, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	hook, err := payments.NewWebhook(cfg.WebhookSecret, s)
	if err != nil {
		t.Fatal(err)
	}
	if err := hook.Activate(123); err != nil {
		t.Fatal(err)
	}
	body := `{"status_id":"1","amount":0,"invoice_id":"FOREIGN-NEVER-REQUEST","client_id":"FORGED"}`
	wake := func() {
		r := httptest.NewRequest("POST", payments.WebhookPath, strings.NewReader(body))
		r.Header.Set(payments.WebhookSecretHeader, cfg.WebhookSecret)
		r.Header.Set("Content-Type", "application/json")
		reply := httptest.NewRecorder()
		hook.ServeHTTP(reply, r)
		if reply.Code != 200 {
			t.Fatalf("wake status=%d", reply.Code)
		}
	}
	wake()
	run := func() {
		service, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- worker.Run(service) }()
		defer func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(10 * time.Second):
				t.Error("payment verifier did not join")
			}
		}()
		select {
		case err := <-observed.result:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("payment observation did not finish")
		}
	}
	run()
	paymentSQL(t, s, ctx, `SELECT p.amount=200.01 AND p.payment_date='2020-01-31' AND p.next_invoice_date='2020-02-29' AND NOT p.review_required AND sub.payment_date=p.payment_date AND sub.next_invoice_date=p.next_invoice_date AND sub.state='awaiting_payment' AND NOT sub.desired_access AND sub.recurring_id IS NULL AND c.sui_id IS NULL FROM payment_confirmations p JOIN subscriptions sub ON sub.client_id=p.client_id JOIN clients c ON c.id=p.client_id WHERE p.client_id=$1`, clientID)
	paymentSQL(t, s, ctx, `SELECT count(*)=3 FROM outbox WHERE message_key LIKE 'payment:%:confirmed:%'`)
	paymentSQL(t, s, ctx, `SELECT NOT EXISTS(SELECT 1 FROM operations WHERE kind <> 'register_client')`)
	// Missed webhook fallback: only the periodic due time changes. Same proof
	// must not create another period/notice. The byte-identical event is deduped.
	wake()
	duePayment(t, s, ctx, clientID)
	run()
	paymentSQL(t, s, ctx, `SELECT count(*)=1 FROM payment_confirmations`)
	paymentSQL(t, s, ctx, `SELECT count(*)=3 FROM outbox WHERE message_key LIKE 'payment:%:confirmed:%'`)
	paymentSQL(t, s, ctx, `SELECT generation=1 FROM payment_wake_state WHERE bot_id=123`)
	if gets.Load() != 12 || mutations.Load() != 0 {
		t.Fatalf("expected two six-GET proofs and no mutations, gets=%d writes=%d", gets.Load(), mutations.Load())
	}
	// A list-page 503 is wrapped as incomplete by the real adapter. It must
	// still defer, not turn the previous valid proof into sticky review.
	outage.Store(true)
	duePayment(t, s, ctx, clientID)
	run()
	paymentSQL(t, s, ctx, `SELECT NOT review_required AND payment_date='2020-01-31' AND next_invoice_date='2020-02-29' FROM payment_confirmations WHERE client_id=$1`, clientID)
	paymentSQL(t, s, ctx, `SELECT last_error_code=503 AND locked_until IS NULL FROM payment_checks WHERE client_id=$1`, clientID)
	paymentSQL(t, s, ctx, `SELECT count(*)=0 FROM outbox WHERE message_key LIKE 'payment:%:review:%'`)
	if gets.Load() != 15 || mutations.Load() != 0 {
		t.Fatal("transport retry crossed GET-only boundary")
	}
}
