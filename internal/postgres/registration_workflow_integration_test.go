//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/registration"
)

type observingRegistrationStore struct {
	*Store
	result chan error
}

func (s *observingRegistrationStore) FinishRegistration(ctx context.Context, lease queue.Lease, customer, invoice string, drafts []queue.Draft) error {
	err := s.Store.FinishRegistration(ctx, lease, customer, invoice, drafts)
	select {
	case s.result <- err:
	default:
	}
	return err
}
func (s *observingRegistrationStore) HoldRegistration(ctx context.Context, lease queue.Lease, reason int, drafts []queue.Draft) error {
	err := s.Store.HoldRegistration(ctx, lease, reason, drafts)
	select {
	case s.result <- errors.New("workflow unexpectedly entered review"):
	default:
	}
	return err
}

// Full boundaries: real isolated PostgreSQL, real write adapter and local HTTP.
// Neither Invoice Ninja nor S-UI/Telegram credentials are used by this test.
func TestRegistrationWorkflowWithDurableStoreAndSyntheticHTTP(t *testing.T) {
	s, ctx := testStore(t)
	op := seedRegistrationWork(t, s, ctx, 424242)
	if _, err := s.pool.Exec(ctx, `UPDATE operations SET status='pending',attempts=0,locked_until=NULL WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var customer invoiceninja.Customer
	var invoice invoiceninja.Invoice
	posts := make(map[registration.Step]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("X-APP-VERSION", "5.13.47")
		w.Header().Set("Content-Type", "application/json")
		respond := func(data any) {
			if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
				t.Error("synthetic response failed")
			}
		}
		list := func(data any, count int) {
			limit, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
			_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "meta": map[string]any{"pagination": map[string]int{"total": count, "count": count, "per_page": limit, "current_page": 1, "total_pages": 1}}})
		}
		journaled := func(step registration.Step, payload any) bool {
			posts[step]++
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Error(err)
				return false
			}
			var committed bool
			err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operation_steps WHERE operation_id=$1 AND step=$2 AND request=$3::jsonb AND external_id IS NULL)`, op.ID, string(step), string(raw)).Scan(&committed)
			if err != nil || !committed {
				t.Error("HTTP write preceded its committed journal")
				w.WriteHeader(500)
				return false
			}
			return true
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/clients":
			items := []invoiceninja.Customer{}
			if customer.ID != "" {
				items = append(items, customer)
			}
			list(items, len(items))
		case "GET /api/v1/clients/synthetic-customer":
			respond(customer)
		case "GET /api/v1/products/synthetic-product":
			respond(invoiceninja.Product{ID: "synthetic-product", ProductKey: "synthetic-plan", Notes: "Synthetic only", Price: "125.25", TaxRate1: "0", TaxRate2: "0", TaxRate3: "0", TaxID: "1"})
		case "GET /api/v1/invoices":
			items := []invoiceninja.Invoice{}
			if invoice.ID != "" {
				items = append(items, invoice)
			}
			list(items, len(items))
		case "GET /api/v1/invoices/synthetic-invoice":
			respond(invoice)
		case "POST /api/v1/clients":
			var draft invoiceninja.CustomerDraft
			if json.NewDecoder(r.Body).Decode(&draft) != nil || !journaled(registration.CustomerCreate, draft) {
				return
			}
			customer = invoiceninja.Customer{ID: "synthetic-customer", Name: draft.Name, IDNumber: draft.IDNumber, PrivateNotes: draft.PrivateNotes}
			respond(customer)
		case "POST /api/v1/invoices":
			var draft invoiceninja.InvoiceDraft
			if json.NewDecoder(r.Body).Decode(&draft) != nil || !journaled(registration.InvoiceCreate, draft) {
				return
			}
			invoice = invoiceninja.Invoice{ID: "synthetic-invoice", ClientID: draft.ClientID, Number: "TEST-001", StatusID: "1", Date: draft.Date, DueDate: draft.DueDate, PrivateNotes: draft.PrivateNotes, UsesInclusiveTaxes: draft.UsesInclusiveTaxes, LineItems: draft.LineItems, Amount: "125.25", Balance: "0", Invitations: []invoiceninja.Invitation{{ID: "synthetic-invitation", Link: "https://billing.example.test/invoice/synthetic-invitation"}}}
			respond(invoice)
		case "POST /api/v1/invoices/bulk":
			var draft invoiceninja.IssueDraft
			if json.NewDecoder(r.Body).Decode(&draft) != nil || !journaled(registration.InvoiceIssue, draft) {
				return
			}
			if draft.Action != "mark_sent" || len(draft.IDs) != 1 || draft.IDs[0] != invoice.ID {
				t.Error("unsafe issue request")
				w.WriteHeader(400)
				return
			}
			invoice.StatusID = "2"
			invoice.Balance = invoice.Amount
			respond([]invoiceninja.Invoice{invoice})
		default:
			t.Error("unexpected external capability used")
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	api, err := invoiceninja.NewBillingClient(server.URL, "synthetic-token", 5*time.Second, 50, 20)
	if err != nil {
		t.Fatal(err)
	}
	observed := &observingRegistrationStore{Store: s, result: make(chan error, 1)}
	cfg := registration.Config{Enabled: true, TelegramEnabled: true, Workers: 1, PollInterval: 100 * time.Millisecond, HTTPTimeout: 5 * time.Second, PageSize: 50, MaxPages: 20, BaseURL: server.URL, Token: "synthetic-token", ProductID: "synthetic-product", TaxModeConfirmed: true, PaymentPhone: "+7 000 000-00-00"}
	worker, err := registration.New(observed, api, 123, []int64{9001, 9002}, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
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
			t.Error("registration worker failed to join")
		}
	}()
	select {
	case err := <-observed.result:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("registration did not finish within the fixture deadline")
	}
	assertRegistrationSQL(t, s, ctx, `SELECT c.invoice_ninja_id='synthetic-customer' AND c.sui_id IS NULL AND sub.current_invoice_id='synthetic-invoice' AND sub.first_invoice_id='synthetic-invoice' AND sub.recurring_id IS NULL AND sub.state='awaiting_payment' AND NOT sub.desired_access AND op.status='completed' FROM clients c JOIN subscriptions sub ON sub.client_id=c.id JOIN operations op ON op.client_id=c.id WHERE op.id=$1`, op.ID)
	assertRegistrationSQL(t, s, ctx, `SELECT count(*)=3 AND bool_and(external_id IS NOT NULL AND confirmed_at IS NOT NULL) FROM operation_steps WHERE operation_id=$1`, op.ID)
	assertRegistrationSQL(t, s, ctx, `SELECT count(*)=3 AND bool_and(position('125.25' in body)>0 AND position('https://billing.example.test/invoice/synthetic-invitation' in body)>0) FROM outbox WHERE message_key LIKE 'registration:%:invoice:%'`)
	mu.Lock()
	defer mu.Unlock()
	for _, step := range []registration.Step{registration.CustomerCreate, registration.InvoiceCreate, registration.InvoiceIssue} {
		if posts[step] != 1 {
			t.Fatal("a financial POST was omitted or repeated")
		}
	}
}
