package invoiceninja

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Synthetic source contracts only, never live calls. Source pin:
// 493c3884e4214028bc5970cb41bafbd9c4d96401 (5.13.47).
const recurringTestToken = "synthetic-recurring-secret"
const recurringTestMarker = "payplump:abcdefghijklmnopqrstuvwx:recurring_create"

func recurringTestProduct() Product {
	return Product{ID: "product-A", ProductKey: "monthly", Notes: "<p>Monthly :MONTH</p>", Price: "999.123456789123456789", TaxName1: "VAT", TaxRate1: "20", TaxName2: "Local", TaxRate2: "1.25", TaxName3: "Other", TaxRate3: "0", TaxID: "7"}
}

func recurringTestDraft(t *testing.T) RecurringDraft {
	t.Helper()
	draft, err := BuildRecurringDraft("client-A", "2040-01-31", "2040-01-31", recurringTestProduct(), false, recurringTestMarker)
	if err != nil {
		t.Fatal(err)
	}
	return draft
}

func recurringTestRecord(t *testing.T) RecurringRecord {
	t.Helper()
	draft := recurringTestDraft(t)
	return RecurringRecord{
		ID: "recurring-A", ClientID: draft.ClientID, StatusID: "1", PrivateNotes: draft.PrivateNotes,
		FrequencyID: "5", NextSendDate: draft.NextSendDate, NextSendDatetime: "2040-02-29T08:00:00.000000Z",
		DueDateDays: "on_receipt", AutoBill: "off", RemainingCycles: -1,
		Amount: "1211.43703603983703603669", Balance: "1211.43703603983703603669", UsesInclusiveTaxes: draft.UsesInclusiveTaxes,
		LineItems: append([]InvoiceLine(nil), draft.LineItems...),
		Discount:  "0", IsAmountDiscount: true, TaxRate1: "0", TaxRate2: "0", TaxRate3: "0",
		CustomSurcharge1: "0", CustomSurcharge2: "0", CustomSurcharge3: "0", CustomSurcharge4: "0",
		Partial: "0", PaidToDate: "0", ExchangeRate: "1",
	}
}

func recurringTestJSON(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func recurringTestMap(t *testing.T, value any) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(recurringTestJSON(t, value), &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

func recurringTestReply(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	_, _ = w.Write(recurringTestJSON(t, value))
}

func recurringTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-TOKEN") != recurringTestToken || r.Header.Get("X-Requested-With") != "XMLHttpRequest" || r.Header.Get("Accept") != "application/json" {
			t.Error("missing authenticated JSON request headers")
		}
		if r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-Idempotency-Key") != "" {
			t.Error("invented idempotency contract")
		}
		if r.Method == http.MethodPost && r.Header.Get("Content-Type") != "application/json" {
			t.Error("POST is not JSON")
		}
		w.Header().Set("X-APP-VERSION", SupportedRecurringVersion)
		handler(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func recurringTestClient(t *testing.T, base string, pageSize, maxPages int) *RecurringClient {
	t.Helper()
	c, err := NewRecurringClient(base, recurringTestToken, time.Second, pageSize, maxPages)
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return time.Date(2040, 1, 1, 12, 0, 0, 0, time.Local) }
	return c
}

func recurringTestPage(data []RecurringRecord, page, size, total int) any {
	pages := total / size
	if total%size != 0 {
		pages++
	}
	return map[string]any{
		"data":  data,
		"meta":  map[string]any{"pagination": Pagination{Total: total, Count: len(data), PerPage: size, CurrentPage: page, TotalPages: pages}},
		"links": map[string]any{"next": "https://must-never-follow.invalid/secret"},
	}
}

func TestRecurringCapabilityAndConfiguration(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeOf((*Client)(nil)), reflect.TypeOf((*BillingClient)(nil))} {
		for _, method := range []string{"CreateDraft", "StartFirst", "Stop", "Resume"} {
			if _, ok := typ.MethodByName(method); ok {
				t.Fatalf("%v exposes %s", typ, method)
			}
		}
	}
	for _, method := range []string{"Post", "Stop", "Resume", "CreateInvoice", "IssueInvoice"} {
		if _, ok := reflect.TypeOf((*RecurringClient)(nil)).MethodByName(method); ok {
			t.Fatalf("unexpected capability %s", method)
		}
	}
	for _, bounds := range [][2]int{{0, 1}, {-1, 1}, {101, 1}, {1, 0}, {1, -1}, {1, 101}} {
		if c, err := NewRecurringClient("https://synthetic.invalid", recurringTestToken, time.Second, bounds[0], bounds[1]); c != nil || err == nil {
			t.Fatalf("accepted bounds %v", bounds)
		}
	}
	for _, args := range []struct {
		base, token string
		timeout     time.Duration
	}{
		{"https://synthetic.invalid/api/v1", recurringTestToken, time.Second},
		{"https://user:secret@synthetic.invalid", recurringTestToken, time.Second},
		{"https://synthetic.invalid?secret=x", recurringTestToken, time.Second},
		{"https://synthetic.invalid/#", recurringTestToken, time.Second},
		{"https://synthetic.invalid/a/../b", recurringTestToken, time.Second},
		{"https://synthetic.invalid", "", time.Second},
		{"https://synthetic.invalid", " secret", time.Second},
		{"https://synthetic.invalid", "secret\n", time.Second},
		{"https://synthetic.invalid", recurringTestToken, 0},
		{"https://synthetic.invalid", recurringTestToken, 31 * time.Second},
	} {
		if c, err := NewRecurringClient(args.base, args.token, args.timeout, 1, 1); c != nil || err == nil {
			t.Fatal("accepted invalid configuration")
		}
	}
	if c, err := NewRecurringClient("https://synthetic.invalid/subpath", recurringTestToken, time.Second, 100, 100); err != nil || c.now == nil {
		t.Fatal("valid configuration rejected")
	}
}

