package invoiceninja_test

import (
	"context"
	"encoding/json"
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

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
)

// All payloads in this file are synthetic, not live-data captures. These tests
// exercise the source-pinned read contract, not real writes or settlement.
const (
	settlementToken          = "synthetic-settlement-token"
	settlementExact          = "9007199254740993.010000000000000001"
	settlementCustomerJSON   = `{"id":"synthetic-customer","id_number":"9007199254740993","group_settings_id":"","settings":{"currency_id":"1"},"currency_id":"do-not-use-root","is_deleted":false,"archived_at":0}`
	settlementInvoiceJSON    = `{"id":"synthetic-invoice","client_id":"synthetic-customer","recurring_id":"","number":"SYN-001","status_id":"4","date":"2040-01-02","due_date":"2040-01-03","amount":9007199254740993.010000000000000001,"balance":0,"paid_to_date":"9007199254740993.010000000000000001","exchange_rate":1.000000000000000001,"is_deleted":false,"archived_at":0}`
	settlementAllocationJSON = `{"id":"synthetic-allocation","invoice_id":"synthetic-invoice","payment_id":"synthetic-payment","amount":9007199254740993.010000000000000001,"refunded":0,"archived_at":0}`
	settlementPaymentJSON    = `{"id":"synthetic-payment","client_id":"synthetic-customer","date":"2040-01-02","status_id":"4","currency_id":"1","exchange_currency_id":"","amount":9007199254740993.010000000000000001,"refunded":0,"applied":"9007199254740993.010000000000000001","exchange_rate":1.000000000000000001,"is_manual":false,"is_deleted":false,"archived_at":0,"paymentables":[` + settlementAllocationJSON + `],"invoices":[{"amount":"not-an-allocation"}]}`
)

func settlementJSONString(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// An empty replacement removes a key; all other replacements are raw JSON.
func settlementField(t *testing.T, body, key, replacement string) string {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatal(err)
	}
	if replacement == "" {
		delete(fields, key)
	} else {
		fields[key] = json.RawMessage(replacement)
	}
	return settlementJSONString(t, fields)
}

func settlementPaymentData(t *testing.T, id, customer, status string) string {
	t.Helper()
	body := settlementField(t, settlementPaymentJSON, "id", settlementJSONString(t, id))
	body = settlementField(t, body, "client_id", settlementJSONString(t, customer))
	body = settlementField(t, body, "status_id", settlementJSONString(t, status))
	allocation := settlementField(t, settlementAllocationJSON, "id", settlementJSONString(t, "allocation-"+id))
	allocation = settlementField(t, allocation, "payment_id", settlementJSONString(t, id))
	return settlementField(t, body, "paymentables", "["+allocation+"]")
}

func settlementPage(data []string, total, perPage, page, pages int) string {
	return fmt.Sprintf(`{"data":[%s],"meta":{"pagination":{"total":%d,"count":%d,"per_page":%d,"current_page":%d,"total_pages":%d}}}`, strings.Join(data, ","), total, len(data), perPage, page, pages)
}

func settlementCheckRequest(t *testing.T, r *http.Request) {
	t.Helper()
	if r.Method != http.MethodGet || r.ContentLength != 0 {
		t.Errorf("settlement must send GET without a body: %s, %d", r.Method, r.ContentLength)
	}
	if r.Header.Get("X-API-TOKEN") != settlementToken || r.Header.Get("X-Requested-With") != "XMLHttpRequest" || r.Header.Get("Accept") != "application/json" {
		t.Error("missing required authenticated JSON request headers")
	}
	if r.Header.Get("Content-Type") != "" {
		t.Error("GET unexpectedly carries a content type")
	}
}

func settlementClientAt(t *testing.T, base string, perPage, maxPages int) *invoiceninja.SettlementClient {
	t.Helper()
	client, err := invoiceninja.NewSettlementClient(base, settlementToken, time.Second, perPage, maxPages)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func settlementStatic(t *testing.T, body string, perPage, maxPages int) *invoiceninja.SettlementClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		settlementCheckRequest(t, r)
		w.Header().Set("X-APP-VERSION", invoiceninja.SupportedSettlementVersion)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return settlementClientAt(t, server.URL, perPage, maxPages)
}

func settlementRedacted(t *testing.T, err error, secrets ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a rejected response")
	}
	for _, secret := range append(secrets, settlementToken, "synthetic-private-body", "synthetic-private-version") {
		if secret != "" && strings.Contains(err.Error(), secret) {
			t.Fatalf("error exposes private input: %v", err)
		}
	}
}

