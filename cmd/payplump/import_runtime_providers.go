package main

import (
	"errors"

	"github.com/PlumpAlbert/payplump-bot/internal/importverify"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/lifecycle"
)

type importedBillingProviders struct {
	Enforcement *invoiceninja.ImportedRecurringClient
	Renewal     *invoiceninja.ImportedRecurringClient
}

// newImportedBillingProviders connects the validated imported source scope to
// native lifecycle consumers. These clients grant only the existing imported
// recurring capability; runtime effects remain governed by the durable store.
func newImportedBillingProviders(runtime importedRuntimeConfig, enforcement lifecycle.EnforcementConfig, renewal lifecycle.RenewalConfig) (importedBillingProviders, error) {
	var providers importedBillingProviders
	if !runtime.Enabled {
		return providers, nil
	}

	if enforcement.Enabled {
		if err := enforcement.Validate(true, true, true); err != nil {
			return importedBillingProviders{}, errors.New("invalid imported enforcement provider configuration")
		}
		scope, err := importverify.SourceScope(enforcement.BaseURL, enforcement.Token, "5.13.47", enforcement.SUIBaseURL, enforcement.SUIToken, enforcement.SUIVersion)
		if err != nil || scope != runtime.SourceScopeHash {
			return importedBillingProviders{}, errors.New("imported enforcement provider source scope mismatch")
		}
		client, err := invoiceninja.NewImportedRecurringClient(enforcement.BaseURL, enforcement.Token, enforcement.HTTPTimeout, 50, 20)
		if err != nil {
			return importedBillingProviders{}, errors.New("invalid imported enforcement provider configuration")
		}
		providers.Enforcement = client
	}

	if renewal.Enabled {
		if err := renewal.Validate(true, true, true); err != nil {
			return importedBillingProviders{}, errors.New("invalid imported renewal provider configuration")
		}
		scope, err := importverify.SourceScope(renewal.BaseURL, renewal.Token, renewal.InvoiceNinjaVersion, renewal.SUIBaseURL, renewal.SUIToken, renewal.SUIVersion)
		if err != nil || scope != runtime.SourceScopeHash {
			return importedBillingProviders{}, errors.New("imported renewal provider source scope mismatch")
		}
		client, err := invoiceninja.NewImportedRecurringClient(renewal.BaseURL, renewal.Token, renewal.HTTPTimeout, 50, 20)
		if err != nil {
			return importedBillingProviders{}, errors.New("invalid imported renewal provider configuration")
		}
		providers.Renewal = client
	}
	return providers, nil
}

var _ lifecycle.EnforcementImportedRecurringWriter = (*invoiceninja.ImportedRecurringClient)(nil)
var _ lifecycle.RenewalImportedRecurringWriter = (*invoiceninja.ImportedRecurringClient)(nil)
