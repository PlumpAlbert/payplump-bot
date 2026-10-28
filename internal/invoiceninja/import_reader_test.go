package invoiceninja

import (
	"context"
	"errors"
	"fmt"
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

const importReaderToken = "synthetic-import-token"

func TestImportReaderGETOnlyConfigurationAndMethods(t *testing.T) {
	typ := reflect.TypeOf((*ImportReader)(nil))
	want := map[string]bool{"FindCustomers": true, "ListRecurring": true}
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		if !want[name] {
			t.Fatalf("unexpected import reader capability: %s", name)
		}
		delete(want, name)
	}
	if len(want) != 0 {
		t.Fatalf("missing methods: %v", want)
	}
	for _, args := range []struct {
		base, token string
		timeout     time.Duration
		pageSize    int
		maxPages    int
	}{
		{"http://synthetic.invalid", importReaderToken, time.Second, 0, 1},
		{"http://synthetic.invalid", importReaderToken, time.Second, 101, 1},
		{"http://synthetic.invalid", importReaderToken, time.Second, 1, 0},
		{"http://synthetic.invalid", importReaderToken, time.Second, 1, maxImportReaderPages + 1},
		{"http://user:secret@synthetic.invalid", importReaderToken, time.Second, 1, 1},
		{"http://synthetic.invalid", "", time.Second, 1, 1},
	} {
		if reader, err := NewImportReader(args.base, args.token, args.timeout, args.pageSize, args.maxPages); err == nil || reader != nil || strings.Contains(fmt.Sprint(err), importReaderToken) {
			t.Fatal("invalid import reader configuration accepted or disclosed")
		}
	}
}

func TestImportReaderCustomersExactFilterAndCompletePages(t *testing.T) {
	customer1 := importCustomerJSON("101", "7001")
	customer2 := importCustomerJSON("102", "7001")
	pages := map[int]string{
		1: importPageSize([]string{customer1}, 2, 1, 2, 1),
		2: importPageSize([]string{customer2}, 2, 2, 2, 1),
	}
	reader, calls := newImportReaderServerBound(t, 1, 10, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/ninja/api/v1/clients" {
			t.Errorf("unexpected method/path: %s %s", r.Method, r.URL.EscapedPath())
		}
		want := url.Values{"id_number": {"7001"}, "status": {billingStatuses}, "sort": {"id|asc"}, "per_page": {"1"}}
		page := r.URL.Query().Get("page")
		want.Set("page", page)
		if !reflect.DeepEqual(r.URL.Query(), want) {
			t.Errorf("wrong exact customer query: %v", r.URL.Query())
		}
		w.Header().Set("X-APP-VERSION", SupportedRecurringVersion)
		fmt.Fprint(w, pages[mustInt(page)])
	})
	got, err := reader.FindCustomers(context.Background(), 7001)
	if err != nil || len(got) != 2 || got[0].ID != "101" || got[1].ID != "102" || calls.Load() != 2 {
		t.Fatalf("complete customer result mismatch: count=%d calls=%d err=%v", len(got), calls.Load(), err)
	}
	if _, err := reader.FindCustomers(context.Background(), 0); err == nil || calls.Load() != 2 {
		t.Fatal("invalid Telegram ID caused provider I/O")
	}
}

func TestImportReaderRecurringExactFilterAndPreservesLegacyRecord(t *testing.T) {
	record := recurringTestRecord(t)
	record.ID, record.ClientID = "301", "201"
	record.PrivateNotes = "legacy note; not a bot marker"
	record.StatusID, record.FrequencyID = "3", "2"
	record.LineItems = append(record.LineItems, record.LineItems[0])
	wire := recurringTestMap(t, record)
	body := importPage([]string{string(recurringTestJSON(t, wire))}, 1, 1, 1)
	reader, calls := newImportReaderServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/ninja/api/v1/recurring_invoices" {
			t.Errorf("unexpected recurring method/path: %s %s", r.Method, r.URL.EscapedPath())
		}
		want := url.Values{"client_id": {"201"}, "status": {billingStatuses}, "client_status": {"all"}, "sort": {"id|asc"}, "per_page": {"10"}, "page": {"1"}}
		if !reflect.DeepEqual(r.URL.Query(), want) {
			t.Errorf("wrong recurring query: %v", r.URL.Query())
		}
		w.Header().Set("X-APP-VERSION", SupportedRecurringVersion)
		fmt.Fprint(w, body)
	})
	got, err := reader.ListRecurring(context.Background(), "201")
	if err != nil || len(got) != 1 || got[0].PrivateNotes != record.PrivateNotes || got[0].StatusID != record.StatusID || len(got[0].LineItems) != 2 || calls.Load() != 1 {
		t.Fatalf("legacy recurring record changed: records=%d calls=%d err=%v", len(got), calls.Load(), err)
	}
}

