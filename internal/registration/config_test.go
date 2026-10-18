package registration

import (
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
)

func testConfig() Config {
	return Config{
		Enabled: true, TelegramEnabled: true, Workers: 2,
		PollInterval: time.Second, HTTPTimeout: 5 * time.Second,
		PageSize: 50, MaxPages: 20, BaseURL: "https://billing.example.test/installation",
		Token: "secret-api-token", ProductID: "product-1", TaxModeConfirmed: true,
		PaymentPhone: "+7 999 111-22-33", PaymentBank: "Банк <test> & Co",
		PaymentRecipient: "Получатель _plain_",
	}
}

func configEnvironment(t *testing.T, enabled bool) {
	t.Helper()
	for _, key := range []string{
		"REGISTRATION_ENABLED", "REGISTRATION_WORKERS", "REGISTRATION_POLL_INTERVAL", "REGISTRATION_HTTP_TIMEOUT", "REGISTRATION_EXCLUDED_TELEGRAM_IDS",
		"INTEGRATION_PAGE_SIZE", "INTEGRATION_MAX_PAGES", "INVOICE_NINJA_BASE_URL", "INVOICE_NINJA_API_TOKEN",
		"INVOICE_NINJA_PRODUCT_ID", "INVOICE_NINJA_TELEGRAM_ID_FIELD", "INVOICE_NINJA_VERSION",
		"INVOICE_NINJA_INCLUSIVE_TAXES", "PAYMENT_PHONE", "PAYMENT_BANK", "PAYMENT_RECIPIENT",
	} {
		t.Setenv(key, "")
	}
	if enabled {
		c := testConfig()
		t.Setenv("REGISTRATION_ENABLED", "true")
		t.Setenv("INVOICE_NINJA_BASE_URL", c.BaseURL)
		t.Setenv("INVOICE_NINJA_API_TOKEN", c.Token)
		t.Setenv("INVOICE_NINJA_PRODUCT_ID", c.ProductID)
		t.Setenv("INVOICE_NINJA_TELEGRAM_ID_FIELD", "id_number")
		t.Setenv("INVOICE_NINJA_INCLUSIVE_TAXES", "false")
		t.Setenv("PAYMENT_PHONE", c.PaymentPhone)
	}
}

func TestConfigDisabledIgnoresUnusedSecretsAndSettings(t *testing.T) {
	configEnvironment(t, false)
	t.Setenv("REGISTRATION_WORKERS", "not-an-integer")
	t.Setenv("INVOICE_NINJA_INCLUSIVE_TAXES", "not-a-bool")
	t.Setenv("INVOICE_NINJA_VERSION", "unsupported")
	for _, value := range []string{"", "false", "0"} {
		t.Setenv("REGISTRATION_ENABLED", value)
		cfg, err := ConfigFromEnv(false)
		if err != nil || cfg.Enabled || cfg.Validate() != nil {
			t.Fatalf("disabled config: %+v, %v", cfg, err)
		}
		if cfg.Token != "" || cfg.BaseURL != "" || cfg.TaxModeConfirmed {
			t.Fatal("disabled configuration loaded write capability")
		}
	}
	t.Setenv("REGISTRATION_ENABLED", "perhaps")
	if _, err := ConfigFromEnv(false); err == nil {
		t.Fatal("malformed opt-in accepted")
	}
}

func TestConfigEnabledDefaultsAndExplicitTaxPolicy(t *testing.T) {
	configEnvironment(t, true)
	for _, version := range []string{"", invoiceninja.SupportedWriteVersion, "v" + invoiceninja.SupportedWriteVersion} {
		t.Setenv("INVOICE_NINJA_VERSION", version)
		for _, inclusive := range []string{"false", "true"} {
			t.Setenv("INVOICE_NINJA_INCLUSIVE_TAXES", inclusive)
			cfg, err := ConfigFromEnv(true)
			if err != nil {
				t.Fatal(err)
			}
			if !cfg.Enabled || !cfg.TelegramEnabled || !cfg.TaxModeConfirmed || cfg.InclusiveTaxes != (inclusive == "true") {
				t.Fatalf("lost explicit opt-in/tax policy: %+v", cfg)
			}
			if cfg.Workers != 1 || cfg.PollInterval != time.Second || cfg.HTTPTimeout != 5*time.Second || cfg.PageSize != 50 || cfg.MaxPages != 20 {
				t.Fatalf("unexpected defaults: %+v", cfg)
			}
		}
	}
	if _, err := ConfigFromEnv(false); err == nil {
		t.Fatal("enabled without Telegram")
	}
}

