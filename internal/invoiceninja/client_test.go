package invoiceninja_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
)

const (
	syntheticToken = "synthetic-api-token"
	precisePrice   = "12345678901234567890.123456789012345678"
	preciseAmount  = "9007199254740993.010000000000000001"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func clientAt(t *testing.T, base string) *invoiceninja.Client {
	t.Helper()
	client, err := invoiceninja.New(base, syntheticToken, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func staticClient(t *testing.T, body string) *invoiceninja.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return clientAt(t, server.URL)
}

func listBody(data string, total, count, perPage, currentPage, totalPages int) string {
	return fmt.Sprintf(`{"data":%s,"meta":{"pagination":{"total":%d,"count":%d,"per_page":%d,"current_page":%d,"total_pages":%d}}}`, data, total, count, perPage, currentPage, totalPages)
}

func TestListContracts(t *testing.T) {
	tests := []struct {
		resource string
		fixture  string
		call     func(*testing.T, *invoiceninja.Client) (invoiceninja.Pagination, string)
	}{
		{"clients", "customers", func(t *testing.T, c *invoiceninja.Client) (invoiceninja.Pagination, string) {
			p, err := c.ListCustomers(context.Background(), 1, 25)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Data) != 3 || p.Data[0].ID != "synthetic-client-1" || p.Data[0].Name != "Synthetic customer A" || p.Data[0].Number != "SYN-C-001" || p.Data[0].CustomValue3 != "123456789" || p.Data[0].CustomValue4 != "42" {
				t.Fatalf("unexpected customers: %+v", p.Data)
			}
			if !p.Data[0].Active() || p.Data[1].Active() || p.Data[2].Active() || p.Data[2].ArchivedAt != 2200000000 {
				t.Fatal("deleted/archived customer activity mismatch")
			}
			id, err := p.Data[0].TelegramID("id_number")
			if err != nil || id != 9007199254740993 {
				t.Fatalf("Telegram ID precision lost: %d, %v", id, err)
			}
			return p.Pagination, p.AppVersion
		}},
		{"products", "products", func(t *testing.T, c *invoiceninja.Client) (invoiceninja.Pagination, string) {
			p, err := c.ListProducts(context.Background(), 1, 25)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Data) != 1 || p.Data[0].Price.String() != precisePrice || p.Data[0].ProductKey != "SYNTHETIC-PLAN" || p.Data[0].Notes != "Synthetic product only" || !p.Data[0].Active() {
				t.Fatalf("unexpected products: %+v", p.Data)
			}
			return p.Pagination, p.AppVersion
		}},
		{"invoices", "invoices", func(t *testing.T, c *invoiceninja.Client) (invoiceninja.Pagination, string) {
			p, err := c.ListInvoices(context.Background(), 1, 25)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Data) != 1 || p.Data[0].Amount.String() != preciseAmount || p.Data[0].Balance.String() != "0" || p.Data[0].ClientID != "synthetic-client-1" || p.Data[0].RecurringID != "synthetic-recurring-1" || p.Data[0].StatusID != "2" || p.Data[0].Date != "2040-01-02" || p.Data[0].DueDate != "2040-02-03" {
				t.Fatalf("unexpected invoices: %+v", p.Data)
			}
			return p.Pagination, p.AppVersion
		}},
		{"payments", "payments", func(t *testing.T, c *invoiceninja.Client) (invoiceninja.Pagination, string) {
			p, err := c.ListPayments(context.Background(), 1, 25)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Data) != 1 || p.Data[0].Amount.String() != preciseAmount || len(p.Data[0].Paymentables) != 1 || len(p.Data[0].Invoices) != 1 {
				t.Fatalf("unexpected payments: %+v", p.Data)
			}
			payment := p.Data[0]
			link := payment.Paymentables[0]
			if link.Amount.String() != preciseAmount || link.Refunded.String() != "0" || link.PaymentID != payment.ID || link.InvoiceID != payment.Invoices[0].ID || payment.Invoices[0].Amount.String() != preciseAmount {
				t.Fatalf("incorrect included payment relationships: %+v", payment)
			}
			return p.Pagination, p.AppVersion
		}},
		{"recurring_invoices", "recurring_invoices", func(t *testing.T, c *invoiceninja.Client) (invoiceninja.Pagination, string) {
			p, err := c.ListRecurringInvoices(context.Background(), 1, 25)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Data) != 1 || p.Data[0].ClientID != "synthetic-client-1" || p.Data[0].StatusID != "2" || p.Data[0].FrequencyID != "5" || p.Data[0].NextSendDate != "2040-02-02" || p.Data[0].LastSentDate != "2040-01-02" {
				t.Fatalf("unexpected recurring invoices: %+v", p.Data)
			}
			return p.Pagination, p.AppVersion
		}},
	}
	for _, tt := range tests {
		t.Run(tt.resource, func(t *testing.T) {
			body := fixture(t, tt.fixture)
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != http.MethodGet || r.ContentLength != 0 {
					t.Errorf("expected bodyless GET, got %s, length %d", r.Method, r.ContentLength)
				}
				if r.URL.Path != "/installed/panel/api/v1/"+tt.resource {
					t.Errorf("path = %q", r.URL.Path)
				}
				wantQuery := url.Values{"page": {"1"}, "per_page": {"25"}}
				if tt.resource == "payments" {
					wantQuery.Set("include", "invoices")
				}
				if !reflect.DeepEqual(r.URL.Query(), wantQuery) {
					t.Errorf("query = %v, want %v", r.URL.Query(), wantQuery)
				}
				if r.Header.Get("X-API-TOKEN") != syntheticToken || r.Header.Get("X-Requested-With") != "XMLHttpRequest" || r.Header.Get("Accept") != "application/json" {
					t.Error("missing API headers")
				}
				w.Header().Set("X-APP-VERSION", "5.13.47")
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			pagination, version := tt.call(t, clientAt(t, server.URL+"/installed/panel/"))
			wantCount := 1
			if tt.resource == "clients" {
				wantCount = 3
			}
			if pagination != (invoiceninja.Pagination{Total: wantCount, Count: wantCount, PerPage: 25, CurrentPage: 1, TotalPages: 1}) || version != "5.13.47" {
				t.Fatalf("unexpected page metadata: %+v, %q", pagination, version)
			}
			if requests.Load() != 1 {
				t.Fatal("list must fetch exactly one page and ignore pagination links")
			}
		})
	}
}