func TestSettlementConstructorAndReadOnlyCapability(t *testing.T) {
	for _, bounds := range [][2]int{{1, 1}, {100, 100}, {1, 100}, {100, 1}} {
		client, err := invoiceninja.NewSettlementClient("http://example.invalid/panel", settlementToken, time.Second, bounds[0], bounds[1])
		if err != nil || client == nil {
			t.Fatalf("valid bounds rejected: %v", err)
		}
		typ := reflect.TypeOf(client)
		if typ.NumMethod() != 3 {
			t.Fatalf("unexpected settlement capabilities: %v", typ)
		}
		for i := 0; i < typ.NumMethod(); i++ {
			switch typ.Method(i).Name {
			case "GetSettlementCustomer", "GetSettlementInvoice", "ListSettlementPayments":
			default:
				t.Fatalf("unexpected exported capability: %s", typ.Method(i).Name)
			}
		}
	}
	for _, bounds := range [][2]int{{0, 1}, {-1, 1}, {101, 1}, {1, 0}, {1, -1}, {1, 101}} {
		if c, err := invoiceninja.NewSettlementClient("http://example.invalid", settlementToken, time.Second, bounds[0], bounds[1]); err == nil || c != nil {
			t.Fatalf("invalid bounds accepted: %v", bounds)
		}
	}
	for _, timeout := range []time.Duration{0, -time.Second} {
		if c, err := invoiceninja.NewSettlementClient("http://example.invalid", settlementToken, timeout, 1, 1); err == nil || c != nil {
			t.Fatal("invalid timeout accepted")
		}
	}
	for _, base := range []string{"", "http://user:secret@example.invalid", "http://example.invalid/api/v1", "http://example.invalid?secret=private", "http://example.invalid/#private"} {
		_, err := invoiceninja.NewSettlementClient(base, settlementToken, time.Second, 1, 1)
		settlementRedacted(t, err, base, "secret", "private")
	}
	for _, token := range []string{"", " private-token", "private-token\n", "private\x00token"} {
		_, err := invoiceninja.NewSettlementClient("http://example.invalid", token, time.Second, 1, 1)
		settlementRedacted(t, err, token)
	}
}

func TestSettlementIndividualGETContractsAndEscaping(t *testing.T) {
	id := "opaque %2F?#&+=id"
	for _, resource := range []string{"clients", "invoices"} {
		t.Run(resource, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				settlementCheckRequest(t, r)
				if r.URL.EscapedPath() != "/panel%20prefix/api/v1/"+resource+"/"+url.PathEscape(id) || r.URL.RawQuery != "" {
					t.Errorf("unexpected escaped GET target: %s", r.URL.RequestURI())
				}
				body := settlementCustomerJSON
				if resource == "invoices" {
					body = settlementInvoiceJSON
				}
				body = settlementField(t, body, "id", settlementJSONString(t, id))
				body = settlementField(t, body, "is_deleted", "true")
				body = settlementField(t, body, "archived_at", "2200000000")
				w.Header().Set("X-APP-VERSION", "5.13.47")
				_, _ = w.Write([]byte(`{"data":` + body + `,"meta":{}}`))
			}))
			defer server.Close()
			client := settlementClientAt(t, server.URL+"/panel%20prefix/", 1, 1)
			if resource == "clients" {
				customer, err := client.GetSettlementCustomer(context.Background(), id)
				if err != nil || customer.ID != id || customer.IDNumber != "9007199254740993" || customer.CurrencyID != "1" || customer.GroupID != "" || !customer.IsDeleted || customer.ArchivedAt != 2200000000 {
					t.Fatalf("customer contract: %+v, %v", customer, err)
				}
			} else {
				invoice, err := client.GetSettlementInvoice(context.Background(), id)
				if err != nil || invoice.ID != id || invoice.ClientID != "synthetic-customer" || invoice.RecurringID != "" || invoice.Number != "SYN-001" || invoice.StatusID != "4" || invoice.Date != "2040-01-02" || invoice.DueDate != "2040-01-03" || invoice.Amount.String() != settlementExact || invoice.Balance.String() != "0" || invoice.PaidToDate.String() != settlementExact || invoice.ExchangeRate.String() != "1.000000000000000001" || !invoice.IsDeleted || invoice.ArchivedAt != 2200000000 {
					t.Fatalf("invoice contract: %+v, %v", invoice, err)
				}
			}
			if calls.Load() != 1 {
				t.Fatal("individual GET made unexpected extra requests")
			}
		})
	}
}

