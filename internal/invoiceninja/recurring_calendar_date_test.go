package invoiceninja

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Synthetic identities with the calendar/time/discount representation observed
// from Ninja 5.13.50. Calendar date and scheduled UTC time are separate fields.
func sqlCalendarRecurringJSON(t *testing.T) string {
	t.Helper()
	record := recurringTestRecord(t)
	record.StatusID = "2"
	record.NextSendDate = "2026-11-21 00:00:00"
	record.NextSendDatetime = "2026-11-21 03:00:00"
	record.LastSentDate = "2026-10-21 03:00:01"
	record.Amount, record.Balance, record.ExchangeRate = "100", "100", "1"
	record.LineItems[0].Cost, record.LineItems[0].Quantity = "100", "1"
	record.LineItems[0].Discount = "0"
	record.LineItems[0].TaxRate1, record.LineItems[0].TaxRate2, record.LineItems[0].TaxRate3 = "0", "0", "0"
	root := recurringTestMap(t, record)
	var lines []map[string]json.RawMessage
	if err := json.Unmarshal(root["line_items"], &lines); err != nil {
		t.Fatal(err)
	}
	lines[0]["is_amount_discount"] = json.RawMessage("1")
	root["line_items"] = recurringTestJSON(t, lines)
	return string(recurringTestJSON(t, root))
}

func assertSQLCalendarRecurring(t *testing.T, record RecurringRecord) {
	t.Helper()
	if record.NextSendDate != "2026-11-21" {
		t.Fatalf("calendar date not normalized: %q", record.NextSendDate)
	}
	if record.NextSendDatetime != "2026-11-21 03:00:00" || record.LastSentDate != "2026-10-21 03:00:01" {
		t.Fatal("an instant was truncated or shifted")
	}
	if record.Amount != "100" || len(record.LineItems) != 1 || !record.LineItems[0].IsAmountDiscount || record.LineItems[0].Discount != "0" {
		t.Fatal("financial data changed during date normalization")
	}
}

func TestRecurringRecordAcceptsSQLMidnightCalendarDate(t *testing.T) {
	var record RecurringRecord
	if err := json.Unmarshal([]byte(sqlCalendarRecurringJSON(t)), &record); err != nil {
		t.Fatalf("valid SQL calendar date rejected: %v", err)
	}
	assertSQLCalendarRecurring(t, record)
}

func TestImportRecurringInventoryAcceptsSQLMidnightCalendarDate(t *testing.T) {
	body := importPageSize([]string{sqlCalendarRecurringJSON(t)}, 1, 1, 1, 50)
	reader, calls := newImportReaderServerBound(t, 50, 20, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/ninja/api/v1/recurring_invoices" || r.URL.Query().Get("client_id") != "client-A" {
			t.Error("unexpected inventory request")
		}
		w.Header().Set("X-APP-VERSION", "5.13.50")
		fmt.Fprint(w, body)
	})
	records, err := reader.ListRecurring(context.Background(), "client-A")
	if err != nil || len(records) != 1 || calls.Load() != 1 {
		t.Fatalf("valid inventory rejected: records=%d calls=%d err=%v", len(records), calls.Load(), err)
	}
	assertSQLCalendarRecurring(t, records[0])
}

func TestRecurringExactGETAcceptsSQLMidnightCalendarDate(t *testing.T) {
	body := `{"data":` + sqlCalendarRecurringJSON(t) + `}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/recurring_invoices/recurring-A" {
			t.Error("unexpected exact request")
		}
		fmt.Fprint(w, body)
	}))
	defer server.Close()
	reader, err := NewRecurringReader(server.URL, "synthetic-token", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	record, err := reader.Get(context.Background(), "recurring-A")
	if err != nil {
		t.Fatalf("valid exact record rejected: %v", err)
	}
	assertSQLCalendarRecurring(t, record)
}

func TestRecurringCalendarNormalizationIsNotTimestampTruncation(t *testing.T) {
	for _, value := range []string{
		"2026-11-21 00:00:01", "2026-11-21 03:00:00", "2026-11-21T00:00:00Z",
		"2026-11-21 00:00:00+03:00", "2026-02-30 00:00:00", "2026-2-03 00:00:00",
		" 2026-11-21 00:00:00", "2026-11-21 00:00:00junk",
	} {
		t.Run(value, func(t *testing.T) {
			root := recurringTestMap(t, recurringTestRecord(t))
			root["next_send_date"] = recurringTestJSON(t, value)
			var record RecurringRecord
			if json.Unmarshal(recurringTestJSON(t, root), &record) == nil {
				t.Fatal("ambiguous or invalid calendar representation accepted")
			}
		})
	}
	wire := sqlCalendarRecurringJSON(t)
	var record RecurringRecord
	if err := json.Unmarshal([]byte(wire), &record); err != nil {
		t.Fatal(err)
	}
	encoded := string(recurringTestJSON(t, record))
	if strings.Contains(encoded, `"next_send_date":"2026-11-21 00:00:00"`) || !strings.Contains(encoded, `"next_send_datetime":"2026-11-21 03:00:00"`) {
		t.Fatal("round trip changed calendar/instant semantics")
	}
}