func TestGetProduct(t *testing.T) {
	c := staticClient(t, fixture(t, "product"))
	p, err := c.GetProduct(context.Background(), "synthetic-product-1")
	if err != nil || p.ID != "synthetic-product-1" || p.Price.String() != precisePrice {
		t.Fatalf("product = %+v, error = %v", p, err)
	}
}

func TestGetProductEscapesIDAndPreservesEncodedPrefix(t *testing.T) {
	const id = "synthetic?product#100%"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/installed%20panel/api/v1/products/"+url.PathEscape(id) || r.URL.RawQuery != "" || r.Method != http.MethodGet {
			t.Errorf("unexpected product request %s %s", r.Method, r.URL.RequestURI())
		}
		_, _ = fmt.Fprintf(w, `{"data":{"id":%q,"price":0}}`, id)
	}))
	defer server.Close()
	if _, err := clientAt(t, server.URL+"/installed%20panel").GetProduct(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}

func TestGetProductRejectsMalformedResults(t *testing.T) {
	for _, body := range []string{
		`{}`, `null`, `{"data":null}`, `{"data":[]}`, `{"data":{}}`,
		`{"data":{"id":"","price":1}}`, `{"data":{"id":"other-product","price":1}}`,
		`{"data":{"id":"synthetic-product-1"}}`,
		`{"data":{"id":"synthetic-product-1","price":null}}`,
		`{"data":{"id":"synthetic-product-1","price":1},`,
	} {
		t.Run(body, func(t *testing.T) {
			p, err := staticClient(t, body).GetProduct(context.Background(), "synthetic-product-1")
			if err == nil || p.ID != "" {
				t.Fatalf("accepted malformed product: %+v, %v", p, err)
			}
		})
	}
}