func TestBuildRecurringCalendarAndFrozenProduct(t *testing.T) {
	for _, tc := range []struct{ paid, next string }{
		{"2039-01-29", "2039-02-28"}, {"2039-01-30", "2039-02-28"}, {"2039-01-31", "2039-02-28"},
		{"2040-01-29", "2040-02-29"}, {"2040-01-30", "2040-02-29"}, {"2040-01-31", "2040-02-29"},
		{"2040-03-31", "2040-04-30"}, {"2040-12-31", "2041-01-31"},
	} {
		d, err := BuildRecurringDraft("client-A", tc.paid, tc.paid, recurringTestProduct(), true, recurringTestMarker)
		if err != nil || d.NextSendDate != tc.next || !d.UsesInclusiveTaxes {
			t.Fatalf("%s => %+v, %v", tc.paid, d, err)
		}
	}
	for _, bad := range []string{"2040-2-01", "2040-02-30", "0000-01-01", "2040-01-01T00:00:00Z", "2040-01-01 ", "9999-12-31"} {
		today := "2040-01-31"
		if bad == "9999-12-31" {
			today = bad // Exercise calendar overflow, not the future-payment guard.
		}
		if _, err := BuildRecurringDraft("client-A", bad, today, recurringTestProduct(), false, recurringTestMarker); err == nil {
			t.Fatalf("accepted payment date %q", bad)
		}
	}
	for _, today := range []string{"2040-1-01", "2040-02-29", "2040-03-01"} {
		if _, err := BuildRecurringDraft("client-A", "2040-01-31", today, recurringTestProduct(), false, recurringTestMarker); err == nil {
			t.Fatalf("accepted today %q", today)
		}
	}
	product := recurringTestProduct()
	draft, err := BuildRecurringDraft("client-A", "2040-01-31", "2040-01-31", product, false, recurringTestMarker)
	if err != nil {
		t.Fatal(err)
	}
	before := product
	product.Price, product.ProductKey, product.Notes, product.TaxRate1 = "1", "changed", "changed", "0"
	line := draft.LineItems[0]
	if line.Cost != before.Price || line.ProductKey != before.ProductKey || line.Notes != before.Notes || line.TaxRate1 != before.TaxRate1 || line.TaxRate2 != before.TaxRate2 || line.TaxRate3 != before.TaxRate3 || line.TaxName1 != before.TaxName1 || line.TaxName2 != before.TaxName2 || line.TaxName3 != before.TaxName3 || line.TaxID != before.TaxID || line.Quantity != "1" || line.Discount != "0" || line.TypeID != "1" || line.IsAmountDiscount {
		t.Fatal("product snapshot differs")
	}
	for _, mutate := range []func(*Product){func(p *Product) { p.IsDeleted = true }, func(p *Product) { p.ArchivedAt = 1 }, func(p *Product) { p.Price = "0" }, func(p *Product) { p.TaxRate1 = "" }, func(p *Product) { p.ProductKey = "" }} {
		p := recurringTestProduct()
		mutate(&p)
		if _, err := BuildRecurringDraft("client-A", "2040-01-31", "2040-01-31", p, false, recurringTestMarker); err == nil {
			t.Fatal("accepted invalid product")
		}
	}
	for _, n := range []int{20, 128} {
		if _, err := BuildRecurringDraft("client-A", "2040-01-31", "2040-01-31", recurringTestProduct(), false, "payplump:"+strings.Repeat("a", n)+":recurring_create"); err != nil {
			t.Fatal("valid marker rejected")
		}
	}
	for _, marker := range []string{"payplump:short:recurring_create", "payplump:" + strings.Repeat("a", 129) + ":recurring_create", "payplump:" + strings.Repeat("a", 20) + ":invoice_create", "payplump:" + strings.Repeat("а", 20) + ":recurring_create"} {
		if _, err := BuildRecurringDraft("client-A", "2040-01-31", "2040-01-31", recurringTestProduct(), false, marker); err == nil {
			t.Fatal("invalid marker accepted")
		}
	}
}

func TestBuildRecurringDraftRejectsFuturePaymentAnchor(t *testing.T) {
	for _, paid := range []string{"2040-01-30", "2040-01-31"} {
		if _, err := BuildRecurringDraft("client-A", paid, "2040-01-31", recurringTestProduct(), false, recurringTestMarker); err != nil {
			t.Fatalf("valid payment anchor %s: %v", paid, err)
		}
	}
	for _, paid := range []string{"2040-02-01", "2041-01-31"} {
		draft, err := BuildRecurringDraft("client-A", paid, "2040-01-31", recurringTestProduct(), false, recurringTestMarker)
		if !errors.Is(err, errRecurringDraft) || !reflect.DeepEqual(draft, RecurringDraft{}) {
			t.Fatalf("future payment anchor %s accepted: %+v %v", paid, draft, err)
		}
	}
}

