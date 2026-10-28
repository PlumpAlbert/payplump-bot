package activation

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func activationTestConfig() Config {
	return Config{Enabled: true, TelegramEnabled: true, PaymentsEnabled: true, ExclusiveWriter: true, TaxModeConfirmed: true,
		BaseURL: "https://billing.invalid/prefix", Token: "synthetic-ninja-token", ProductID: "product-A", CurrencyID: "1", TelegramIDField: "id_number",
		SUIBaseURL: "http://panel.invalid/protected", SUIToken: "synthetic-panel-token", SUIVersion: "1.6.4", InboundIDs: []int64{7}, AllowedTelegramIDs: []int64{101},
		Workers: 1, PageSize: 50, MaxPages: 20, MaxAccounts: 1000, PollInterval: time.Second, HTTPTimeout: time.Second}
}
func activationEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"ACTIVATION_ENABLED", "ACTIVATION_EXCLUSIVE_WRITER", "ACTIVATION_ALLOWED_TELEGRAM_IDS", "ACTIVATION_WORKERS", "ACTIVATION_MAX_SUI_ACCOUNTS", "ACTIVATION_POLL_INTERVAL", "ACTIVATION_HTTP_TIMEOUT", "INVOICE_NINJA_BASE_URL", "INVOICE_NINJA_API_TOKEN", "INVOICE_NINJA_PRODUCT_ID", "INVOICE_NINJA_TELEGRAM_ID_FIELD", "INVOICE_NINJA_VERSION", "INVOICE_NINJA_INCLUSIVE_TAXES", "PAYMENTS_CURRENCY_ID", "SUI_BASE_URL", "SUI_API_TOKEN", "SUI_VERSION", "SUI_INBOUND_IDS", "REGISTRATION_EXCLUDED_TELEGRAM_IDS", "INTEGRATION_PAGE_SIZE", "INTEGRATION_MAX_PAGES"} {
		t.Setenv(key, "")
	}
}
func enabledActivationEnv(t *testing.T) {
	t.Helper()
	activationEnv(t)
	for k, v := range map[string]string{"ACTIVATION_ENABLED": "true", "ACTIVATION_EXCLUSIVE_WRITER": "true", "ACTIVATION_ALLOWED_TELEGRAM_IDS": "101, 102", "INVOICE_NINJA_BASE_URL": "https://billing.invalid/prefix", "INVOICE_NINJA_API_TOKEN": "synthetic-ninja-token", "INVOICE_NINJA_PRODUCT_ID": "product-A", "INVOICE_NINJA_TELEGRAM_ID_FIELD": "id_number", "INVOICE_NINJA_VERSION": "5.13.47", "INVOICE_NINJA_INCLUSIVE_TAXES": "false", "PAYMENTS_CURRENCY_ID": "1", "SUI_BASE_URL": "http://panel.invalid/protected", "SUI_API_TOKEN": "synthetic-panel-token", "SUI_VERSION": "1.6.4", "SUI_INBOUND_IDS": "7,9"} {
		t.Setenv(k, v)
	}
}
func TestActivationDisabledIgnoresUnusedSettings(t *testing.T) {
	activationEnv(t)
	t.Setenv("SUI_BASE_URL", "not-a-url")
	t.Setenv("ACTIVATION_WORKERS", "bad")
	c, e := ConfigFromEnv(false, false)
	if e != nil || c.Enabled || c.Validate() != nil {
		t.Fatal("disabled activation required external credentials")
	}
	t.Setenv("ACTIVATION_ENABLED", "not-bool")
	if _, e = ConfigFromEnv(false, false); e == nil {
		t.Fatal("invalid activation flag accepted")
	}
}
func TestActivationConfigExplicitPilotAndDependencies(t *testing.T) {
	enabledActivationEnv(t)
	c, e := ConfigFromEnv(true, true)
	if e != nil || !c.TaxModeConfirmed || c.InclusiveTaxes || !slices.Equal(c.AllowedTelegramIDs, []int64{101, 102}) || !slices.Equal(c.InboundIDs, []int64{7, 9}) {
		t.Fatal("valid explicit pilot config rejected")
	}
	for _, deps := range [][2]bool{{false, true}, {true, false}, {false, false}} {
		if _, e := ConfigFromEnv(deps[0], deps[1]); e == nil {
			t.Fatal("activation enabled without its dependencies")
		}
	}
	for key, values := range map[string][]string{
		"ACTIVATION_EXCLUSIVE_WRITER": {"", "false", "bad"}, "ACTIVATION_ALLOWED_TELEGRAM_IDS": {"", "0", "-1", "01", "1,1", "1,", "9223372036854775808"},
		"REGISTRATION_EXCLUDED_TELEGRAM_IDS": {"0", "01", "1,1"}, "SUI_INBOUND_IDS": {"", "0", "1,1"},
		"INVOICE_NINJA_TELEGRAM_ID_FIELD": {"", "number", "custom_value1"}, "INVOICE_NINJA_INCLUSIVE_TAXES": {"", "bad"},
		"PAYMENTS_CURRENCY_ID": {"", "RUB", "01", "0", "1e2"}, "ACTIVATION_WORKERS": {"0", "5", "bad"}, "ACTIVATION_MAX_SUI_ACCOUNTS": {"0", "1001", "bad"},
		"ACTIVATION_POLL_INTERVAL": {"0s", "1ns", "2m", "bad"}, "ACTIVATION_HTTP_TIMEOUT": {"500ms", "11s", "bad"}, "INTEGRATION_PAGE_SIZE": {"0", "101", "bad"}, "INTEGRATION_MAX_PAGES": {"0", "101", "bad"},
		"INVOICE_NINJA_BASE_URL": {"https://billing.invalid/api/v1", "https://user:synthetic@billing.invalid", "https://billing.invalid?secret=synthetic"},
		"SUI_BASE_URL":           {"https://panel.invalid/apiv2", "http://panel.invalid/?private=x"}, "INVOICE_NINJA_API_TOKEN": {"", "synthetic\n"}, "SUI_API_TOKEN": {"", " synthetic"},
	} {
		for _, value := range values {
			t.Run(key+"/"+value, func(t *testing.T) {
				t.Setenv(key, value)
				if _, e := ConfigFromEnv(true, true); e == nil {
					t.Fatal("unsafe activation configuration accepted")
				}
			})
		}
	}
}
func TestActivationConfigValidateAlsoProtectsDirectConstruction(t *testing.T) {
	good := activationTestConfig()
	if good.Validate() != nil {
		t.Fatal("valid direct config rejected")
	}
	mutations := []func(*Config){func(c *Config) { c.ExclusiveWriter = false }, func(c *Config) { c.TaxModeConfirmed = false }, func(c *Config) { c.TelegramIDField = "name" }, func(c *Config) { c.AllowedTelegramIDs = nil }, func(c *Config) {
		c.AllowedTelegramIDs = make([]int64, 21)
		for i := range c.AllowedTelegramIDs {
			c.AllowedTelegramIDs[i] = int64(i + 1)
		}
	}, func(c *Config) { c.ExcludedTelegramIDs = []int64{1, 1} }, func(c *Config) { c.InboundIDs = nil }, func(c *Config) { c.CurrencyID = "01" }, func(c *Config) { c.Token = "synthetic\u00a0token" }, func(c *Config) { c.Workers = 5 }}
	for _, mutate := range mutations {
		c := good
		mutate(&c)
		if c.Validate() == nil {
			t.Fatal("direct config bypasses policy")
		}
	}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		text := fmt.Sprintf(format, good)
		if strings.Contains(text, good.Token) || strings.Contains(text, good.SUIToken) || strings.Contains(text, "panel.invalid") {
			t.Fatal("config formatting leaked private data")
		}
	}
}
func TestActivationPolicyFingerprintFreezesScopeNotSecretRotation(t *testing.T) {
	c := activationTestConfig()
	c.InboundIDs = []int64{7, 9}
	c.AllowedTelegramIDs = []int64{102, 101}
	c.ExcludedTelegramIDs = []int64{202, 201}
	base := c.PolicyHash()
	if base == ([32]byte{}) {
		t.Fatal("empty policy hash")
	}
	reordered := c
	reordered.AllowedTelegramIDs = []int64{101, 102}
	reordered.ExcludedTelegramIDs = []int64{201, 202}
	reordered.Token = "rotated-ninja"
	reordered.SUIToken = "rotated-panel"
	reordered.Workers = 2
	reordered.HTTPTimeout = 2 * time.Second
	if reordered.PolicyHash() != base {
		t.Fatal("secret rotation or set ordering changed policy")
	}
	for _, mutate := range []func(*Config){func(c *Config) { c.BaseURL += "/other" }, func(c *Config) { c.SUIBaseURL += "/other" }, func(c *Config) { c.CurrencyID = "2" }, func(c *Config) { c.ProductID = "other" }, func(c *Config) { c.InclusiveTaxes = true }, func(c *Config) { c.TelegramIDField = "name" }, func(c *Config) { c.InboundIDs = []int64{9, 7} }, func(c *Config) { c.AllowedTelegramIDs = []int64{101} }, func(c *Config) { c.ExcludedTelegramIDs = []int64{201} }} {
		changed := c
		mutate(&changed)
		if changed.PolicyHash() == base {
			t.Fatal("authorization policy change was not fenced")
		}
	}
}

