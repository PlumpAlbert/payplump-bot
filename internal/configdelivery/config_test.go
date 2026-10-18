package configdelivery

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func configurationTestConfig() Config {
	return Config{Enabled: true, TelegramEnabled: true, PaymentsEnabled: true, Revision: 1, BaseURL: "https://billing.example.test/company/", Token: "synthetic-ninja-token", CurrencyID: "1", TelegramIDField: "id_number", SUIBaseURL: "http://panel.example.test/private/", SUIToken: "synthetic-panel-token", SUIVersion: "1.6.4", SubscriptionBaseURL: "https://subscription.example.test/prefix/", AllowedTelegramIDs: []int64{101, 102}, ExcludedTelegramIDs: []int64{103}, HTTPTimeout: 3 * time.Second, PageSize: 50, MaxPages: 20}
}
func configurationEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"CONFIGURATION_DELIVERY_ENABLED", "CONFIGURATION_POLICY_REVISION", "CONFIGURATION_HTTP_TIMEOUT", "INVOICE_NINJA_VERSION", "INTEGRATION_PAGE_SIZE", "INTEGRATION_MAX_PAGES", "ACTIVATION_ALLOWED_TELEGRAM_IDS", "REGISTRATION_EXCLUDED_TELEGRAM_IDS"} {
		t.Setenv(k, "")
	}
	for k, v := range map[string]string{"CONFIGURATION_DELIVERY_ENABLED": "true", "CONFIGURATION_POLICY_REVISION": "1", "INVOICE_NINJA_BASE_URL": "https://billing.example.test/company/", "INVOICE_NINJA_API_TOKEN": "synthetic-ninja-token", "INVOICE_NINJA_TELEGRAM_ID_FIELD": "id_number", "PAYMENTS_CURRENCY_ID": "1", "SUI_BASE_URL": "http://panel.example.test/private/", "SUI_API_TOKEN": "synthetic-panel-token", "SUI_VERSION": "1.6.4", "SUI_SUBSCRIPTION_BASE_URL": "https://subscription.example.test/prefix/", "ACTIVATION_ALLOWED_TELEGRAM_IDS": "101,102"} {
		t.Setenv(k, v)
	}
}
func TestConfigurationDisabledIgnoresUnusedSecretsAndDependencies(t *testing.T) {
	configurationEnv(t)
	t.Setenv("CONFIGURATION_DELIVERY_ENABLED", "")
	t.Setenv("CONFIGURATION_POLICY_REVISION", "not-an-integer")
	t.Setenv("SUI_SUBSCRIPTION_BASE_URL", "unsafe")
	t.Setenv("SUI_API_TOKEN", "")
	cfg, err := ConfigFromEnv(false, false)
	if err != nil || cfg.Enabled || cfg.Policy() != (Policy{}) || cfg.Allows(101) {
		t.Fatal("configuration delivery was not default-off")
	}
	t.Setenv("CONFIGURATION_DELIVERY_ENABLED", "invalid")
	if _, err := ConfigFromEnv(false, false); err == nil {
		t.Fatal("invalid feature flag accepted")
	}
}
func TestConfigurationEnvIsExplicitCanonicalAndBounded(t *testing.T) {
	configurationEnv(t)
	cfg, err := ConfigFromEnv(true, true)
	if err != nil || cfg.Validate() != nil || cfg.HTTPTimeout != 3*time.Second || cfg.Revision != 1 || !cfg.Allows(101) {
		t.Fatal("synthetic pilot config rejected")
	}
	for _, tc := range []struct{ key, value string }{{"CONFIGURATION_POLICY_REVISION", ""}, {"CONFIGURATION_POLICY_REVISION", "0"}, {"CONFIGURATION_POLICY_REVISION", "01"}, {"CONFIGURATION_POLICY_REVISION", "+1"}, {"CONFIGURATION_POLICY_REVISION", "9223372036854775808"}, {"ACTIVATION_ALLOWED_TELEGRAM_IDS", ""}, {"ACTIVATION_ALLOWED_TELEGRAM_IDS", "101,101"}, {"ACTIVATION_ALLOWED_TELEGRAM_IDS", "0101"}, {"ACTIVATION_ALLOWED_TELEGRAM_IDS", "-101"}, {"ACTIVATION_ALLOWED_TELEGRAM_IDS", "101,"}, {"REGISTRATION_EXCLUDED_TELEGRAM_IDS", "103,103"}, {"INVOICE_NINJA_TELEGRAM_ID_FIELD", "name"}, {"PAYMENTS_CURRENCY_ID", "01"}, {"INVOICE_NINJA_VERSION", "5.13.46"}, {"SUI_VERSION", "1.6.5"}, {"CONFIGURATION_HTTP_TIMEOUT", "999ms"}, {"CONFIGURATION_HTTP_TIMEOUT", "6s"}, {"CONFIGURATION_HTTP_TIMEOUT", "broken"}, {"INTEGRATION_PAGE_SIZE", "0"}, {"INTEGRATION_MAX_PAGES", "101"}, {"INTEGRATION_MAX_PAGES", "broken"}, {"SUI_API_TOKEN", "contains space"}, {"INVOICE_NINJA_API_TOKEN", "token\n"}, {"SUI_SUBSCRIPTION_BASE_URL", "http://subscription.example.test/"}, {"SUI_SUBSCRIPTION_BASE_URL", "https://user:pass@subscription.example.test/"}, {"SUI_SUBSCRIPTION_BASE_URL", "https://subscription.example.test/?other=value"}, {"SUI_SUBSCRIPTION_BASE_URL", "https://subscription.example.test/#fragment"}} {
		t.Run(tc.key+"/"+fmt.Sprint(len(tc.value)), func(t *testing.T) {
			configurationEnv(t)
			t.Setenv(tc.key, tc.value)
			if _, err := ConfigFromEnv(true, true); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	configurationEnv(t)
	for _, deps := range [][2]bool{{false, true}, {true, false}} {
		if _, err := ConfigFromEnv(deps[0], deps[1]); err == nil {
			t.Fatal("delivery without payment/Telegram verification accepted")
		}
	}
	configurationEnv(t)
	t.Setenv("REGISTRATION_EXCLUDED_TELEGRAM_IDS", "101")
	cfg, err = ConfigFromEnv(true, true)
	if err != nil || cfg.Allows(101) || !cfg.Allows(102) || cfg.Allows(0) || cfg.Allows(999) {
		t.Fatal("exclusion did not dominate pilot scope")
	}
}
func TestConfigurationPolicyFreezesScopeAndEndpointsNotSecrets(t *testing.T) {
	original := configurationTestConfig()
	if original.Validate() != nil {
		t.Fatal("invalid synthetic config")
	}
	base := original.Policy()
	for _, mutate := range []func(*Config){func(c *Config) { c.Token = "rotated"; c.SUIToken = "rotated" }, func(c *Config) { c.AllowedTelegramIDs = []int64{102, 101} }, func(c *Config) { c.HTTPTimeout = time.Second; c.PageSize = 1; c.MaxPages = 1 }} {
		c := configurationTestConfig()
		mutate(&c)
		if c.Policy() != base {
			t.Fatal("operational change invalidated policy")
		}
	}
	for _, mutate := range []func(*Config){func(c *Config) { c.BaseURL = "https://other.example.test/" }, func(c *Config) { c.SUIBaseURL = "http://other.example.test/" }, func(c *Config) { c.SubscriptionBaseURL = "https://other.example.test/" }, func(c *Config) { c.CurrencyID = "2" }, func(c *Config) { c.AllowedTelegramIDs = []int64{101} }, func(c *Config) { c.ExcludedTelegramIDs = []int64{101} }, func(c *Config) { c.SUIVersion = "v1.6.4" }} {
		c := configurationTestConfig()
		mutate(&c)
		if c.Policy().Hash == base.Hash {
			t.Fatal("authorization policy drift not fenced")
		}
	}
	c := configurationTestConfig()
	c.Revision++
	if c.Policy().Revision == base.Revision || c.Policy().Hash != base.Hash {
		t.Fatal("policy revision must be a separate monotonic fence")
	}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		got := fmt.Sprintf(format, original)
		for _, private := range []string{original.Token, original.SUIToken, original.BaseURL, original.SubscriptionBaseURL} {
			if strings.Contains(got, private) {
				t.Fatal("private configuration formatting leaked")
			}
		}
	}
}
func TestConfigurationScopeAndURLBounds(t *testing.T) {
	for _, mutate := range []func(*Config){func(c *Config) { c.AllowedTelegramIDs = nil }, func(c *Config) { c.AllowedTelegramIDs = []int64{101, 101} }, func(c *Config) { c.ExcludedTelegramIDs = []int64{-1} }, func(c *Config) { c.ExcludedTelegramIDs = make([]int64, 1001) }, func(c *Config) {
		c.SubscriptionBaseURL = "https://subscription.example.test/" + strings.Repeat("x", 1024)
	}} {
		c := configurationTestConfig()
		mutate(&c)
		if c.Validate() == nil {
			t.Fatal("unbounded or invalid scope accepted")
		}
	}
	c := configurationTestConfig()
	c.AllowedTelegramIDs = nil
	for id := int64(1); id <= 20; id++ {
		c.AllowedTelegramIDs = append(c.AllowedTelegramIDs, id)
	}
	if c.Validate() != nil {
		t.Fatal("maximum pilot rejected")
	}
	c.AllowedTelegramIDs = append(c.AllowedTelegramIDs, 21)
	if c.Validate() == nil {
		t.Fatal("broad rollout accepted")
	}
}
