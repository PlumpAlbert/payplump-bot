package invoiceninja_test

import (
	"bytes"
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

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
)

// Synthetic contracts only. Source pin: 493c3884e4214028bc5970cb41bafbd9c4d96401.
// No live write verification is authorized or represented by these tests.
const billingMarker = "payplump:abcdefghijklmnopqrstuvwx:"

func billingClientAt(t *testing.T, base string, pageSize, maxPages int) *invoiceninja.BillingClient {
	t.Helper()
	c, err := invoiceninja.NewBillingClient(base, syntheticToken, time.Second, pageSize, maxPages)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func billingServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-APP-VERSION", "5.13.47")
		handler(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func billingDraft() invoiceninja.CustomerDraft {
	return invoiceninja.CustomerDraft{Name: "Synthetic customer", IDNumber: "9007199254740993", PrivateNotes: billingMarker + "customer_create"}
}

func billingInvoice(t *testing.T) invoiceninja.Invoice {
	t.Helper()
	var envelope struct{ Data invoiceninja.Invoice }
	if err := json.Unmarshal([]byte(fixture(t, "billing_invoice_draft")), &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Data
}

func billingInvoiceDraft(t *testing.T) invoiceninja.InvoiceDraft {
	t.Helper()
	i := billingInvoice(t)
	return invoiceninja.InvoiceDraft{ClientID: i.ClientID, Date: i.Date, DueDate: i.DueDate, PrivateNotes: i.PrivateNotes, UsesInclusiveTaxes: i.UsesInclusiveTaxes, LineItems: i.LineItems}
}

func billingJSON(t *testing.T, value any) string {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func billingCheckHeaders(t *testing.T, r *http.Request) {
	t.Helper()
	if r.Header.Get("X-API-TOKEN") != syntheticToken || r.Header.Get("X-Requested-With") != "XMLHttpRequest" || r.Header.Get("Accept") != "application/json" {
		t.Error("missing authenticated JSON headers")
	}
	if r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-Idempotency-Key") != "" {
		t.Error("invented idempotency contract")
	}
}

func TestBillingCapabilityAndConfiguration(t *testing.T) {
	for _, name := range []string{"CreateCustomer", "CreateInvoice", "IssueInvoice", "Post"} {
		if _, ok := reflect.TypeOf((*invoiceninja.Client)(nil)).MethodByName(name); ok {
			t.Fatalf("read-only Client exposes %s", name)
		}
	}
	for _, bounds := range [][2]int{{0, 1}, {101, 1}, {1, 0}, {1, -1}, {1, 1001}} {
		if c, err := invoiceninja.NewBillingClient("https://synthetic.invalid", syntheticToken, time.Second, bounds[0], bounds[1]); c != nil || err == nil {
			t.Fatalf("accepted invalid bounds %v", bounds)
		}
	}
	for _, args := range []struct {
		base, token string
		timeout     time.Duration
	}{{"https://synthetic.invalid/api/v1", syntheticToken, time.Second}, {"https://synthetic.invalid", "bad\nsecret", time.Second}, {"https://synthetic.invalid", syntheticToken, 0}} {
		if c, err := invoiceninja.NewBillingClient(args.base, args.token, args.timeout, 1, 1); c != nil || err == nil {
			t.Fatal("accepted invalid billing configuration")
		}
	}
}

func TestBillingFindCustomersCompleteInventory(t *testing.T) {
	pages := []string{fixture(t, "billing_customers_page1"), fixture(t, "billing_customers_page2")}
	var requests atomic.Int32
	s := billingServer(t, func(w http.ResponseWriter, r *http.Request) {
		billingCheckHeaders(t, r)
		n := int(requests.Add(1))
		if n > 2 {
			t.Error("followed remote pagination link")
			w.WriteHeader(500)
			return
		}
		want := url.Values{"page": {strconv.Itoa(n)}, "per_page": {"1"}, "status": {"active,archived,deleted"}, "id_number": {"9007199254740993"}}
		if r.Method != "GET" || r.URL.EscapedPath() != "/installed%20panel/api/v1/clients" || !reflect.DeepEqual(r.URL.Query(), want) {
			t.Errorf("unexpected inventory request: %s %s", r.Method, r.URL)
		}
		_, _ = io.WriteString(w, pages[n-1])
	})
	customers, err := billingClientAt(t, s.URL+"/installed%20panel", 1, 2).FindCustomers(context.Background(), 9007199254740993)
	if err != nil || len(customers) != 2 || requests.Load() != 2 || customers[0].PrivateNotes != billingMarker+"customer_create" || customers[1].Active() {
		t.Fatalf("incomplete customer inventory: %+v, %v", customers, err)
	}
}

func TestBillingFindInvoicesCompleteInventory(t *testing.T) {
	const clientID = "opaque?client#100%"
	invoice := billingInvoice(t)
	invoice.ClientID = clientID
	var requests atomic.Int32
	s := billingServer(t, func(w http.ResponseWriter, r *http.Request) {
		billingCheckHeaders(t, r)
		n := int(requests.Add(1))
		want := url.Values{"page": {strconv.Itoa(n)}, "per_page": {"1"}, "status": {"active,archived,deleted"}, "client_id": {clientID}}
		if r.Method != "GET" || r.URL.Path != "/prefix/api/v1/invoices" || !reflect.DeepEqual(r.URL.Query(), want) {
			t.Errorf("unexpected invoice lookup: %s %s", r.Method, r.URL)
		}
		item := invoice
		item.ID += strconv.Itoa(n)
		item.IsDeleted = n == 2
		_, _ = io.WriteString(w, listBody("["+billingJSON(t, item)+"]", 2, 1, 1, n, 2))
	})
	invoices, err := billingClientAt(t, s.URL+"/prefix", 1, 2).FindInvoices(context.Background(), clientID)
	if err != nil || len(invoices) != 2 || requests.Load() != 2 || invoices[1].ClientID != clientID || !invoices[1].IsDeleted || invoices[0].Amount.String() != preciseAmount || len(invoices[0].Invitations) != 1 {
		t.Fatalf("incomplete invoice inventory: %+v, %v", invoices, err)
	}
}

func TestBillingLookupFailsClosed(t *testing.T) {
	item := `[{"id":"one","id_number":"42"}]`
	first := listBody(item, 2, 1, 1, 1, 2)
	last := listBody(`[{"id":"two","id_number":"42"}]`, 2, 1, 1, 2, 2)
	for _, tt := range []struct {
		name       string
		pages      []string
		maxPages   int
		badVersion bool
	}{
		{"duplicate ID", []string{first, strings.Replace(last, `"two"`, `"one"`, 1)}, 2, false},
		{"changed total", []string{first, listBody(item, 3, 1, 1, 2, 3)}, 3, false},
		{"changed page size", []string{first, listBody(item, 3, 1, 2, 2, 2)}, 2, false},
		{"wrong page", []string{first, first}, 2, false},
		{"page bound", []string{first}, 1, false},
		{"server clamps page size", []string{listBody(item, 1, 1, 2, 1, 1)}, 2, false},
		{"malformed metadata", []string{`{"data":[],"meta":{}}`}, 2, false},
		{"missing data", []string{`{"meta":{"pagination":{}}}`}, 2, false},
		{"incomplete data", []string{listBody(`[]`, 1, 0, 1, 1, 1)}, 2, false},
		{"negative total", []string{listBody(`[]`, -1, 0, 1, 1, 0)}, 2, false},
		{"overflow total", []string{strings.Replace(first, `"total":2`, `"total":999999999999999999999999999`, 1)}, 2, false},
		{"ignored filter", []string{strings.Replace(first, `"42"`, `"43"`, 1)}, 2, false},
		{"noncanonical identity", []string{strings.Replace(first, `"42"`, `"042"`, 1)}, 2, false},
		{"overflow identity", []string{strings.Replace(first, `"42"`, `"9223372036854775808"`, 1)}, 2, false},
		{"invalid opaque ID", []string{strings.Replace(first, `"one"`, `"../one"`, 1)}, 2, false},
		{"bad second page version", []string{first, last}, 2, true},
		{"malformed second response", []string{first, `{`}, 2, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			s := billingServer(t, func(w http.ResponseWriter, r *http.Request) {
				n := int(requests.Add(1))
				if tt.badVersion && n == 2 {
					w.Header().Set("X-APP-VERSION", "5.99.0")
				}
				if n > len(tt.pages) {
					t.Error("exceeded expected page bound")
					w.WriteHeader(500)
					return
				}
				_, _ = io.WriteString(w, tt.pages[n-1])
			})
			got, err := billingClientAt(t, s.URL, 1, tt.maxPages).FindCustomers(context.Background(), 42)
			if !errors.Is(err, invoiceninja.ErrLookupIncomplete) || got != nil || requests.Load() > int32(tt.maxPages) {
				t.Fatalf("partial inventory escaped: %+v, %v", got, err)
			}
			if tt.badVersion && !errors.Is(err, invoiceninja.ErrWriteVersion) {
				t.Fatalf("version failure lost: %v", err)
			}
		})
	}
	for _, pages := range []int{0, 1} {
		s := billingServer(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, listBody(`[]`, 0, 0, 1, 1, pages))
		})
		got, err := billingClientAt(t, s.URL, 1, 1).FindCustomers(context.Background(), 42)
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("empty inventory rejected: %+v, %v", got, err)
		}
	}
}

