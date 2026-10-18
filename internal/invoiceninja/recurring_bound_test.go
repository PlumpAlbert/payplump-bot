package invoiceninja

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

func recurringBoundFixture(t *testing.T) (RecurringDraft, RecurringRecord) {
	t.Helper()
	draft := recurringTestDraft(t)
	draft.LineItems[0].Cost = "200"
	draft.LineItems[0].TaxRate1, draft.LineItems[0].TaxRate2 = "0", "0"
	record := recurringTestRecord(t)
	record.LineItems = slices.Clone(draft.LineItems)
	record.Amount, record.Balance = "200", "200"
	if err := ValidateRecurringForDraft(record, draft); err != nil {
		t.Fatal(err)
	}
	return draft, record
}

func TestRecurringBoundInvalidReceiptNeverReachesIO(t *testing.T) {
	draft, record := recurringBoundFixture(t)
	valid := RecurringReceipt{Amount: record.Amount, NextSendDatetime: record.NextSendDatetime}
	type invalidCase struct {
		name    string
		receipt RecurringReceipt
		nilAuth bool
	}
	cases := []invalidCase{
		{name: "nil authorization", receipt: valid, nilAuth: true},
	}
	for _, amount := range []json.Number{"", "0", "0e2", "-1", "NaN", "1/2", "+200", "1e129", json.Number(strings.Repeat("1", 129))} {
		receipt := valid
		receipt.Amount = amount
		cases = append(cases, invalidCase{name: "amount/" + amount.String(), receipt: receipt})
	}
	for _, datetime := range []string{"", "2040-02-29", "2040-02-29T08:00:00", "2040-02-30T08:00:00Z", "0000-01-01T00:00:00Z"} {
		receipt := valid
		receipt.NextSendDatetime = datetime
		cases = append(cases, invalidCase{name: "datetime/" + datetime, receipt: receipt})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			s := recurringTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				http.Error(w, "unexpected I/O", http.StatusInternalServerError)
			})
			c := recurringTestClient(t, s.URL, 1, 1)
			calls := 0
			authorize := func(context.Context) error { calls++; return nil }
			want := ErrRecurringReceipt
			if tc.nilAuth {
				authorize, want = nil, errRecurringDraft
			}
			got, err := c.StartFirstBoundAuthorized(context.Background(), record.ID, draft, tc.receipt, authorize)
			if err != want || got.ID != "" || requests.Load() != 0 || calls != 0 || errors.Is(err, ErrRecurringUnknown) {
				t.Fatalf("invalid bound arguments reached I/O: err=%v requests=%d callbacks=%d", err, requests.Load(), calls)
			}
		})
	}
}

func TestRecurringBoundLastGETMustMatchOriginalReceipt(t *testing.T) {
	for _, change := range []string{"amount", "sub-float amount", "UTC second", "UTC nanosecond"} {
		t.Run(change, func(t *testing.T) {
			draft, original := recurringBoundFixture(t)
			changed := original
			switch change {
			case "amount":
				changed.Amount, changed.Balance = "999", "999"
			case "sub-float amount":
				changed.Amount, changed.Balance = "200.000000000000000001", "200.000000000000000001"
			case "UTC second":
				changed.NextSendDatetime = "2040-02-29T08:00:01Z"
			case "UTC nanosecond":
				changed.NextSendDatetime = "2040-02-29T08:00:00.000000001Z"
			}
			if !reflect.DeepEqual(changed.LineItems, original.LineItems) || original.LineItems[0].Cost != "200" {
				t.Fatal("regression must preserve the original 200 financial line")
			}
			var gets, posts atomic.Int32
			s := recurringTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET /api/v1/recurring_invoices/recurring-A":
					record := original
					if gets.Add(1) > 1 {
						record = changed
					}
					recurringTestReply(t, w, map[string]any{"data": record})
				case "GET /api/v1/ping":
					recurringTestReply(t, w, map[string]any{})
				case "POST /api/v1/recurring_invoices/bulk":
					posts.Add(1)
					after := changed
					after.StatusID = "2"
					recurringTestReply(t, w, map[string]any{"data": []RecurringRecord{after}})
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			})
			c := recurringTestClient(t, s.URL, 1, 1)
			first, err := c.Get(context.Background(), original.ID)
			if err != nil {
				t.Fatal(err)
			}
			receipt := RecurringReceipt{Amount: first.Amount, NextSendDatetime: first.NextSendDatetime}
			calls := 0
			got, err := c.StartFirstBoundAuthorized(context.Background(), original.ID, draft, receipt, func(context.Context) error { calls++; return nil })
			if err != ErrRecurringReceipt || errors.Is(err, ErrRecurringUnknown) || got.ID != "" || gets.Load() != 2 || posts.Load() != 0 || calls != 0 {
				t.Fatalf("changed last GET was not safely rejected: err=%v gets=%d posts=%d callbacks=%d", err, gets.Load(), posts.Load(), calls)
			}
			if receipt.Amount != original.Amount || receipt.NextSendDatetime != original.NextSendDatetime {
				t.Fatal("preflight refreshed the original receipt")
			}
		})
	}
}