func TestSettlementCustomerCurrencyIsOnlyExplicitOverride(t *testing.T) {
	for _, settings := range []string{"{}", "[]", "[ ]", `{"currency_id":null}`, `{"currency_id":""}`, `{"unrelated":"value"}`} {
		t.Run(settings, func(t *testing.T) {
			body := settlementField(t, settlementCustomerJSON, "settings", settings)
			body = settlementField(t, body, "group_settings_id", `"synthetic-group"`)
			client := settlementStatic(t, `{"data":`+body+`}`, 1, 1)
			customer, err := client.GetSettlementCustomer(context.Background(), "synthetic-customer")
			if err != nil || customer.CurrencyID != "" || customer.GroupID != "synthetic-group" {
				t.Fatalf("unexpected inferred currency/group: %+v, %v", customer, err)
			}
		})
	}
	for _, settings := range []string{"", "null", `{"currency_id":1}`, `{"currency_id":true}`, `{"currency_id":" currency "}`, `{"currency_id":"bad/id"}`, `""`, `["1"]`} {
		client := settlementStatic(t, `{"data":`+settlementField(t, settlementCustomerJSON, "settings", settings)+`}`, 1, 1)
		_, err := client.GetSettlementCustomer(context.Background(), "synthetic-customer")
		settlementRedacted(t, err)
	}
}

func TestSettlementPendingRecordCanHaveNoCurrencyYet(t *testing.T) {
	body := settlementField(t, settlementPaymentJSON, "status_id", `"1"`)
	body = settlementField(t, body, "currency_id", `""`)
	items, err := settlementStatic(t, settlementPage([]string{body}, 1, 1, 1, 1), 1, 1).ListSettlementPayments(context.Background(), "synthetic-customer")
	if err != nil || len(items) != 1 || items[0].CurrencyID != "" {
		t.Fatal("pending factory record was lost from complete inventory")
	}
}

func TestSettlementPaymentsCompleteInventoryRetainsEveryStatus(t *testing.T) {
	customerID := "customer %2F?#&+=id"
	var foreignCalls, calls atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignCalls.Add(1)
		t.Error("followed an untrusted pagination URL")
	}))
	defer foreign.Close()
	var data []string
	for status := 1; status <= 6; status++ {
		body := settlementPaymentData(t, fmt.Sprintf("payment-%d", status), customerID, strconv.Itoa(status))
		body = settlementField(t, body, "is_manual", "true")
		body = settlementField(t, body, "refunded", "0.125")
		body = settlementField(t, body, "exchange_currency_id", `"2"`)
		if status == 5 {
			body = settlementField(t, body, "archived_at", "2200000000")
		}
		if status == 6 {
			body = settlementField(t, body, "is_deleted", "true")
		}
		data = append(data, body)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := int(calls.Add(1))
		settlementCheckRequest(t, r)
		want := url.Values{"client_id": {customerID}, "include": {"invoices"}, "status": {"active,archived,deleted"}, "sort": {"id|asc"}, "page": {strconv.Itoa(call)}, "per_page": {"2"}}
		if r.URL.Path != "/prefix/api/v1/payments" || !reflect.DeepEqual(r.URL.Query(), want) {
			t.Errorf("payment query must use only client_id, not invoice_id: %s", r.URL.RequestURI())
		}
		if call > 3 {
			t.Error("unbounded pagination")
			http.Error(w, "unexpected page", 500)
			return
		}
		body := settlementPage(data[(call-1)*2:call*2], 6, 2, call, 3)
		body = strings.Replace(body, `"pagination":{`, `"pagination":{"links":{"next":`+settlementJSONString(t, foreign.URL+"/private")+`},`, 1)
		body = settlementField(t, body, "links", `{"next":`+settlementJSONString(t, foreign.URL)+`}`)
		w.Header().Set("X-APP-VERSION", "5.13.47")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	payments, err := settlementClientAt(t, server.URL+"/prefix", 2, 3).ListSettlementPayments(context.Background(), customerID)
	if err != nil || len(payments) != 6 || calls.Load() != 3 || foreignCalls.Load() != 0 {
		t.Fatalf("incomplete inventory: %d payments, %v", len(payments), err)
	}
	for i, payment := range payments {
		if payment.StatusID != strconv.Itoa(i+1) || payment.ClientID != customerID || payment.Date != "2040-01-02" || payment.CurrencyID != "1" || payment.ExchangeCurrencyID != "2" || payment.Amount.String() != settlementExact || payment.Applied.String() != settlementExact || payment.Refunded.String() != "0.125" || payment.ExchangeRate.String() != "1.000000000000000001" || !payment.IsManual || len(payment.Allocations) != 1 {
			t.Fatalf("lost payment evidence: %+v", payment)
		}
		allocation := payment.Allocations[0]
		if allocation.ID != "allocation-"+payment.ID || allocation.InvoiceID != "synthetic-invoice" || allocation.CreditID != "" || allocation.PaymentID != payment.ID || allocation.Amount.String() != settlementExact || allocation.Refunded.String() != "0" || allocation.ArchivedAt != 0 {
			t.Fatalf("lost allocation evidence: %+v", allocation)
		}
	}
	if payments[4].ArchivedAt != 2200000000 || !payments[5].IsDeleted {
		t.Fatal("archived/deleted payments were discarded")
	}
}

