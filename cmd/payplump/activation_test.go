package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
)

func TestActivationCapabilitiesConstructOnlyForValidatedPilot(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	providers, e := newActivationProviders(activation.Config{})
	if e != nil || providers.Payments != nil || providers.Products != nil || providers.SUI != nil || providers.Recurring != nil {
		t.Fatal("disabled activation constructed a capability")
	}
	c := activation.Config{Enabled: true, TelegramEnabled: true, PaymentsEnabled: true, ExclusiveWriter: true, TaxModeConfirmed: true, BaseURL: server.URL + "/ninja", Token: "synthetic-ninja-token", ProductID: "product-A", CurrencyID: "1", TelegramIDField: "id_number", SUIBaseURL: server.URL + "/panel", SUIToken: "synthetic-panel-token", SUIVersion: "1.6.4", InboundIDs: []int64{7}, AllowedTelegramIDs: []int64{101}, Workers: 1, PageSize: 50, MaxPages: 20, MaxAccounts: 100, PollInterval: time.Second, HTTPTimeout: time.Second}
	providers, e = newActivationProviders(c)
	if e != nil || providers.Payments == nil || providers.Products == nil || providers.SUI == nil || providers.Recurring == nil || requests.Load() != 0 {
		t.Fatal("capability construction failed or issued a request")
	}
	c.ExclusiveWriter = false
	if _, e = newActivationProviders(c); e == nil || requests.Load() != 0 {
		t.Fatal("unsafe pilot reached providers")
	}
}

func TestInvalidActivationFlagFailsBeforeDatabaseOrTelegram(t *testing.T) {
	t.Setenv("TELEGRAM_ENABLED", "true")
	t.Setenv("REGISTRATION_ENABLED", "")
	t.Setenv("PAYMENTS_ENABLED", "")
	t.Setenv("ACTIVATION_ENABLED", "invalid")
	t.Setenv("DATABASE_URL", "invalid-database-config")
	e := run(slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if e == nil || !strings.Contains(e.Error(), "ACTIVATION_ENABLED") {
		t.Fatal("activation configuration was not checked before database access")
	}
}

func TestLegacyActivationLoaderRequiresPaymentVerification(t *testing.T) {
	t.Setenv("TELEGRAM_ENABLED", "false")
	t.Setenv("REGISTRATION_ENABLED", "false")
	t.Setenv("PAYMENTS_ENABLED", "false")
	t.Setenv("ACTIVATION_ENABLED", "true")
	t.Setenv("DATABASE_URL", "invalid-database-config")
	// Complete enough to reach the dependency check without touching any API.
	for k, v := range map[string]string{"ACTIVATION_EXCLUSIVE_WRITER": "true", "ACTIVATION_ALLOWED_TELEGRAM_IDS": "101", "INVOICE_NINJA_BASE_URL": "https://billing.invalid", "INVOICE_NINJA_API_TOKEN": "synthetic-ninja-token", "INVOICE_NINJA_PRODUCT_ID": "product-A", "INVOICE_NINJA_TELEGRAM_ID_FIELD": "id_number", "INVOICE_NINJA_VERSION": "5.13.47", "INVOICE_NINJA_INCLUSIVE_TAXES": "false", "PAYMENTS_CURRENCY_ID": "1", "SUI_BASE_URL": "http://panel.invalid", "SUI_API_TOKEN": "synthetic-panel-token", "SUI_VERSION": "1.6.4", "SUI_INBOUND_IDS": "7", "REGISTRATION_EXCLUDED_TELEGRAM_IDS": ""} {
		t.Setenv(k, v)
	}
	// The legacy loader remains independently safe; normal service startup
	// rejects partial off-gates earlier as a configuration-migration error.
	_, e := activation.ConfigFromEnv(false, false)
	if e == nil || !strings.Contains(e.Error(), "requires Telegram and payment verification") {
		t.Fatal("activation did not enforce dependencies")
	}
}