func TestActivationPolicyHashPreservesLegacyVersionLabel(t *testing.T) {
	c := activationTestConfig()
	base := c.PolicyHash()
	if got := fmt.Sprintf("%x", base); got != "f1d73de66f87e47a77b433d3e1022469e6684e5ea04dd334b533a2e1ddfe0a3b" {
		t.Fatalf("legacy default policy hash changed: %s", got)
	}
	for _, version := range []string{"1.6.4", "", "v1.6.4", "1.6.5", "custom"} {
		t.Run(version, func(t *testing.T) {
			c := activationTestConfig()
			c.SUIVersion = version
			if c.PolicyHash() != base {
				t.Fatal("version metadata changed policy hash")
			}
			if c.SUIVersion != version {
				t.Fatal("policy hashing changed version metadata")
			}
		})
	}
}

func TestActivationVersionsAreOptionalAndInformational(t *testing.T) {
	enabledActivationEnv(t)
	for _, ninja := range []string{"", "5.13.46", "5.13.48", "v99.2.1+local", "custom-build"} {
		for _, panel := range []string{"", "1.6.3", "1.7.0", "v99.2.1-dev", "custom-build"} {
			t.Run(ninja+"/"+panel, func(t *testing.T) {
				t.Setenv("INVOICE_NINJA_VERSION", ninja)
				t.Setenv("SUI_VERSION", panel)
				cfg, err := ConfigFromEnv(true, true)
				if err != nil {
					t.Fatalf("informational versions rejected: %v", err)
				}
				if cfg.SUIVersion != panel {
					t.Fatal("SUI version metadata was not preserved")
				}
				if err := cfg.Validate(); err != nil {
					t.Fatalf("version metadata failed validation: %v", err)
				}
			})
		}
	}
}
