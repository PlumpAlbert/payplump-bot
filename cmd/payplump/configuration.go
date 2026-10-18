package main

import (
	"context"
	"errors"

	"github.com/PlumpAlbert/payplump-bot/internal/configdelivery"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
)

type configurationPolicyDisabler interface{ DisableConfigurationPolicies(context.Context) error }

// This runs before readiness even without Telegram or a usable webhook. A
// disabled replacement must revoke new grants held by older known policies.
func fenceConfigurationStartup(ctx context.Context, store configurationPolicyDisabler, cfg configdelivery.Config) error {
	if cfg.Enabled {
		return nil
	}
	return store.DisableConfigurationPolicies(ctx)
}

// Only read-only capabilities are constructed for delivery. Enabling it does
// not instantiate S-UI or recurring writers or require activation to be running.
func newConfigurationProviders(cfg configdelivery.Config) (configdelivery.Providers, error) {
	if err := cfg.Validate(); err != nil {
		return configdelivery.Providers{}, err
	}
	if !cfg.Enabled {
		return configdelivery.Providers{}, nil
	}
	payments, err := invoiceninja.NewSettlementClient(cfg.BaseURL, cfg.Token, cfg.HTTPTimeout, cfg.PageSize, cfg.MaxPages)
	if err != nil {
		return configdelivery.Providers{}, errors.New("invalid configuration settlement reader")
	}
	panel, err := sui.NewRecordReader(cfg.SUIBaseURL, cfg.SUIToken, cfg.SUIVersion, cfg.HTTPTimeout)
	if err != nil {
		return configdelivery.Providers{}, errors.New("invalid configuration panel reader")
	}
	recurring, err := invoiceninja.NewRecurringReader(cfg.BaseURL, cfg.Token, cfg.HTTPTimeout)
	if err != nil {
		return configdelivery.Providers{}, errors.New("invalid configuration recurring reader")
	}
	return configdelivery.Providers{Payments: payments, SUI: panel, Recurring: recurring}, nil
}

type configurationGateway struct {
	*telegram.Client
	delivery *configdelivery.Service
}

func (g *configurationGateway) SendConfiguration(ctx context.Context, message queue.Message) (int64, error) {
	return g.delivery.Deliver(ctx, message)
}