func TestRecurringInvalidArgumentsAndExpiredIntentNeverNetwork(t *testing.T) {
	var calls atomic.Int32
	s := recurringTestServer(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Error(w, "unexpected", 500) })
	c := recurringTestClient(t, s.URL, 1, 1)
	mutations := []func(*RecurringDraft){
		func(d *RecurringDraft) { d.ClientID = "../client" }, func(d *RecurringDraft) { d.FrequencyID = 4 },
		func(d *RecurringDraft) { d.NextSendDate = "2040-2-29" }, func(d *RecurringDraft) { d.DueDateDays = "0" },
		func(d *RecurringDraft) { d.AutoBill = "always" }, func(d *RecurringDraft) { d.RemainingCycles = 0 },
		func(d *RecurringDraft) { d.PrivateNotes = "wrong" }, func(d *RecurringDraft) { d.LineItems = nil },
		func(d *RecurringDraft) { d.LineItems = append(d.LineItems, d.LineItems[0]) },
		func(d *RecurringDraft) { d.LineItems[0].Discount = "1" }, func(d *RecurringDraft) { d.LineItems[0].Discount = "" },
		func(d *RecurringDraft) { d.LineItems[0].IsAmountDiscount = true }, func(d *RecurringDraft) { d.LineItems[0].Cost = "0" },
		func(d *RecurringDraft) { d.LineItems[0].Cost = "1e99999" }, func(d *RecurringDraft) { d.LineItems[0].Quantity = "2" },
		func(d *RecurringDraft) { d.LineItems[0].TypeID = "2" }, func(d *RecurringDraft) { d.LineItems[0].TaxRate1 = "" },
	}
	for i, mutate := range mutations {
		draft := recurringTestDraft(t)
		mutate(&draft)
		if ValidateRecurringDraft(draft) == nil {
			t.Fatalf("invalid draft %d passed helper", i)
		}
		if _, err := c.CreateDraft(context.Background(), draft); err == nil {
			t.Fatalf("invalid create %d", i)
		}
		if _, err := c.StartFirst(context.Background(), "recurring-A", draft); err == nil {
			t.Fatalf("invalid start %d", i)
		}
	}
	for _, id := range []string{"", "..", "a/b"} {
		if _, err := c.Get(context.Background(), id); err == nil {
			t.Fatal("invalid GET")
		}
		if _, err := c.Find(context.Background(), id); err == nil {
			t.Fatal("invalid Find")
		}
		if _, err := c.StartFirst(context.Background(), id, recurringTestDraft(t)); err == nil {
			t.Fatal("invalid start ID")
		}
	}
	c.now = func() time.Time { return time.Date(2040, 2, 29, 0, 0, 0, 0, time.Local) }
	for _, start := range []bool{false, true} {
		var err error
		if start {
			_, err = c.StartFirst(context.Background(), "recurring-A", recurringTestDraft(t))
		} else {
			_, err = c.CreateDraft(context.Background(), recurringTestDraft(t))
		}
		if !errors.Is(err, ErrRecurringExpired) {
			t.Fatalf("expired intent: %v", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid arguments reached network")
	}
}

func TestRecurringCreateHTTPContractAndCallerSliceSnapshot(t *testing.T) {
	draft := recurringTestDraft(t)
	record := recurringTestRecord(t)
	record.LineItems[0].Notes = "sanitized presentation"
	var paths []string
	s := recurringTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if r.URL.RawQuery != "" {
			t.Error("unexpected action/query flags")
		}
		switch r.URL.Path {
		case "/ninja/api/v1/recurring_invoices":
			if r.Method != "POST" {
				t.Error("create must POST")
			}
			body, _ := io.ReadAll(r.Body)
			var got map[string]json.RawMessage
			if json.Unmarshal(body, &got) != nil {
				t.Fatal("invalid JSON")
			}
			want := recurringTestMap(t, recurringTestDraft(t))
			if !reflect.DeepEqual(got, want) {
				t.Errorf("create payload changed: %s", body)
			}
			if string(got["frequency_id"]) != "5" || string(got["uses_inclusive_taxes"]) != "false" || string(got["remaining_cycles"]) != "-1" {
				t.Error("wrong scalar encoding")
			}
			for _, name := range []string{"action", "status_id", "currency_id", "first_invoice_id", "amount", "payment", "clone", "send_now", "email", "auto_bill_enabled", "due_date", "date"} {
				if _, ok := got[name]; ok {
					t.Errorf("forbidden field %s", name)
				}
			}
			recurringTestReply(t, w, map[string]any{"data": record})
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	c := recurringTestClient(t, s.URL+"/ninja", 1, 1)
	clockCalls := 0
	c.now = func() time.Time {
		clockCalls++
		if clockCalls == 2 {
			draft.LineItems[0].Cost = "1"
			draft.LineItems[0].ProductKey = "caller mutated"
		}
		return time.Date(2040, 1, 1, 12, 0, 0, 0, time.Local)
	}
	got, err := c.CreateDraft(context.Background(), draft)
	if err != nil || got.ID != record.ID || got.StatusID != "1" || !recurringEqual(got.Balance, got.Amount) {
		t.Fatalf("create = %+v, %v", got, err)
	}
	if !reflect.DeepEqual(paths, []string{"POST /ninja/api/v1/recurring_invoices"}) {
		t.Fatalf("unexpected requests %v", paths)
	}
	if draft.LineItems[0].Cost != "1" {
		t.Fatal("snapshot test did not mutate caller data")
	}
}

func TestRecurringSourceShapedDraftBalanceCreationAndStart(t *testing.T) {
	// BaseRepository:454 -> getRecurringInvoice(); InvoiceSum:260-267 and
	// InvoiceSumInclusive:227-235 assign balance = amount, including DRAFT.
	for _, inclusive := range []bool{false, true} {
		t.Run(fmt.Sprintf("inclusive=%t", inclusive), func(t *testing.T) {
			draft := recurringTestDraft(t)
			draft.UsesInclusiveTaxes = inclusive
			record := recurringTestRecord(t)
			record.UsesInclusiveTaxes = inclusive
			record.Amount = "1211.437036"
			if inclusive {
				record.Amount = "999.123457"
			}
			record.Balance = record.Amount
			var paths []string
			s := recurringTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				path := r.Method + " " + r.URL.Path
				paths = append(paths, path)
				switch path {
				case "POST /api/v1/recurring_invoices", "GET /api/v1/recurring_invoices/recurring-A":
					recurringTestReply(t, w, map[string]any{"data": record})
				case "POST /api/v1/recurring_invoices/bulk":
					after := record
					after.StatusID = "2" // Starting changes status, not balance/amount.
					recurringTestReply(t, w, map[string]any{"data": []RecurringRecord{after}})
				default:
					t.Errorf("unexpected request %s", path)
					http.NotFound(w, r)
				}
			})
			c := recurringTestClient(t, s.URL, 1, 1)
			created, err := c.CreateDraft(context.Background(), draft)
			if err != nil || created.StatusID != "1" || !recurringEqual(created.Amount, record.Amount) || !recurringEqual(created.Balance, record.Amount) {
				t.Fatalf("source-shaped creation: %+v %v", created, err)
			}
			started, err := c.StartFirst(context.Background(), created.ID, draft)
			if err != nil || started.StatusID != "2" || !recurringEqual(started.Amount, created.Amount) || !recurringEqual(started.Balance, started.Amount) {
				t.Fatalf("source-shaped start: %+v %v", started, err)
			}
			want := []string{"POST /api/v1/recurring_invoices", "GET /api/v1/recurring_invoices/recurring-A", "POST /api/v1/recurring_invoices/bulk"}
			if !reflect.DeepEqual(paths, want) {
				t.Fatalf("unexpected workflow %v", paths)
			}
		})
	}
}