func TestConfigEnvironmentRejectsUnsafeOrMalformedSettings(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"INVOICE_NINJA_TELEGRAM_ID_FIELD", ""}, {"INVOICE_NINJA_TELEGRAM_ID_FIELD", "custom_value1"},
		{"INVOICE_NINJA_INCLUSIVE_TAXES", ""}, {"INVOICE_NINJA_INCLUSIVE_TAXES", "automatic"},
		{"INVOICE_NINJA_BASE_URL", ""}, {"INVOICE_NINJA_API_TOKEN", ""}, {"INVOICE_NINJA_PRODUCT_ID", ""}, {"PAYMENT_PHONE", ""},
		{"INVOICE_NINJA_VERSION", "5.13.46"}, {"INVOICE_NINJA_VERSION", "5.13.47+local"}, {"INVOICE_NINJA_VERSION", "5.14.0"},
		{"REGISTRATION_WORKERS", "bad"}, {"REGISTRATION_WORKERS", "0"}, {"REGISTRATION_WORKERS", "5"},
		{"REGISTRATION_POLL_INTERVAL", "bad"}, {"REGISTRATION_POLL_INTERVAL", "99ms"}, {"REGISTRATION_POLL_INTERVAL", "61s"},
		{"REGISTRATION_HTTP_TIMEOUT", "bad"}, {"REGISTRATION_HTTP_TIMEOUT", "999ms"}, {"REGISTRATION_HTTP_TIMEOUT", "11s"},
		{"INTEGRATION_PAGE_SIZE", "bad"}, {"INTEGRATION_PAGE_SIZE", "101"}, {"INTEGRATION_MAX_PAGES", "0"}, {"INTEGRATION_MAX_PAGES", "101"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			configEnvironment(t, true)
			t.Setenv(tc.key, tc.value)
			if _, err := ConfigFromEnv(true); err == nil {
				t.Fatal("unsafe configuration accepted")
			}
		})
	}
}

func TestConfigValidationBounds(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Config)
	}{
		{"telegram", func(c *Config) { c.TelegramEnabled = false }},
		{"base-query", func(c *Config) { c.BaseURL += "?token=secret" }},
		{"base-api-root", func(c *Config) { c.BaseURL += "/api/v1/" }},
		{"base-userinfo", func(c *Config) { c.BaseURL = "https://user:secret@example.test" }},
		{"token-whitespace", func(c *Config) { c.Token += " " }},
		{"token-control", func(c *Config) { c.Token += "\n" }},
		{"product-path", func(c *Config) { c.ProductID = "a/b" }},
		{"product-backslash", func(c *Config) { c.ProductID = `a\b` }},
		{"product-dot", func(c *Config) { c.ProductID = ".." }},
		{"product-length", func(c *Config) { c.ProductID = strings.Repeat("x", 257) }},
		{"tax-unconfirmed", func(c *Config) { c.TaxModeConfirmed = false }},
		{"phone-length", func(c *Config) { c.PaymentPhone = strings.Repeat("я", 129) }},
		{"phone-control", func(c *Config) { c.PaymentPhone = "123\n456" }},
		{"bank-padding", func(c *Config) { c.PaymentBank = " Bank" }},
		{"recipient-length", func(c *Config) { c.PaymentRecipient = strings.Repeat("я", 257) }},
		{"recipient-utf8", func(c *Config) { c.PaymentRecipient = "\xff" }},
		{"workers-low", func(c *Config) { c.Workers = 0 }},
		{"workers-high", func(c *Config) { c.Workers = 5 }},
		{"poll-low", func(c *Config) { c.PollInterval = 100*time.Millisecond - 1 }},
		{"poll-high", func(c *Config) { c.PollInterval = time.Minute + 1 }},
		{"timeout-low", func(c *Config) { c.HTTPTimeout = time.Second - 1 }},
		{"timeout-high", func(c *Config) { c.HTTPTimeout = 10*time.Second + 1 }},
		{"page-low", func(c *Config) { c.PageSize = 0 }},
		{"page-high", func(c *Config) { c.PageSize = 101 }},
		{"pages-low", func(c *Config) { c.MaxPages = 0 }},
		{"pages-high", func(c *Config) { c.MaxPages = 101 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			tc.edit(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("unsafe configuration accepted")
			}
		})
	}
	for _, workers := range []int{1, 4} {
		for _, upper := range []bool{false, true} {
			cfg := testConfig()
			cfg.Workers = workers
			cfg.PageSize, cfg.MaxPages = 1, 1
			cfg.PollInterval, cfg.HTTPTimeout = 100*time.Millisecond, time.Second
			if upper {
				cfg.PageSize, cfg.MaxPages = 100, 100
				cfg.PollInterval, cfg.HTTPTimeout = time.Minute, 10*time.Second
			}
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
		}
	}
}
