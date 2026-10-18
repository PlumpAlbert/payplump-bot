package invoiceninja_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
)

// These adapters are deliberately exercised through New, never BillingClient.
// Partial read DTOs must not require the write pin or complete financial fields.
func TestMenuReadOnlyGETContractsAndOpaqueIDs(t *testing.T) {
	const customerID = "synthetic?customer#100%+ opaque"
	const invoiceID = "synthetic?invoice#100%+ opaque"
	var requests atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil || len(body) != 0 || r.ContentLength != 0 || r.Method != http.MethodGet {
			t.Error("read adapter did not send a bodyless GET")
		}
		if r.URL.RawQuery != "" || r.URL.Fragment != "" {
			t.Error("opaque ID escaped into query/fragment or triggered a list lookup")
		}
		if r.Header.Get("X-API-TOKEN") != syntheticToken || r.Header.Get("X-Requested-With") != "XMLHttpRequest" || r.Header.Get("Accept") != "application/json" {
			t.Error("missing read-only authentication/JSON headers")
		}
		if r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-Idempotency-Key") != "" {
			t.Error("read method acquired a write-side idempotency contract")
		}
		w.Header().Set("X-APP-VERSION", "91.2.345-rc.7+synthetic.001")
		switch r.URL.EscapedPath() {
		case "/installed%20panel/api/v1/clients/" + url.PathEscape(customerID):
			_, _ = fmt.Fprintf(w, `{"data":{"id":%q,"id_number":"9007199254740993"}}`, customerID)
		case "/installed%20panel/api/v1/invoices/" + url.PathEscape(invoiceID):
			_, _ = fmt.Fprintf(w, `{"data":{"id":%q,"client_id":%q,"amount":%s,"balance":1.25e-18}}`, invoiceID, customerID, preciseAmount)
		default:
			t.Errorf("unexpected read path %s", r.URL.EscapedPath())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer s.Close()
	c := clientAt(t, s.URL+"/installed%20panel")
	customer, err := c.GetCustomer(context.Background(), customerID)
	if err != nil || customer.ID != customerID || customer.IDNumber != "9007199254740993" {
		t.Fatalf("customer GET failed or lost actor precision: %+v, %v", customer, err)
	}
	invoice, err := c.GetInvoice(context.Background(), invoiceID)
	if err != nil || invoice.ID != invoiceID || invoice.ClientID != customerID || invoice.Amount.String() != preciseAmount || invoice.Balance.String() != "1.25e-18" {
		t.Fatalf("invoice GET changed IDs or decimal lexemes: %+v, %v", invoice, err)
	}
	if requests.Load() != 2 {
		t.Fatalf("GETs made %d calls, want exactly two", requests.Load())
	}
}

func TestMenuReadOnlyMethodsAcceptArbitraryValidSemVer(t *testing.T) {
	for _, version := range []string{"", "0.0.0", "6.0.0", "9.123.456-beta.0+synthetic.001", "5.13.47"} {
		t.Run(version, func(t *testing.T) {
			var calls atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodGet {
					t.Error("read-only method attempted a write")
				}
				if version != "" {
					w.Header().Set("X-APP-VERSION", version)
				}
				if strings.Contains(r.URL.Path, "/clients/") {
					_, _ = io.WriteString(w, `{"data":{"id":"synthetic-customer"}}`)
				} else {
					_, _ = io.WriteString(w, `{"data":{"id":"synthetic-invoice","client_id":"synthetic-customer","amount":1,"balance":1}}`)
				}
			}))
			defer s.Close()
			c := clientAt(t, s.URL)
			if _, err := c.GetCustomer(context.Background(), "synthetic-customer"); err != nil {
				t.Fatalf("customer read requires a write version/complete billing fields: %v", err)
			}
			if _, err := c.GetInvoice(context.Background(), "synthetic-invoice"); err != nil {
				t.Fatalf("invoice read requires a write version/complete billing fields: %v", err)
			}
			if calls.Load() != 2 {
				t.Fatal("read methods probed billing or fetched inventory")
			}
		})
	}
	for _, version := range []string{"05.13.47", "5.13.47-01", "v5.13.47", "synthetic-private-header"} {
		t.Run("invalid-"+version, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-APP-VERSION", version)
				_, _ = io.WriteString(w, `{"data":{"id":"synthetic-object","client_id":"synthetic-customer","amount":1,"balance":1}}`)
			}))
			defer s.Close()
			c := clientAt(t, s.URL)
			customer, customerErr := c.GetCustomer(context.Background(), "synthetic-object")
			invoice, invoiceErr := c.GetInvoice(context.Background(), "synthetic-object")
			if customerErr == nil || invoiceErr == nil || customer.ID != "" || invoice.ID != "" {
				t.Fatal("malformed version header was accepted")
			}
			if strings.Contains(customerErr.Error(), version) || strings.Contains(invoiceErr.Error(), version) {
				t.Fatal("remote version header leaked through error")
			}
		})
	}
}