func TestRecurringStrictPresenceAndTypes(t *testing.T) {
	base := recurringTestMap(t, recurringTestRecord(t))
	for key := range base {
		for _, null := range []bool{false, true} {
			t.Run(fmt.Sprintf("root/%s/null=%t", key, null), func(t *testing.T) {
				fields := recurringTestMap(t, recurringTestRecord(t))
				if null {
					fields[key] = json.RawMessage("null")
				} else {
					delete(fields, key)
				}
				var record RecurringRecord
				if json.Unmarshal(recurringTestJSON(t, fields), &record) == nil {
					t.Fatal("missing/null root field accepted")
				}
			})
		}
	}
	lineFields := recurringTestMap(t, recurringTestRecord(t).LineItems[0])
	for key := range lineFields {
		for _, null := range []bool{false, true} {
			t.Run(fmt.Sprintf("line/%s/null=%t", key, null), func(t *testing.T) {
				root := recurringTestMap(t, recurringTestRecord(t))
				line := recurringTestMap(t, recurringTestRecord(t).LineItems[0])
				if null {
					line[key] = json.RawMessage("null")
				} else {
					delete(line, key)
				}
				root["line_items"] = recurringTestJSON(t, []any{line})
				var record RecurringRecord
				if json.Unmarshal(recurringTestJSON(t, root), &record) == nil {
					t.Fatal("missing/null line field accepted")
				}
			})
		}
	}
	for key, bad := range map[string]string{"id": "3", "client_id": "false", "frequency_id": "5", "remaining_cycles": "\"-1\"", "auto_bill_enabled": "0", "uses_inclusive_taxes": "0", "is_deleted": "0", "amount": "\"NaN\"", "balance": "\"1e99999\"", "exchange_rate": "true", "archived_at": "-1", "next_send_date": "\"2040-2-29\"", "next_send_datetime": "\"2040-02-29\"", "last_sent_date": "\"2039-12-01T08:00:00\"", "due_date": "\"2040-02-29T08:00:00Z\"", "line_items": "{}"} {
		root := recurringTestMap(t, recurringTestRecord(t))
		root[key] = json.RawMessage(bad)
		var record RecurringRecord
		if json.Unmarshal(recurringTestJSON(t, root), &record) == nil {
			t.Errorf("invalid type/value accepted: %s", key)
		}
	}
	root := recurringTestMap(t, recurringTestRecord(t))
	root["unknown_future_metadata"] = json.RawMessage(`{"anything":[1,true]}`)
	var record RecurringRecord
	if err := json.Unmarshal(recurringTestJSON(t, root), &record); err != nil {
		t.Fatal("unknown metadata rejected", err)
	}
	// Preserve the legacy read-only DTO's permissive partial decoder.
	var legacy RecurringInvoice
	if json.Unmarshal([]byte(`{"id":"old","frequency_id":"5"}`), &legacy) != nil {
		t.Fatal("legacy DTO compatibility changed")
	}
}

func TestRecurringMatchingPolicyAndExactFinancials(t *testing.T) {
	draft := recurringTestDraft(t)
	good := recurringTestRecord(t)
	good.LineItems[0].Notes = "API rewrites :MONTH"
	good.LineItems[0].Cost = "999.1234567891234567890"
	good.LineItems[0].Quantity = "1.0"
	good.LineItems[0].TaxRate1 = "2e1"
	good.Amount = "12345678901234567890.123456789" // authoritative, NOT locally recomputed
	good.Balance = good.Amount
	if err := ValidateRecurringForDraft(good, draft); err != nil {
		t.Fatal("valid exact amounts rejected", err)
	}
	for _, field := range []string{"discount", "tax_rate1", "tax_rate2", "tax_rate3", "custom_surcharge1", "custom_surcharge2", "custom_surcharge3", "custom_surcharge4", "partial", "paid_to_date"} {
		root := recurringTestMap(t, recurringTestRecord(t))
		root[field] = json.RawMessage(`0.000000000000000001`)
		var record RecurringRecord
		if err := json.Unmarshal(recurringTestJSON(t, root), &record); err != nil {
			t.Fatal("foreign financial history not preserved", err)
		}
		if ValidateRecurringForDraft(record, draft) == nil {
			t.Errorf("accepted nonzero %s", field)
		}
	}
	equivalent := recurringTestRecord(t)
	equivalent.Balance = "1211.437036039837036036690"
	if err := ValidateRecurringForDraft(equivalent, draft); err != nil {
		t.Fatal("numerically equal balance rejected", err)
	}
	for _, balance := range []json.Number{"0", "1", "1211.43703603983703603668", "-1"} {
		r := recurringTestRecord(t)
		r.Balance = balance
		if ValidateRecurringForDraft(r, draft) == nil {
			t.Errorf("accepted balance %s different from amount %s", balance, r.Amount)
		}
	}
	mutations := []func(*RecurringRecord){
		func(r *RecurringRecord) { r.ClientID = "other" }, func(r *RecurringRecord) { r.PrivateNotes += "x" },
		func(r *RecurringRecord) { r.FrequencyID = "4" }, func(r *RecurringRecord) { r.NextSendDate = "2040-03-01" },
		func(r *RecurringRecord) { r.NextSendDatetime = "" }, func(r *RecurringRecord) { r.LastSentDate = "2040-01-01" },
		func(r *RecurringRecord) { r.LastSentDate = "2040-01-01 08:00:00" }, func(r *RecurringRecord) { r.LastSentDate = "2040-01-01T08:00:00.000000Z" },
		func(r *RecurringRecord) { r.DueDate = "2040-02-29" }, func(r *RecurringRecord) { r.DueDateDays = "0" },
		func(r *RecurringRecord) { r.AutoBill = "always" }, func(r *RecurringRecord) { r.AutoBillEnabled = true },
		func(r *RecurringRecord) { r.RemainingCycles = 1 }, func(r *RecurringRecord) { r.UsesInclusiveTaxes = true },
		func(r *RecurringRecord) { r.Amount, r.Balance = "0", "0" }, func(r *RecurringRecord) { r.ExchangeRate = "1.000000000000000001" },
		func(r *RecurringRecord) { r.LineItems = append(r.LineItems, r.LineItems[0]) },
		func(r *RecurringRecord) { r.LineItems[0].ProductKey = "other" }, func(r *RecurringRecord) { r.LineItems[0].TaxID = "other" },
		func(r *RecurringRecord) { r.LineItems[0].TaxName2 = "other" }, func(r *RecurringRecord) { r.LineItems[0].TaxRate3 = "1" },
		func(r *RecurringRecord) { r.LineItems[0].Cost = "999.123456789123456788" }, func(r *RecurringRecord) { r.LineItems[0].Quantity = "2" },
		func(r *RecurringRecord) { r.LineItems[0].Discount = "1" }, func(r *RecurringRecord) { r.LineItems[0].IsAmountDiscount = true },
		func(r *RecurringRecord) { r.LineItems[0].TypeID = "2" },
	}
	for i, mutate := range mutations {
		r := recurringTestRecord(t)
		mutate(&r)
		if ValidateRecurringForDraft(r, draft) == nil {
			t.Errorf("policy mismatch %d accepted", i)
		}
	}
	// Matching payload is not an authorization/adoption decision about lifecycle.
	good.IsDeleted = true
	good.ArchivedAt = 123
	good.StatusID = "2"
	if err := ValidateRecurringForDraft(good, draft); err != nil {
		t.Fatal("helper should leave lifecycle to caller", err)
	}
}

