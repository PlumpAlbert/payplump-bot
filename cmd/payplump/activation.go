package main

import (
	"errors"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

// Construct capabilities only for an explicit, validated pilot. Constructors do
// no networking; every mutation will still require the journal authorizer.
func newActivationProviders(cfg activation.Config) (activation.Providers, error) {
	if err := cfg.Validate(); err != nil {
		return activation.Providers{}, err
	}
	if !cfg.Enabled {
		return activation.Providers{}, nil
	}
	settlement, err := invoiceninja.NewSettlementClient(cfg.BaseURL, cfg.Token, cfg.HTTPTimeout, cfg.PageSize, cfg.MaxPages)
	if err != nil {
		return activation.Providers{}, errors.New("invalid activation payment reader")
	}
	products, err := invoiceninja.New(cfg.BaseURL, cfg.Token, cfg.HTTPTimeout)
	if err != nil {
		return activation.Providers{}, errors.New("invalid activation product reader")
	}
	panel, err := sui.NewManagedClient(cfg.SUIBaseURL, cfg.SUIToken, cfg.SUIVersion, cfg.HTTPTimeout, cfg.MaxAccounts)
	if err != nil {
		return activation.Providers{}, errors.New("invalid activation SUI writer")
	}
	recurring, err := invoiceninja.NewRecurringClient(cfg.BaseURL, cfg.Token, cfg.HTTPTimeout, cfg.PageSize, cfg.MaxPages)
	if err != nil {
		return activation.Providers{}, errors.New("invalid activation recurring writer")
	}
	return activation.Providers{Payments: settlement, Products: products, SUI: panel, Recurring: recurring}, nil
}
