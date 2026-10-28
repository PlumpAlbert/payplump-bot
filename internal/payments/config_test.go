package payments

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

const testPaymentSecret = "payments_private_test_secret_1234567890"

func validPaymentConfig() Config {
	return Config{
		Enabled: true, TelegramEnabled: true,
		BaseURL: "https://invoice.invalid/installation", Token: "api-private-test-token", WebhookSecret: testPaymentSecret, CurrencyID: "3",
		Workers: 1, PageSize: 50, MaxPages: 20, PollInterval: time.Second, RecheckInterval: 5 * time.Minute, HTTPTimeout: 5 * time.Second,
	}
}

// Clear only relevant variables, never load .env or rely on developer secrets.
func paymentEnv(t *testing.T, enabled bool) {
	t.Helper()
	for _, key := range []string{
		"PAYMENTS_ENABLED", "INVOICE_NINJA_BASE_URL", "INVOICE_NINJA_API_TOKEN", "INVOICE_NINJA_WEBHOOK_SECRET", "PAYMENTS_CURRENCY_ID",
		"INVOICE_NINJA_TELEGRAM_ID_FIELD", "INVOICE_NINJA_VERSION", "TELEGRAM_WEBHOOK_SECRET", "TELEGRAM_BOT_TOKEN",
		"PAYMENTS_WORKERS", "INTEGRATION_PAGE_SIZE", "INTEGRATION_MAX_PAGES", "PAYMENTS_POLL_INTERVAL", "PAYMENTS_RECHECK_INTERVAL", "PAYMENTS_HTTP_TIMEOUT", "REGISTRATION_EXCLUDED_TELEGRAM_IDS",
	} {
		t.Setenv(key, "")
	}
	if enabled {
		cfg := validPaymentConfig()
		for key, value := range map[string]string{
			"PAYMENTS_ENABLED": "true", "INVOICE_NINJA_BASE_URL": cfg.BaseURL, "INVOICE_NINJA_API_TOKEN": cfg.Token,
			"INVOICE_NINJA_WEBHOOK_SECRET": cfg.WebhookSecret, "PAYMENTS_CURRENCY_ID": cfg.CurrencyID,
			"INVOICE_NINJA_TELEGRAM_ID_FIELD": "id_number", "INVOICE_NINJA_VERSION": "5.13.47",
		} {
			t.Setenv(key, value)
		}
	}
}