func TestRecurringGetAndFindCompleteForeignHistories(t *testing.T) {
	a := recurringTestRecord(t)
	a.LastSentDate = "2039-11-01" // Legacy date-only history remains readable.
	b := recurringTestRecord(t)
	b.ID = "recurring-B"
	b.IsDeleted = true
	b.ArchivedAt = 100
	b.LineItems = append(b.LineItems, b.LineItems[0])
	b.LineItems[1].Quantity = "2"
	b.LineItems[1].Cost = "-2"
	b.Discount = "3"
	b.LastSentDate = "2039-12-01 08:00:00" // Raw SQL UTC datetime.
	b.StatusID = "3"
	cRecord := recurringTestRecord(t)
	cRecord.ID = "recurring-C"
	cRecord.ArchivedAt = 101
	cRecord.LastSentDate = "2039-12-02T08:00:00.000000Z" // Carbon serialization.
	var requests atomic.Int32
	s := recurringTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != "GET" {
			t.Error("lookup wrote")
		}
		if strings.HasSuffix(r.URL.Path, "/recurring-B") {
			if r.URL.RawQuery != "" {
				t.Error("GET query")
			}
			recurringTestReply(t, w, map[string]any{"data": b})
			return
		}
		if r.URL.Path != "/api/v1/recurring_invoices" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		want := url.Values{"client_id": {"client-A"}, "status": {"active,archived,deleted"}, "client_status": {"all"}, "sort": {"id|asc"}, "per_page": {"2"}, "page": {r.URL.Query().Get("page")}}
		if !reflect.DeepEqual(r.URL.Query(), want) {
			t.Errorf("query %v", r.URL.Query())
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page == 1 {
			recurringTestReply(t, w, recurringTestPage([]RecurringRecord{a, b}, 1, 2, 3))
		} else {
			recurringTestReply(t, w, recurringTestPage([]RecurringRecord{cRecord}, 2, 2, 3))
		}
	})
	c := recurringTestClient(t, s.URL, 2, 2)
	record, err := c.Get(context.Background(), b.ID)
	if err != nil || !reflect.DeepEqual(record, b) {
		t.Fatalf("Get=%+v %v", record, err)
	}
	records, err := c.Find(context.Background(), "client-A")
	if err != nil || !reflect.DeepEqual(records, []RecurringRecord{a, b, cRecord}) {
		t.Fatalf("Find=%+v %v", records, err)
	}
	if requests.Load() != 3 {
		t.Fatal("followed remote links or incomplete inventory")
	}
	if ValidateRecurringForDraft(records[1], recurringTestDraft(t)) == nil {
		t.Fatal("foreign history matches first-cycle policy")
	}
}

func TestRecurringFindRejectsInvalidLastSentDatetime(t *testing.T) {
	for _, value := range []string{"2039-12-01T08:00:00", "2039-02-29 08:00:00", "2039-12-01 25:00:00", "2039-12-01 8:00:00", "0000-01-01T00:00:00Z", "2039-12-01T08:00:00Zjunk"} {
		t.Run(value, func(t *testing.T) {
			var calls atomic.Int32
			s := recurringTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				page, _ := strconv.Atoi(r.URL.Query().Get("page"))
				record := recurringTestRecord(t)
				record.ID = fmt.Sprintf("recurring-%d", page)
				record.LastSentDate = "2039-12-01 08:00:00"
				if page == 2 {
					record.LastSentDate = value
				}
				recurringTestReply(t, w, recurringTestPage([]RecurringRecord{record}, page, 1, 2))
			})
			records, err := recurringTestClient(t, s.URL, 1, 2).Find(context.Background(), "client-A")
			if records != nil || !errors.Is(err, ErrLookupIncomplete) || calls.Load() != 2 {
				t.Fatalf("invalid history exposed partial inventory: %v %v calls=%d", records, err, calls.Load())
			}
		})
	}
	// Only last_sent_date gains datetime support; these remain date-only.
	for _, field := range []string{"next_send_date", "due_date"} {
		for _, value := range []string{"2040-02-29 08:00:00", "2040-02-29T08:00:00.000000Z"} {
			root := recurringTestMap(t, recurringTestRecord(t))
			root[field] = recurringTestJSON(t, value)
			var record RecurringRecord
			if json.Unmarshal(recurringTestJSON(t, root), &record) == nil {
				t.Errorf("datetime accepted for date-only %s: %s", field, value)
			}
		}
	}
}

func TestRecurringFindFailsClosed(t *testing.T) {
	for _, scenario := range []string{"foreign", "duplicate", "changed-total", "changed-size", "wrong-page", "missing-meta", "null-data", "missing-field", "cap", "pages-cap", "http", "empty"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int32
			s := recurringTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				page, _ := strconv.Atoi(r.URL.Query().Get("page"))
				record := recurringTestRecord(t)
				record.ID = fmt.Sprintf("recurring-%d", page)
				if scenario == "http" {
					http.Error(w, "remote-secret", 500)
					return
				}
				if scenario == "empty" {
					recurringTestReply(t, w, recurringTestPage([]RecurringRecord{}, 1, 1, 0))
					return
				}
				if scenario == "foreign" {
					record.ClientID = "other"
				}
				if scenario == "duplicate" {
					record.ID = "same"
				}
				total, size := 2, 1
				if scenario == "changed-total" && page == 2 {
					total = 3
				}
				if scenario == "changed-size" && page == 2 {
					size = 2
				}
				if scenario == "wrong-page" {
					page = 2
				}
				if scenario == "cap" {
					total = 2001
				}
				if scenario == "pages-cap" {
					total = 3
				}
				response := recurringTestPage([]RecurringRecord{record}, page, size, total).(map[string]any)
				if scenario == "missing-meta" {
					delete(response, "meta")
				}
				if scenario == "null-data" {
					response["data"] = nil
				}
				if scenario == "missing-field" {
					fields := recurringTestMap(t, record)
					delete(fields, "auto_bill_enabled")
					response["data"] = []any{fields}
				}
				recurringTestReply(t, w, response)
			})
			c := recurringTestClient(t, s.URL, 1, 2)
			records, err := c.Find(context.Background(), "client-A")
			if scenario == "empty" {
				if err != nil || records == nil || len(records) != 0 {
					t.Fatalf("empty inventory %v %v", records, err)
				}
				return
			}
			if !errors.Is(err, ErrLookupIncomplete) || records != nil {
				t.Fatalf("partial inventory exposed %v %v", records, err)
			}
			if strings.Contains(err.Error(), "remote-secret") {
				t.Fatal("response body leaked")
			}
			if calls.Load() > 2 {
				t.Fatal("pagination bound exceeded")
			}
		})
	}
	// The 2000-record cap applies even when page count itself is within bounds.
	s := recurringTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		records := make([]RecurringRecord, 100)
		for i := range records {
			records[i] = recurringTestRecord(t)
			records[i].ID = fmt.Sprintf("r-%d", i)
		}
		recurringTestReply(t, w, recurringTestPage(records, 1, 100, 2001))
	})
	if records, err := recurringTestClient(t, s.URL, 100, 100).Find(context.Background(), "client-A"); records != nil || !errors.Is(err, ErrLookupIncomplete) {
		t.Fatal("record cap ignored")
	}
}