func TestRecurringAllStartsCompareReceiptSemantically(t *testing.T) {
	datetimes := []string{"2040-02-29T08:00:00.000000Z", "2040-02-29T08:00:00Z", "2040-02-29T11:00:00+03:00", "2040-02-29 08:00:00"}
	for _, variant := range []string{"legacy", "authorized", "bound"} {
		for _, beforeTime := range datetimes {
			for _, afterTime := range datetimes {
				t.Run(variant+"/"+beforeTime+"/"+afterTime, func(t *testing.T) {
					draft, original := recurringBoundFixture(t)
					before, after := original, original
					before.Amount, before.Balance, before.NextSendDatetime = "200.0000", "200.0000", beforeTime
					after.Amount, after.Balance, after.NextSendDatetime, after.StatusID = "2e2", "2e2", afterTime, "2"
					var gets, posts atomic.Int32
					s := recurringTestServer(t, func(w http.ResponseWriter, r *http.Request) {
						switch r.Method + " " + r.URL.Path {
						case "GET /api/v1/recurring_invoices/recurring-A":
							gets.Add(1)
							recurringTestReply(t, w, map[string]any{"data": before})
						case "GET /api/v1/ping":
							recurringTestReply(t, w, map[string]any{})
						case "POST /api/v1/recurring_invoices/bulk":
							posts.Add(1)
							recurringTestReply(t, w, map[string]any{"data": []RecurringRecord{after}})
						default:
							t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
							http.NotFound(w, r)
						}
					})
					c := recurringTestClient(t, s.URL, 1, 1)
					calls := 0
					authorize := func(context.Context) error { calls++; return nil }
					var got RecurringRecord
					var err error
					switch variant {
					case "legacy":
						got, err = c.StartFirst(context.Background(), original.ID, draft)
					case "authorized":
						got, err = c.StartFirstAuthorized(context.Background(), original.ID, draft, authorize)
					case "bound":
						receipt := RecurringReceipt{Amount: original.Amount, NextSendDatetime: original.NextSendDatetime}
						got, err = c.StartFirstBoundAuthorized(context.Background(), original.ID, draft, receipt, authorize)
					}
					wantCalls := 1
					if variant == "legacy" {
						wantCalls = 0
					}
					if err != nil || got.ID != original.ID || got.StatusID != "2" || got.NextSendDatetime != afterTime || gets.Load() != 1 || posts.Load() != 1 || calls != wantCalls {
						t.Fatalf("equivalent decimal/UTC receipt became unknown: err=%v gets=%d posts=%d callbacks=%d", err, gets.Load(), posts.Load(), calls)
					}
				})
			}
		}
	}
}

func TestRecurringBoundChangedPOSTReceiptIsUnknown(t *testing.T) {
	for _, change := range []string{"amount", "sub-float amount", "UTC second", "UTC nanosecond", "status", "deleted", "archived", "sent"} {
		t.Run(change, func(t *testing.T) {
			draft, original := recurringBoundFixture(t)
			after := original
			after.StatusID = "2"
			switch change {
			case "amount":
				after.Amount, after.Balance = "999", "999"
			case "sub-float amount":
				after.Amount, after.Balance = "200.000000000000000001", "200.000000000000000001"
			case "UTC second":
				after.NextSendDatetime = "2040-02-29T08:00:01Z"
			case "UTC nanosecond":
				after.NextSendDatetime = "2040-02-29T08:00:00.000000001Z"
			case "status":
				after.StatusID = "1"
			case "deleted":
				after.IsDeleted = true
			case "archived":
				after.ArchivedAt = 1
			case "sent":
				after.LastSentDate = "2040-02-29 08:00:00"
			}
			var posts atomic.Int32
			s := recurringTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET /api/v1/recurring_invoices/recurring-A":
					recurringTestReply(t, w, map[string]any{"data": original})
				case "GET /api/v1/ping":
					recurringTestReply(t, w, map[string]any{})
				case "POST /api/v1/recurring_invoices/bulk":
					posts.Add(1)
					recurringTestReply(t, w, map[string]any{"data": []RecurringRecord{after}})
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			})
			c := recurringTestClient(t, s.URL, 1, 1)
			receipt := RecurringReceipt{Amount: original.Amount, NextSendDatetime: original.NextSendDatetime}
			calls := 0
			got, err := c.StartFirstBoundAuthorized(context.Background(), original.ID, draft, receipt, func(context.Context) error { calls++; return nil })
			if !errors.Is(err, ErrRecurringUnknown) || errors.Is(err, ErrRecurringReceipt) || got.ID != "" || posts.Load() != 1 || calls != 1 {
				t.Fatalf("changed POST receipt did not retain unknown barrier: err=%v posts=%d callbacks=%d", err, posts.Load(), calls)
			}
		})
	}
}