func TestMenuReadOnlyRejectsInvalidIDsBeforeHTTP(t *testing.T) {
	var requests atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer s.Close()
	c := clientAt(t, s.URL)
	for _, id := range []string{"", ".", "..", "a/b", `a\b`, " leading", "trailing ", "\u00a0leading", "x\n", "x\r", "x\t", "x\x00", "x\x7f", string([]byte{0xff})} {
		t.Run(fmt.Sprintf("id-%x", id), func(t *testing.T) {
			customer, err := c.GetCustomer(context.Background(), id)
			if err == nil || !reflect.DeepEqual(customer, invoiceninja.Customer{}) {
				t.Error("invalid customer ID was accepted or returned partial data")
			}
			invoice, err := c.GetInvoice(context.Background(), id)
			if err == nil || !reflect.DeepEqual(invoice, invoiceninja.Invoice{}) {
				t.Error("invalid invoice ID was accepted or returned partial data")
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid identifiers caused %d network calls", requests.Load())
	}
}

func TestMenuGetCustomerRejectsMalformedOrMismatchedResponse(t *testing.T) {
	for index, body := range []string{
		``, `{`, `null`, `[]`, `{}`, `{"data":null}`, `{"data":[]}`, `{"data":{}}`,
		`{"data":{"id":null}}`, `{"data":{"id":42}}`, `{"data":{"id":""}}`,
		`{"data":{"id":"different-customer","id_number":"9007199254740993"}}`,
		`{"data":{"id":"synthetic-customer","id_number":9007199254740993}}`,
		`{"data":{"id":"synthetic-customer","is_deleted":"false"}}`,
		`{"data":{"id":"synthetic-customer","archived_at":"synthetic-private-field"}}`,
		`{"data":{"id":"synthetic-customer"}} {}`,
	} {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			customer, err := staticClient(t, body).GetCustomer(context.Background(), "synthetic-customer")
			if err == nil || !reflect.DeepEqual(customer, invoiceninja.Customer{}) {
				t.Fatalf("malformed customer returned partial data: %+v, %v", customer, err)
			}
			if strings.Contains(err.Error(), "synthetic-private-field") {
				t.Fatal("decoding error exposed remote PII")
			}
		})
	}
}

func TestMenuGetInvoiceRejectsMalformedOrMismatchedResponse(t *testing.T) {
	bodies := []string{
		``, `{`, `null`, `[]`, `{}`, `{"data":null}`, `{"data":[]}`, `{"data":{}}`,
		`{"data":{"id":"different-invoice","client_id":"synthetic-customer","amount":1,"balance":1}}`,
		`{"data":{"client_id":"synthetic-customer","amount":1,"balance":1}}`,
		`{"data":{"id":null,"client_id":"synthetic-customer","amount":1,"balance":1}}`,
		`{"data":{"id":"synthetic-invoice","amount":1,"balance":1}}`,
		`{"data":{"id":"synthetic-invoice","client_id":null,"amount":1,"balance":1}}`,
		`{"data":{"id":"synthetic-invoice","client_id":"","amount":1,"balance":1}}`,
		`{"data":{"id":"synthetic-invoice","client_id":"../foreign","amount":1,"balance":1}}`,
		`{"data":{"id":"synthetic-invoice","client_id":"synthetic-customer","balance":1}}`,
		`{"data":{"id":"synthetic-invoice","client_id":"synthetic-customer","amount":1}}`,
		`{"data":{"id":"synthetic-invoice","client_id":"synthetic-customer","amount":1,"balance":1}} {}`,
	}
	for _, field := range []string{"amount", "balance"} {
		for _, value := range []string{`null`, `true`, `{}`, `[]`, `""`, `"NaN"`, `"Infinity"`, `"01"`, `"+1"`, `" 1 "`, `"1."`, `"1e"`, `"synthetic-private-money"`} {
			amount, balance := "1", "1"
			if field == "amount" {
				amount = value
			} else {
				balance = value
			}
			bodies = append(bodies, fmt.Sprintf(`{"data":{"id":"synthetic-invoice","client_id":"synthetic-customer","amount":%s,"balance":%s}}`, amount, balance))
		}
	}
	for index, body := range bodies {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			invoice, err := staticClient(t, body).GetInvoice(context.Background(), "synthetic-invoice")
			if err == nil || !reflect.DeepEqual(invoice, invoiceninja.Invoice{}) {
				t.Fatalf("malformed invoice returned partial data: %+v, %v", invoice, err)
			}
			if strings.Contains(err.Error(), "synthetic-private-money") {
				t.Fatal("decoding error exposed remote financial data")
			}
		})
	}
}