func TestRecurringGetWrongEntityAndEnvelope(t *testing.T) {
	for _, scenario := range []string{"wrong-id", "foreign-client", "null", "missing", "array", "invalid"} {
		t.Run(scenario, func(t *testing.T) {
			s := recurringTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				record := recurringTestRecord(t)
				switch scenario {
				case "wrong-id":
					record.ID = "other"
				case "foreign-client":
					record.ClientID = "other"
				case "null":
					recurringTestReply(t, w, map[string]any{"data": nil})
					return
				case "missing":
					recurringTestReply(t, w, map[string]any{})
					return
				case "array":
					recurringTestReply(t, w, map[string]any{"data": []RecurringRecord{record}})
					return
				case "invalid":
					fields := recurringTestMap(t, record)
					delete(fields, "due_date")
					recurringTestReply(t, w, map[string]any{"data": fields})
					return
				}
				recurringTestReply(t, w, map[string]any{"data": record})
			})
			record, err := recurringTestClient(t, s.URL, 1, 1).Get(context.Background(), "recurring-A")
			if scenario == "foreign-client" {
				if err != nil || record.ClientID != "other" {
					t.Fatal("Get must return identity for explicit caller validation")
				}
				return
			}
			if err == nil || record.ID != "" {
				t.Fatal("wrong entity/envelope accepted")
			}
		})
	}
}

