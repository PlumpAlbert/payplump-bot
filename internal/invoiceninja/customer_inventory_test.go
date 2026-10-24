package invoiceninja

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"testing"
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
func TestConfiguredProductKeyRequiresUniquePinnedCompleteActiveSource(t *testing.T) {
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
		{"wrong-version", "5.13.46", `{"data":{"id":"p1","product_key":"monthly","is_deleted":false,"archived_at":0}}`, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			legacy, _ := newImportReaderServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/ninja/api/v1/products/p1" {
					t.Fatal("unexpected product request")
				}
				w.Header().Set("X-APP-VERSION", tt.version)
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