func TestConfigDefaultsDisabledAndIgnoresIntegrationSecrets(t *testing.T) {
	paymentEnv(t, false)
	for key, value := range map[string]string{
		"INVOICE_NINJA_BASE_URL": "bad url", "INVOICE_NINJA_API_TOKEN": "private bad token", "INVOICE_NINJA_WEBHOOK_SECRET": "short",
		"INVOICE_NINJA_TELEGRAM_ID_FIELD": "wrong", "INVOICE_NINJA_VERSION": "unrecognized", "PAYMENTS_CURRENCY_ID": "RUB",
		"PAYMENTS_WORKERS": "not integer", "INTEGRATION_PAGE_SIZE": "invalid", "PAYMENTS_HTTP_TIMEOUT": "invalid", "REGISTRATION_EXCLUDED_TELEGRAM_IDS": "invalid",
	} {
		t.Setenv(key, value)
	}
	cfg, err := ConfigFromEnv(false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Enabled || cfg.TelegramEnabled || cfg.BaseURL != "" || cfg.Token != "" || cfg.WebhookSecret != "" || cfg.CurrencyID != "" || len(cfg.ExcludedTelegramIDs) != 0 {
		t.Fatalf("disabled config consumed integration settings: %+v", cfg)
	}
	if cfg.Workers != 1 || cfg.PageSize != 50 || cfg.MaxPages != 20 || cfg.PollInterval != time.Second || cfg.RecheckInterval != 5*time.Minute || cfg.HTTPTimeout != 5*time.Second {
		t.Fatalf("unexpected disabled defaults: %+v", cfg)
	}
	t.Setenv("PAYMENTS_ENABLED", "false")
	if _, err := ConfigFromEnv(true); err != nil {
		t.Fatalf("explicitly disabled feature must ignore stale secrets: %v", err)
	}
	t.Setenv("PAYMENTS_ENABLED", "definitely")
	if _, err := ConfigFromEnv(true); err == nil {
		t.Fatal("malformed feature toggle silently accepted")
	}
	if err := (Config{Workers: -1, WebhookSecret: "invalid"}).Validate(); err != nil {
		t.Fatal("disabled config should not require enabled-only settings")
	}
}

func TestConfigEnabledRequiresIndependentCredentialsNativePolicyAndPinnedContract(t *testing.T) {
	for _, tc := range []struct{ name, key, value string }{
		{"missing API URL", "INVOICE_NINJA_BASE_URL", ""},
		{"URL credentials", "INVOICE_NINJA_BASE_URL", "https://user:password@invoice.invalid"},
		{"URL query", "INVOICE_NINJA_BASE_URL", "https://invoice.invalid?token=secret"},
		{"missing API token", "INVOICE_NINJA_API_TOKEN", ""},
		{"token newline", "INVOICE_NINJA_API_TOKEN", "private\nheader"},
		{"missing id_number mapping", "INVOICE_NINJA_TELEGRAM_ID_FIELD", ""},
		{"different mapping", "INVOICE_NINJA_TELEGRAM_ID_FIELD", "custom_value1"},
		{"unknown company currency", "PAYMENTS_CURRENCY_ID", ""},
		{"symbol instead of currency id", "PAYMENTS_CURRENCY_ID", "RUB"},
		{"zero currency id", "PAYMENTS_CURRENCY_ID", "0"},
		{"leading zero currency id", "PAYMENTS_CURRENCY_ID", "03"},
		{"unsupported version", "INVOICE_NINJA_VERSION", "5.13.48"},
		{"missing dedicated secret", "INVOICE_NINJA_WEBHOOK_SECRET", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paymentEnv(t, true)
			t.Setenv(tc.key, tc.value)
			if _, err := ConfigFromEnv(true); err == nil {
				t.Fatal("accepted unsafe enabled configuration")
			}
		})
	}
	paymentEnv(t, true)
	if _, err := ConfigFromEnv(false); err == nil {
		t.Fatal("enabled verification requires Telegram startup")
	}
	for _, version := range []string{"", "5.13.47", "v5.13.47"} {
		t.Setenv("INVOICE_NINJA_VERSION", version)
		cfg, err := ConfigFromEnv(true)
		if err != nil {
			t.Fatalf("supported source pin %q rejected: %v", version, err)
		}
		if !cfg.Enabled || cfg.CurrencyID != "3" {
			t.Fatal("explicit native policy lost")
		}
	}
}

func TestConfigFromEnvBoundsAndExclusions(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"PAYMENTS_WORKERS", "0"}, {"PAYMENTS_WORKERS", "5"}, {"PAYMENTS_WORKERS", "many"},
		{"INTEGRATION_PAGE_SIZE", "0"}, {"INTEGRATION_PAGE_SIZE", "101"}, {"INTEGRATION_PAGE_SIZE", "1.5"},
		{"INTEGRATION_MAX_PAGES", "0"}, {"INTEGRATION_MAX_PAGES", "101"},
		{"PAYMENTS_POLL_INTERVAL", "99ms"}, {"PAYMENTS_POLL_INTERVAL", "61s"}, {"PAYMENTS_POLL_INTERVAL", "quick"},
		{"PAYMENTS_RECHECK_INTERVAL", "29s"}, {"PAYMENTS_RECHECK_INTERVAL", "61m"},
		{"PAYMENTS_HTTP_TIMEOUT", "999ms"}, {"PAYMENTS_HTTP_TIMEOUT", "11s"},
		{"REGISTRATION_EXCLUDED_TELEGRAM_IDS", "0"}, {"REGISTRATION_EXCLUDED_TELEGRAM_IDS", "-1"},
		{"REGISTRATION_EXCLUDED_TELEGRAM_IDS", "01"}, {"REGISTRATION_EXCLUDED_TELEGRAM_IDS", "+1"},
		{"REGISTRATION_EXCLUDED_TELEGRAM_IDS", "1,1"}, {"REGISTRATION_EXCLUDED_TELEGRAM_IDS", "1,,2"},
		{"REGISTRATION_EXCLUDED_TELEGRAM_IDS", "9223372036854775808"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			paymentEnv(t, true)
			t.Setenv(tc.key, tc.value)
			if _, err := ConfigFromEnv(true); err == nil {
				t.Fatal("accepted out-of-bounds or malformed setting")
			}
		})
	}
	paymentEnv(t, true)
	for key, value := range map[string]string{
		"PAYMENTS_WORKERS": "4", "INTEGRATION_PAGE_SIZE": "100", "INTEGRATION_MAX_PAGES": "100",
		"PAYMENTS_POLL_INTERVAL": "100ms", "PAYMENTS_RECHECK_INTERVAL": "30s", "PAYMENTS_HTTP_TIMEOUT": "10s", "REGISTRATION_EXCLUDED_TELEGRAM_IDS": " 17, 18 ",
	} {
		t.Setenv(key, value)
	}
	cfg, err := ConfigFromEnv(true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Workers != 4 || cfg.PageSize != 100 || cfg.MaxPages != 100 || cfg.PollInterval != 100*time.Millisecond || cfg.RecheckInterval != 30*time.Second || cfg.HTTPTimeout != 10*time.Second || !reflect.DeepEqual(cfg.ExcludedTelegramIDs, []int64{17, 18}) {
		t.Fatalf("lost explicit settings: %+v", cfg)
	}
	another, err := ConfigFromEnv(true)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ExcludedTelegramIDs[0] = 999
	if another.ExcludedTelegramIDs[0] != 17 {
		t.Fatal("parsed exclusions unexpectedly share mutable backing storage")
	}
}

