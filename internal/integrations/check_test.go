package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

type fakeNinja struct {
	pages          map[int]invoiceninja.Page[invoiceninja.Customer]
	called         []int
	fail           string
	ver            string
	productVersion string
	product        invoiceninja.Product
}

func (f *fakeNinja) ListCustomers(_ context.Context, page, _ int) (invoiceninja.Page[invoiceninja.Customer], error) {
	f.called = append(f.called, page)
	return f.pages[page], nil
}
func (f *fakeNinja) GetProductWithVersion(context.Context, string) (invoiceninja.Product, string, error) {
	if f.fail == "product" {
		return invoiceninja.Product{}, "", errors.New("synthetic product unavailable")
	}
	return f.product, f.productVersion, nil
}
func (f *fakeNinja) ListInvoices(context.Context, int, int) (invoiceninja.Page[invoiceninja.Invoice], error) {
	return invoiceninja.Page[invoiceninja.Invoice]{Data: []invoiceninja.Invoice{}, AppVersion: f.ver}, nil
}
func (f *fakeNinja) ListPayments(context.Context, int, int) (invoiceninja.Page[invoiceninja.Payment], error) {
	return invoiceninja.Page[invoiceninja.Payment]{Data: []invoiceninja.Payment{}, AppVersion: f.ver}, nil
}
func (f *fakeNinja) ListRecurringInvoices(context.Context, int, int) (invoiceninja.Page[invoiceninja.RecurringInvoice], error) {
	return invoiceninja.Page[invoiceninja.RecurringInvoice]{Data: []invoiceninja.RecurringInvoice{}, AppVersion: f.ver}, nil
}

type fakeSUI struct {
	accounts []sui.Account
	inbounds []sui.Inbound
	settings sui.Settings
}

func (f *fakeSUI) ListAccounts(context.Context) ([]sui.Account, error) { return f.accounts, nil }
func (f *fakeSUI) ListInbounds(context.Context) ([]sui.Inbound, error) { return f.inbounds, nil }
func (f *fakeSUI) GetSettings(context.Context) (sui.Settings, error)   { return f.settings, nil }

func exampleCheck() (Config, *fakeNinja, *fakeSUI) {
	cfg := validConfig()
	cfg.PageSize = 2
	cfg.InvoiceNinja.ExpectedVersion = "5.13.47"
	cfg.SUI.DeclaredVersion = "v1.6.4"
	n := &fakeNinja{ver: "5.13.47", productVersion: "5.13.47", product: invoiceninja.Product{ID: "synthetic-product", Price: json.Number("100")}, pages: map[int]invoiceninja.Page[invoiceninja.Customer]{
		1: {Data: []invoiceninja.Customer{{ID: "synthetic-a", IDNumber: "9007199254740993", Name: "never-print-name"}, {ID: "synthetic-b", IDNumber: "42"}}, Pagination: invoiceninja.Pagination{Total: 3, Count: 2, PerPage: 2, CurrentPage: 1, TotalPages: 2}, AppVersion: "5.13.47"},
		2: {Data: []invoiceninja.Customer{{ID: "synthetic-archived", IDNumber: "43", ArchivedAt: 1}}, Pagination: invoiceninja.Pagination{Total: 3, Count: 1, PerPage: 2, CurrentPage: 2, TotalPages: 2}, AppVersion: "5.13.47"},
	}}
	s := &fakeSUI{
		accounts: []sui.Account{
			{ID: 101, Name: "9007199254740993", Description: "never-print-label", Enabled: true, Inbounds: []int64{2}},
			{ID: 102, Name: "42", Enabled: false, Inbounds: []int64{2}},
			{ID: 103, Name: "synthetic-unmanaged", Description: "never-print-unmanaged", Enabled: true, Inbounds: []int64{2}},
		},
		inbounds: []sui.Inbound{{ID: 2, Type: "vless", Tag: "synthetic-inbound"}},
		settings: sui.Settings{SubURI: cfg.SUI.SubscriptionBaseURL + "/"},
	}
	return cfg, n, s
}