func TestImportReaderRejectsIncompleteOrAmbiguousCustomerInventory(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"duplicate id", importPage([]string{importCustomerJSON("101", "7001"), importCustomerJSON("101", "7001")}, 2, 1, 1)},
		{"ignored filter foreign id number", importPage([]string{importCustomerJSON("101", "7002")}, 1, 1, 1)},
		{"missing identity", importPage([]string{`{"id":"101","is_deleted":false,"archived_at":0,"private_notes":""}`}, 1, 1, 1)},
		{"case alias", importPage([]string{`{"ID":"101","id_number":"7001","private_notes":"","is_deleted":false,"archived_at":0}`}, 1, 1, 1)},
		{"casefold duplicate", importPage([]string{`{"id":"101","ID":"102","id_number":"7001","private_notes":"","is_deleted":false,"archived_at":0}`}, 1, 1, 1)},
		{"null id number", importPage([]string{`{"id":"101","id_number":null,"private_notes":"","is_deleted":false,"archived_at":0}`}, 1, 1, 1)},
		{"duplicate json key", `{"data":[{"id":"101","id":"102","id_number":"7001","private_notes":"","is_deleted":false,"archived_at":0}],"meta":{"pagination":{"total":1,"count":1,"per_page":10,"current_page":1,"total_pages":1}}}`},
		{"incomplete pagination", `{"data":[],"meta":{"pagination":{"total":0,"count":0,"per_page":10,"current_page":1}}}`},
		{"not all pages", importPage([]string{importCustomerJSON("101", "7001")}, 3, 1, 3)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader, _ := newImportReaderServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-APP-VERSION", SupportedRecurringVersion)
				fmt.Fprint(w, tc.body)
			})
			if _, err := reader.FindCustomers(context.Background(), 7001); err == nil {
				t.Fatal("invalid or incomplete inventory accepted")
			}
		})
	}
	t.Run("configured page bound", func(t *testing.T) {
		reader, calls := newImportReaderServerBound(t, 1, 1, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-APP-VERSION", SupportedRecurringVersion)
			fmt.Fprint(w, importPageSize([]string{importCustomerJSON("101", "7001")}, 2, 1, 2, 1))
		})
		if _, err := reader.FindCustomers(context.Background(), 7001); err == nil || calls.Load() != 1 {
			t.Fatal("inventory over configured page bound accepted or fetched unboundedly")
		}
	})
}

func TestImportReaderRejectsRecurringDuplicatesForeignRecordsAndDrift(t *testing.T) {
	record := recurringTestRecord(t)
	record.ID, record.ClientID = "301", "201"
	valid := string(recurringTestJSON(t, recurringTestMap(t, record)))
	foreign := recurringTestRecord(t)
	foreign.ID, foreign.ClientID = "302", "999"
	cases := []struct {
		name string
		page func(int) string
	}{
		{"duplicate ID", func(int) string { return importPage([]string{valid, valid}, 2, 1, 1) }},
		{"foreign customer", func(int) string {
			return importPage([]string{string(recurringTestJSON(t, recurringTestMap(t, foreign)))}, 1, 1, 1)
		}},
		{"partial recurring", func(int) string { return importPage([]string{`{"id":"301","client_id":"201"}`}, 1, 1, 1) }},
		{"casefold duplicate JSON", func(int) string { return importPage([]string{`{"id":"301","ID":"302","client_id":"201"}`}, 1, 1, 1) }},
		{"pagination drift", func(page int) string {
			if page == 1 {
				return importPageSize([]string{valid}, 2, 1, 2, 1)
			}
			return importPageSize([]string{valid}, 3, 2, 3, 1)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader, _ := newImportReaderServerBound(t, 1, 10, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-APP-VERSION", SupportedRecurringVersion)
				fmt.Fprint(w, tc.page(mustInt(r.URL.Query().Get("page"))))
			})
			if _, err := reader.ListRecurring(context.Background(), "201"); err == nil {
				t.Fatal("invalid or incomplete inventory accepted")
			}
		})
	}
}

