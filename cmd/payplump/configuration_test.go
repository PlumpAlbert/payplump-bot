package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/clientmenu"
	"github.com/PlumpAlbert/payplump-bot/internal/configdelivery"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
)

type startupFenceFake struct {
	calls int
	err   error
}

func (f *startupFenceFake) DisableConfigurationPolicies(context.Context) error {
	f.calls++
	return f.err
}
func TestDisabledDeliveryRevokesPoliciesBeforeReadinessWithoutTelegram(t *testing.T) {
	for _, telegramEnabled := range []bool{false, true} {
		fake := &startupFenceFake{}
		cfg := configdelivery.Config{TelegramEnabled: telegramEnabled}
		if err := fenceConfigurationStartup(context.Background(), fake, cfg); err != nil || fake.calls != 1 {
			t.Fatal("disabled startup did not revoke persisted policies")
		}
	}
	fake := &startupFenceFake{err: errors.New("synthetic database failure")}
	if err := fenceConfigurationStartup(context.Background(), fake, configdelivery.Config{}); !errors.Is(err, fake.err) {
		t.Fatal("failed startup fence was ignored")
	}
	fake = &startupFenceFake{}
	if err := fenceConfigurationStartup(context.Background(), fake, configdelivery.Config{Enabled: true}); err != nil || fake.calls != 0 {
		t.Fatal("enabled policy was revoked before verified bot setup")
	}
}
func configurationRuntimeConfig() configdelivery.Config {
	return configdelivery.Config{Enabled: true, TelegramEnabled: true, PaymentsEnabled: true, Revision: 1, BaseURL: "https://billing.example.test/", Token: "synthetic-ninja-token", CurrencyID: "1", TelegramIDField: "id_number", SUIBaseURL: "http://panel.example.test/private/", SUIToken: "synthetic-panel-token", SUIVersion: "1.6.4", SubscriptionBaseURL: "https://subscription.example.test/prefix/", AllowedTelegramIDs: []int64{42}, HTTPTimeout: 3 * time.Second, PageSize: 50, MaxPages: 20}
}
func TestConfigurationRuntimeConstructsOnlyGETCapabilities(t *testing.T) {
	p, err := newConfigurationProviders(configdelivery.Config{})
	if err != nil || p.Payments != nil || p.SUI != nil || p.Recurring != nil {
		t.Fatal("disabled delivery constructed integrations")
	}
	p, err = newConfigurationProviders(configurationRuntimeConfig())
	if err != nil || p.Payments == nil || p.SUI == nil || p.Recurring == nil {
		t.Fatal("valid read-only providers unavailable")
	}
	for _, capability := range []any{p.SUI, p.Recurring} {
		typ := reflect.TypeOf(capability)
		for i := 0; i < typ.NumMethod(); i++ {
			name := typ.Method(i).Name
			if name != "Get" && name != "GetRecord" {
				t.Fatal("delivery factory exposed an external writer or inventory capability")
			}
		}
	}
	invalid := configurationRuntimeConfig()
	invalid.CurrencyID = ""
	if _, err := newConfigurationProviders(invalid); err == nil {
		t.Fatal("invalid delivery policy accepted")
	}
}
func TestConfigurationMenuRuntimePinsPolicyWithoutProviderReads(t *testing.T) {
	for _, key := range []string{"INVOICE_NINJA_BASE_URL", "INVOICE_NINJA_API_TOKEN", "REGISTRATION_EXCLUDED_TELEGRAM_IDS", "PAYMENT_PHONE", "PAYMENT_BANK", "PAYMENT_RECIPIENT"} {
		t.Setenv(key, "")
	}
	cfg := configurationRuntimeConfig()
	store := &menuStoreFake{snapshot: clientmenu.Snapshot{Known: true, ClientID: 1, State: "active", DesiredAccess: true, SUIID: "17", RecurringID: "recurring-synthetic"}}
	gateway, err := newMenuGatewayWithConfiguration(store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.ApplyTelegramUpdate(context.Background(), 77, "SyntheticBot", menuUpdate(9001, "/config")); err != nil {
		t.Fatal(err)
	}
	if !store.last.ProtectedConfiguration || store.last.Text != queue.ConfigurationIntentBody || store.last.ConfigurationPolicyRevision != cfg.Revision || store.last.ConfigurationPolicyHash != cfg.Policy().Hash {
		t.Fatal("runtime menu lost protected policy intent")
	}
}