func TestMoneyLexemes(t *testing.T) {
	for _, literal := range []string{"0", "42", "-0.00100", precisePrice, "1e9999", `"9007199254740993.000000000000000001"`, `"0"`, `"-1.2500"`, `"1.25e+10"`} {
		t.Run("valid_"+literal, func(t *testing.T) {
			body := `{"data":{"id":"synthetic-product-1","price":` + literal + `}}`
			p, err := staticClient(t, body).GetProduct(context.Background(), "synthetic-product-1")
			if err != nil || p.Price.String() != strings.Trim(literal, `"`) {
				t.Fatalf("number was rejected or rounded: %q, %v", p.Price, err)
			}
		})
	}
	for _, literal := range []string{"null", "true", "{}", "[]", `""`, `"NaN"`, `"Infinity"`, `"01"`, `"+1"`, `" 1 "`, `"1."`, `"1e"`, `"synthetic-private-value"`} {
		t.Run("invalid_"+literal, func(t *testing.T) {
			body := `{"data":{"id":"synthetic-product-1","price":` + literal + `}}`
			p, err := staticClient(t, body).GetProduct(context.Background(), "synthetic-product-1")
			if err == nil || p.ID != "" {
				t.Fatalf("accepted invalid price %s: %+v", literal, p)
			}
			if strings.Contains(err.Error(), "synthetic-private-value") {
				t.Fatal("decode error leaked a remote value")
			}
		})
	}
}

