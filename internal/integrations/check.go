package integrations

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

// Reports deliberately contain only counts, booleans and validated versions.
// Never add names, identifiers, URLs, invoice contents or subscription links.
type Report struct {
	ReadOnly     bool          `json:"read_only"`
	Status       string        `json:"status"`
	FailedCheck  string        `json:"failed_check,omitempty"`
	InvoiceNinja InvoiceReport `json:"invoice_ninja"`
	SUI          SUIReport     `json:"sui"`
	Mapping      MappingReport `json:"exact_id_mapping"`
}

type InvoiceReport struct {
	AppVersion          string `json:"app_version,omitempty"`
	ClientsRead         int    `json:"clients_read"`
	ActiveClients       int    `json:"active_clients"`
	PagesRead           int    `json:"pages_read"`
	InventoryComplete   bool   `json:"inventory_complete"`
	SelectedProduct     bool   `json:"selected_product_found"`
	ProductActive       bool   `json:"selected_product_active"`
	InvoicesSampled     int    `json:"invoices_sampled"`
	PaymentsSampled     int    `json:"payments_sampled"`
	RecurringSampled    int    `json:"recurring_invoices_sampled"`
	InvoiceReadsPassed  bool   `json:"invoice_read_passed"`
	PaymentReadsPassed  bool   `json:"payment_read_passed"`
	RecurringReadPassed bool   `json:"recurring_invoice_read_passed"`
}

type SUIReport struct {
	DeclaredVersion         string `json:"declared_version,omitempty"`
	VersionVerified         bool   `json:"version_verified"`
	AccountsRead            int    `json:"accounts_read"`
	InboundsRead            int    `json:"inbounds_read"`
	ConfiguredInboundsFound bool   `json:"configured_inbounds_found"`
	SubscriptionBaseMatches *bool  `json:"subscription_base_matches_public_override"`
}

type MappingReport struct {
	ExactMatches                     int `json:"exact_matches"`
	InvoiceClientsWithoutSUI         int `json:"invoice_clients_without_exact_sui_match"`
	SUIOutsideExactMapping           int `json:"sui_records_outside_exact_mapping"`
	InvoiceClientsWithoutTelegramID  int `json:"invoice_clients_without_telegram_id"`
	InvalidTelegramIDs               int `json:"invalid_telegram_ids"`
	DuplicateTelegramIDs             int `json:"duplicate_telegram_ids"`
	DuplicateSUINames                int `json:"duplicate_sui_names"`
	DuplicateSUIRecordIDs            int `json:"duplicate_sui_record_ids"`
	MatchedAccountsDisabled          int `json:"matched_accounts_disabled"`
	MatchedWithoutConfiguredInbounds int `json:"matched_accounts_without_configured_inbounds"`
}

type invoiceReader interface {
	ListCustomers(context.Context, int, int) (invoiceninja.Page[invoiceninja.Customer], error)
	GetProductWithVersion(context.Context, string) (invoiceninja.Product, string, error)
	ListInvoices(context.Context, int, int) (invoiceninja.Page[invoiceninja.Invoice], error)
	ListPayments(context.Context, int, int) (invoiceninja.Page[invoiceninja.Payment], error)
	ListRecurringInvoices(context.Context, int, int) (invoiceninja.Page[invoiceninja.RecurringInvoice], error)
}

type suiReader interface {
	ListAccounts(context.Context) ([]sui.Account, error)
	ListInbounds(context.Context) ([]sui.Inbound, error)
	GetSettings(context.Context) (sui.Settings, error)
}

// Check performs GET requests only. It neither opens the bot database nor
// imports these potential mappings or changes any external object.
func Check(ctx context.Context, cfg Config) (Report, error) {
	initial := Report{ReadOnly: true, Status: "failed", FailedCheck: "configuration"}
	if err := cfg.Validate(); err != nil {
		return initial, err
	}
	ninja, err := invoiceninja.New(cfg.InvoiceNinja.BaseURL, cfg.InvoiceNinja.Token, cfg.HTTPTimeout)
	if err != nil {
		return initial, errors.New("could not initialize Invoice Ninja adapter")
	}
	panel, err := sui.New(cfg.SUI.BaseURL, cfg.SUI.Token, cfg.HTTPTimeout)
	if err != nil {
		return initial, errors.New("could not initialize S-UI adapter")
	}
	return check(ctx, cfg, ninja, panel)
}