func TestRecurringStartFirstHTTPContractAndSnapshot(t *testing.T) {
	draft := recurringTestDraft(t)
	before := recurringTestRecord(t)
	after := recurringTestRecord(t)
	after.StatusID = "2"
	after.LineItems[0].Notes = "sanitized"
	var paths []string
	s := recurringTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if r.URL.RawQuery != "" {
			t.Error("unexpected query flags")
		}
		switch r.URL.Path {
		case "/api/v1/recurring_invoices/recurring-A":
			recurringTestReply(t, w, map[string]any{"data": before})
		case "/api/v1/recurring_invoices/bulk":
			body, _ := io.ReadAll(r.Body)
			if r.Method != "POST" || string(body) != `{"ids":["recurring-A"],"action":"start"}` {
				t.Errorf("bulk payload/method %s %s", r.Method, body)
			}
			recurringTestReply(t, w, map[string]any{"data": []RecurringRecord{after}})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	c := recurringTestClient(t, s.URL, 1, 1)
	clockCalls := 0
	c.now = func() time.Time {
		clockCalls++
		if clockCalls == 3 {
			draft.LineItems[0].Cost = "2"
		}
		return time.Date(2040, 1, 1, 12, 0, 0, 0, time.Local)
	}
	got, err := c.StartFirst(context.Background(), before.ID, draft)
	if err != nil || got.StatusID != "2" || got.NextSendDate != before.NextSendDate || got.LastSentDate != "" {
		t.Fatalf("start=%+v %v", got, err)
	}
	if !reflect.DeepEqual(paths, []string{"GET /api/v1/recurring_invoices/recurring-A", "POST /api/v1/recurring_invoices/bulk"}) {
		t.Fatalf("wrong order %v", paths)
	}
}

func TestRecurringFirstStartRejectsExistingStateWithoutPOST(t *testing.T) {
	for _, scenario := range []string{"active", "paused", "completed", "deleted", "archived", "foreign", "wrong-id", "wrong-line", "sent", "sent-sql", "sent-carbon", "zero-balance", "wrong-balance", "due-date", "past-utc", "equal-utc", "empty-utc"} {
		t.Run(scenario, func(t *testing.T) {
			r := recurringTestRecord(t)
			switch scenario {
			case "active":
				r.StatusID = "2"
			case "paused":
				r.StatusID = "3"
			case "completed":
				r.StatusID = "4"
			case "deleted":
				r.IsDeleted = true
			case "archived":
				r.ArchivedAt = 100
			case "foreign":
				r.ClientID = "other"
			case "wrong-id":
				r.ID = "other"
			case "wrong-line":
				r.LineItems[0].Cost = "1"
			case "sent":
				r.LastSentDate = "2039-12-01"
			case "sent-sql":
				r.LastSentDate = "2039-12-01 08:00:00"
			case "sent-carbon":
				r.LastSentDate = "2039-12-01T08:00:00.000000Z"
			case "zero-balance":
				r.Balance = "0"
			case "wrong-balance":
				r.Balance = "1211.43703603983703603668"
			case "due-date":
				r.DueDate = "2040-02-29"
			case "past-utc":
				r.NextSendDatetime = "2040-01-01 11:59:59"
			case "equal-utc":
				r.NextSendDatetime = "2040-01-01T12:00:00Z"
			case "empty-utc":
				r.NextSendDatetime = ""
			}
			var gets, posts atomic.Int32
			s := recurringTestServer(t, func(w http.ResponseWriter, req *http.Request) {
				if req.Method == "POST" {
					posts.Add(1)
				} else {
					gets.Add(1)
				}
				recurringTestReply(t, w, map[string]any{"data": r})
			})
			c := recurringTestClient(t, s.URL, 1, 1)
			c.now = func() time.Time { return time.Date(2040, 1, 1, 12, 0, 0, 0, time.UTC) }
			if got, err := c.StartFirst(context.Background(), "recurring-A", recurringTestDraft(t)); err == nil || got.ID != "" {
				t.Fatal("unsafe first start accepted")
			}
			if posts.Load() != 0 || gets.Load() != 1 {
				t.Fatalf("unsafe lifecycle issued requests: gets %d posts %d", gets.Load(), posts.Load())
			}
		})
	}
}

func TestRecurringUTCFormats(t *testing.T) {
	for _, value := range []string{"2040-02-29T08:00:00.000000Z", "2040-02-29T11:00:00+03:00", "2040-02-29 08:00:00"} {
		parsed, err := recurringUTC(value)
		if err != nil || !parsed.Equal(time.Date(2040, 2, 29, 8, 0, 0, 0, time.UTC)) {
			t.Fatalf("UTC timestamp %q: %v %v", value, parsed, err)
		}
	}
	for _, value := range []string{"2040-02-29", "2040-02-29T08:00:00", "2040-02-30 08:00:00", "2040-02-29 8:00:00", "", "0000-01-01T00:00:00Z"} {
		if _, err := recurringUTC(value); err == nil {
			t.Fatalf("unsafe UTC timestamp %q", value)
		}
	}
}

func TestRecurringCrossMidnightAndUTCRechecks(t *testing.T) {
	for _, scenario := range []string{"create-local-midnight", "start-local-midnight", "start-utc-due-before-post", "start-utc-due-after-post"} {
		t.Run(scenario, func(t *testing.T) {
			var posts, reads atomic.Int32
			clockCalls := 0
			r := recurringTestRecord(t)
			utcBefore := time.Date(2040, 2, 27, 12, 0, 0, 0, time.Local).UTC()
			if strings.Contains(scenario, "utc") {
				r.NextSendDatetime = utcBefore.Add(time.Second).Format(time.RFC3339Nano)
			} else {
				r.NextSendDatetime = time.Date(2040, 2, 29, 12, 0, 0, 0, time.Local).UTC().Format(time.RFC3339Nano)
			}
			s := recurringTestServer(t, func(w http.ResponseWriter, req *http.Request) {
				if req.Method == "POST" {
					posts.Add(1)
					after := r
					after.StatusID = "2"
					recurringTestReply(t, w, map[string]any{"data": []RecurringRecord{after}})
					return
				}
				reads.Add(1)
				recurringTestReply(t, w, map[string]any{"data": r})
			})
			c := recurringTestClient(t, s.URL, 1, 1)
			c.now = func() time.Time {
				clockCalls++
				if strings.Contains(scenario, "local-midnight") {
					if clockCalls == 1 {
						return time.Date(2040, 2, 28, 23, 59, 59, 0, time.Local)
					}
					return time.Date(2040, 2, 29, 0, 0, 0, 0, time.Local)
				}
				// The local date stays future while the actual UTC send instant
				// expires, without changing process-global time.Local.
				if (scenario == "start-utc-due-before-post" && clockCalls >= 3) || (scenario == "start-utc-due-after-post" && posts.Load() > 0) {
					return utcBefore.Add(time.Second)
				}
				return utcBefore
			}
			var err error
			if strings.HasPrefix(scenario, "create") {
				_, err = c.CreateDraft(context.Background(), recurringTestDraft(t))
			} else {
				_, err = c.StartFirst(context.Background(), r.ID, recurringTestDraft(t))
			}
			if !errors.Is(err, ErrRecurringExpired) {
				t.Fatalf("expired schedule not rejected: %v", err)
			}
			wantPosts := int32(0)
			if scenario == "start-utc-due-after-post" {
				wantPosts = 1
				if !errors.Is(err, ErrRecurringUnknown) {
					t.Fatal("post-expiry did not require HOLD")
				}
			}
			wantReads := int32(1)
			if strings.HasPrefix(scenario, "create") {
				wantReads = 0
			}
			if posts.Load() != wantPosts || reads.Load() != wantReads {
				t.Fatalf("posts %d reads %d", posts.Load(), reads.Load())
			}
		})
	}
}

func TestRecurringGETAndPOSTAcceptReleaseHeaders(t *testing.T) {
	for _, operation := range []string{"get", "find", "create-response", "start-get", "start-response"} {
		for _, version := range []string{"", "5.13.47", "5.13.50", "5.13.46", "5.13.48", "5.13.47+build", "duplicate"} {
			t.Run(operation+"/"+version, func(t *testing.T) {
				var posts atomic.Int32
				s := recurringTestServer(t, func(w http.ResponseWriter, r *http.Request) {
					bad := operation == "get" || operation == "find" || (strings.HasSuffix(operation, "response") && r.Method == "POST") || (operation == "start-get" && strings.HasSuffix(r.URL.Path, "/recurring-A"))
					if bad {
						w.Header().Del("X-APP-VERSION")
						if version == "duplicate" {
							w.Header().Add("X-APP-VERSION", "5.13.47")
							w.Header().Add("X-APP-VERSION", "5.13.47")
						} else if version != "" {
							w.Header().Set("X-APP-VERSION", version)
						}
					}
					record := recurringTestRecord(t)
					if r.Method == "POST" {
						posts.Add(1)
						if strings.HasSuffix(r.URL.Path, "/bulk") {
							record.StatusID = "2"
							recurringTestReply(t, w, map[string]any{"data": []RecurringRecord{record}})
						} else {
							recurringTestReply(t, w, map[string]any{"data": record})
						}
						return
					}
					if operation == "find" {
						recurringTestReply(t, w, recurringTestPage([]RecurringRecord{record}, 1, 1, 1))
						return
					}
					recurringTestReply(t, w, map[string]any{"data": record})
				})
				c := recurringTestClient(t, s.URL, 1, 1)
				var err error
				switch {
				case operation == "get":
					_, err = c.Get(context.Background(), "recurring-A")
				case operation == "find":
					_, err = c.Find(context.Background(), "client-A")
				case strings.HasPrefix(operation, "create"):
					_, err = c.CreateDraft(context.Background(), recurringTestDraft(t))
				default:
					_, err = c.StartFirst(context.Background(), "recurring-A", recurringTestDraft(t))
				}
				if err != nil {
					t.Fatalf("release header gated valid response: %v", err)
				}
				wantPosts := int32(1)
				if operation == "get" || operation == "find" {
					wantPosts = 0
				}
				if posts.Load() != wantPosts {
					t.Fatalf("unexpected POST count %d", posts.Load())
				}
			})
		}
	}
}

func TestRecurringCreationAndFirstStartReceipts(t *testing.T) {
	for _, start := range []bool{false, true} {
		for _, scenario := range []string{"null", "missing-field", "wrong-id", "foreign", "status", "deleted", "archived", "sent", "sent-sql", "sent-carbon", "due-date", "changed-date", "changed-datetime", "changed-amount", "zero-balance", "wrong-balance", "extra-line", "empty-array", "two-records", "wrong-shape"} {
			if !start && (scenario == "wrong-id" || scenario == "changed-datetime" || scenario == "changed-amount") {
				continue
			} // create has no preassigned ID/authoritative amount or UTC clock requirement
			t.Run(fmt.Sprintf("start=%t/%s", start, scenario), func(t *testing.T) {
				var posts atomic.Int32
				s := recurringTestServer(t, func(w http.ResponseWriter, r *http.Request) {
					record := recurringTestRecord(t)
					if r.Method != "POST" {
						recurringTestReply(t, w, map[string]any{"data": record})
						return
					}
					posts.Add(1)
					if start {
						record.StatusID = "2"
					}
					switch scenario {
					case "null":
						recurringTestReply(t, w, map[string]any{"data": nil})
						return
					case "wrong-id":
						record.ID = "other"
					case "foreign":
						record.ClientID = "other"
					case "status":
						if start {
							record.StatusID = "1"
						} else {
							record.StatusID = "2"
						}
					case "deleted":
						record.IsDeleted = true
					case "archived":
						record.ArchivedAt = 1
					case "sent":
						record.LastSentDate = "2040-01-01"
					case "sent-sql":
						record.LastSentDate = "2040-01-01 08:00:00"
					case "sent-carbon":
						record.LastSentDate = "2040-01-01T08:00:00.000000Z"
					case "due-date":
						record.DueDate = "2040-02-29"
					case "changed-date":
						record.NextSendDate = "2040-03-01"
					case "changed-datetime":
						record.NextSendDatetime = "2040-02-29T09:00:00Z"
					case "changed-amount":
						record.Amount, record.Balance = "2", "2"
					case "zero-balance":
						record.Balance = "0"
					case "wrong-balance":
						record.Balance = "1211.43703603983703603668"
					case "extra-line":
						record.LineItems = append(record.LineItems, record.LineItems[0])
					}
					var data any = record
					if start {
						data = []RecurringRecord{record}
					}
					switch scenario {
					case "missing-field":
						fields := recurringTestMap(t, record)
						delete(fields, "uses_inclusive_taxes")
						data = fields
						if start {
							data = []any{fields}
						}
					case "empty-array":
						data = []RecurringRecord{}
					case "two-records":
						data = []RecurringRecord{record, record}
					case "wrong-shape":
						if start {
							data = record
						} else {
							data = []RecurringRecord{record}
						}
					}
					recurringTestReply(t, w, map[string]any{"data": data})
				})
				c := recurringTestClient(t, s.URL, 1, 1)
				var got RecurringRecord
				var err error
				if start {
					got, err = c.StartFirst(context.Background(), "recurring-A", recurringTestDraft(t))
				} else {
					got, err = c.CreateDraft(context.Background(), recurringTestDraft(t))
				}
				if !errors.Is(err, ErrRecurringUnknown) || got.ID != "" || posts.Load() != 1 {
					t.Fatalf("bad receipt not held: %+v %v posts=%d", got, err, posts.Load())
				}
			})
		}
	}
}

func TestRecurringUnknownOutcomesNeverAutoReplay(t *testing.T) {
	for _, start := range []bool{false, true} {
		for _, scenario := range []string{"disconnect", "malformed", "http", "redirect", "truncated"} {
			t.Run(fmt.Sprintf("start=%t/%s", start, scenario), func(t *testing.T) {
				var posts, reads atomic.Int32
				s := recurringTestServer(t, func(w http.ResponseWriter, r *http.Request) {
					if r.Method != "POST" {
						reads.Add(1)
						recurringTestReply(t, w, map[string]any{"data": recurringTestRecord(t)})
						return
					}
					posts.Add(1)
					switch scenario {
					case "disconnect":
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = conn.Close()
					case "malformed":
						_, _ = io.WriteString(w, `{"data":`)
					case "http":
						http.Error(w, recurringTestToken+" private body", 500)
					case "redirect":
						w.Header().Set("Location", "/must-not-follow")
						w.WriteHeader(http.StatusTemporaryRedirect)
					case "truncated":
						w.Header().Set("Content-Length", "9999")
						_, _ = io.WriteString(w, `{}`)
					}
				})
				c := recurringTestClient(t, s.URL, 1, 1)
				var err error
				if start {
					_, err = c.StartFirst(context.Background(), "recurring-A", recurringTestDraft(t))
				} else {
					_, err = c.CreateDraft(context.Background(), recurringTestDraft(t))
				}
				if !errors.Is(err, ErrRecurringUnknown) {
					t.Fatalf("unknown outcome not held: %v", err)
				}
				if strings.Contains(err.Error(), recurringTestToken) || strings.Contains(err.Error(), s.URL) || strings.Contains(err.Error(), "private body") {
					t.Fatal("error leaked secret")
				}
				wantReads := int32(0)
				if start {
					wantReads = 1
				}
				if posts.Load() != 1 || reads.Load() != wantReads {
					t.Fatalf("unknown result replayed/reconciled automatically: posts=%d reads=%d", posts.Load(), reads.Load())
				}
			})
		}
	}
}

func TestRecurringMalformed2xxPOSTIsUnknownAcrossReleases(t *testing.T) {
	for _, version := range []string{"5.13.47", "5.13.50", ""} {
		for _, body := range []string{`{`, `{"data":null}`, `{"data":{"id":"recurring-A"}}`, `{"data":{},"DATA":{}}`} {
			t.Run(version+"/"+body, func(t *testing.T) {
				var posts, reads atomic.Int32
				s := recurringTestServer(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Del("X-APP-VERSION")
					if version != "" {
						w.Header().Set("X-APP-VERSION", version)
					}
					if r.Method == http.MethodGet {
						reads.Add(1)
						recurringTestReply(t, w, map[string]any{})
						return
					}
					posts.Add(1)
					_, _ = io.WriteString(w, body)
				})
				c := recurringTestClient(t, s.URL, 1, 1)
				calls := 0
				got, err := c.CreateDraftAuthorized(context.Background(), recurringTestDraft(t), func(context.Context) error {
					calls++
					if reads.Load() != 0 || posts.Load() != 0 {
						t.Error("authorization ordering changed")
					}
					return nil
				})
				if !errors.Is(err, ErrRecurringUnknown) || got.ID != "" || calls != 1 || posts.Load() != 1 || reads.Load() != 0 {
					t.Fatalf("malformed POST was accepted or replayed: %+v %v posts=%d reads=%d", got, err, posts.Load(), reads.Load())
				}
			})
		}
	}
}