func TestSettlementRequiredFieldsRejectMissingAndNull(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		fields []string
		wrap   func(string) string
		read   func(*invoiceninja.SettlementClient) error
	}{
		{"customer", settlementCustomerJSON, []string{"id", "id_number", "group_settings_id", "is_deleted", "archived_at"}, func(body string) string { return `{"data":` + body + `}` }, func(c *invoiceninja.SettlementClient) error {
			_, err := c.GetSettlementCustomer(context.Background(), "synthetic-customer")
			return err
		}},
		{"invoice", settlementInvoiceJSON, []string{"id", "client_id", "recurring_id", "number", "status_id", "date", "due_date", "amount", "balance", "paid_to_date", "exchange_rate", "is_deleted", "archived_at"}, func(body string) string { return `{"data":` + body + `}` }, func(c *invoiceninja.SettlementClient) error {
			_, err := c.GetSettlementInvoice(context.Background(), "synthetic-invoice")
			return err
		}},
		{"payment", settlementPaymentJSON, []string{"id", "client_id", "date", "status_id", "currency_id", "exchange_currency_id", "amount", "refunded", "applied", "exchange_rate", "is_manual", "is_deleted", "archived_at", "paymentables"}, func(body string) string { return settlementPage([]string{body}, 1, 1, 1, 1) }, func(c *invoiceninja.SettlementClient) error {
			_, err := c.ListSettlementPayments(context.Background(), "synthetic-customer")
			return err
		}},
		{"allocation", settlementAllocationJSON, []string{"id", "payment_id", "amount", "refunded", "archived_at"}, func(body string) string {
			payment := settlementField(t, settlementPaymentJSON, "paymentables", "["+body+"]")
			return settlementPage([]string{payment}, 1, 1, 1, 1)
		}, func(c *invoiceninja.SettlementClient) error {
			_, err := c.ListSettlementPayments(context.Background(), "synthetic-customer")
			return err
		}},
	}
	for _, test := range tests {
		for _, field := range test.fields {
			for _, replacement := range []string{"", "null"} {
				t.Run(test.name+"/"+field+"/"+replacement, func(t *testing.T) {
					body := test.wrap(settlementField(t, test.body, field, replacement))
					err := test.read(settlementStatic(t, body, 1, 1))
					settlementRedacted(t, err)
					if (test.name == "payment" || test.name == "allocation") && !errors.Is(err, invoiceninja.ErrSettlementIncomplete) {
						t.Fatalf("malformed inventory must be incomplete: %v", err)
					}
				})
			}
		}
	}
}