func TestImportReaderReleaseHeadersAndRedactedErrors(t *testing.T) {
	for _, version := range []string{"", "5.13.47", "5.13.50", "5.13.46", "5.13.48", "v5.13.47", "5.13.47,5.13.47"} {
		t.Run(fmt.Sprintf("version-%q", version), func(t *testing.T) {
			reader, calls := newImportReaderServer(t, func(w http.ResponseWriter, r *http.Request) {
				if version != "" {
					w.Header().Set("X-APP-VERSION", version)
				}
				fmt.Fprint(w, importPage(nil, 0, 1, 0))
			})
			_, err := reader.ListRecurring(context.Background(), "201")
			if err != nil || calls.Load() != 1 {
				t.Fatalf("release header gated valid inventory: %v", err)
			}
		})
	}
	for _, status := range []int{401, 500, 307} {
		t.Run(fmt.Sprintf("http-%d", status), func(t *testing.T) {
			reader, calls := newImportReaderServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "http://remote-secret.invalid")
				w.WriteHeader(status)
				fmt.Fprint(w, "remote-secret")
			})
			_, err := reader.ListRecurring(context.Background(), "201")
			if err == nil || calls.Load() != 1 || strings.Contains(fmt.Sprint(err), "remote-secret") || strings.Contains(fmt.Sprint(err), "invalid") {
				t.Fatal("HTTP error followed, retried or disclosed")
			}
		})
	}
	t.Run("transient remains retryable", func(t *testing.T) {
		reader, _ := newImportReaderServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		})
		_, err := reader.ListRecurring(context.Background(), "201")
		if err == nil || errors.Is(err, ErrLookupIncomplete) {
			t.Fatalf("transient provider failure was made sticky: %v", err)
		}
	})
}

func newImportReaderServer(t *testing.T, handler http.HandlerFunc) (*ImportReader, *atomic.Int32) {
	return newImportReaderServerBound(t, 10, 10, handler)
}

func newImportReaderServerBound(t *testing.T, pageSize, maxPages int, handler http.HandlerFunc) (*ImportReader, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("X-API-TOKEN") != importReaderToken || r.Header.Get("X-Requested-With") != "XMLHttpRequest" || r.Header.Get("Accept") != "application/json" {
			t.Errorf("incorrect GET auth/accept headers")
		}
		handler(w, r)
	}))
	t.Cleanup(s.Close)
	reader, err := NewImportReader(s.URL+"/ninja", importReaderToken, time.Second, pageSize, maxPages)
	if err != nil {
		t.Fatal(err)
	}
	return reader, &calls
}

func importCustomerJSON(id, idNumber string) string {
	return fmt.Sprintf(`{"id":%q,"id_number":%q,"private_notes":"legacy","is_deleted":false,"archived_at":0}`, id, idNumber)
}

func importPage(records []string, total, page, totalPages int) string {
	return importPageSize(records, total, page, totalPages, 10)
}

func importPageSize(records []string, total, page, totalPages, perPage int) string {
	data := "[]"
	if records != nil {
		data = "[" + strings.Join(records, ",") + "]"
	}
	return fmt.Sprintf(`{"data":%s,"meta":{"pagination":{"total":%d,"count":%d,"per_page":%d,"current_page":%d,"total_pages":%d,"links":{"next":"https://must-not-follow.invalid/pagination"}},"other":{"next":"https://must-not-follow.invalid/meta"}},"links":{"next":"https://must-not-follow.invalid/root"}}`, data, total, len(records), perPage, page, totalPages)
}

func mustInt(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