func TestCheckCountsOnlyExactActiveMappingsAndLeavesUnmanagedAlone(t *testing.T) {
	cfg, ninja, panel := exampleCheck()
	report, err := check(context.Background(), cfg, ninja, panel)
	if err != nil {
		t.Fatal(err)
	}
	if !report.ReadOnly || report.Status != "ok" || !report.InvoiceNinja.InventoryComplete || report.InvoiceNinja.ClientsRead != 3 || report.InvoiceNinja.ActiveClients != 2 || report.InvoiceNinja.PagesRead != 2 {
		t.Fatalf("wrong inventory summary: %+v", report)
	}
	if report.Mapping.ExactMatches != 2 || report.Mapping.SUIOutsideExactMapping != 1 || report.Mapping.MatchedAccountsDisabled != 1 || report.SUI.VersionVerified || report.SUI.DeclaredVersion != "v1.6.4" {
		t.Fatal("scope, raw enable state or manual version declaration interpreted incorrectly")
	}
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"9007199254740993", "never-print", "synthetic-a", "synthetic-unmanaged", cfg.InvoiceNinja.Token, cfg.SUI.Token, cfg.InvoiceNinja.BaseURL, cfg.SUI.SubscriptionBaseURL} {
		if strings.Contains(string(body), private) {
			t.Fatal("report exposed an identifier, name, token or URL")
		}
	}
}

func TestMissingSUIIsInformationalNotProofOfUnpaidOrMissingConfig(t *testing.T) {
	cfg, ninja, panel := exampleCheck()
	panel.accounts = panel.accounts[1:]
	report, err := check(context.Background(), cfg, ninja, panel)
	if err != nil || report.Mapping.ExactMatches != 1 || report.Mapping.InvoiceClientsWithoutSUI != 1 || report.Status != "ok" {
		t.Fatalf("absence of an exact mapping was treated as a billing failure: %v", err)
	}
}

func TestCheckRejectsPaginationBoundsRepeatedRowsAndChanges(t *testing.T) {
	for _, kind := range []string{"limit", "repeated", "changing-total", "incomplete"} {
		t.Run(kind, func(t *testing.T) {
			cfg, n, s := exampleCheck()
			switch kind {
			case "limit":
				cfg.MaxPages = 1
			case "repeated":
				page := n.pages[2]
				page.Data[0].ID = "synthetic-a"
				n.pages[2] = page
			case "changing-total":
				page := n.pages[2]
				page.Pagination.Total = 4
				n.pages[2] = page
			case "incomplete":
				page := n.pages[1]
				page.Pagination.TotalPages = 1
				n.pages[1] = page
			}
			report, err := check(context.Background(), cfg, n, s)
			if err == nil || report.Status != "failed" || report.InvoiceNinja.InventoryComplete {
				t.Fatal("incomplete or inconsistent inventory reported as complete")
			}
			if kind == "limit" && len(n.called) != 1 {
				t.Fatal("pagination exceeded the configured bound")
			}
		})
	}
}

func TestCheckRejectsAmbiguousIdentifiersWithoutLabelMatching(t *testing.T) {
	for _, kind := range []string{"duplicate-ninja", "duplicate-sui-name", "duplicate-sui-id", "invalid-id", "wrong-label-only"} {
		t.Run(kind, func(t *testing.T) {
			cfg, n, s := exampleCheck()
			switch kind {
			case "duplicate-ninja":
				page := n.pages[1]
				page.Data[1].IDNumber = page.Data[0].IDNumber
				n.pages[1] = page
			case "duplicate-sui-name":
				s.accounts[1].Name = s.accounts[0].Name
			case "duplicate-sui-id":
				s.accounts[1].ID = s.accounts[0].ID
			case "invalid-id":
				page := n.pages[1]
				page.Data[0].IDNumber = "not-an-identifier"
				n.pages[1] = page
			case "wrong-label-only":
				s.accounts[0].Name = "different-existing-name"
				s.accounts[0].Description = "never-print-name"
			}
			report, err := check(context.Background(), cfg, n, s)
			if kind == "wrong-label-only" {
				if err != nil || report.Mapping.ExactMatches != 1 || report.Mapping.InvoiceClientsWithoutSUI != 1 {
					t.Fatal("display label was used as identity")
				}
			} else if err == nil || report.FailedCheck != "identity_mapping" {
				t.Fatal("ambiguous/invalid identifiers did not require manual review")
			}
		})
	}
}