func TestSettlementAllocationTargetsAndIdentity(t *testing.T) {
	creditOnly := settlementField(t, settlementAllocationJSON, "invoice_id", "")
	creditOnly = settlementField(t, creditOnly, "credit_id", `"synthetic-credit"`)
	creditOnly = settlementField(t, creditOnly, "refunded", "0.25")
	creditOnly = settlementField(t, creditOnly, "archived_at", "2200000000")
	invoiceAndEmptyCredit := settlementField(t, settlementAllocationJSON, "credit_id", `""`)
	creditAndEmptyInvoice := settlementField(t, creditOnly, "invoice_id", `""`)
	for _, allocation := range []string{settlementAllocationJSON, creditOnly, invoiceAndEmptyCredit, creditAndEmptyInvoice} {
		body := settlementField(t, settlementPaymentJSON, "paymentables", "["+allocation+"]")
		payments, err := settlementStatic(t, settlementPage([]string{body}, 1, 1, 1, 1), 1, 1).ListSettlementPayments(context.Background(), "synthetic-customer")
		if err != nil || len(payments) != 1 || len(payments[0].Allocations) != 1 {
			t.Fatalf("valid one-target allocation rejected: %v", err)
		}
		if allocation == creditOnly && (payments[0].Allocations[0].CreditID != "synthetic-credit" || payments[0].Allocations[0].Refunded.String() != "0.25" || payments[0].Allocations[0].ArchivedAt != 2200000000) {
			t.Fatal("credit evidence lost")
		}
	}
	for name, allocation := range map[string]string{
		"neither":       settlementField(t, settlementAllocationJSON, "invoice_id", ""),
		"empty":         settlementField(t, settlementAllocationJSON, "invoice_id", `""`),
		"both":          settlementField(t, settlementAllocationJSON, "credit_id", `"synthetic-credit"`),
		"null-invoice":  settlementField(t, settlementAllocationJSON, "invoice_id", "null"),
		"null-credit":   settlementField(t, settlementAllocationJSON, "credit_id", "null"),
		"wrong-payment": settlementField(t, settlementAllocationJSON, "payment_id", `"other-payment"`),
		"bad-credit":    settlementField(t, creditOnly, "credit_id", `"bad/id"`),
		"negative-time": settlementField(t, settlementAllocationJSON, "archived_at", "-1"),
	} {
		t.Run(name, func(t *testing.T) {
			body := settlementField(t, settlementPaymentJSON, "paymentables", "["+allocation+"]")
			payments, err := settlementStatic(t, settlementPage([]string{body}, 1, 1, 1, 1), 1, 1).ListSettlementPayments(context.Background(), "synthetic-customer")
			if payments != nil || !errors.Is(err, invoiceninja.ErrSettlementIncomplete) {
				t.Fatalf("unsafe allocation accepted: %+v, %v", payments, err)
			}
		})
	}
	body := settlementField(t, settlementPaymentJSON, "paymentables", "[]")
	payments, err := settlementStatic(t, settlementPage([]string{body}, 1, 1, 1, 1), 1, 1).ListSettlementPayments(context.Background(), "synthetic-customer")
	if err != nil || len(payments) != 1 || payments[0].Allocations == nil || len(payments[0].Allocations) != 0 {
		t.Fatalf("explicit empty allocations rejected: %+v, %v", payments, err)
	}
}

func TestSettlementNumberLexicalBounds(t *testing.T) {
	for _, value := range []string{"0", "-0", "-1.25", "1e128", "1e-128", "1E+128", strings.Repeat("9", 128), "1e" + strings.Repeat("0", 100) + "128"} {
		body := settlementField(t, settlementInvoiceJSON, "amount", value)
		invoice, err := settlementStatic(t, `{"data":`+body+`}`, 1, 1).GetSettlementInvoice(context.Background(), "synthetic-invoice")
		if err != nil || invoice.Amount.String() != value {
			t.Fatalf("bounded number changed/rejected: %q, %v", value, err)
		}
	}
	for _, field := range []string{"amount", "balance", "paid_to_date", "exchange_rate"} {
		for _, value := range []string{`""`, `"NaN"`, `"+1"`, `"01"`, `"1."`, `" 1"`, `"1e129"`, `"1e-129"`, `"1e999999999999999999999999999"`, settlementJSONString(t, strings.Repeat("9", 129)), "true", "{}"} {
			body := settlementField(t, settlementInvoiceJSON, field, value)
			_, err := settlementStatic(t, `{"data":`+body+`}`, 1, 1).GetSettlementInvoice(context.Background(), "synthetic-invoice")
			settlementRedacted(t, err)
		}
	}
	for _, field := range []string{"amount", "refunded", "applied", "exchange_rate"} {
		body := settlementField(t, settlementPaymentJSON, field, `"1e129"`)
		_, err := settlementStatic(t, settlementPage([]string{body}, 1, 1, 1, 1), 1, 1).ListSettlementPayments(context.Background(), "synthetic-customer")
		if !errors.Is(err, invoiceninja.ErrSettlementIncomplete) {
			t.Fatalf("unbounded payment %s accepted: %v", field, err)
		}
	}
	for _, field := range []string{"amount", "refunded"} {
		allocation := settlementField(t, settlementAllocationJSON, field, `"1e-129"`)
		body := settlementField(t, settlementPaymentJSON, "paymentables", "["+allocation+"]")
		_, err := settlementStatic(t, settlementPage([]string{body}, 1, 1, 1, 1), 1, 1).ListSettlementPayments(context.Background(), "synthetic-customer")
		if !errors.Is(err, invoiceninja.ErrSettlementIncomplete) {
			t.Fatalf("unbounded allocation %s accepted: %v", field, err)
		}
	}
}

