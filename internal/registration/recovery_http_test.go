package registration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
)

func TestRealAdapterPreservesTwoLineConflictForManualHold(t *testing.T) {
	f := newWorkerFixture(t)
	f.passes(2)
	invoice := cloneInvoice(f.api.invoices[0])
	invoice.LineItems = append(invoice.LineItems, invoice.LineItems[0])
	invoice.Amount = "2001"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-APP-VERSION", invoiceninja.SupportedWriteVersion)
		if r.Method != "GET" {
			t.Error("observed conflict attempted another write")
			w.WriteHeader(500)
			return
		}
		var data any
		switch r.URL.Path {
		case "/api/v1/clients/" + f.api.customers[0].ID:
			data = f.api.customers[0]
		case "/api/v1/invoices/" + invoice.ID:
			data = invoice
		default:
			t.Error("unexpected reconciliation request")
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer server.Close()
	api, err := invoiceninja.NewBillingClient(server.URL, "synthetic-token", time.Second, 50, 20)
	if err != nil {
		t.Fatal(err)
	}
	// Validate the actual decoder boundary, not only a directly constructed DTO.
	if _, err := api.GetInvoice(context.Background(), invoice.ID); err != nil {
		t.Fatal("valid conflicting invoice was hidden as a transport retry")
	}
	f.worker.api = api
	f.pass()
	f.wantHeld(ReviewInvoiceConflict)
	f.wantPosts(1, 1, 0)
}