func TestInvalidArgumentsNeverReachNetwork(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer server.Close()
	c := clientAt(t, server.URL)
	methods := []func(context.Context, int, int) error{
		func(ctx context.Context, p, n int) error { _, err := c.ListCustomers(ctx, p, n); return err },
		func(ctx context.Context, p, n int) error { _, err := c.ListProducts(ctx, p, n); return err },
		func(ctx context.Context, p, n int) error { _, err := c.ListInvoices(ctx, p, n); return err },
		func(ctx context.Context, p, n int) error { _, err := c.ListPayments(ctx, p, n); return err },
		func(ctx context.Context, p, n int) error { _, err := c.ListRecurringInvoices(ctx, p, n); return err },
	}
	for i, call := range methods {
		for _, args := range [][2]int{{0, 25}, {-1, 25}, {1, 0}, {1, -1}, {1, 101}} {
			if err := call(context.Background(), args[0], args[1]); err == nil {
				t.Errorf("method %d accepted pagination %v", i, args)
			}
		}
	}
	for _, id := range []string{"", ".", "..", "a/b", `a\b`, " leading", "trailing ", "x\n", "x\x00"} {
		if _, err := c.GetProduct(context.Background(), id); err == nil {
			t.Errorf("accepted invalid product ID %q", id)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("invalid inputs reached the network")
	}
}

func TestEmptyAndLastPage(t *testing.T) {
	for _, totalPages := range []int{0, 1} {
		c := staticClient(t, listBody(`[]`, 0, 0, 25, 1, totalPages))
		p, err := c.ListCustomers(context.Background(), 1, 25)
		if err != nil || p.Data == nil || len(p.Data) != 0 || p.Pagination.Total != 0 {
			t.Fatalf("invalid empty page: %+v, %v", p, err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "2" || r.URL.Query().Get("per_page") != "1" {
			t.Errorf("wrong last-page query: %v", r.URL.Query())
		}
		_, _ = w.Write([]byte(listBody(`[{"id":"synthetic-client-2"}]`, 2, 1, 1, 2, 2)))
	}))
	defer server.Close()
	p, err := clientAt(t, server.URL).ListCustomers(context.Background(), 2, 1)
	if err != nil || p.Pagination.CurrentPage != 2 || len(p.Data) != 1 {
		t.Fatalf("invalid last page: %+v, %v", p, err)
	}
}

func TestMalformedListEnvelopes(t *testing.T) {
	const item = `[{"id":"synthetic-client-1"}]`
	bodies := []string{
		``, `{`, `null`, `[]`, `{}`, `{"data":[]}`, `{"data":[],"meta":null}`,
		`{"data":[],"meta":{}}`, `{"data":[],"meta":{"pagination":null}}`,
		`{"data":[],"meta":{"pagination":{}}}`, `{"data":[],"meta":{"pagination":[]}}`,
		listBody(`null`, 0, 0, 25, 1, 1), listBody(`{}`, 0, 0, 25, 1, 1),
		listBody(`[null]`, 1, 1, 25, 1, 1), listBody(`[{}]`, 1, 1, 25, 1, 1),
		listBody(`[{"id":""}]`, 1, 1, 25, 1, 1),
		listBody(`[{"id":123}]`, 1, 1, 25, 1, 1),
		listBody(`[{"id":"synthetic-client","name":{}}]`, 1, 1, 25, 1, 1),
		listBody(`[{"id":"synthetic-client","custom_value1":123}]`, 1, 1, 25, 1, 1),
		listBody(`[{"id":"synthetic-client","archived_at":"0"}]`, 1, 1, 25, 1, 1),
		listBody(`[{"id":"synthetic-client","archived_at":0.5}]`, 1, 1, 25, 1, 1),
		listBody(`[{"id":"synthetic-client","archived_at":9223372036854775808}]`, 1, 1, 25, 1, 1),
		listBody(`[{"id":"synthetic-client","is_deleted":"false"}]`, 1, 1, 25, 1, 1),
		listBody(item, 1, 1, 25, 2, 1), // wrong requested page
		listBody(item, 1, 0, 25, 1, 1), // wrong count
		listBody(item, 0, 1, 25, 1, 1), // total below count
		listBody(item, 1, 1, 0, 1, 1), listBody(item, 1, 1, 101, 1, 1),
		listBody(item, 1, 1, 25, 0, 1), listBody(item, 1, 1, 25, 1, 0),
		listBody(item, 1, 1, 25, 1, -1), listBody(item, 1, 1, 25, 1, 2),
		listBody(item, 26, 1, 25, 1, 2), // incomplete positive page
		listBody(`[]`, 1, 0, 25, 1, 1), listBody(`[]`, -1, 0, 25, 1, 1),
		listBody(`[]`, 0, 0, 25, 1, 2),
		listBody(`[{"id":"a"},{"id":"b"}]`, 2, 2, 1, 1, 2), // per_page below length
		strings.Replace(listBody(item, 1, 1, 25, 1, 1), `"total":1`, `"total":null`, 1),
		strings.Replace(listBody(item, 1, 1, 25, 1, 1), `"total":1`, `"total":"1"`, 1),
		strings.Replace(listBody(item, 1, 1, 25, 1, 1), `"total":1,`, ``, 1),
		listBody(item, 1, 1, 25, 1, 1) + `{}`, // trailing JSON
		strings.TrimSuffix(listBody(item, 1, 1, 25, 1, 1), "}"),
	}
	for i, body := range bodies {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			p, err := staticClient(t, body).ListCustomers(context.Background(), 1, 25)
			if err == nil || p.Data != nil {
				t.Fatalf("accepted malformed envelope %s: %+v, %v", body, p, err)
			}
		})
	}
	c := staticClient(t, listBody(`[]`, 0, 0, 25, 2, 1))
	if _, err := c.ListCustomers(context.Background(), 2, 25); err == nil {
		t.Fatal("accepted page two of an empty collection")
	}
}

