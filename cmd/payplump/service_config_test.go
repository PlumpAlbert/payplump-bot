package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/broadcast"
	"github.com/PlumpAlbert/payplump-bot/internal/configdelivery"
	"github.com/PlumpAlbert/payplump-bot/internal/lifecycle"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/registration"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
)

func normalServiceValues() map[string]string {
	return map[string]string{
		"TELEGRAM_BOT_TOKEN": "synthetic-bot-token", "TELEGRAM_ADMIN_IDS": "101",
		"TELEGRAM_WEBHOOK_URL": "https://bot.invalid/telegram/webhook", "TELEGRAM_WEBHOOK_SECRET": strings.Repeat("t", 32),
		"INVOICE_NINJA_BASE_URL": "https://billing.invalid/company", "INVOICE_NINJA_API_TOKEN": "synthetic-ninja-token",
		"INVOICE_NINJA_PRODUCT_ID": "product-a", "INVOICE_NINJA_INCLUSIVE_TAXES": "false",
		"INVOICE_NINJA_WEBHOOK_SECRET": strings.Repeat("n", 32), "INVOICE_LIFECYCLE_WEBHOOK_SECRET": strings.Repeat("l", 32),
		"PAYMENTS_CURRENCY_ID": "1", "PAYMENTS_CURRENCY_CODE": "USD",
		"SUI_BASE_URL": "http://panel.invalid/private", "SUI_API_TOKEN": "synthetic-panel-token",
		"SUI_INBOUND_IDS": "7,9", "SUI_SUBSCRIPTION_BASE_URL": "https://subs.invalid/",
		"PAYMENT_PHONE": "+10000000000", "TZ": "UTC",
	}
}

func parseNormalService(values map[string]string) error {
	get, managed, e := serviceEnvironment(func(k string) string { return values[k] })
	if e != nil {
		return e
	}
	tg, e := telegram.ConfigFromEnvironment(get)
	if e != nil {
		return e
	}
	b, e := broadcast.ConfigFromEnvironment(get, tg.Enabled, tg.AdminIDs)
	if e != nil {
		return e
	}
	r, e := registration.ConfigFromEnvironment(get, tg.Enabled)
	if e != nil {
		return e
	}
	p, e := payments.ConfigFromEnvironment(get, tg.Enabled)
	if e != nil {
		return e
	}
	l, e := lifecycle.ConfigFromEnvironment(get, tg.Enabled, p.Enabled, p.WebhookSecret, tg.WebhookSecret, tg.Token, managed)
	if e != nil {
		return e
	}
	o, e := lifecycle.EnforcementConfigFromEnvironment(get, l.Enabled, tg.Enabled, p.Enabled, managed)
	if e != nil {
		return e
	}
	n, e := lifecycle.RenewalConfigFromEnvironment(get, l.Enabled, tg.Enabled, p.Enabled, managed)
	if e != nil {
		return e
	}
	a, e := activation.ConfigFromEnvironment(get, tg.Enabled, p.Enabled, managed)
	if e != nil {
		return e
	}
	c, e := configdelivery.ConfigFromEnvironment(get, tg.Enabled, p.Enabled, managed)
	if e != nil {
		return e
	}
	if !tg.Enabled || !b.Enabled || !r.Enabled || !p.Enabled || !l.Enabled || !o.Enabled || !n.Enabled || !a.Enabled || !c.Enabled {
		return os.ErrInvalid
	}
	if !a.ManagedClients || !l.ManagedClients || !o.ManagedClients || !n.ManagedClients || !c.ManagedClients {
		return os.ErrPermission
	}
	return nil
}

