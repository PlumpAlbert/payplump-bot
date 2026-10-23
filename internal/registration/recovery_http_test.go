package registration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
)

func TestBillingLookupTransportFailuresRetryThenProgress(t *testing.T) {
	for _, mode := range []string{"503", "429", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			f := newWorkerFixture(t)
			var lookups, posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-APP-VERSION", invoiceninja.SupportedWriteVersion)
				if r.Method == http.MethodGet && r.URL.Path == "/api/v1/clients" && r.URL.Query().Get("id_number") != "" {
					if lookups.Add(1) == 1 {
						switch mode {
						case "503":
							w.WriteHeader(http.StatusServiceUnavailable)
							return
						case "429":
							w.WriteHeader(http.StatusTooManyRequests)
							return
						case "timeout":
							time.Sleep(300 * time.Millisecond)
						}
					}
					_, _ = io.WriteString(w, `{"data":[],"meta":{"pagination":{"total":0,"count":0,"per_page":50,"current_page":1,"total_pages":1}}}`)
					return
				}
				if r.Method == http.MethodGet {
					_, _ = io.WriteString(w, `{"data":[]}`)
					return
				}
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/clients" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				posts.Add(1)
				var draft invoiceninja.CustomerDraft
				if err := json.NewDecoder(r.Body).Decode(&draft); err != nil {
					t.Error("invalid customer draft")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": invoiceninja.Customer{
					ID: "created-customer", Name: draft.Name, IDNumber: draft.IDNumber,
					PrivateNotes: draft.PrivateNotes, IsDeleted: false, ArchivedAt: 0,
				}})
			}))
			defer server.Close()
			api, err := invoiceninja.NewBillingClient(server.URL, "synthetic-token", 100*time.Millisecond, 50, 20)
			if err != nil {
				t.Fatal(err)
			}
			f.worker.api = api

			f.pass()
			state := f.store.states[1]
			if state.held || state.retries != 1 || len(state.snapshot.Steps) != 0 || posts.Load() != 0 {
				t.Fatalf("temporary lookup failure held or wrote: held=%v retries=%d steps=%v posts=%d", state.held, state.retries, state.snapshot.Steps, posts.Load())
			}

			f.worker.now = func() time.Time { return f.now }
			f.pass()
			state = f.store.states[1]
			if state.held || state.retries != 2 || state.snapshot.Steps[CustomerCreate].ExternalID != "created-customer" || posts.Load() != 1 || lookups.Load() != 2 {
				t.Fatalf("valid lookup did not progress registration: held=%v retries=%d step=%+v lookups=%d posts=%d", state.held, state.retries, state.snapshot.Steps[CustomerCreate], lookups.Load(), posts.Load())
			}
		})
	}
}

func TestMalformedBillingReceiptLeavesUnknownIntentUnreplayed(t *testing.T) {
	f := newWorkerFixture(t)
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-APP-VERSION", invoiceninja.SupportedWriteVersion)
		if r.Method == http.MethodGet && r.URL.Query().Get("id_number") != "" {
			_, _ = io.WriteString(w, `{"data":[],"meta":{"pagination":{"total":0,"count":0,"per_page":50,"current_page":1,"total_pages":1}}}`)
			return
		}
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"data":[]}`)
			return
		}
		posts.Add(1)
		var draft invoiceninja.CustomerDraft
		if err := json.NewDecoder(r.Body).Decode(&draft); err != nil {
			t.Error("invalid customer draft")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		receipt, err := json.Marshal(invoiceninja.Customer{
			ID: "created-customer", Name: draft.Name, IDNumber: draft.IDNumber,
			PrivateNotes: draft.PrivateNotes, IsDeleted: false, ArchivedAt: 0,
		})
		if err != nil {
			t.Errorf("could not encode synthetic receipt: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = fmt.Fprintf(w, `{"data":null,"DATA":%s}`, receipt)
	}))
	defer server.Close()
	api, err := invoiceninja.NewBillingClient(server.URL, "synthetic-token", time.Second, 50, 20)
	if err != nil {
		t.Fatal(err)
	}
	f.worker.api = api

	f.pass()
	intent := f.store.states[1].snapshot.Steps[CustomerCreate]
	if posts.Load() != 1 || f.store.states[1].held || f.store.states[1].retries != 1 || intent.ExternalID != "" {
		t.Fatalf("malformed receipt was not retained as unknown intent: posts=%d state=%+v", posts.Load(), f.store.states[1])
	}
	f.pass()
	if posts.Load() != 1 {
		t.Fatalf("unknown intent was rearmed after malformed receipt: posts=%d", posts.Load())
	}
	f.wantHeld(ReviewUnknownWrite)
}

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
