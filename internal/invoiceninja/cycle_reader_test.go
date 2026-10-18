package invoiceninja_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
)

const cycleInvoiceJSON = `{"id":"inv-1","client_id":"customer-1","recurring_id":"","number":"N-1","private_notes":"","status_id":"2","date":"2025-01-01","due_date":"2025-02-01","amount":12345678901234567890.123456789,"balance":0,"paid_to_date":0,"exchange_rate":1,"is_deleted":false,"archived_at":0,"updated_at":0}`

func cyclePage(rows []string, total, page, pageSize int) string {
	pages := 0
	if total > 0 {
		pages = (total + pageSize - 1) / pageSize
	}
	return fmt.Sprintf(`{"data":[%s],"meta":{"pagination":{"total":%d,"count":%d,"per_page":%d,"current_page":%d,"total_pages":%d}}}`, strings.Join(rows, ","), total, len(rows), pageSize, page, pages)
}

func TestCycleReaderInventoryGlobalAndStrictPagination(t *testing.T) {
	rows := []string{
		strings.ReplaceAll(cycleInvoiceJSON, `"inv-1"`, `"inv-a"`),
		strings.ReplaceAll(strings.ReplaceAll(cycleInvoiceJSON, `"inv-1"`, `"inv-b"`), `"customer-1"`, `"foreign"`),
		strings.ReplaceAll(cycleInvoiceJSON, `"inv-1"`, `"inv-c"`),
	}
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/prefix/api/v1/invoices" || r.Header.Get("X-API-TOKEN") != "token" || r.Header.Get("X-Requested-With") != "XMLHttpRequest" {
			t.Errorf("unexpected request: %s %v", r.URL.Path, r.Header)
		}
		q := r.URL.Query()
		if q.Get("status") != "active,archived,deleted" || q.Get("client_status") != "all" || q.Get("sort") != "id|asc" || q.Get("per_page") != "2" || q.Get("page") != fmt.Sprint(calls) || q.Has("client_id") || q.Has("recurring_id") {
			t.Errorf("unexpected query: %v", q)
		}
		w.Header().Set("X-APP-VERSION", "5.13.47")
		if calls == 1 {
			fmt.Fprint(w, cyclePage(rows[:2], 3, 1, 2))
		} else {
			fmt.Fprint(w, cyclePage(rows[2:], 3, 2, 2))
		}
	}))
	defer s.Close()
	reader, err := invoiceninja.NewCycleReader(strings.Replace(s.URL, "http://", "http://", 1)+"/prefix", "token", time.Second, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.ListInvoices(context.Background(), "customer-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].ID != "inv-c" || got[0].Amount.String() != "12345678901234567890.123456789" || calls != 2 {
		t.Fatalf("unexpected inventory: %#v calls=%d", got, calls)
	}
	if reflect.TypeOf(reader).NumMethod() != 2 {
		t.Fatal("cycle reader exposes unexpected capability")
	}
}

func TestCycleReaderGetExactIDAndVersion(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.EscapedPath() != "/api/v1/invoices/inv%20one" {
			t.Errorf("unexpected path: %s", r.URL.EscapedPath())
		}
		w.Header().Set("X-APP-VERSION", "5.13.47")
		fmt.Fprintf(w, `{"data":%s}`, strings.ReplaceAll(cycleInvoiceJSON, `"inv-1"`, `"wrong"`))
	}))
	defer server.Close()
	reader, err := invoiceninja.NewCycleReader(server.URL, "token", time.Second, 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.GetInvoice(context.Background(), "../bad"); err == nil || calls != 0 {
		t.Fatal("invalid ID reached HTTP", err, calls)
	}
	if _, err := reader.GetInvoice(context.Background(), "inv one"); err == nil || calls != 1 {
		t.Fatal("mismatched ID accepted", err, calls)
	}
}

func TestCycleInvoiceStrictFields(t *testing.T) {
	var invoice invoiceninja.CycleInvoice
	if err := json.Unmarshal([]byte(cycleInvoiceJSON), &invoice); err != nil {
		t.Fatal(err)
	}
	if invoice.PrivateNotes != "" || invoice.UpdatedAt != 0 || invoice.Amount.String() != "12345678901234567890.123456789" {
		t.Fatalf("lost wire facts: %#v", invoice)
	}
	for _, raw := range []string{
		strings.Replace(cycleInvoiceJSON, `,"private_notes":""`, "", 1),
		strings.Replace(cycleInvoiceJSON, `"id":"inv-1"`, `"id":"inv-1","ID":"inv-1"`, 1),
		strings.Replace(cycleInvoiceJSON, `"updated_at":0`, `"updated_at":null`, 1),
		strings.Replace(cycleInvoiceJSON, `"date":"2025-01-01"`, `"date":"2025-02-30"`, 1),
		strings.Replace(cycleInvoiceJSON, `"amount":12345678901234567890.123456789`, `"amount":1e10000`, 1),
	} {
		if json.Unmarshal([]byte(raw), &invoice) == nil {
			t.Fatalf("accepted invalid invoice: %s", raw)
		}
	}
}