func TestBillingLookupRejectsAmbiguousRawAuthorities(t *testing.T) {
	base := listBody(`[{"id":"existing","id_number":"42"}]`, 1, 1, 1, 1, 1)
	for name, body := range map[string]string{
		"data case-fold existing customer": `{"data":[{"id":"existing","id_number":"42"}],"DATA":[],"meta":{"pagination":{"total":0,"count":0,"per_page":1,"current_page":1,"total_pages":1}}}`,
		"duplicate DTO id":                 strings.Replace(base, `"id":"existing"`, `"id":"other","ID":"existing"`, 1),
		"pagination case-fold":             strings.Replace(base, `"total":1`, `"total":0,"TOTAL":1`, 1),
		"unknown metadata duplicate":       strings.Replace(base, `"pagination":{`, `"unknown":{"x":1,"X":2},"pagination":{`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			s := billingServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) })
			got, err := billingClientAt(t, s.URL, 1, 1).FindCustomers(context.Background(), 42)
			if got != nil || !errors.Is(err, invoiceninja.ErrLookupIncomplete) {
				t.Fatalf("ambiguous billing inventory escaped: %+v, %v", got, err)
			}
		})
	}
}

func TestBillingExactGetRejectsAmbiguousRawAuthorities(t *testing.T) {
	customer := `{"id":"billing-client","id_number":"9007199254740993","private_notes":"payplump:abcdefghijklmnopqrstuvwx:customer_create","is_deleted":false,"archived_at":0}`
	for name, body := range map[string]string{
		"envelope case-fold": `{"data":null,"DATA":` + customer + `}`,
		"DTO case-fold":      strings.Replace(customer, `"id":"billing-client"`, `"id":"other","ID":"billing-client"`, 1),
		"unknown metadata":   `{"data":` + customer + `,"meta":{"unknown":{"s":1,"S":2}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := billingServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) })
			got, err := billingClientAt(t, s.URL, 1, 1).GetCustomer(context.Background(), "billing-client")
			if got.ID != "" || !errors.Is(err, invoiceninja.ErrLookupIncomplete) {
				t.Fatalf("ambiguous exact billing read escaped: %+v, %v", got, err)
			}
		})
	}
}

func TestBillingWritesRejectAmbiguousReceiptsAndDoNotReplay(t *testing.T) {
	customer := `{"id":"billing-client","name":"Synthetic customer","id_number":"9007199254740993","private_notes":"payplump:abcdefghijklmnopqrstuvwx:customer_create","is_deleted":false,"archived_at":0}`
	var invoiceEnvelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(fixture(t, "billing_invoice_draft")), &invoiceEnvelope); err != nil {
		t.Fatal(err)
	}
	var issuedEnvelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(fixture(t, "billing_invoice_issued")), &issuedEnvelope); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		body string
		call func(*testing.T, *invoiceninja.BillingClient) error
	}{
		{"customer", `{"data":null,"DATA":` + customer + `}`, func(_ *testing.T, c *invoiceninja.BillingClient) error {
			_, err := c.CreateCustomer(context.Background(), billingDraft())
			return err
		}},
		{"invoice", `{"data":null,"DATA":` + string(invoiceEnvelope.Data) + `}`, func(t *testing.T, c *invoiceninja.BillingClient) error {
			_, err := c.CreateInvoice(context.Background(), billingInvoiceDraft(t))
			return err
		}},
		{"issue", `{"data":null,"DATA":` + string(issuedEnvelope.Data) + `}`, func(_ *testing.T, c *invoiceninja.BillingClient) error {
			_, err := c.IssueInvoice(context.Background(), invoiceninja.IssueDraft{Action: "mark_sent", IDs: []string{"billing-invoice"}})
			return err
		}},
		{"unknown metadata", `{"data":` + customer + `,"meta":{"unknown":{"x":1,"X":2}}}`, func(_ *testing.T, c *invoiceninja.BillingClient) error {
			_, err := c.CreateCustomer(context.Background(), billingDraft())
			return err
		}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			var posts atomic.Int32
			s := billingServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = io.WriteString(w, `{"data":[]}`)
					return
				}
				posts.Add(1)
				_, _ = io.WriteString(w, tt.body)
			})
			if err := tt.call(t, billingClientAt(t, s.URL, 1, 1)); err == nil || posts.Load() != 1 {
				t.Fatalf("ambiguous write receipt accepted or replayed: err=%v posts=%d", err, posts.Load())
			}
		})
	}
}

func TestBillingWriteVersionPreflightRequiresUniqueRawJSON(t *testing.T) {
	for _, body := range []string{
		`{"data":[],"DATA":[]}`,
		`{"data":[],"meta":{"pagination":{"total":0,"TOTAL":1}}}`,
		`{"data":[],"unknown":{"k":1,"K":2}}`,
	} {
		t.Run(body, func(t *testing.T) {
			var posts atomic.Int32
			s := billingServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts.Add(1)
				}
				_, _ = io.WriteString(w, body)
			})
			if _, err := billingClientAt(t, s.URL, 1, 1).CreateCustomer(context.Background(), billingDraft()); err == nil || posts.Load() != 0 {
				t.Fatalf("ambiguous version preflight authorized a write: %v posts=%d", err, posts.Load())
			}
		})
	}
}

func TestBillingLookupConnectionFailureIsRetryable(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := s.URL
	s.Close()
	got, err := billingClientAt(t, base, 1, 1).FindCustomers(context.Background(), 42)
	if got != nil || err == nil || errors.Is(err, invoiceninja.ErrLookupIncomplete) {
		t.Fatalf("connection failure was classified as incomplete inventory: %+v, %v", got, err)
	}
}

func TestBillingInvoiceLookupRechecksFilterAndNumbers(t *testing.T) {
	for _, change := range []func(*invoiceninja.Invoice){
		func(i *invoiceninja.Invoice) { i.ClientID = "other-client" },
		func(i *invoiceninja.Invoice) { i.Amount = "1e99999999999999999999999" },
		func(i *invoiceninja.Invoice) { i.Balance = "-1" },
	} {
		i := billingInvoice(t)
		change(&i)
		body := listBody("["+billingJSON(t, i)+"]", 1, 1, 1, 1, 1)
		s := billingServer(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) })
		got, err := billingClientAt(t, s.URL, 1, 1).FindInvoices(context.Background(), "billing-client")
		if got != nil || !errors.Is(err, invoiceninja.ErrLookupIncomplete) {
			t.Fatalf("accepted inconsistent invoice: %+v, %v", got, err)
		}
	}
}

func TestBillingIndividualReadsIncludeTrashedAndPinVersion(t *testing.T) {
	const id = "opaque?id#%"
	invoice := billingInvoice(t)
	invoice.ID, invoice.IsDeleted, invoice.ArchivedAt = id, true, 2200000000
	customer := invoiceninja.Customer{ID: id, IsDeleted: true, ArchivedAt: 2200000000}
	s := billingServer(t, func(w http.ResponseWriter, r *http.Request) {
		billingCheckHeaders(t, r)
		if r.Method != "GET" || r.URL.RawQuery != "" || !strings.HasSuffix(r.URL.EscapedPath(), "/"+url.PathEscape(id)) {
			t.Errorf("unexpected individual GET: %s %s", r.Method, r.URL)
		}
		switch r.URL.EscapedPath() {
		case "/installed%20panel/api/v1/clients/" + url.PathEscape(id):
			_, _ = io.WriteString(w, billingJSON(t, map[string]any{"data": customer}))
		case "/installed%20panel/api/v1/invoices/" + url.PathEscape(id):
			_, _ = io.WriteString(w, billingJSON(t, map[string]any{"data": invoice}))
		default:
			t.Error("unexpected individual endpoint")
			w.WriteHeader(404)
		}
	})
	c := billingClientAt(t, s.URL+"/installed%20panel", 1, 1)
	if got, err := c.GetCustomer(context.Background(), id); err != nil || !got.IsDeleted {
		t.Fatalf("trashed customer unavailable: %+v, %v", got, err)
	}
	if got, err := c.GetInvoice(context.Background(), id); err != nil || !got.IsDeleted || got.LineItems[0].Cost.String() != precisePrice || got.Invitations[0].ID != "synthetic-invitation" {
		t.Fatalf("trashed invoice unavailable: %+v, %v", got, err)
	}
	for _, resource := range []string{"clients", "invoices"} {
		for _, body := range []string{`null`, `{}`, `{"data":null}`, `{"data":[]}`, `{"data":{"id":"other","amount":1,"balance":1}}`} {
			s := billingServer(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) })
			c := billingClientAt(t, s.URL, 1, 1)
			if resource == "clients" {
				if got, err := c.GetCustomer(context.Background(), "wanted"); err == nil || got.ID != "" {
					t.Fatalf("accepted malformed customer: %+v, %v", got, err)
				}
			} else if got, err := c.GetInvoice(context.Background(), "wanted"); err == nil || got.ID != "" {
				t.Fatalf("accepted malformed invoice: %+v, %v", got, err)
			}
		}
	}
}

func TestBillingEveryReadAndMutationRequiresExactVersion(t *testing.T) {
	for _, versions := range [][]string{nil, {"5.13.46"}, {"5.13.48"}, {"5.99.0"}, {"5.13.47+build"}, {"5.13.47-beta.1"}, {"v5.13.47"}, {"synthetic-private-version"}, {"5.13.47", "5.13.47"}} {
		t.Run(fmt.Sprint(versions), func(t *testing.T) {
			var posts atomic.Int32
			s := billingServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Del("X-APP-VERSION")
				for _, version := range versions {
					w.Header().Add("X-APP-VERSION", version)
				}
				if r.Method == "POST" {
					posts.Add(1)
				}
				_, _ = io.WriteString(w, `{}`)
			})
			c := billingClientAt(t, s.URL, 1, 1)
			calls := []func() error{
				func() error { _, e := c.GetCustomer(context.Background(), "customer"); return e },
				func() error { _, e := c.GetInvoice(context.Background(), "invoice"); return e },
				func() error { _, e := c.FindCustomers(context.Background(), 42); return e },
				func() error { _, e := c.FindInvoices(context.Background(), "customer"); return e },
				func() error { _, e := c.CreateCustomer(context.Background(), billingDraft()); return e },
				func() error { _, e := c.CreateInvoice(context.Background(), billingInvoiceDraft(t)); return e },
				func() error {
					_, e := c.IssueInvoice(context.Background(), invoiceninja.IssueDraft{Action: "mark_sent", IDs: []string{"invoice"}})
					return e
				},
			}
			for _, call := range calls {
				if err := call(); !errors.Is(err, invoiceninja.ErrWriteVersion) || strings.Contains(err.Error(), "synthetic-private-version") {
					t.Fatalf("unsafe version handling: %v", err)
				}
			}
			if posts.Load() != 0 {
				t.Fatal("POST reached mismatched version")
			}
		})
	}
}

func TestBillingProductTaxCopyAndMutationContracts(t *testing.T) {
	productBody := fixture(t, "billing_product")
	customerBody := fixture(t, "billing_customer")
	invoiceBody := fixture(t, "billing_invoice_draft")
	issuedBody := fixture(t, "billing_invoice_issued")
	var requests []string
	var bodies []string
	s := billingServer(t, func(w http.ResponseWriter, r *http.Request) {
		billingCheckHeaders(t, r)
		requests = append(requests, r.Method+" "+r.URL.Path)
		if r.Method == "GET" {
			if r.URL.Path == "/panel/api/v1/products/billing-product" {
				// Inherited GetProduct keeps legacy format-only version validation.
				w.Header().Set("X-APP-VERSION", "5.99.0")
				_, _ = io.WriteString(w, productBody)
				return
			}
			want := url.Values{"page": {"1"}, "per_page": {"1"}, "status": {"active,archived,deleted"}}
			if r.URL.Path != "/panel/api/v1/clients" || !reflect.DeepEqual(r.URL.Query(), want) || r.ContentLength != 0 {
				t.Errorf("unbounded/wrong version probe: %s", r.URL)
			}
			_, _ = io.WriteString(w, `{"data":[]}`)
			return
		}
		if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" || r.URL.RawQuery != "" {
			t.Error("invalid mutation request")
		}
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		switch r.URL.Path {
		case "/panel/api/v1/clients":
			_, _ = io.WriteString(w, customerBody)
		case "/panel/api/v1/invoices":
			_, _ = io.WriteString(w, invoiceBody)
		case "/panel/api/v1/invoices/bulk":
			_, _ = io.WriteString(w, issuedBody)
		default:
			t.Error("unexpected POST endpoint")
			w.WriteHeader(404)
		}
	})
	c := billingClientAt(t, s.URL+"/panel", 25, 3)
	product, err := c.GetProduct(context.Background(), "billing-product")
	if err != nil || product.Price.String() != precisePrice || product.TaxID != "synthetic-tax-code" || product.TaxRate1.String() != "19.600000000000000001" || product.TaxRate2.String() != "0" || product.TaxName2 != "" {
		t.Fatalf("product precision/taxes lost: %+v, %v", product, err)
	}
	customer, err := c.CreateCustomer(context.Background(), billingDraft())
	if err != nil || customer.ID != "billing-client" {
		t.Fatalf("create customer: %+v, %v", customer, err)
	}
	draft := billingInvoiceDraft(t)
	draft.LineItems = []invoiceninja.InvoiceLine{{Discount: "0", TypeID: "1", ProductKey: product.ProductKey, Notes: product.Notes, Cost: product.Price, Quantity: "1", TaxName1: product.TaxName1, TaxName2: product.TaxName2, TaxName3: product.TaxName3, TaxRate1: product.TaxRate1, TaxRate2: product.TaxRate2, TaxRate3: product.TaxRate3, TaxID: product.TaxID}}
	invoice, err := c.CreateInvoice(context.Background(), draft)
	if err != nil || invoice.StatusID != "1" || invoice.Amount.String() != preciseAmount || invoice.LineItems[0].Cost.String() != precisePrice || invoice.UsesInclusiveTaxes || len(invoice.Invitations) != 1 {
		t.Fatalf("create invoice: %+v, %v", invoice, err)
	}
	issue := invoiceninja.IssueDraft{Action: "mark_sent", IDs: []string{invoice.ID}}
	issued, err := c.IssueInvoice(context.Background(), issue)
	if err != nil || issued.StatusID != "2" || issued.Amount.String() != preciseAmount || issued.Balance.String() != preciseAmount {
		t.Fatalf("issue invoice: %+v, %v", issued, err)
	}
	wantRequests := []string{"GET /panel/api/v1/products/billing-product", "GET /panel/api/v1/clients", "POST /panel/api/v1/clients", "GET /panel/api/v1/clients", "POST /panel/api/v1/invoices", "GET /panel/api/v1/clients", "POST /panel/api/v1/invoices/bulk"}
	if !reflect.DeepEqual(requests, wantRequests) || len(bodies) != 3 {
		t.Fatalf("each POST must have its own version probe: %v", requests)
	}
	for n, expected := range []any{billingDraft(), draft, issue} {
		if bodies[n] != billingJSON(t, expected) {
			t.Fatalf("POST contract differs: %s", bodies[n])
		}
		for _, forbidden := range []string{"contacts", "email", "send_email", "auto_bill", "paid", "amount", "amount_paid", "product_id", "idempotency"} {
			if strings.Contains(bodies[n], `"`+forbidden+`"`) {
				t.Fatalf("forbidden mutation field %s", forbidden)
			}
		}
	}
	if !strings.Contains(bodies[1], `"cost":`+precisePrice) || !strings.Contains(bodies[1], `"tax_rate2":0`) || !strings.Contains(bodies[1], `"tax_name2":""`) || !strings.Contains(bodies[1], `"uses_inclusive_taxes":false`) {
		t.Fatal("number precision, explicit false or empty/zero tax fields lost")
	}
}

func TestBillingInvalidDraftsNeverReachNetwork(t *testing.T) {
	var requests atomic.Int32
	s := billingServer(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1) })
	c := billingClientAt(t, s.URL, 1, 1)
	for _, change := range []func(*invoiceninja.CustomerDraft){
		func(d *invoiceninja.CustomerDraft) { d.Name = "" },
		func(d *invoiceninja.CustomerDraft) { d.Name = strings.Repeat("я", 257) },
		func(d *invoiceninja.CustomerDraft) { d.Name = "synthetic\x00private" },
		func(d *invoiceninja.CustomerDraft) { d.Name = "\xff" },
		func(d *invoiceninja.CustomerDraft) { d.PrivateNotes += "\n" },
		func(d *invoiceninja.CustomerDraft) { d.PrivateNotes = billingMarker + "invoice_create" },
		func(d *invoiceninja.CustomerDraft) { d.PrivateNotes = "payplump:short:customer_create" },
		func(d *invoiceninja.CustomerDraft) {
			d.PrivateNotes = "payplump:" + strings.Repeat("a", 129) + ":customer_create"
		},
	} {
		d := billingDraft()
		change(&d)
		if got, err := c.CreateCustomer(context.Background(), d); err == nil || got.ID != "" {
			t.Fatalf("accepted invalid customer draft: %+v", d)
		}
	}
	for _, id := range []string{"", "0", "01", "+1", "-1", "1.0", "1e2", " 1", "1\n", "9223372036854775808", strings.Repeat("1", 1000)} {
		d := billingDraft()
		d.IDNumber = id
		if _, err := c.CreateCustomer(context.Background(), d); err == nil {
			t.Fatalf("accepted noncanonical ID number %q", id)
		}
	}
	for _, change := range []func(*invoiceninja.InvoiceDraft){
		func(d *invoiceninja.InvoiceDraft) { d.ClientID = "../client" },
		func(d *invoiceninja.InvoiceDraft) { d.ClientID = strings.Repeat("a", 257) },
		func(d *invoiceninja.InvoiceDraft) { d.Date, d.DueDate = "2041-02-29", "2041-02-29" },
		func(d *invoiceninja.InvoiceDraft) { d.Date, d.DueDate = "2040-2-29", "2040-2-29" },
		func(d *invoiceninja.InvoiceDraft) { d.DueDate = "2040-03-01" },
		func(d *invoiceninja.InvoiceDraft) { d.PrivateNotes = billingMarker + "customer_create" },
		func(d *invoiceninja.InvoiceDraft) { d.LineItems = nil },
		func(d *invoiceninja.InvoiceDraft) { d.LineItems = append(d.LineItems, d.LineItems[0]) },
		func(d *invoiceninja.InvoiceDraft) { d.LineItems[0].Quantity = "2" },
		func(d *invoiceninja.InvoiceDraft) { d.LineItems[0].ProductKey = "" },
		func(d *invoiceninja.InvoiceDraft) { d.LineItems[0].Notes = strings.Repeat("a", 4097) },
		func(d *invoiceninja.InvoiceDraft) { d.LineItems[0].TaxName1 = strings.Repeat("a", 257) },
		func(d *invoiceninja.InvoiceDraft) { d.LineItems[0].TaxID = "bad\x00tax" },
		func(d *invoiceninja.InvoiceDraft) { d.LineItems[0].TaxRate1 = "-0.1" },
		func(d *invoiceninja.InvoiceDraft) { d.LineItems[0].TaxRate2 = "" },
		func(d *invoiceninja.InvoiceDraft) { d.LineItems[0].TaxRate3 = "1e129" },
	} {
		d := billingInvoiceDraft(t)
		change(&d)
		if got, err := c.CreateInvoice(context.Background(), d); err == nil || got.ID != "" {
			t.Fatalf("accepted invalid invoice draft: %+v", d)
		}
	}
	for _, number := range []string{"", "0", "-0.0001", "NaN", "Infinity", "+1", "01", "1.", "1e", "1e999999999999999999", "1e129", "1e-129", strings.Repeat("9", 129)} {
		d := billingInvoiceDraft(t)
		d.LineItems[0].Cost = json.Number(number)
		if _, err := c.CreateInvoice(context.Background(), d); err == nil {
			t.Fatalf("accepted invalid/unbounded cost %q", number)
		}
	}
	for _, draft := range []invoiceninja.IssueDraft{{Action: "send_email", IDs: []string{"one"}}, {Action: "mark_sent"}, {Action: "mark_sent", IDs: []string{"one", "two"}}, {Action: "mark_sent", IDs: []string{"../one"}}} {
		if _, err := c.IssueInvoice(context.Background(), draft); err == nil {
			t.Fatal("accepted invalid issue draft")
		}
	}
	for _, id := range []string{"", "../id", "bad/id", "bad\x00id", strings.Repeat("a", 257)} {
		if _, err := c.GetCustomer(context.Background(), id); err == nil {
			t.Fatal("accepted invalid customer read ID")
		}
		if _, err := c.GetInvoice(context.Background(), id); err == nil {
			t.Fatal("accepted invalid invoice read ID")
		}
		if _, err := c.FindInvoices(context.Background(), id); err == nil {
			t.Fatal("accepted invalid client filter")
		}
	}
	for _, id := range []int64{0, -1} {
		if _, err := c.FindCustomers(context.Background(), id); err == nil {
			t.Fatal("accepted invalid Telegram ID")
		}
	}
	if requests.Load() != 0 {
		t.Fatal("invalid draft reached network (even a version probe)")
	}
}

func TestBillingCreateCustomerResponseValidation(t *testing.T) {
	base := fixture(t, "billing_customer")
	for _, body := range []string{`{}`, `{"data":null}`, `{"data":[]}`, strings.Replace(base, `"billing-client"`, `"bad/id"`, 1), strings.Replace(base, `"9007199254740993"`, `"42"`, 1), strings.Replace(base, "customer_create", "invoice_create", 1), strings.Replace(base, `"is_deleted": false`, `"is_deleted": true`, 1)} {
		var posts atomic.Int32
		s := billingServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "GET" {
				_, _ = io.WriteString(w, `{}`)
				return
			}
			posts.Add(1)
			_, _ = io.WriteString(w, body)
		})
		if got, err := billingClientAt(t, s.URL, 1, 1).CreateCustomer(context.Background(), billingDraft()); err == nil || got.ID != "" || posts.Load() != 1 {
			t.Fatalf("accepted malformed customer response: %+v, %v", got, err)
		}
	}
}

func TestBillingCreateInvoiceResponseValidation(t *testing.T) {
	for name, change := range map[string]func(*invoiceninja.Invoice){
		"ID":                    func(i *invoiceninja.Invoice) { i.ID = "bad/id" },
		"client":                func(i *invoiceninja.Invoice) { i.ClientID = "other-client" },
		"date":                  func(i *invoiceninja.Invoice) { i.Date = "2040-03-01" },
		"due date":              func(i *invoiceninja.Invoice) { i.DueDate = "2040-03-01" },
		"marker":                func(i *invoiceninja.Invoice) { i.PrivateNotes = "other-marker" },
		"inclusive":             func(i *invoiceninja.Invoice) { i.UsesInclusiveTaxes = true },
		"status":                func(i *invoiceninja.Invoice) { i.StatusID = "2" },
		"deleted":               func(i *invoiceninja.Invoice) { i.IsDeleted = true },
		"recurring":             func(i *invoiceninja.Invoice) { i.RecurringID = "unexpected-recurring" },
		"line count":            func(i *invoiceninja.Invoice) { i.LineItems = nil },
		"cost precision":        func(i *invoiceninja.Invoice) { i.LineItems[0].Cost = "12345678901234567890.123456789012345679" },
		"quantity":              func(i *invoiceninja.Invoice) { i.LineItems[0].Quantity = "2" },
		"product":               func(i *invoiceninja.Invoice) { i.LineItems[0].ProductKey = "other-product" },
		"tax name":              func(i *invoiceninja.Invoice) { i.LineItems[0].TaxName2 = "other-tax" },
		"tax rate":              func(i *invoiceninja.Invoice) { i.LineItems[0].TaxRate1 = "19.600000000000000002" },
		"tax ID":                func(i *invoiceninja.Invoice) { i.LineItems[0].TaxID = "other-tax-code" },
		"zero amount":           func(i *invoiceninja.Invoice) { i.Amount, i.Balance = "0", "0" },
		"nonzero draft balance": func(i *invoiceninja.Invoice) { i.Balance = i.Amount },
		"unbounded amount":      func(i *invoiceninja.Invoice) { i.Amount = "1e9999" },
	} {
		t.Run(name, func(t *testing.T) {
			i := billingInvoice(t)
			change(&i)
			body := billingJSON(t, map[string]any{"data": i})
			billingRejectCreateInvoice(t, body)
		})
	}
	base := fixture(t, "billing_invoice_draft")
	for _, body := range []string{`{}`, `{"data":null}`, strings.Replace(base, `"uses_inclusive_taxes": false,`, ``, 1), strings.Replace(base, `"uses_inclusive_taxes": false`, `"uses_inclusive_taxes": null`, 1), strings.Replace(base, `"tax_name2": "",`, ``, 1), strings.Replace(base, `"tax_rate2": 0`, `"tax_rate2": null`, 1), strings.Replace(base, `"synthetic-tax-code"`, `123`, 1)} {
		billingRejectCreateInvoice(t, body)
	}
}

func billingRejectCreateInvoice(t *testing.T, body string) {
	t.Helper()
	var posts atomic.Int32
	s := billingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			_, _ = io.WriteString(w, `{}`)
			return
		}
		posts.Add(1)
		_, _ = io.WriteString(w, body)
	})
	got, err := billingClientAt(t, s.URL, 1, 1).CreateInvoice(context.Background(), billingInvoiceDraft(t))
	if err == nil || got.ID != "" || posts.Load() != 1 {
		t.Fatalf("accepted malformed invoice response: %+v, %v", got, err)
	}
}

func TestBillingInclusivePolicyAndNumericEquality(t *testing.T) {
	i := billingInvoice(t)
	i.UsesInclusiveTaxes = true
	i.LineItems[0].Cost = "12345678901234567890123456789012345678e-18"
	i.LineItems[0].Quantity = "1.000"
	i.LineItems[0].TaxRate2 = "0.000"
	body := billingJSON(t, map[string]any{"data": i})
	s := billingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			_, _ = io.WriteString(w, `{}`)
			return
		}
		var draft invoiceninja.InvoiceDraft
		if err := json.NewDecoder(r.Body).Decode(&draft); err != nil || !draft.UsesInclusiveTaxes {
			t.Error("inclusive operator policy not sent")
		}
		_, _ = io.WriteString(w, body)
	})
	draft := billingInvoiceDraft(t)
	draft.UsesInclusiveTaxes = true
	if got, err := billingClientAt(t, s.URL, 1, 1).CreateInvoice(context.Background(), draft); err != nil || !got.UsesInclusiveTaxes {
		t.Fatalf("equivalent numeric response or inclusive policy rejected: %+v, %v", got, err)
	}
}

func TestBillingIssueRequiresOneUnpaidSentInvoice(t *testing.T) {
	valid := billingInvoice(t)
	valid.StatusID = "2"
	bodies := []string{`{}`, `{"data":null}`, `{"data":[]}`, billingJSON(t, map[string]any{"data": valid}), billingJSON(t, map[string]any{"data": []invoiceninja.Invoice{valid, valid}})}
	for _, change := range []func(*invoiceninja.Invoice){
		func(i *invoiceninja.Invoice) { i.ID = "other-invoice" },
		func(i *invoiceninja.Invoice) { i.StatusID = "1" },
		func(i *invoiceninja.Invoice) { i.StatusID = "3" },
		func(i *invoiceninja.Invoice) { i.StatusID = "4" },
		func(i *invoiceninja.Invoice) { i.StatusID = "5" },
		func(i *invoiceninja.Invoice) { i.Amount, i.Balance = "0", "0" },
		func(i *invoiceninja.Invoice) { i.Amount, i.Balance = "-1", "-1" },
		func(i *invoiceninja.Invoice) { i.Balance = "9007199254740993.010000000000000002" },
		func(i *invoiceninja.Invoice) { i.Balance = "0" },
		func(i *invoiceninja.Invoice) { i.IsDeleted = true },
		func(i *invoiceninja.Invoice) { i.ArchivedAt = 1 },
	} {
		i := valid
		change(&i)
		bodies = append(bodies, billingJSON(t, map[string]any{"data": []invoiceninja.Invoice{i}}))
	}
	for n, body := range bodies {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			var posts atomic.Int32
			s := billingServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					_, _ = io.WriteString(w, `{}`)
					return
				}
				posts.Add(1)
				_, _ = io.WriteString(w, body)
			})
			got, err := billingClientAt(t, s.URL, 1, 1).IssueInvoice(context.Background(), invoiceninja.IssueDraft{Action: "mark_sent", IDs: []string{valid.ID}})
			if err == nil || got.ID != "" || posts.Load() != 1 {
				t.Fatalf("accepted ambiguous/non-sent response: %+v, %v", got, err)
			}
		})
	}
}

func TestBillingUnknownIssuePOSTCalledOnceAndErrorsRedacted(t *testing.T) {
	issuedBody := fixture(t, "billing_invoice_issued")
	for _, outcome := range []string{"500", "invalid JSON", "disconnect", "wrong version"} {
		t.Run(outcome, func(t *testing.T) {
			var posts, gets atomic.Int32
			s := billingServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					gets.Add(1)
					_, _ = io.WriteString(w, `{}`)
					return
				}
				posts.Add(1) // Treat this as an externally accepted write.
				switch outcome {
				case "500":
					w.WriteHeader(500)
				case "disconnect":
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
					return
				case "wrong version":
					w.Header().Set("X-APP-VERSION", "synthetic-private-version")
					_, _ = io.WriteString(w, issuedBody)
					return
				}
				_, _ = io.WriteString(w, `{"message":"synthetic-private-name `+syntheticToken+` `+billingMarker+`"`)
			})
			got, err := billingClientAt(t, s.URL+"/synthetic-private-path", 1, 1).IssueInvoice(context.Background(), invoiceninja.IssueDraft{Action: "mark_sent", IDs: []string{"synthetic-private-invoice"}})
			if err == nil || got.ID != "" || posts.Load() != 1 || gets.Load() != 1 {
				t.Fatalf("retried/adopted uncertain issue: %+v, %v, POST=%d GET=%d", got, err, posts.Load(), gets.Load())
			}
			if outcome == "wrong version" && !errors.Is(err, invoiceninja.ErrWriteVersion) {
				t.Fatalf("POST version failure lost: %v", err)
			}
			for _, secret := range []string{"synthetic-private", syntheticToken, billingMarker, s.URL} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error leaked private content: %v", err)
				}
			}
		})
	}
}

func TestBillingPostResponseVersionAndFreshProbe(t *testing.T) {
	customerBody := fixture(t, "billing_customer")
	for _, postVersion := range []string{"", "5.13.48", "5.13.47"} {
		var gets, posts atomic.Int32
		s := billingServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "GET" {
				if gets.Add(1) > 1 {
					w.Header().Set("X-APP-VERSION", "5.13.48")
				}
				_, _ = io.WriteString(w, `{}`)
				return
			}
			posts.Add(1)
			w.Header().Set("X-APP-VERSION", postVersion)
			_, _ = io.WriteString(w, customerBody)
		})
		c := billingClientAt(t, s.URL, 1, 1)
		_, err := c.CreateCustomer(context.Background(), billingDraft())
		if postVersion != "5.13.47" && !errors.Is(err, invoiceninja.ErrWriteVersion) {
			t.Fatalf("POST version not checked: %v", err)
		}
		if postVersion == "5.13.47" && err != nil {
			t.Fatal(err)
		}
		if _, err := c.CreateCustomer(context.Background(), billingDraft()); !errors.Is(err, invoiceninja.ErrWriteVersion) || posts.Load() != 1 || gets.Load() != 2 {
			t.Fatalf("cached version allowed second POST: %v", err)
		}
	}
}

func TestBillingExportedValidationHelpers(t *testing.T) {
	if invoiceninja.SupportedWriteVersion != "5.13.47" {
		t.Fatal("source-pinned version changed")
	}
	customerDraft := billingDraft()
	customer := invoiceninja.Customer{ID: "billing-client", Name: customerDraft.Name, IDNumber: customerDraft.IDNumber, PrivateNotes: customerDraft.PrivateNotes}
	if err := invoiceninja.ValidateCustomerDraft(customerDraft); err != nil {
		t.Fatal(err)
	}
	if err := invoiceninja.ValidateCustomerForDraft(customer, customerDraft); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*invoiceninja.Customer){
		func(c *invoiceninja.Customer) { c.ID = "bad/id" },
		func(c *invoiceninja.Customer) { c.IDNumber = "42" },
		func(c *invoiceninja.Customer) { c.PrivateNotes += "other" },
	} {
		got := customer
		change(&got)
		if invoiceninja.ValidateCustomerForDraft(got, customerDraft) == nil {
			t.Fatal("customer comparison accepted mismatching identity/payload")
		}
	}
	// Presentation may be sanitized, renamed, or expand reserved keywords.
	// Neither a display name nor line description is an ownership identifier.
	customer.Name = "Changed display name"
	if err := invoiceninja.ValidateCustomerForDraft(customer, customerDraft); err != nil {
		t.Fatal("display name was treated as identity")
	}
	customerDraft.IDNumber = "042"
	if invoiceninja.ValidateCustomerDraft(customerDraft) == nil || invoiceninja.ValidateCustomerForDraft(customer, customerDraft) == nil {
		t.Fatal("helper accepted invalid customer draft")
	}
	invoice := billingInvoice(t)
	draft := billingInvoiceDraft(t)
	if err := invoiceninja.ValidateInvoiceDraft(draft); err != nil {
		t.Fatal(err)
	}
	// Lifecycle checks deliberately belong to the caller, not payload matching.
	for _, status := range []string{"1", "2", "3", "4", "5"} {
		invoice.StatusID = status
		if err := invoiceninja.ValidateInvoiceForDraft(invoice, draft); err != nil {
			t.Fatalf("payload helper imposed lifecycle status %s: %v", status, err)
		}
	}
	for _, change := range []func(*invoiceninja.Invoice){
		func(i *invoiceninja.Invoice) { i.ClientID += "other" },
		func(i *invoiceninja.Invoice) { i.Date = "2040-03-01" },
		func(i *invoiceninja.Invoice) { i.DueDate = "2040-03-01" },
		func(i *invoiceninja.Invoice) { i.PrivateNotes += "other" },
		func(i *invoiceninja.Invoice) { i.UsesInclusiveTaxes = true },
		func(i *invoiceninja.Invoice) { i.LineItems[0].ProductKey += "other" },
		func(i *invoiceninja.Invoice) { i.LineItems[0].Cost = "1" },
		func(i *invoiceninja.Invoice) { i.LineItems[0].Quantity = "2" },
		func(i *invoiceninja.Invoice) { i.LineItems[0].TaxName1 += "other" },
		func(i *invoiceninja.Invoice) { i.LineItems[0].TaxName2 += "other" },
		func(i *invoiceninja.Invoice) { i.LineItems[0].TaxName3 += "other" },
		func(i *invoiceninja.Invoice) { i.LineItems[0].TaxRate1 = "1" },
		func(i *invoiceninja.Invoice) { i.LineItems[0].TaxRate2 = "1" },
		func(i *invoiceninja.Invoice) { i.LineItems[0].TaxRate3 = "1" },
		func(i *invoiceninja.Invoice) { i.LineItems[0].TaxID += "other" },
	} {
		got := billingInvoice(t)
		change(&got)
		if invoiceninja.ValidateInvoiceForDraft(got, draft) == nil {
			t.Fatal("invoice comparison accepted mismatching identity/payload")
		}
	}
	draft.LineItems[0].Cost = "0"
	if invoiceninja.ValidateInvoiceDraft(draft) == nil || invoiceninja.ValidateInvoiceForDraft(invoice, draft) == nil {
		t.Fatal("helper accepted invalid invoice draft")
	}
}

func TestBillingLookupHTTPFailureRemainsRetryableAndRedacted(t *testing.T) {
	s := billingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = io.WriteString(w, `{"message":"synthetic-private-value `+syntheticToken+`"}`)
	})
	got, err := billingClientAt(t, s.URL+"/synthetic-private-path", 1, 1).FindCustomers(context.Background(), 42)
	var remote *integrationhttp.HTTPError
	if got != nil || errors.Is(err, invoiceninja.ErrLookupIncomplete) || !errors.As(err, &remote) || remote.StatusCode != http.StatusInternalServerError || bytes.Contains([]byte(err.Error()), []byte("synthetic")) {
		t.Fatalf("HTTP failure was not a redacted retryable lookup error: %+v, %v", got, err)
	}
}