func TestSettlementStrictTypesIDsAndRootIdentity(t *testing.T) {
	for _, id := range []string{"", ".", "..", "slash/id", `back\slash`, " id", "id ", "control\x00id", strings.Repeat("x", 257)} {
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
		client := settlementClientAt(t, server.URL, 1, 1)
		_, customerErr := client.GetSettlementCustomer(context.Background(), id)
		_, invoiceErr := client.GetSettlementInvoice(context.Background(), id)
		_, paymentsErr := client.ListSettlementPayments(context.Background(), id)
		server.Close()
		if customerErr == nil || invoiceErr == nil || paymentsErr == nil || calls.Load() != 0 {
			t.Fatalf("unsafe identifier reached transport: %q", id)
		}
	}
	for _, resource := range []string{"customer", "invoice"} {
		base := settlementCustomerJSON
		if resource == "invoice" {
			base = settlementInvoiceJSON
		}
		changes := map[string][]string{
			"id":          {`"another-root-id"`, `""`, `"bad/id"`, settlementJSONString(t, strings.Repeat("x", 257))},
			"is_deleted":  {"0", `"false"`},
			"archived_at": {"-1", "0.5", `"0"`, "9223372036854775808"},
		}
		if resource == "customer" {
			changes["id_number"] = []string{"123"}
			changes["group_settings_id"] = []string{`"bad/id"`, "1"}
		} else {
			for _, field := range []string{"client_id", "status_id"} {
				changes[field] = []string{`""`, "1", `" bad "`}
			}
			changes["recurring_id"] = []string{`"bad/id"`}
			for _, field := range []string{"date", "due_date", "number"} {
				changes[field] = []string{"1", "false"}
			}
		}
		for field, values := range changes {
			for _, value := range values {
				client := settlementStatic(t, `{"data":`+settlementField(t, base, field, value)+`}`, 1, 1)
				var err error
				if resource == "customer" {
					_, err = client.GetSettlementCustomer(context.Background(), "synthetic-customer")
				} else {
					_, err = client.GetSettlementInvoice(context.Background(), "synthetic-invoice")
				}
				settlementRedacted(t, err)
			}
		}
	}
	for field, values := range map[string][]string{
		"id": {`"bad/id"`, `""`}, "client_id": {`"other-customer"`, `"bad/id"`},
		"status_id": {`""`, "4"}, "currency_id": {"1"}, "exchange_currency_id": {"1", `"bad/id"`},
		"date": {"1"}, "is_manual": {"0", `"false"`}, "is_deleted": {"0"},
		"archived_at": {"-1", "0.25", `"0"`}, "paymentables": {"{}", "[null]"},
	} {
		for _, value := range values {
			body := settlementField(t, settlementPaymentJSON, field, value)
			_, err := settlementStatic(t, settlementPage([]string{body}, 1, 1, 1, 1), 1, 1).ListSettlementPayments(context.Background(), "synthetic-customer")
			if !errors.Is(err, invoiceninja.ErrSettlementIncomplete) {
				t.Fatalf("invalid payment %s accepted: %v", field, err)
			}
		}
	}
}