func check(ctx context.Context, cfg Config, ninja invoiceReader, panel suiReader) (Report, error) {
	report := Report{ReadOnly: true, Status: "failed"}
	report.SUI.DeclaredVersion = cfg.SUI.DeclaredVersion // Manual declaration, not detected by the API.
	fail := func(stage string, err error) (Report, error) {
		report.FailedCheck = stage
		return report, fmt.Errorf("%s: %w", stage, err)
	}
	var customers []invoiceninja.Customer
	seen := make(map[string]bool)
	expectedTotal := -1
	for page := 1; page <= cfg.MaxPages; page++ {
		batch, err := ninja.ListCustomers(ctx, page, cfg.PageSize)
		if err != nil {
			return fail("invoice_ninja_clients", err)
		}
		report.InvoiceNinja.PagesRead++
		if err := recordVersion(&report.InvoiceNinja.AppVersion, cfg.InvoiceNinja.ExpectedVersion, batch.AppVersion); err != nil {
			return fail("invoice_ninja_version", err)
		}
		if expectedTotal == -1 {
			expectedTotal = batch.Pagination.Total
		} else if expectedTotal != batch.Pagination.Total {
			return fail("invoice_ninja_clients", errors.New("client inventory changed between pages; repeat the check"))
		}
		for _, customer := range batch.Data {
			if seen[customer.ID] {
				return fail("invoice_ninja_clients", errors.New("repeated client across pages; inventory is not stable"))
			}
			seen[customer.ID] = true
			customers = append(customers, customer)
		}
		report.InvoiceNinja.ClientsRead = len(customers)
		if batch.Pagination.TotalPages == 0 || page >= batch.Pagination.TotalPages {
			if len(customers) != expectedTotal {
				return fail("invoice_ninja_clients", errors.New("client pagination ended before the complete inventory was read"))
			}
			report.InvoiceNinja.InventoryComplete = true
			break
		}
		if len(batch.Data) == 0 {
			return fail("invoice_ninja_clients", errors.New("client pagination returned an unexpected empty page"))
		}
	}
	if !report.InvoiceNinja.InventoryComplete {
		return fail("invoice_ninja_clients", errors.New("client inventory exceeds INTEGRATION_MAX_PAGES; no complete inventory was assumed"))
	}
	product, productVersion, err := ninja.GetProductWithVersion(ctx, cfg.InvoiceNinja.ProductID)
	if err != nil {
		return fail("invoice_ninja_product", err)
	}
	if err := recordVersion(&report.InvoiceNinja.AppVersion, cfg.InvoiceNinja.ExpectedVersion, productVersion); err != nil {
		return fail("invoice_ninja_version", err)
	}
	report.InvoiceNinja.SelectedProduct = true
	report.InvoiceNinja.ProductActive = product.Active()
	if !product.Active() {
		return fail("invoice_ninja_product", errors.New("selected product is archived or deleted"))
	}
	sampleSize := min(cfg.PageSize, 2)
	invoices, err := ninja.ListInvoices(ctx, 1, sampleSize)
	if err != nil {
		return fail("invoice_ninja_invoices", err)
	}
	report.InvoiceNinja.InvoiceReadsPassed, report.InvoiceNinja.InvoicesSampled = true, len(invoices.Data)
	payments, err := ninja.ListPayments(ctx, 1, sampleSize)
	if err != nil {
		return fail("invoice_ninja_payments", err)
	}
	report.InvoiceNinja.PaymentReadsPassed, report.InvoiceNinja.PaymentsSampled = true, len(payments.Data)
	recurring, err := ninja.ListRecurringInvoices(ctx, 1, sampleSize)
	if err != nil {
		return fail("invoice_ninja_recurring", err)
	}
	report.InvoiceNinja.RecurringReadPassed, report.InvoiceNinja.RecurringSampled = true, len(recurring.Data)
	for _, version := range []string{invoices.AppVersion, payments.AppVersion, recurring.AppVersion} {
		if err := recordVersion(&report.InvoiceNinja.AppVersion, cfg.InvoiceNinja.ExpectedVersion, version); err != nil {
			return fail("invoice_ninja_version", err)
		}
	}
	accounts, err := panel.ListAccounts(ctx)
	if err != nil {
		return fail("sui_clients", err)
	}
	report.SUI.AccountsRead = len(accounts)
	inbounds, err := panel.ListInbounds(ctx)
	if err != nil {
		return fail("sui_inbounds", err)
	}
	report.SUI.InboundsRead = len(inbounds)
	foundInbounds := make(map[int64]bool)
	for _, inbound := range inbounds {
		foundInbounds[inbound.ID] = true
	}
	report.SUI.ConfiguredInboundsFound = true
	for _, id := range cfg.SUI.InboundIDs {
		if !foundInbounds[id] {
			report.SUI.ConfiguredInboundsFound = false
		}
	}
	if !report.SUI.ConfiguredInboundsFound {
		return fail("sui_inbounds", errors.New("one or more configured inbounds were not returned by S-UI"))
	}
	settings, err := panel.GetSettings(ctx)
	if err != nil {
		return fail("sui_settings", err)
	}
	if settings.SubURI != "" {
		if integrationhttp.ValidateBaseURL(settings.SubURI) != nil {
			return fail("sui_subscription_base", errors.New("S-UI public subscription override is not a supported base URL"))
		}
		matches := strings.TrimRight(settings.SubURI, "/") == strings.TrimRight(cfg.SUI.SubscriptionBaseURL, "/")
		report.SUI.SubscriptionBaseMatches = &matches
		if !matches {
			return fail("sui_subscription_base", errors.New("configured subscription base differs from the S-UI public override"))
		}
	}
	mapExactIDs(&report, customers, accounts, cfg)
	if report.Mapping.InvalidTelegramIDs+report.Mapping.DuplicateTelegramIDs+report.Mapping.DuplicateSUINames+report.Mapping.DuplicateSUIRecordIDs > 0 {
		return fail("identity_mapping", errors.New("invalid or ambiguous identifiers require manual review"))
	}
	report.Status = "ok"
	return report, nil
}

func recordVersion(current *string, _, actual string) error {
	if actual != "" && !versionPattern.MatchString(actual) {
		return errors.New("API returned an unsupported version header")
	}
	// Release strings are report metadata, not API compatibility checks. A
	// rolling upgrade can legitimately produce different versions across reads.
	actual = strings.TrimPrefix(actual, "v")
	if actual != "" {
		*current = actual
	}
	return nil
}
