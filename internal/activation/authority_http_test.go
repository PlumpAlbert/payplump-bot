package activation

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
)

// Exercise actual strict settlement, product and recurring adapters against a
// synthetic Ninja. No panel provider exists, and no other write route is served.
func TestAuthorityActivationHTTPPaidInvoiceToOriginalRecurringReceipts(t *testing.T) {
	f := newAuthorityFixture(t)
	var recurring invoiceninja.RecurringRecord
	var writes []Step
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		reply := func(data any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": data}) }
		list := func(data any, n int) {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "meta": map[string]any{"pagination": invoiceninja.Pagination{Total: n, Count: n, PerPage: 50, CurrentPage: 1, TotalPages: 1}}})
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/clients/customer-A":
			reply(map[string]any{"id": "customer-A", "id_number": "101", "settings": map[string]any{}, "group_settings_id": "", "is_deleted": false, "archived_at": 0})
		case "GET /api/v1/invoices/invoice-A":
			reply(f.p.invoice)
		case "GET /api/v1/payments":
			list(f.p.cash, len(f.p.cash))
		case "GET /api/v1/products/product-A":
			reply(f.api.product)
		case "GET /api/v1/recurring_invoices":
			list([]invoiceninja.RecurringRecord{}, 0)
		case "GET /api/v1/recurring_invoices/recurring-A":
			reply(recurring)
		case "POST /api/v1/recurring_invoices":
			var draft invoiceninja.RecurringDraft
			if json.NewDecoder(r.Body).Decode(&draft) != nil || invoiceninja.ValidateRecurringDraft(draft) != nil {
				t.Error("invalid recurring request")
				w.WriteHeader(400)
				return
			}
			recurring = workerRecurring(draft)
			writes = append(writes, RecurringCreate)
			reply(recurring)
		case "POST /api/v1/recurring_invoices/bulk":
			var request StartRequest
			if json.NewDecoder(r.Body).Decode(&request) != nil || request.Action != "start" || !slices.Equal(request.IDs, []string{"recurring-A"}) || recurring.StatusID != "1" {
				t.Error("invalid start request")
				w.WriteHeader(400)
				return
			}
			recurring.StatusID = "2"
			writes = append(writes, RecurringStart)
			reply([]invoiceninja.RecurringRecord{recurring})
		default:
			t.Error("unexpected provider call, including SUI or ordinary invoice write")
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	cfg := f.w.cfg
	cfg.BaseURL = server.URL
	settlement, err := invoiceninja.NewSettlementClient(server.URL, cfg.Token, cfg.HTTPTimeout, 50, 20)
	if err != nil {
		t.Fatal(err)
	}
	products, err := invoiceninja.New(server.URL, cfg.Token, cfg.HTTPTimeout)
	if err != nil {
		t.Fatal(err)
	}
	recurringAPI, err := invoiceninja.NewRecurringClient(server.URL, cfg.Token, cfg.HTTPTimeout, 50, 20)
	if err != nil {
		t.Fatal(err)
	}
	w, err := New(f.s, Providers{Payments: settlement, Products: products, Recurring: recurringAPI}, 42, f.w.admins, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.w = w
	w.now = func() time.Time { return f.now }
	f.s.s.PolicyHash = cfg.PolicyHash()
	f.progress(3)
	if f.s.finishCalls != 1 || len(f.s.s.Steps) != 2 || !slices.Equal(writes, []Step{RecurringCreate, RecurringStart}) {
		t.Fatal("real adapters did not finish recurring-only activation")
	}
	created, started := f.s.s.Steps[RecurringCreate], f.s.s.Steps[RecurringStart]
	if created.ExternalID != recurring.ID || started.ExternalID != recurring.ID || created.RecurringAmount != "200" || started.RecurringAmount != created.RecurringAmount || started.RecurringDatetime != created.RecurringDatetime || recurring.StatusID != "2" {
		t.Fatal("original provider receipts were not persisted exactly")
	}
}