func TestConfigValidateDirectBounds(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Config)
	}{
		{"zero workers", func(c *Config) { c.Workers = 0 }},
		{"five workers", func(c *Config) { c.Workers = 5 }},
		{"zero page size", func(c *Config) { c.PageSize = 0 }},
		{"over page size", func(c *Config) { c.PageSize = 101 }},
		{"zero pages", func(c *Config) { c.MaxPages = 0 }},
		{"over pages", func(c *Config) { c.MaxPages = 101 }},
		{"short polling", func(c *Config) { c.PollInterval = 100*time.Millisecond - time.Nanosecond }},
		{"long polling", func(c *Config) { c.PollInterval = time.Minute + time.Nanosecond }},
		{"short recheck", func(c *Config) { c.RecheckInterval = 30*time.Second - time.Nanosecond }},
		{"long recheck", func(c *Config) { c.RecheckInterval = time.Hour + time.Nanosecond }},
		{"short HTTP", func(c *Config) { c.HTTPTimeout = time.Second - time.Nanosecond }},
		{"long HTTP", func(c *Config) { c.HTTPTimeout = 10*time.Second + time.Nanosecond }},
		{"negative exclusion", func(c *Config) { c.ExcludedTelegramIDs = []int64{-1} }},
		{"duplicate exclusion", func(c *Config) { c.ExcludedTelegramIDs = []int64{17, 17} }},
		{"too many exclusions", func(c *Config) {
			c.ExcludedTelegramIDs = make([]int64, 1001)
			for i := range c.ExcludedTelegramIDs {
				c.ExcludedTelegramIDs[i] = int64(i + 1)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validPaymentConfig()
			tc.edit(&cfg)
			if cfg.Validate() == nil {
				t.Fatal("accepted invalid direct configuration")
			}
		})
	}
	for _, size := range []int{32, 256} {
		cfg := validPaymentConfig()
		cfg.WebhookSecret = strings.Repeat("a", size)
		cfg.Workers, cfg.PageSize, cfg.MaxPages = 4, 1, 1
		cfg.PollInterval, cfg.RecheckInterval, cfg.HTTPTimeout = time.Minute, time.Hour, time.Second
		if err := cfg.Validate(); err != nil {
			t.Fatalf("valid inclusive boundary rejected: %v", err)
		}
	}
}

func TestConfigValidateAllowsSecretReuseWithoutEnvironmentParser(t *testing.T) {
	cfg := validPaymentConfig()
	cfg.Token = cfg.WebhookSecret
	if err := cfg.Validate(); err != nil {
		t.Fatalf("shared secret rejected: %v", err)
	}
}