func TestSettlementInventoryRejectsTruncationDriftAndDuplicates(t *testing.T) {
	p1 := settlementPaymentData(t, "p1", "synthetic-customer", "4")
	p2 := settlementPaymentData(t, "p2", "synthetic-customer", "4")
	p3 := settlementPaymentData(t, "p3", "synthetic-customer", "4")
	p4 := settlementPaymentData(t, "p4", "synthetic-customer", "4")
	duplicatePivot := settlementField(t, settlementPaymentJSON, "paymentables", "["+settlementAllocationJSON+","+settlementAllocationJSON+"]")
	otherPaymentSamePivot := settlementField(t, settlementPaymentJSON, "id", `"other-payment"`)
	otherAllocation := settlementField(t, settlementAllocationJSON, "payment_id", `"other-payment"`)
	otherPaymentSamePivot = settlementField(t, otherPaymentSamePivot, "paymentables", "["+otherAllocation+"]")
	for _, test := range []struct {
		name     string
		perPage  int
		maxPages int
		pages    []string
	}{
		{"max-pages", 1, 1, []string{settlementPage([]string{p1}, 2, 1, 1, 2)}},
		{"short-page", 2, 2, []string{settlementPage([]string{p1}, 2, 2, 1, 1)}},
		{"wrong-per-page", 2, 2, []string{settlementPage([]string{p1}, 1, 1, 1, 1)}},
		{"wrong-current-page", 1, 2, []string{settlementPage([]string{p1}, 2, 1, 2, 2)}},
		{"wrong-total-pages", 1, 2, []string{settlementPage([]string{p1}, 2, 1, 1, 1)}},
		{"negative-total", 1, 1, []string{settlementPage([]string{p1}, -1, 1, 1, 1)}},
		{"duplicate-same-page", 2, 1, []string{settlementPage([]string{p1, p1}, 2, 2, 1, 1)}},
		{"duplicate-across-pages", 1, 2, []string{settlementPage([]string{p1}, 2, 1, 1, 2), settlementPage([]string{p1}, 2, 1, 2, 2)}},
		{"total-drift", 2, 2, []string{settlementPage([]string{p1, p2}, 3, 2, 1, 2), settlementPage([]string{p3, p4}, 4, 2, 2, 2)}},
		{"per-page-drift", 1, 2, []string{settlementPage([]string{p1}, 2, 1, 1, 2), settlementPage([]string{}, 2, 2, 2, 1)}},
		{"second-page-empty", 1, 2, []string{settlementPage([]string{p1}, 2, 1, 1, 2), settlementPage([]string{}, 2, 1, 2, 2)}},
		{"duplicate-pivot", 1, 1, []string{settlementPage([]string{duplicatePivot}, 1, 1, 1, 1)}},
		{"duplicate-pivot-across-payments", 1, 2, []string{settlementPage([]string{settlementPaymentJSON}, 2, 1, 1, 2), settlementPage([]string{otherPaymentSamePivot}, 2, 1, 2, 2)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				settlementCheckRequest(t, r)
				page := int(calls.Add(1))
				if page > len(test.pages) {
					t.Error("reader exceeded declared local pages")
					http.Error(w, "unexpected page", 500)
					return
				}
				w.Header().Set("X-APP-VERSION", "5.13.47")
				_, _ = w.Write([]byte(test.pages[page-1]))
			}))
			defer server.Close()
			payments, err := settlementClientAt(t, server.URL, test.perPage, test.maxPages).ListSettlementPayments(context.Background(), "synthetic-customer")
			if payments != nil || !errors.Is(err, invoiceninja.ErrSettlementIncomplete) || calls.Load() > int32(test.maxPages) {
				t.Fatalf("unsafe inventory accepted/returned partial results: %+v, %v", payments, err)
			}
		})
	}
}

func TestSettlementEnvelopesAndPaginationPresence(t *testing.T) {
	for _, body := range []string{`{}`, `{"data":null}`, `{"data":[]}`, `{"data":{}}`, `{"data":[null]}`, `{"data":[],"meta":null}`, `{"data":[],"meta":{}}`, `{"data":[],"meta":{"pagination":null}}`} {
		_, err := settlementStatic(t, body, 1, 1).ListSettlementPayments(context.Background(), "synthetic-customer")
		if !errors.Is(err, invoiceninja.ErrSettlementIncomplete) {
			t.Fatalf("missing inventory metadata accepted: %s, %v", body, err)
		}
	}
	pagination := `{"total":0,"count":0,"per_page":1,"current_page":1,"total_pages":1}`
	for _, field := range []string{"total", "count", "per_page", "current_page", "total_pages"} {
		for _, replacement := range []string{"", "null", `"1"`, "1.5"} {
			body := `{"data":[],"meta":{"pagination":` + settlementField(t, pagination, field, replacement) + `}}`
			_, err := settlementStatic(t, body, 1, 1).ListSettlementPayments(context.Background(), "synthetic-customer")
			if !errors.Is(err, invoiceninja.ErrSettlementIncomplete) {
				t.Fatalf("invalid pagination %s accepted: %v", field, err)
			}
		}
	}
	for _, pages := range []int{0, 1} {
		payments, err := settlementStatic(t, settlementPage(nil, 0, 1, 1, pages), 1, 1).ListSettlementPayments(context.Background(), "synthetic-customer")
		if err != nil || payments == nil || len(payments) != 0 {
			t.Fatalf("explicit complete empty inventory rejected: %+v, %v", payments, err)
		}
	}
	for _, body := range []string{`null`, `{}`, `{"data":null}`, `{"data":[]}`} {
		client := settlementStatic(t, body, 1, 1)
		_, customerErr := client.GetSettlementCustomer(context.Background(), "synthetic-customer")
		_, invoiceErr := client.GetSettlementInvoice(context.Background(), "synthetic-invoice")
		if customerErr == nil || invoiceErr == nil {
			t.Fatalf("invalid individual envelope accepted: %s", body)
		}
	}
}

