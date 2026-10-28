package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/importverify"
)

func TestImportVerifyRejectsBeforeConfigurationOrProviders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, []byte(`{"records":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	called := false
	getenv := func(string) string { t.Fatal("environment read before plan hash validation"); return "" }
	factory := func(verifyConfig) (importverify.Providers, error) {
		called = true
		return importverify.Providers{}, nil
	}
	for _, args := range [][]string{{"--file", path, "--expected-hash", strings.Repeat("0", 64), "--read-only"}, {"--file", path, "--expected-hash", strings.Repeat("0", 64), "--read-only", "--apply"}, {"--file", path, "--expected-hash", strings.Repeat("0", 64), "--read-only=false"}, {"--file", path, "--expected-hash", strings.Repeat("0", 64)}} {
		if err := importVerifyWith(args, &bytes.Buffer{}, getenv, factory, nil); err == nil {
			t.Fatalf("accepted args %q", args)
		}
	}
	if called {
		t.Fatal("provider construction before hash validation")
	}
}

func TestVerifyConfigUsesExplicitCanonicalPolicy(t *testing.T) {
	values := map[string]string{
		"INVOICE_NINJA_BASE_URL": "https://ninja.invalid/instance/", "INVOICE_NINJA_API_TOKEN": "synthetic-ninja-token", "INVOICE_NINJA_VERSION": "v5.13.47", "INVOICE_NINJA_TELEGRAM_ID_FIELD": "id_number",
		"SUI_BASE_URL": "http://panel.invalid/prefix/", "SUI_API_TOKEN": "synthetic-panel-token", "SUI_VERSION": "1.6.4", "SUI_SUBSCRIPTION_BASE_URL": "https://subs.invalid/", "SUI_INBOUND_IDS": "2,1",
		"PAYMENTS_CURRENCY_ID": "7", "IMPORT_VERIFY_BOT_ID": "99", "IMPORT_VERIFY_ALLOWED_TELEGRAM_IDS": "42,7", "IMPORT_VERIFY_EXCLUDED_TELEGRAM_IDS": "42", "REGISTRATION_EXCLUDED_TELEGRAM_IDS": "8", "TELEGRAM_ADMIN_IDS": "9", "IMPORT_VERIFY_CURRENCY_CODE": "USD", "IMPORT_VERIFY_PRODUCT_KEY": "legacy-monthly",
	}
	cfg, err := loadVerifyConfig(func(k string) string { return values[k] })
	if err != nil {
		t.Fatal("synthetic config rejected")
	}
	assertVerifyConfigRedacted(t, cfg, []string{"synthetic-ninja-token", "synthetic-panel-token", "ninja.invalid", "panel.invalid", "subs.invalid", "99", "42", "legacy-monthly"})
	if cfg.policy.BotID != 99 || strings.Join(cfg.policy.PilotIDs, ",") != "42,7" || strings.Join(cfg.policy.ExcludedIDs, ",") != "42,8,9,99" || cfg.policy.MaxRecords != 20 || cfg.policy.MaxInventoryRecords != 1000 {
		t.Fatal("operator policy defaults or exclusions wrong")
	}
	for _, version := range []string{"", "5.13.50", "future-release"} {
		values["INVOICE_NINJA_VERSION"], values["SUI_VERSION"] = version, version
		updated, err := loadVerifyConfig(func(k string) string { return values[k] })
		if err != nil || updated.policy.SourceScopeHash != cfg.policy.SourceScopeHash {
			t.Fatal("release metadata rejected or changed persisted source scope")
		}
	}
}

func assertVerifyConfigRedacted(t *testing.T, cfg verifyConfig, secrets []string) {
	t.Helper()
	formatted := fmt.Sprintf("%v %+v %#v", cfg, cfg, cfg)
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal("redacted configuration JSON failed")
	}
	var logged bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			if attr.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return attr
		},
	}))
	logger.Info("config", "value", cfg)
	for _, output := range []string{formatted, string(encoded), logged.String()} {
		for _, secret := range secrets {
			if strings.Contains(output, secret) {
				t.Fatalf("configuration disclosure: %s", output)
			}
		}
	}
}

func TestPrivateEvidenceIsCreateOnlyAndPrivate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "evidence.json")
	secretID := "synthetic-customer-id"
	item := importverify.PrivateEvidence{CustomerID: secretID}
	result := importverify.Result{PlanHash: strings.Repeat("a", 64), EvidenceHash: strings.Repeat("b", 64)}
	if err := writePrivateEvidence(path, result, []importverify.PrivateEvidence{item}); err != nil {
		t.Fatal("private evidence write failed")
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("evidence file is not mode 0600")
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), secretID) {
		t.Fatal("private JSON evidence missing")
	}
	if err := writePrivateEvidence(path, result, nil); err == nil {
		t.Fatal("existing evidence file overwritten")
	}
	link := filepath.Join(dir, "evidence-link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := writePrivateEvidence(link, result, nil); err == nil {
		t.Fatal("evidence symlink followed")
	}
}

func TestVerifyFactoryCreatesOnlyScopedReadCapabilities(t *testing.T) {
	values := map[string]string{
		"INVOICE_NINJA_BASE_URL": "https://ninja.invalid/", "INVOICE_NINJA_API_TOKEN": "synthetic-ninja-token", "INVOICE_NINJA_VERSION": "5.13.47", "INVOICE_NINJA_TELEGRAM_ID_FIELD": "id_number",
		"SUI_BASE_URL": "http://panel.invalid/", "SUI_API_TOKEN": "synthetic-panel-token", "SUI_VERSION": "1.6.4", "SUI_SUBSCRIPTION_BASE_URL": "https://subs.invalid/", "SUI_INBOUND_IDS": "1",
		"PAYMENTS_CURRENCY_ID": "1", "IMPORT_VERIFY_BOT_ID": "9", "IMPORT_VERIFY_ALLOWED_TELEGRAM_IDS": "42", "IMPORT_VERIFY_CURRENCY_CODE": "USD", "IMPORT_VERIFY_PRODUCT_KEY": "legacy",
	}
	cfg, err := loadVerifyConfig(func(k string) string { return values[k] })
	if err != nil {
		t.Fatal("synthetic config rejected")
	}
	providers, err := newVerifyProviders(cfg)
	if err != nil {
		t.Fatal("read-only provider construction failed")
	}
	for provider, allowed := range map[any][]string{
		providers.Customers: {"FindCustomers", "ListRecurring"}, providers.Recurring: {"Get"}, providers.Invoices: {"GetInvoice", "ListInvoices"},
		providers.Payments: {"GetSettlementCustomer", "GetSettlementInvoice", "ListSettlementPayments"}, providers.Identities: {"ListIdentities"}, providers.Records: {"GetRecord"}, providers.Settings: {"GetSettings"},
	} {
		typeOf := reflect.TypeOf(provider)
		got := make([]string, typeOf.NumMethod())
		for i := range got {
			got[i] = typeOf.Method(i).Name
		}
		sort.Strings(got)
		sort.Strings(allowed)
		if !reflect.DeepEqual(got, allowed) {
			t.Fatalf("unexpected provider capability method set: %v", got)
		}
	}
}

func TestImportVerifyMainDispatchDoesNotReachDatabase(t *testing.T) {
	// A malformed invocation is handled by the command's own parser before normal startup.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := run(logger, []string{"import-verify", "--apply"}); err == nil || strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatal("verification command fell through to service startup")
	}
}