func TestWrongEntityShapes(t *testing.T) {
	tests := []struct {
		fixture string
		old     string
		replace string
		call    func(*invoiceninja.Client) error
	}{
		{"products", `"price": ` + precisePrice, `"price": null`, func(c *invoiceninja.Client) error { _, err := c.ListProducts(context.Background(), 1, 25); return err }},
		{"products", `"id": "synthetic-product-1"`, `"id": ""`, func(c *invoiceninja.Client) error { _, err := c.ListProducts(context.Background(), 1, 25); return err }},
		{"invoices", `"status_id": "2"`, `"status_id": 2`, func(c *invoiceninja.Client) error { _, err := c.ListInvoices(context.Background(), 1, 25); return err }},
		{"invoices", `"balance": 0`, `"balance": null`, func(c *invoiceninja.Client) error { _, err := c.ListInvoices(context.Background(), 1, 25); return err }},
		{"payments", `"refunded": 0`, `"refunded": null`, func(c *invoiceninja.Client) error { _, err := c.ListPayments(context.Background(), 1, 25); return err }},
		{"payments", `"id": "synthetic-payment-1"`, `"id": ""`, func(c *invoiceninja.Client) error { _, err := c.ListPayments(context.Background(), 1, 25); return err }},
		{"recurring_invoices", `"frequency_id": "5"`, `"frequency_id": 5`, func(c *invoiceninja.Client) error {
			_, err := c.ListRecurringInvoices(context.Background(), 1, 25)
			return err
		}},
		{"recurring_invoices", `"id": "synthetic-recurring-1"`, `"id": ""`, func(c *invoiceninja.Client) error {
			_, err := c.ListRecurringInvoices(context.Background(), 1, 25)
			return err
		}},
	}
	for i, tt := range tests {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			body := fixture(t, tt.fixture)
			if !strings.Contains(body, tt.old) {
				t.Fatal("fixture replacement did not match")
			}
			if err := tt.call(staticClient(t, strings.Replace(body, tt.old, tt.replace, 1))); err == nil {
				t.Fatal("accepted malformed entity")
			}
		})
	}
}

func TestPaymentCollectionsRequireArrays(t *testing.T) {
	for _, field := range []string{"paymentables", "invoices"} {
		for _, value := range []string{`{}`, `null`, `"synthetic-private-value"`, `1`, `true`} {
			t.Run(field+"_"+value, func(t *testing.T) {
				paymentables, invoices := `[]`, `[]`
				if field == "paymentables" {
					paymentables = value
				} else {
					invoices = value
				}
				data := fmt.Sprintf(`[{"id":"synthetic-payment","amount":1,"paymentables":%s,"invoices":%s}]`, paymentables, invoices)
				c := staticClient(t, listBody(data, 1, 1, 25, 1, 1))
				if p, err := c.ListPayments(context.Background(), 1, 25); err == nil || p.Data != nil {
					t.Fatalf("accepted non-array %s: %+v, %v", field, p, err)
				}
			})
		}
	}
	for _, data := range []string{
		`[{"id":"synthetic-payment","amount":1,"paymentables":[{"id":"synthetic-link","amount":1,"refunded":0}],"invoices":[{"id":"","amount":1,"balance":0}]}]`,
		`[{"id":"synthetic-payment","amount":1,"paymentables":[{"id":"","amount":1,"refunded":0}],"invoices":[]}]`,
		`[{"id":"synthetic-payment","amount":1,"paymentables":[],"invoices":[null]}]`,
	} {
		c := staticClient(t, listBody(data, 1, 1, 25, 1, 1))
		if _, err := c.ListPayments(context.Background(), 1, 25); err == nil {
			t.Fatal("accepted malformed included record")
		}
	}
	c := staticClient(t, listBody(`[{"id":"synthetic-payment","amount":1,"paymentables":[],"invoices":[]}]`, 1, 1, 100, 1, 1))
	if p, err := c.ListPayments(context.Background(), 1, 100); err != nil || len(p.Data) != 1 || p.Data[0].Amount.String() != "1" {
		t.Fatalf("valid empty relationships/integer amount rejected: %+v, %v", p, err)
	}
}