func TestCheckVersionsAreInformational(t *testing.T) {
	for _, kind := range []string{"expected-mismatch", "missing-version", "version-change", "product-version", "product-missing-version", "sample-version", "all-missing", "no-declarations", "new-declarations"} {
		t.Run(kind, func(t *testing.T) {
			cfg, n, s := exampleCheck()
			wantVersion := n.ver
			switch kind {
			case "expected-mismatch":
				cfg.InvoiceNinja.ExpectedVersion = "5.13.46"
			case "missing-version":
				page := n.pages[1]
				page.AppVersion = ""
				n.pages[1] = page
			case "version-change":
				page := n.pages[2]
				page.AppVersion = "5.13.50"
				n.pages[2] = page
				n.productVersion, n.ver = "", ""
				wantVersion = "5.13.50"
			case "product-version":
				n.productVersion, n.ver = "v5.13.50", ""
				wantVersion = "5.13.50"
			case "product-missing-version":
				n.productVersion = ""
			case "sample-version":
				n.ver = "v5.13.50"
				wantVersion = "5.13.50"
			case "all-missing":
				for number, page := range n.pages {
					page.AppVersion = ""
					n.pages[number] = page
				}
				n.productVersion, n.ver, wantVersion = "", "", ""
			case "no-declarations":
				cfg.InvoiceNinja.ExpectedVersion, cfg.SUI.DeclaredVersion = "", ""
			case "new-declarations":
				cfg.InvoiceNinja.ExpectedVersion, cfg.SUI.DeclaredVersion = "5.13.50", "v1.7.0"
			}
			report, err := check(context.Background(), cfg, n, s)
			if err != nil || report.Status != "ok" || report.FailedCheck != "" || !report.InvoiceNinja.InventoryComplete || report.InvoiceNinja.AppVersion != wantVersion {
				t.Fatalf("informational version rejected or last observed version lost: %+v, %v", report, err)
			}
			if report.SUI.VersionVerified || report.SUI.DeclaredVersion != cfg.SUI.DeclaredVersion {
				t.Fatal("manual S-UI version declaration interpreted incorrectly")
			}
		})
	}
}

func TestCheckRejectsInvalidHeadersInboundsProductAndPublicOverride(t *testing.T) {
	for _, tc := range []struct{ kind, stage string }{
		{"inbound", "sui_inbounds"},
		{"product", "invoice_ninja_product"},
		{"archived-product", "invoice_ninja_product"},
		{"deleted-product", "invoice_ninja_product"},
		{"public-base", "sui_subscription_base"},
		{"private-version-header", "invoice_ninja_version"},
		{"private-product-version", "invoice_ninja_version"},
		{"private-sample-version", "invoice_ninja_version"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			cfg, n, s := exampleCheck()
			switch tc.kind {
			case "inbound":
				s.inbounds = nil
			case "product":
				n.fail = "product"
			case "archived-product":
				n.product.ArchivedAt = 1
			case "deleted-product":
				n.product.IsDeleted = true
			case "public-base":
				s.settings.SubURI = "https://different.example.invalid/never-print-url"
			case "private-version-header":
				page := n.pages[1]
				page.AppVersion = "never-print-header"
				n.pages[1] = page
			case "private-product-version":
				n.productVersion = "never-print-header"
			case "private-sample-version":
				n.ver = "never-print-header"
			}
			report, err := check(context.Background(), cfg, n, s)
			if err == nil || report.Status != "failed" || report.FailedCheck != tc.stage || strings.Contains(err.Error(), "never-print") {
				t.Fatalf("invalid integration data accepted or remote data leaked: %+v, %v", report, err)
			}
			body, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), "never-print") {
				t.Fatal("report exposed remote data")
			}
		})
	}
}