func TestMenuGetInvoicePreservesMoneyLexemesAndLegacyLineBooleans(t *testing.T) {
	for _, literal := range []string{preciseAmount, `"9007199254740993.000000000000000001"`, `1.25e+10`, `"1e9999"`, `0`} {
		for _, mode := range []string{"0", "1", "false", "true"} {
			t.Run(literal+"/mode="+mode, func(t *testing.T) {
				body := fmt.Sprintf(`{"data":{"id":"synthetic-invoice","client_id":"synthetic-customer","amount":%s,"balance":%s,"line_items":[{"cost":1,"quantity":1,"discount":0,"is_amount_discount":%s}]}}`, literal, literal, mode)
				invoice, err := staticClient(t, body).GetInvoice(context.Background(), "synthetic-invoice")
				if err != nil || invoice.Amount.String() != strings.Trim(literal, `"`) || invoice.Balance.String() != strings.Trim(literal, `"`) {
					t.Fatalf("read rejected or rounded valid decimal: %+v, %v", invoice, err)
				}
				if len(invoice.LineItems) != 1 || invoice.LineItems[0].IsAmountDiscount != (mode == "1" || mode == "true") {
					t.Fatal("read DTO broke legacy 0/1 line discount mode compatibility")
				}
			})
		}
	}
}

func TestMenuReadOnlyErrorsAreSanitizedAndNeverReturnPartialData(t *testing.T) {
	for _, resource := range []string{"customer", "invoice"} {
		for _, status := range []int{401, 403, 404, 429, 500} {
			t.Run(fmt.Sprintf("%s/%d", resource, status), func(t *testing.T) {
				var calls atomic.Int32
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.WriteHeader(status)
					_, _ = fmt.Fprintf(w, `{"message":"synthetic-private-name %s %s"}`, syntheticToken, r.URL)
				}))
				defer s.Close()
				c := clientAt(t, s.URL+"/synthetic-private-path")
				var err error
				if resource == "customer" {
					var got invoiceninja.Customer
					got, err = c.GetCustomer(context.Background(), "synthetic-private-id")
					if !reflect.DeepEqual(got, invoiceninja.Customer{}) {
						t.Error("HTTP error returned customer data")
					}
				} else {
					var got invoiceninja.Invoice
					got, err = c.GetInvoice(context.Background(), "synthetic-private-id")
					if !reflect.DeepEqual(got, invoiceninja.Invoice{}) {
						t.Error("HTTP error returned invoice data")
					}
				}
				var apiErr *integrationhttp.HTTPError
				if !errors.As(err, &apiErr) || apiErr.StatusCode != status {
					t.Fatalf("expected sanitized HTTP status %d, got %v", status, err)
				}
				for _, secret := range []string{syntheticToken, "synthetic-private-name", "synthetic-private-id", "synthetic-private-path", s.URL} {
					if strings.Contains(err.Error(), secret) {
						t.Errorf("API error leaked %q", secret)
					}
				}
				if calls.Load() != 1 {
					t.Error("read adapter retried or performed an extra lookup")
				}
			})
		}
	}
}

func TestMenuReadOnlyCancellationAndDeadline(t *testing.T) {
	for _, resource := range []string{"customer", "invoice"} {
		t.Run(resource, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				cancel() // Cancel an in-flight local GET without sleeping.
				<-r.Context().Done()
			}))
			defer s.Close()
			c := clientAt(t, s.URL+"/synthetic-private-path")
			var err error
			if resource == "customer" {
				var got invoiceninja.Customer
				got, err = c.GetCustomer(ctx, "synthetic-private-id")
				if !reflect.DeepEqual(got, invoiceninja.Customer{}) {
					t.Error("cancelled GET returned customer data")
				}
			} else {
				var got invoiceninja.Invoice
				got, err = c.GetInvoice(ctx, "synthetic-private-id")
				if !reflect.DeepEqual(got, invoiceninja.Invoice{}) {
					t.Error("cancelled GET returned invoice data")
				}
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("GET ignored context cancellation: %v", err)
			}
			if strings.Contains(err.Error(), "synthetic-private") || strings.Contains(err.Error(), s.URL) {
				t.Fatal("cancellation leaked a token-bearing URL or private ID")
			}
		})
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer s.Close()
	c := clientAt(t, s.URL)
	if _, err := c.GetCustomer(ctx, "synthetic-customer"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("customer GET ignored elapsed deadline: %v", err)
	}
	if _, err := c.GetInvoice(ctx, "synthetic-invoice"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("invoice GET ignored elapsed deadline: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("elapsed contexts reached HTTP")
	}
}

func TestMenuReadOnlyClientDoesNotAcquireWriteCapabilities(t *testing.T) {
	typ := reflect.TypeOf((*invoiceninja.Client)(nil))
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		if strings.HasPrefix(name, "Create") || strings.HasPrefix(name, "Issue") || name == "Post" {
			t.Errorf("read-only Client exposes write capability %s", name)
		}
	}
	// Compile-time verification of the small read capability used by the menu.
	var _ interface {
		GetCustomer(context.Context, string) (invoiceninja.Customer, error)
		GetInvoice(context.Context, string) (invoiceninja.Invoice, error)
	} = (*invoiceninja.Client)(nil)
}
