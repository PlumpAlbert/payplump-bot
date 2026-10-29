package importverify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

type discoveryCustomers struct {
	CustomerReader
	rows         []invoiceninja.Customer
	err          error
	recurrings   []invoiceninja.RecurringRecord
	recurringErr error
}

func (d discoveryCustomers) ListCustomers(context.Context) ([]invoiceninja.Customer, error) {
	return d.rows, d.err
}

func (d discoveryCustomers) ListRecurring(context.Context, string) ([]invoiceninja.RecurringRecord, error) {
	return d.recurrings, d.recurringErr
}

type discoveryIdentityReader struct {
	IdentityReader
	rows []sui.Identity
	err  error
}

func (d discoveryIdentityReader) ListIdentities(context.Context, int) ([]sui.Identity, error) {
	return d.rows, d.err
}

type discoveryOtherProviders struct {
	RecurringReader
	InvoiceReader
	PaymentReader
	RecordReader
	SettingsReader
}

func discoveryTestPolicy() Policy {
	return Policy{Automatic: true, BotID: 9001, SourceScopeHash: strings.Repeat("a", 64), NinjaVersion: "5.13.47", SUIVersion: "1.6.4", NativeCurrencyID: "1", NativeCurrencyCode: "USD", ProductKey: "legacy", PublicSubscriptionBase: "https://subscriptions.invalid/", ExpectedInboundIDs: []int64{1}, MaxRecords: 20, MaxInventoryRecords: 2, MaxPages: 2, MaxHistoricalAnchors: 2, EvidenceTTL: time.Minute}
}

func TestDiscoverRequiresCompleteCustomerInventory(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []invoiceninja.Customer
		err  error
	}{
		{name: "api failure", err: errors.New("private provider response")},
		{name: "partial rows with error", rows: []invoiceninja.Customer{{ID: "one", IDNumber: "42"}}, err: errors.New("private provider response")},
		{name: "truncated at bound", rows: []invoiceninja.Customer{{ID: "one"}, {ID: "two"}, {ID: "three"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			providers := Providers{Customers: discoveryCustomers{rows: tc.rows, err: tc.err}, Identities: discoveryIdentityReader{}, Recurring: discoveryOtherProviders{}, Invoices: discoveryOtherProviders{}, Payments: discoveryOtherProviders{}, Records: discoveryOtherProviders{}, Settings: discoveryOtherProviders{}}
			svc, err := New(discoveryTestPolicy(), providers)
			if err != nil {
				t.Fatal(err)
			}
			got, err := svc.Discover(context.Background(), time.Now(), nil)
			if err == nil || got != nil {
				t.Fatalf("partial inventory must fail closed: discoveries=%v err=%v", got, err)
			}
			if strings.Contains(err.Error(), "private provider response") {
				t.Fatalf("provider response leaked: %v", err)
			}
		})
	}
}

func TestDiscoverRequiresAutomaticPolicyAndInventoryCapability(t *testing.T) {
	p := discoveryTestPolicy()
	p.Automatic = false
	providers := Providers{Customers: discoveryCustomers{}, Identities: discoveryIdentityReader{}, Recurring: discoveryOtherProviders{}, Invoices: discoveryOtherProviders{}, Payments: discoveryOtherProviders{}, Records: discoveryOtherProviders{}, Settings: discoveryOtherProviders{}}
	svc, err := New(p, providers)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Discover(context.Background(), time.Now(), nil); err == nil {
		t.Fatal("manual verification policy unexpectedly enabled discovery")
	}
	p.Automatic = true
	providers.Customers = customerWithoutInventory{}
	svc, err = New(p, providers)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Discover(context.Background(), time.Now(), nil); err == nil {
		t.Fatal("legacy customer reader unexpectedly qualified as complete inventory")
	}
}

type customerWithoutInventory struct{ CustomerReader }

func (customerWithoutInventory) FindCustomers(context.Context, int64) ([]invoiceninja.Customer, error) {
	return nil, nil
}
func (customerWithoutInventory) ListRecurring(context.Context, string) ([]invoiceninja.RecurringRecord, error) {
	return nil, nil
}