func TestCheckUsingRealAdaptersAndSyntheticFixtures(t *testing.T) {
	fixtures := make(map[string][]byte)
	for path, file := range map[string]string{
		"/billing/api/v1/clients":                      "../invoiceninja/testdata/customers.json",
		"/billing/api/v1/products/synthetic-product-1": "../invoiceninja/testdata/product.json",
		"/billing/api/v1/invoices":                     "../invoiceninja/testdata/invoices.json",
		"/billing/api/v1/payments":                     "../invoiceninja/testdata/payments.json",
		"/billing/api/v1/recurring_invoices":           "../invoiceninja/testdata/recurring_invoices.json",
		"/panel/apiv2/clients":                         "../sui/testdata/clients.json",
		"/panel/apiv2/inbounds":                        "../sui/testdata/inbounds.json",
		"/panel/apiv2/settings":                        "../sui/testdata/settings.json",
	} {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		fixtures[path] = body
	}
	fixtures["/panel/apiv2/clients"] = []byte(strings.Replace(string(fixtures["/panel/apiv2/clients"]), "synthetic-account-a", "9007199254740993", 1))
	var requests atomic.Int32
	appVersion, productVersion := "5.13.50", "5.13.50"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.ContentLength != 0 {
			t.Error("preflight attempted a mutation")
			w.WriteHeader(405)
			return
		}
		body, ok := fixtures[r.URL.Path]
		if !ok {
			t.Error("preflight called an unexpected endpoint")
			w.WriteHeader(404)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/billing/") {
			if r.Header.Get("X-API-TOKEN") != "synthetic-invoice-token" || r.Header.Get("Token") != "" {
				t.Error("wrong Invoice Ninja authentication")
			}
			version := appVersion
			if r.URL.Path == "/billing/api/v1/products/synthetic-product-1" {
				version = productVersion
			}
			if version != "" {
				w.Header().Set("X-APP-VERSION", version)
			}
		} else if r.Header.Get("Token") != "synthetic-panel-token" || r.Header.Get("X-API-TOKEN") != "" {
			t.Error("wrong S-UI authentication")
		}
		_, _ = w.Write(body)
	}))
	defer server.Close()
	cfg := validConfig()
	cfg.InvoiceNinja.BaseURL, cfg.SUI.BaseURL = server.URL+"/billing", server.URL+"/panel"
	cfg.InvoiceNinja.ProductID = "synthetic-product-1"
	cfg.InvoiceNinja.ExpectedVersion = "5.13.47"
	cfg.SUI.SubscriptionBaseURL = "https://subscriptions.invalid/synthetic-prefix/"
	cfg.SUI.InboundIDs = []int64{9007199254740993}
	report, err := Check(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 8 || report.Mapping.ExactMatches != 1 || report.Mapping.SUIOutsideExactMapping != 1 || report.InvoiceNinja.AppVersion != "5.13.50" || report.Status != "ok" {
		t.Fatalf("unexpected integration preflight summary: requests=%d, report=%+v", requests.Load(), report)
	}
	for _, version := range []string{"5.13.51", ""} {
		productVersion = version
		report, err = Check(context.Background(), cfg)
		if err != nil || report.Status != "ok" || report.InvoiceNinja.AppVersion != "5.13.50" {
			t.Fatalf("informational product endpoint version %q was rejected: %+v, %v", version, report, err)
		}
	}
	appVersion, productVersion = "", ""
	report, err = Check(context.Background(), cfg)
	if err != nil || report.Status != "ok" || report.InvoiceNinja.AppVersion != "" {
		t.Fatalf("missing informational version headers rejected: %+v, %v", report, err)
	}
}
