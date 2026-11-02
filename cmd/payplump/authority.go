package main

import (
	"log/slog"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/importverify"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/ninjaauthority"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

// No editable S-UI capability escapes this constructor. The reconciler's sole
// writer is the create-only adapter; existing source records remain read-only.
func newAuthorityReconciler(store ninjaauthority.Store, bot int64, admins []int64, cfg activation.Config, logger *slog.Logger) (*ninjaauthority.Service, error) {
	source, err := importverify.SourceScope(cfg.BaseURL, cfg.Token, "5.13.47", cfg.SUIBaseURL, cfg.SUIToken, "1.6.4")
	if err != nil {
		return nil, err
	}
	customers, err := invoiceninja.NewCustomerInventoryReader(cfg.BaseURL, cfg.Token, cfg.HTTPTimeout, cfg.PageSize, cfg.MaxPages)
	if err != nil {
		return nil, err
	}
	invoices, err := invoiceninja.NewCycleReader(cfg.BaseURL, cfg.Token, cfg.HTTPTimeout, cfg.PageSize, cfg.MaxPages)
	if err != nil {
		return nil, err
	}
	panel, err := sui.NewAuthorityClient(cfg.SUIBaseURL, cfg.SUIToken, cfg.HTTPTimeout, cfg.MaxAccounts)
	if err != nil {
		return nil, err
	}
	return ninjaauthority.New(store, customers, invoices, panel, ninjaauthority.Config{BotID: bot, SourceScopeHash: source, InboundIDs: cfg.InboundIDs, ExcludedIDs: cfg.ExcludedTelegramIDs, AdminIDs: admins}, logger)
}