// Identities have intentionally private fields; obtain synthetic hints through
// the real read-only decoder, using only a loopback test server.
func discoveryIdentities(t *testing.T, clients string) []sui.Identity {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/apiv2/clients" {
			t.Error("unexpected identity request")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"success":true,"obj":{"clients":%s}}`, clients)
	}))
	t.Cleanup(server.Close)
	reader, err := sui.NewIdentityReader(server.URL, "synthetic-token", "1.6.4", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := reader.ListIdentities(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func discoveryService(t *testing.T, customers discoveryCustomers, identities discoveryIdentityReader, other discoveryOtherProviders) *Service {
	t.Helper()
	svc, err := New(discoveryTestPolicy(), Providers{Customers: customers, Identities: identities, Recurring: other, Invoices: other, Payments: other, Records: other, Settings: other})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestDiscoverPairDiagnosticsAndHash(t *testing.T) {
	one := discoveryIdentities(t, `[{"id":1,"name":"42","enable":true}]`)
	// The production decoder rejects duplicate names globally. Independent
	// synthetic hints exercise discovery's own defensive match-count check.
	two := append(append([]sui.Identity{}, one...), discoveryIdentities(t, `[{"id":2,"name":"42","enable":true}]`)...)
	customer := invoiceninja.Customer{ID: "private-customer", IDNumber: "42"}
	inactive := customer
	inactive.IsDeleted = true
	duplicate := customer
	duplicate.ID = "other-private-customer"
	var missingHash string
	for _, tc := range []struct {
		name, check string
		customers   []invoiceninja.Customer
		identities  []sui.Identity
		count       int
	}{
		{"missing", "sui_match_missing", []invoiceninja.Customer{customer}, nil, 0},
		{"multiple SUI", "sui_match_multiple", []invoiceninja.Customer{customer}, two, 2},
		{"inactive", "customer_inactive", []invoiceninja.Customer{inactive}, one, 0},
		{"duplicate Ninja", "duplicate_ninja_actor", []invoiceninja.Customer{customer, duplicate}, one, 2},
		{"missing Ninja", "ninja_match_missing", nil, one, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := discoveryService(t, discoveryCustomers{rows: tc.customers}, discoveryIdentityReader{rows: tc.identities}, discoveryOtherProviders{})
			rows, err := svc.Discover(context.Background(), time.Now(), nil)
			if err != nil || len(rows) == 0 {
				t.Fatalf("discovery: rows=%v err=%v", rows, err)
			}
			for _, row := range rows {
				want := Diagnostic{Stage: "pairing", Check: tc.check, Count: tc.count}
				if row.Reason != ReasonAmbiguous || row.Diagnostic != want {
					t.Fatalf("reason=%s diagnostic=%+v want=%+v", row.Reason, row.Diagnostic, want)
				}
			}
			if tc.name == "missing" {
				missingHash = rows[0].FactsHash
			}
			// Same Ninja facts and empty plan, changed SUI diagnosis must rearm.
			if tc.name == "multiple SUI" && rows[0].FactsHash == missingHash {
				t.Fatal("changed diagnosis did not change facts hash")
			}
		})
	}
}

func TestDiscoverPartialSUIInventoryIsGlobalOnly(t *testing.T) {
	ids := discoveryIdentities(t, `[{"id":1,"name":"42","enable":true}]`)
	svc := discoveryService(t, discoveryCustomers{rows: []invoiceninja.Customer{{ID: "private", IDNumber: "42"}}}, discoveryIdentityReader{rows: ids, err: fmt.Errorf("private-token body: %w", &integrationhttp.HTTPError{StatusCode: 403})}, discoveryOtherProviders{})
	rows, err := svc.Discover(context.Background(), time.Now(), nil)
	var d Diagnostic
	if rows != nil || !errors.As(err, &d) || d.Stage != "sui_inventory" || d.Category != "http" || d.HTTPStatus != 403 {
		t.Fatalf("partial inventory not globally diagnosed: rows=%v err=%v", rows, err)
	}
	if strings.Contains(err.Error(), "private") || strings.Contains(fmt.Sprintf("%+v", err), "token") {
		t.Fatal("raw remote error retained")
	}
}

type discoveryRecordSources struct {
	InvoiceReader
	RecordReader
	PaymentReader
	invoices                          []invoiceninja.CycleInvoice
	invoiceErr, recordErr, paymentErr error
}

func (d discoveryRecordSources) ListInvoices(context.Context, string) ([]invoiceninja.CycleInvoice, error) {
	return d.invoices, d.invoiceErr
}
func (d discoveryRecordSources) GetRecord(context.Context, int64) (sui.ClientRecord, error) {
	return sui.ClientRecord{}, d.recordErr
}
func (d discoveryRecordSources) GetSettlementCustomer(context.Context, string) (invoiceninja.SettlementCustomer, error) {
	return invoiceninja.SettlementCustomer{}, d.paymentErr
}

func TestDiscoverRecordDiagnostics(t *testing.T) {
	id := discoveryIdentities(t, `[{"id":1,"name":"42","enable":true}]`)[0]
	customer := invoiceninja.Customer{ID: "private-customer", IDNumber: "42"}
	rec := invoiceninja.RecurringRecord{ID: "private-recurring", ClientID: customer.ID, StatusID: "2"}
	invoice := invoiceninja.CycleInvoice{ID: "private-invoice", ClientID: customer.ID, RecurringID: rec.ID, StatusID: "4", Date: "2026-03-01", Amount: "10"}
	otherRec, foreignRec := rec, rec
	otherRec.ID, foreignRec.ClientID = "another-recurring", "foreign"
	otherInvoice, foreignInvoice := invoice, invoice
	otherInvoice.ID, foreignInvoice.ClientID = "another-invoice", "foreign"
	secretError := fmt.Errorf("https://private.invalid?token=private-token private-body: %w", &integrationhttp.HTTPError{StatusCode: 403})
	for _, tc := range []struct {
		name         string
		recurrings   []invoiceninja.RecurringRecord
		recurringErr error
		sources      discoveryRecordSources
		reason       string
		want         Diagnostic
	}{
		{"zero recurring", nil, nil, discoveryRecordSources{}, ReasonAmbiguous, Diagnostic{Stage: "recurring", Check: "live_recurring_count"}},
		{"multiple recurring", []invoiceninja.RecurringRecord{rec, otherRec}, nil, discoveryRecordSources{}, ReasonAmbiguous, Diagnostic{Stage: "recurring", Check: "live_recurring_count", Count: 2}},
		{"duplicate recurring", []invoiceninja.RecurringRecord{rec, rec}, nil, discoveryRecordSources{}, ReasonAmbiguous, Diagnostic{Stage: "recurring", Check: "duplicate_id", Count: 2}},
		{"foreign recurring", []invoiceninja.RecurringRecord{foreignRec}, nil, discoveryRecordSources{}, ReasonAmbiguous, Diagnostic{Stage: "recurring", Check: "foreign_customer"}},
		{"no invoice", []invoiceninja.RecurringRecord{rec}, nil, discoveryRecordSources{}, ReasonAmbiguous, Diagnostic{Stage: "invoices", Check: "current_invoice_missing"}},
		{"invoice tie", []invoiceninja.RecurringRecord{rec}, nil, discoveryRecordSources{invoices: []invoiceninja.CycleInvoice{invoice, otherInvoice}}, ReasonAmbiguous, Diagnostic{Stage: "invoices", Check: "latest_invoice_tie", Count: 2}},
		{"duplicate invoice", []invoiceninja.RecurringRecord{rec}, nil, discoveryRecordSources{invoices: []invoiceninja.CycleInvoice{invoice, invoice}}, ReasonAmbiguous, Diagnostic{Stage: "invoices", Check: "duplicate_id", Count: 2}},
		{"foreign invoice", []invoiceninja.RecurringRecord{rec}, nil, discoveryRecordSources{invoices: []invoiceninja.CycleInvoice{foreignInvoice}}, ReasonAmbiguous, Diagnostic{Stage: "invoices", Check: "foreign_customer"}},
		{"recurring API", nil, secretError, discoveryRecordSources{}, ReasonSourceFailure, Diagnostic{Stage: "recurring", Check: "read", Category: "http", HTTPStatus: 403}},
		{"record API", []invoiceninja.RecurringRecord{rec}, nil, discoveryRecordSources{recordErr: secretError}, ReasonSourceFailure, Diagnostic{Stage: "sui_record", Check: "read", Category: "http", HTTPStatus: 403}},
		{"invoice API", []invoiceninja.RecurringRecord{rec}, nil, discoveryRecordSources{invoiceErr: secretError}, ReasonSourceFailure, Diagnostic{Stage: "invoices", Check: "read", Category: "http", HTTPStatus: 403}},
		{"payment API", []invoiceninja.RecurringRecord{rec}, nil, discoveryRecordSources{invoices: []invoiceninja.CycleInvoice{invoice}, paymentErr: secretError}, ReasonSourceFailure, Diagnostic{Stage: "payments", Check: "observe", Category: "http", HTTPStatus: 403}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := discoveryService(t, discoveryCustomers{recurrings: tc.recurrings, recurringErr: tc.recurringErr}, discoveryIdentityReader{}, discoveryOtherProviders{InvoiceReader: tc.sources, RecordReader: tc.sources, PaymentReader: tc.sources})
			_, reason, d := svc.discoverRecord(context.Background(), customer, id, time.Now())
			if reason != tc.reason || d != tc.want {
				t.Fatalf("reason=%s diagnostic=%+v want reason=%s diagnostic=%+v", reason, d, tc.reason, tc.want)
			}
			if strings.Contains(d.Error(), "private") {
				t.Fatal("remote error leaked")
			}
		})
	}
}

func TestDiscoveryFailureCategories(t *testing.T) {
	for _, tc := range []struct {
		err      error
		category string
	}{
		{context.Canceled, "canceled"},
		{context.DeadlineExceeded, "deadline"},
		{integrationhttp.ErrInvalidResponse, "invalid_response"},
		{integrationhttp.ErrConnection, "connection"},
		{sui.ErrManagedResponse, "invalid_response"},
		{invoiceninja.ErrSettlementContract, "invalid_response"},
		{invoiceninja.ErrLookupIncomplete, "incomplete_inventory"},
		{invoiceninja.ErrCycleLookupIncomplete, "incomplete_inventory"},
		{invoiceninja.ErrSettlementIncomplete, "incomplete_inventory"},
		{io.EOF, "unavailable"},
	} {
		d := FailureDiagnostic("customer_inventory", "read", fmt.Errorf("private-token: %w", tc.err))
		if d.Category != tc.category || strings.Contains(d.Error(), "private") {
			t.Fatalf("bad category or leak: %v", d)
		}
	}
}
