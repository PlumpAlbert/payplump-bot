package invoiceninja

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
)

func TestCustomerInventoryIsCompleteBeforeActorFiltering(t *testing.T) {
	legacy, calls := newImportReaderServerBound(t, 1, 2, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/ninja/api/v1/clients" || r.URL.Query().Has("id_number") {
			t.Fatal("discovery did not request complete GET-only inventory")
		}
		w.Header().Set("X-APP-VERSION", SupportedRecurringVersion)
		if r.URL.Query().Get("page") == "1" {
			fmt.Fprint(w, importPageSize([]string{importCustomerJSON("c1", "42")}, 2, 1, 2, 1))
		} else {
			fmt.Fprint(w, importPageSize([]string{importCustomerJSON("c2", "42")}, 2, 2, 2, 1))
		}
	})
	reader := &CustomerInventoryReader{ImportReader: legacy}
	rows, e := reader.ListCustomers(context.Background())
	if e != nil || len(rows) != 2 || rows[0].IDNumber != "42" || rows[1].IDNumber != "42" || calls.Load() != 2 {
		t.Fatal("duplicate actor hidden by prefilter or incomplete pagination")
	}
}
func TestCustomerInventoryDoesNotReturnPartialData(t *testing.T) {
	legacy, _ := newImportReaderServerBound(t, 1, 2, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-APP-VERSION", SupportedRecurringVersion)
		if r.URL.Query().Get("page") == "1" {
			fmt.Fprint(w, importPageSize([]string{importCustomerJSON("c1", "42")}, 2, 1, 2, 1))
		} else {
			http.Error(w, "synthetic", http.StatusServiceUnavailable)
		}
	})
	rows, e := (&CustomerInventoryReader{ImportReader: legacy}).ListCustomers(context.Background())
	if e == nil || rows != nil {
		t.Fatal("partial discovery inventory accepted")
	}
}
func TestCustomerInventoryHasOnlyGetCapabilities(t *testing.T) {
	typ := reflect.TypeOf((*CustomerInventoryReader)(nil))
	want := map[string]bool{"FindCustomers": true, "ListRecurring": true, "ListCustomers": true, "ProductKey": true}
	for i := 0; i < typ.NumMethod(); i++ {
		if !want[typ.Method(i).Name] {
			t.Fatal("unexpected inventory capability")
		}
		delete(want, typ.Method(i).Name)
	}
	if len(want) != 0 {
		t.Fatal("missing inventory read capability")
	}
}
func TestConfiguredProductKeyRequiresUniqueCompleteActiveSource(t *testing.T) {
	cases := []struct {
		name, version, body string
		valid               bool
	}{
		{"valid", SupportedRecurringVersion, `{"data":{"id":"p1","product_key":"monthly","is_deleted":false,"archived_at":0}}`, true},
		{"duplicate", SupportedRecurringVersion, `{"data":{"id":"p1","product_key":"monthly","product_key":"other","is_deleted":false,"archived_at":0}}`, false},
		{"case-alias", SupportedRecurringVersion, `{"data":{"ID":"p1","product_key":"monthly","is_deleted":false,"archived_at":0}}`, false},
		{"missing-deletion", SupportedRecurringVersion, `{"data":{"id":"p1","product_key":"monthly","archived_at":0}}`, false},
		{"missing-archive", SupportedRecurringVersion, `{"data":{"id":"p1","product_key":"monthly","is_deleted":false}}`, false},
		{"deleted", SupportedRecurringVersion, `{"data":{"id":"p1","product_key":"monthly","is_deleted":true,"archived_at":0}}`, false},
		{"wrong-id", SupportedRecurringVersion, `{"data":{"id":"other","product_key":"monthly","is_deleted":false,"archived_at":0}}`, false},
		{"new-version", "5.13.50", `{"data":{"id":"p1","product_key":"monthly","is_deleted":false,"archived_at":0}}`, true},
		{"missing-version", "", `{"data":{"id":"p1","product_key":"monthly","is_deleted":false,"archived_at":0}}`, true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			legacy, _ := newImportReaderServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/ninja/api/v1/products/p1" {
					t.Fatal("unexpected product request")
				}
				if tt.version != "" {
					w.Header().Set("X-APP-VERSION", tt.version)
				}
				fmt.Fprint(w, tt.body)
			})
			key, e := (&CustomerInventoryReader{ImportReader: legacy}).ProductKey(context.Background(), "p1")
			if tt.valid {
				if e != nil || key != "monthly" {
					t.Fatal("valid configured product rejected")
				}
			} else if e == nil || key != "" {
				t.Fatal("untrusted product key accepted")
			}
		})
	}
}