func TestNormalServiceNeedsNoFeatureSwitchesOrManualScope(t *testing.T) {
	values := normalServiceValues()
	for key := range values {
		if strings.HasSuffix(key, "_ENABLED") || strings.Contains(key, "_REVISION") || strings.HasPrefix(key, "IMPORT_VERIFY_") {
			t.Fatal("fixture accidentally supplies staging keys")
		}
	}
	if e := parseNormalService(values); e != nil {
		t.Fatal(e)
	}
}
func TestNormalServicePollingNeedsNoWebhookSettings(t *testing.T) {
	for _, unusedSecret := range []string{"", "unused secret with invalid characters"} {
		values := normalServiceValues()
		delete(values, "TELEGRAM_WEBHOOK_URL")
		values["TELEGRAM_WEBHOOK_SECRET"] = unusedSecret
		if err := parseNormalService(values); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNormalServiceInvalidWebhookDoesNotFallBackToPolling(t *testing.T) {
	for key, value := range map[string]string{
		"TELEGRAM_WEBHOOK_URL":    "http://bot.invalid/telegram/webhook",
		"TELEGRAM_WEBHOOK_SECRET": "",
	} {
		values := normalServiceValues()
		values[key] = value
		err := parseNormalService(values)
		if err == nil || !strings.Contains(err.Error(), key) || strings.Contains(err.Error(), values["TELEGRAM_BOT_TOKEN"]) {
			t.Fatal("invalid webhook configuration must fail without leaking credentials")
		}
	}
}

func TestNormalServiceDefaultsDoNotChangeProcessEnvironment(t *testing.T) {
	t.Setenv("ACTIVATION_ENABLED", "false")
	if e := parseNormalService(normalServiceValues()); e != nil {
		t.Fatal(e)
	}
	if os.Getenv("ACTIVATION_ENABLED") != "false" {
		t.Fatal("global environment mutated")
	}
}
func TestNormalServiceRejectsLegacyDisablingAndManualRevisions(t *testing.T) {
	for _, key := range append(append([]string{}, serviceFeatures...), "ACTIVATION_EXCLUSIVE_WRITER", "INVOICE_OVERDUE_EXCLUSIVE_WRITER", "INVOICE_RENEWAL_EXCLUSIVE_WRITER", "ACTIVATION_ALLOWED_TELEGRAM_IDS", "INVOICE_LIFECYCLE_ALLOWED_TELEGRAM_IDS", "CONFIGURATION_POLICY_REVISION", "BROADCAST_POLICY_REVISION", "IMPORTED_RUNTIME_REVISION", "IMPORTED_EFFECTS_REVISION") {
		t.Run(key, func(t *testing.T) {
			values := normalServiceValues()
			values[key] = "false"
			e := parseNormalService(values)
			if e == nil || !strings.Contains(e.Error(), key) {
				t.Fatal("legacy key not diagnosed")
			}
			if strings.Contains(e.Error(), values["TELEGRAM_BOT_TOKEN"]) {
				t.Fatal("secret in error")
			}
		})
	}
}
func TestNormalServiceValidatesActualBusinessParameters(t *testing.T) {
	for _, key := range []string{"TELEGRAM_BOT_TOKEN", "TELEGRAM_ADMIN_IDS", "INVOICE_NINJA_BASE_URL", "INVOICE_NINJA_API_TOKEN", "INVOICE_NINJA_PRODUCT_ID", "INVOICE_NINJA_INCLUSIVE_TAXES", "PAYMENTS_CURRENCY_ID", "PAYMENTS_CURRENCY_CODE", "SUI_BASE_URL", "SUI_API_TOKEN", "SUI_INBOUND_IDS", "SUI_SUBSCRIPTION_BASE_URL", "PAYMENT_PHONE", "TZ"} {
		t.Run(key, func(t *testing.T) {
			values := normalServiceValues()
			delete(values, key)
			if e := parseNormalService(values); e == nil {
				t.Fatal("missing business parameter accepted")
			}
		})
	}
	for key, value := range map[string]string{"SUI_VERSION": "1.6.5", "INVOICE_NINJA_VERSION": "5.13.46", "ACTIVATION_WORKERS": "0", "INVOICE_NINJA_TELEGRAM_ID_FIELD": "name", "TZ": "Not/A_Timezone"} {
		t.Run(key, func(t *testing.T) {
			values := normalServiceValues()
			values[key] = value
			if parseNormalService(values) == nil {
				t.Fatal("unsupported parameter accepted")
			}
		})
	}
}
func TestServiceImportReusesBusinessSettingsAndPreservesAdminExclusions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/ninja/api/v1/products/product-a" {
			t.Errorf("unexpected source request")
		}
		w.Header().Set("X-APP-VERSION", "5.13.47")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{"id":"product-a","product_key":"monthly","is_deleted":false,"archived_at":0}}`)
	}))
	defer server.Close()
	values := normalServiceValues()
	values["INVOICE_NINJA_BASE_URL"] = server.URL + "/ninja"
	values["TELEGRAM_ADMIN_IDS"] = "101, 102"
	values["REGISTRATION_EXCLUDED_TELEGRAM_IDS"] = "44"
	get, _, err := serviceEnvironment(func(k string) string { return values[k] })
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := activation.ConfigFromEnvironment(get, true, true, true)
	if err != nil {
		t.Fatal(err)
	}
	verification, err := serviceVerificationConfig(context.Background(), get, 9, cfg)
	if err != nil {
		t.Fatal(err)
	}
	p := verification.policy
	if !p.Automatic || p.BotID != 9 || p.ProductKey != "monthly" || p.NativeCurrencyID != "1" || p.NativeCurrencyCode != "USD" || !reflect.DeepEqual(p.ExcludedIDs, []string{"101", "102", "44"}) || !reflect.DeepEqual(p.ExpectedInboundIDs, []int64{7, 9}) {
		t.Fatal("shared source settings or import exclusions changed")
	}
	enforcement, err := lifecycle.EnforcementConfigFromEnvironment(get, true, true, true, true)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := serviceImportedRuntimeConfig(enforcement)
	if err != nil || !runtime.Enabled || !runtime.EffectsEnabled || runtime.SourceScopeHash != p.SourceScopeHash {
		t.Fatal("normal imported providers did not retain the verified source scope")
	}
	enforcement.BaseURL = "https://user:synthetic-secret@billing.invalid"
	if _, err := serviceImportedRuntimeConfig(enforcement); err == nil || strings.Contains(err.Error(), "synthetic-secret") {
		t.Fatal("invalid source accepted or leaked into an error")
	}
	registrationConfig, err := registration.ConfigFromEnvironment(get, true)
	if err != nil || slices.Contains(registrationConfig.ExcludedTelegramIDs, 101) {
		t.Fatal("import exclusions incorrectly excluded administrators from OWN registration")
	}
}

func TestExplicitHealthOnlyEnvironmentForcesAllFeaturesOff(t *testing.T) {
	values := map[string]string{"TELEGRAM_ENABLED": "true", "UNRELATED_SETTING": "preserved"}
	for _, key := range serviceFeatures {
		values[key] = "true"
	}
	get := healthEnvironment(func(k string) string { return values[k] })
	if get("TELEGRAM_ENABLED") != "false" {
		t.Fatal("health-only mode did not force Telegram off")
	}
	for _, key := range serviceFeatures {
		if get(key) != "false" {
			t.Fatalf("health-only mode did not force %s off", key)
		}
	}
	if get("UNRELATED_SETTING") != "preserved" {
		t.Fatal("health-only mode changed an unrelated setting")
	}
	cfg, e := telegram.ConfigFromEnvironment(get)
	if e != nil || cfg.Enabled {
		t.Fatal("health-only configured Telegram")
	}
	r, e := registration.ConfigFromEnvironment(get, false)
	if e != nil || r.Enabled {
		t.Fatal("health-only configured registration")
	}
}

func TestServeHealthRoutesToDatabaseStartupWithoutBusinessCredentials(t *testing.T) {
	t.Setenv("DATABASE_URL", "invalid-database-config")
	if err := run(slog.New(slog.NewTextHandler(io.Discard, nil)), []string{"serve-health"}); err == nil || strings.Contains(err.Error(), "TELEGRAM") {
		t.Fatal("serve-health did not reach safe database configuration validation")
	}
}

func TestNormalServiceDiagnosesTelegramOffBeforeMissingCredentials(t *testing.T) {
	_, _, err := serviceEnvironment(func(key string) string {
		if key == "TELEGRAM_ENABLED" {
			return "false"
		}
		return ""
	})
	if err == nil || !strings.Contains(err.Error(), "TELEGRAM_ENABLED") {
		t.Fatal("legacy Telegram-off setting was not diagnosed before credentials")
	}
}

func TestNormalServiceRejectsLegacyTelegramOffWithCompleteConfiguration(t *testing.T) {
	values := normalServiceValues()
	values["TELEGRAM_ENABLED"] = "false"
	if err := parseNormalService(values); err == nil || !strings.Contains(err.Error(), "TELEGRAM_ENABLED") {
		t.Fatal("legacy Telegram-off configuration was not diagnosed")
	}
}