func TestAppVersion(t *testing.T) {
	for _, versions := range [][]string{nil, {"5.13.47"}, {"5.13.50"}, {"5.13.47-beta.1+synthetic.001"}, {"5.13.47-0"}, {"0.0.0"}, {"05.13.47"}, {"5.13.47-01"}, {"v5.13.47"}, {"5.13"}, {""}, {"5.13.47", "5.13.47"}, {"synthetic-private-header"}, {"5.13.47+"}, {strings.Repeat("1", 129) + ".0.0"}} {
		t.Run(fmt.Sprint(versions), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for _, v := range versions {
					w.Header().Add("X-APP-VERSION", v)
				}
				_, _ = w.Write([]byte(listBody(`[]`, 0, 0, 25, 1, 1)))
			}))
			defer server.Close()
			p, err := clientAt(t, server.URL).ListCustomers(context.Background(), 1, 25)
			valid := versions == nil || (len(versions) == 1 && (versions[0] == "5.13.47" || versions[0] == "5.13.50" || versions[0] == "5.13.47-beta.1+synthetic.001" || versions[0] == "5.13.47-0" || versions[0] == "0.0.0"))
			if valid {
				want := ""
				if versions != nil {
					want = versions[0]
				}
				if err != nil || p.AppVersion != want {
					t.Fatalf("version = %q, error = %v", p.AppVersion, err)
				}
			} else if err != nil || p.AppVersion != "" {
				t.Fatalf("unsafe version handling: %q, %v", p.AppVersion, err)
			}
		})
	}
}

func TestAPIErrorsAreSanitized(t *testing.T) {
	for _, status := range []int{401, 403, 404, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = fmt.Fprintf(w, `{"message":"synthetic-private-name %s %s"}`, syntheticToken, r.URL)
			}))
			defer server.Close()
			p, err := clientAt(t, server.URL+"/synthetic-private-path").ListCustomers(context.Background(), 1, 25)
			var apiErr *integrationhttp.HTTPError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != status || p.Data != nil {
				t.Fatalf("unexpected API error: %v", err)
			}
			for _, secret := range []string{syntheticToken, "synthetic-private-name", "synthetic-private-path", server.URL} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("API error leaked private content: %v", err)
				}
			}
		})
	}
	c := staticClient(t, `{"data":[{"id":"synthetic-client","archived_at":"synthetic-private-value"}],"meta":{}}`)
	if _, err := c.ListCustomers(context.Background(), 1, 25); err == nil || strings.Contains(err.Error(), "synthetic-private-value") {
		t.Fatalf("unsafe decoding error: %v", err)
	}
}

func TestValidateClientConfigurationMatchesConstructor(t *testing.T) {
	for _, tc := range []struct {
		name, base, token string
		timeout           time.Duration
		valid             bool
	}{
		{"valid root", "https://synthetic.invalid", syntheticToken, time.Second, true},
		{"valid installed path", "https://synthetic.invalid/installed%20panel", syntheticToken, time.Second, true},
		{"valid embedded token space", "https://synthetic.invalid", "synthetic token", time.Second, true},
		{"invalid token", "https://synthetic.invalid", "bad\ntoken", time.Second, false},
		{"invalid service path", "https://synthetic.invalid/api/v1", syntheticToken, time.Second, false},
		{"invalid query", "https://synthetic.invalid/panel?secret=x", syntheticToken, time.Second, false},
		{"invalid timeout", "https://synthetic.invalid", syntheticToken, 31 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			validationErr := invoiceninja.ValidateClientConfiguration(tc.base, tc.token, tc.timeout)
			client, constructorErr := invoiceninja.New(tc.base, tc.token, tc.timeout)
			if valid := validationErr == nil; valid != tc.valid {
				t.Fatalf("validator valid=%v want=%v err=%v", valid, tc.valid, validationErr)
			}
			if valid := constructorErr == nil && client != nil; valid != tc.valid {
				t.Fatalf("constructor valid=%v want=%v err=%v", valid, tc.valid, constructorErr)
			}
		})
	}
}
