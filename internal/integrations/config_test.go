package integrations

import (
	"strings"
	"testing"
	"time"
)

func validConfig() Config {
	return Config{
		InvoiceNinja: InvoiceNinjaConfig{BaseURL: "https://billing.example.invalid/prefix", Token: "synthetic-invoice-token", ProductID: "synthetic-product", TelegramIDField: "id_number"},
		SUI:          SUIConfig{BaseURL: "http://panel.example.invalid/app", Token: "synthetic-panel-token", InboundIDs: []int64{2}, SubscriptionBaseURL: "https://subscriptions.example.invalid/sub"},
		HTTPTimeout:  10 * time.Second, PageSize: 50, MaxPages: 20,
	}
}

func setConfigEnv(t *testing.T) {
	t.Helper()
	for key, value := range map[string]string{
		"INVOICE_NINJA_BASE_URL": "https://billing.example.invalid/prefix", "INVOICE_NINJA_API_TOKEN": "synthetic-invoice-token",
		"INVOICE_NINJA_PRODUCT_ID": "synthetic-product", "INVOICE_NINJA_TELEGRAM_ID_FIELD": "id_number", "INVOICE_NINJA_VERSION": "",
		"SUI_BASE_URL": "http://panel.example.invalid/app", "SUI_API_TOKEN": "synthetic-panel-token", "SUI_INBOUND_IDS": "2",
		"SUI_SUBSCRIPTION_BASE_URL": "https://subscriptions.example.invalid/sub", "SUI_VERSION": "",
		"INTEGRATION_HTTP_TIMEOUT": "", "INTEGRATION_PAGE_SIZE": "", "INTEGRATION_MAX_PAGES": "",
	} {
		t.Setenv(key, value)
	}
}

func TestConfigFromEnv(t *testing.T) {
	setConfigEnv(t)
	cfg, err := ConfigFromEnv()
	if err != nil || cfg.HTTPTimeout != 10*time.Second || cfg.PageSize != 50 || cfg.MaxPages != 20 || cfg.InvoiceNinja.TelegramIDField != "id_number" || cfg.SUI.InboundIDs[0] != 2 {
		t.Fatalf("defaults: %+v", err)
	}
	t.Setenv("SUI_INBOUND_IDS", "2, 5")
	t.Setenv("SUI_VERSION", "v1.6.4")
	t.Setenv("INVOICE_NINJA_VERSION", "5.13.47")
	t.Setenv("INTEGRATION_HTTP_TIMEOUT", "15s")
	t.Setenv("INTEGRATION_PAGE_SIZE", "25")
	t.Setenv("INTEGRATION_MAX_PAGES", "3")
	cfg, err = ConfigFromEnv()
	if err != nil || len(cfg.SUI.InboundIDs) != 2 || cfg.SUI.InboundIDs[1] != 5 || cfg.SUI.DeclaredVersion != "v1.6.4" || cfg.HTTPTimeout != 15*time.Second || cfg.PageSize != 25 || cfg.MaxPages != 3 {
		t.Fatalf("overrides: %v", err)
	}
}

func TestConfigRejectsInvalidValuesAndRedactsInputs(t *testing.T) {
	for _, change := range []func(*Config){
		func(c *Config) { c.InvoiceNinja.BaseURL = "https://user:never-print-me@example.invalid" },
		func(c *Config) { c.InvoiceNinja.BaseURL += "/api/v1/" },
		func(c *Config) { c.SUI.BaseURL += "/apiv2" },
		func(c *Config) { c.SUI.SubscriptionBaseURL += "?key=never-print-me" },
		func(c *Config) { c.SUI.Token = "never-print-me\ninvalid" },
		func(c *Config) { c.InvoiceNinja.Token = "never-print-me has-space" },
		func(c *Config) { c.InvoiceNinja.ProductID = "../never-print-me" },
		func(c *Config) { c.InvoiceNinja.ProductID = "" },
		func(c *Config) { c.InvoiceNinja.TelegramIDField = "number" },
		func(c *Config) { c.InvoiceNinja.TelegramIDField = "id" },
		func(c *Config) { c.InvoiceNinja.ExpectedVersion = "never-print-me" },
		func(c *Config) { c.SUI.DeclaredVersion = "never-print-me" },
		func(c *Config) { c.SUI.InboundIDs = nil },
		func(c *Config) { c.SUI.InboundIDs = []int64{2, 2} },
		func(c *Config) { c.SUI.InboundIDs = []int64{-1} },
		func(c *Config) { c.HTTPTimeout = 31 * time.Second },
		func(c *Config) { c.HTTPTimeout = 999 * time.Millisecond },
		func(c *Config) { c.PageSize = 101 },
		func(c *Config) { c.MaxPages = 0 },
	} {
		cfg := validConfig()
		change(&cfg)
		err := cfg.Validate()
		if err == nil || strings.Contains(err.Error(), "never-print-me") {
			t.Fatal("invalid configuration accepted or sensitive input leaked")
		}
	}
}

func TestConfigRejectsMalformedEnvironment(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"SUI_INBOUND_IDS", ""}, {"SUI_INBOUND_IDS", "2,,3"}, {"SUI_INBOUND_IDS", "0"}, {"SUI_INBOUND_IDS", "never-print-me"},
		{"INTEGRATION_HTTP_TIMEOUT", "never-print-me"}, {"INTEGRATION_PAGE_SIZE", "never-print-me"}, {"INTEGRATION_MAX_PAGES", "never-print-me"},
	} {
		t.Run(tc.name+"/"+tc.value, func(t *testing.T) {
			setConfigEnv(t)
			t.Setenv(tc.name, tc.value)
			_, err := ConfigFromEnv()
			if err == nil || !strings.Contains(err.Error(), tc.name) || strings.Contains(err.Error(), "never-print-me") {
				t.Fatal("malformed environment accepted or not redacted")
			}
		})
	}
}