func TestSettlementAcceptsReleaseHeadersOnEveryReadAndPage(t *testing.T) {
	for _, versions := range [][]string{nil, {"5.13.47"}, {"5.13.50"}, {""}, {"5.13.46"}, {"5.13.48"}, {"v5.13.47"}, {"5.13.47+build"}, {"synthetic-private-version"}, {"5.13.47", "5.13.47"}, {"5.13.47,5.13.47"}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			settlementCheckRequest(t, r)
			for _, version := range versions {
				w.Header().Add("X-APP-VERSION", version)
			}
			body := `{"data":` + settlementCustomerJSON + `}`
			if strings.Contains(r.URL.Path, "/invoices/") {
				body = `{"data":` + settlementInvoiceJSON + `}`
			} else if strings.HasSuffix(r.URL.Path, "/payments") {
				body = settlementPage([]string{settlementPaymentJSON}, 1, 1, 1, 1)
			}
			_, _ = w.Write([]byte(body))
		}))
		client := settlementClientAt(t, server.URL, 1, 1)
		_, customerErr := client.GetSettlementCustomer(context.Background(), "synthetic-customer")
		_, invoiceErr := client.GetSettlementInvoice(context.Background(), "synthetic-invoice")
		_, paymentsErr := client.ListSettlementPayments(context.Background(), "synthetic-customer")
		server.Close()
		for _, err := range []error{customerErr, invoiceErr, paymentsErr} {
			if err != nil {
				t.Fatalf("release header gated valid response: %v, %v", versions, err)
			}
		}
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := int(calls.Add(1))
		w.Header().Set("X-APP-VERSION", "5.13.47")
		if page == 2 {
			w.Header().Set("X-APP-VERSION", "5.13.48")
		}
		payment := settlementPaymentData(t, "p"+strconv.Itoa(page), "synthetic-customer", "4")
		_, _ = w.Write([]byte(settlementPage([]string{payment}, 2, 1, page, 2)))
	}))
	defer server.Close()
	payments, err := settlementClientAt(t, server.URL, 1, 2).ListSettlementPayments(context.Background(), "synthetic-customer")
	if len(payments) != 2 || calls.Load() != 2 || err != nil {
		t.Fatalf("release change gated later page: %+v, %v", payments, err)
	}
}

func TestSettlementRedirectsNeverFollowedAndErrorsAreRedacted(t *testing.T) {
	var foreignCalls atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignCalls.Add(1)
		t.Error("followed an untrusted redirect")
	}))
	defer foreign.Close()
	for _, status := range []int{301, 302, 303, 307, 308, 401, 403, 429, 500} {
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			settlementCheckRequest(t, r)
			w.Header().Set("X-APP-VERSION", "5.13.47")
			w.Header().Set("Location", foreign.URL+"/synthetic-private-body")
			w.WriteHeader(status)
			_, _ = w.Write([]byte("synthetic-private-body " + settlementToken))
		}))
		client := settlementClientAt(t, server.URL+"/private-prefix", 1, 1)
		_, customerErr := client.GetSettlementCustomer(context.Background(), "private-customer-id")
		_, invoiceErr := client.GetSettlementInvoice(context.Background(), "private-invoice-id")
		payments, paymentsErr := client.ListSettlementPayments(context.Background(), "private-customer-id")
		server.Close()
		for _, err := range []error{customerErr, invoiceErr, paymentsErr} {
			settlementRedacted(t, err, server.URL, foreign.URL, "private-prefix", "private-customer-id", "private-invoice-id")
		}
		if calls.Load() != 3 || foreignCalls.Load() != 0 || payments != nil || !errors.Is(paymentsErr, invoiceninja.ErrSettlementIncomplete) {
			t.Fatalf("redirect/retry/partial-result violation: calls=%d, %v", calls.Load(), paymentsErr)
		}
	}
	for _, body := range []string{`{"data":"synthetic-private-body"}`, `{"data":` + settlementField(t, settlementCustomerJSON, "archived_at", `"synthetic-private-body"`) + `}`, `{"data":"` + settlementToken + `",`, `{} {"secret":"synthetic-private-body"}`} {
		client := settlementStatic(t, body, 1, 1)
		_, err := client.GetSettlementCustomer(context.Background(), "synthetic-customer")
		settlementRedacted(t, err)
	}
}