func TestConfiguredProductKeyErrorsAreSpecificAndRedacted(t *testing.T) {
	const valid = `{"data":{"id":"p1","product_key":"monthly","is_deleted":false,"archived_at":0}}`
	for _, tt := range []struct{ body, reason string }{
		{`null`, "invalid product envelope"},
		{`{}`, "missing product data envelope"},
		{`{"data":null}`, "missing product data envelope"},
		{`{"data":[]}`, "invalid product data envelope"},
		{`{"data":{"id":"p1","is_deleted":false,"archived_at":0}}`, "missing required product field product_key"},
		{strings.Replace(valid, `"is_deleted":false,`, "", 1), "missing required product field is_deleted"},
		{strings.Replace(valid, `,"archived_at":0`, "", 1), "missing required product field archived_at"},
		{strings.Replace(valid, `"id":"p1",`, "", 1), "missing required product field id"},
		{strings.Replace(valid, `"id":"p1"`, `"id":"remote-secret"`, 1), "product ID does not match configured ID"},
		{strings.Replace(valid, `"is_deleted":false`, `"is_deleted":true`, 1), "product is deleted"},
		{strings.Replace(valid, `"archived_at":0`, `"archived_at":1`, 1), "product is archived"},
		{strings.Replace(valid, `"monthly"`, `""`, 1), "empty product_key"},
		{strings.Replace(valid, `"monthly"`, `" remote-secret"`, 1), "invalid product_key"},
		{strings.Replace(valid, `"monthly"`, `{"remote-secret":true}`, 1), "invalid product_key"},
		{strings.Replace(valid, `"is_deleted":false`, `"is_deleted":"remote-secret"`, 1), "invalid product is_deleted"},
		{strings.Replace(valid, `"archived_at":0`, `"archived_at":"remote-secret"`, 1), "invalid product archived_at"},
		{strings.Replace(valid, `"id":"p1"`, `"id":[]`, 1), "invalid product id"},
		{`{"data":{},"DATA":{"remote-secret":true}}`, "invalid product JSON response"},
	} {
		t.Run(tt.reason, func(t *testing.T) {
			reader, calls := newImportReaderServer(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tt.body) })
			key, err := (&CustomerInventoryReader{ImportReader: reader}).ProductKey(context.Background(), "p1")
			if key != "" || !errors.Is(err, ErrLookupIncomplete) || !strings.Contains(err.Error(), tt.reason) || calls.Load() != 1 {
				t.Fatalf("missing schema diagnostic: key=%q err=%v calls=%d", key, err, calls.Load())
			}
			if strings.Contains(err.Error(), "remote-secret") || strings.Contains(err.Error(), importReaderToken) {
				t.Fatal("remote content leaked")
			}
		})
	}
	for _, status := range []int{401, 403, 404} {
		reader, calls := newImportReaderServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			fmt.Fprint(w, "remote-secret")
		})
		_, err := (&CustomerInventoryReader{ImportReader: reader}).ProductKey(context.Background(), "p1")
		var apiErr *integrationhttp.HTTPError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != status || calls.Load() != 1 || strings.Contains(err.Error(), "remote-secret") || strings.Contains(err.Error(), importReaderToken) {
			t.Fatalf("HTTP status lost or response leaked: %v", err)
		}
	}
}
