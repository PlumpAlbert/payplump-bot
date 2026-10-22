package lifecycle

import "testing"

func clearEnforcementEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"INVOICE_OVERDUE_ENFORCEMENT_ENABLED", "INVOICE_OVERDUE_EXCLUSIVE_WRITER", "INVOICE_OVERDUE_WORKERS",
		"INVOICE_OVERDUE_MAX_ACCOUNTS", "INVOICE_OVERDUE_POLL_INTERVAL", "INVOICE_OVERDUE_HTTP_TIMEOUT",
		"INVOICE_LIFECYCLE_ALLOWED_TELEGRAM_IDS", "REGISTRATION_EXCLUDED_TELEGRAM_IDS", "INVOICE_NINJA_BASE_URL",
		"INVOICE_NINJA_API_TOKEN", "INVOICE_NINJA_VERSION", "SUI_BASE_URL", "SUI_API_TOKEN", "SUI_VERSION",
	} {
		t.Setenv(key, "")
	}
}

func TestEnforcementConfigDefaultsOff(t *testing.T) {
	clearEnforcementEnv(t)
	cfg, err := EnforcementConfigFromEnv(false, false, false)
	if err != nil || cfg.Enabled {
		t.Fatalf("default-off config rejected or enabled: %#v %v", cfg, err)
	}
}

func TestEnforcementConfigRequiresSeparateExplicitGates(t *testing.T) {
	clearEnforcementEnv(t)
	t.Setenv("INVOICE_OVERDUE_ENFORCEMENT_ENABLED", "true")
	if _, err := EnforcementConfigFromEnv(false, true, true); err == nil {
		t.Fatal("enabled without Stage 1 was accepted")
	}
	t.Setenv("INVOICE_LIFECYCLE_ALLOWED_TELEGRAM_IDS", "101")
	t.Setenv("INVOICE_NINJA_BASE_URL", "https://billing.example.test")
	t.Setenv("INVOICE_NINJA_API_TOKEN", "synthetic-api-token")
	t.Setenv("SUI_BASE_URL", "https://panel.example.test")
	t.Setenv("SUI_API_TOKEN", "synthetic-sui-token")
	t.Setenv("SUI_VERSION", "1.6.4")
	if _, err := EnforcementConfigFromEnv(true, true, true); err == nil {
		t.Fatal("enabled without explicit exclusive-writer acknowledgement was accepted")
	}
	t.Setenv("INVOICE_OVERDUE_EXCLUSIVE_WRITER", "true")
	cfg, err := EnforcementConfigFromEnv(true, true, true)
	if err != nil || !cfg.Enabled || !cfg.ExclusiveWriter || len(cfg.AllowedTelegramIDs) != 1 || cfg.AllowedTelegramIDs[0] != 101 {
		t.Fatalf("synthetic pilot config rejected: %#v %v", cfg, err)
	}
}

func TestEnforcementConfigRejectsEmptyEffectiveAllowlist(t *testing.T) {
	clearEnforcementEnv(t)
	t.Setenv("INVOICE_OVERDUE_ENFORCEMENT_ENABLED", "true")
	t.Setenv("INVOICE_OVERDUE_EXCLUSIVE_WRITER", "true")
	t.Setenv("INVOICE_LIFECYCLE_ALLOWED_TELEGRAM_IDS", "101")
	t.Setenv("REGISTRATION_EXCLUDED_TELEGRAM_IDS", "101")
	t.Setenv("INVOICE_NINJA_BASE_URL", "https://billing.example.test")
	t.Setenv("INVOICE_NINJA_API_TOKEN", "synthetic-api-token")
	t.Setenv("SUI_BASE_URL", "https://panel.example.test")
	t.Setenv("SUI_API_TOKEN", "synthetic-sui-token")
	t.Setenv("SUI_VERSION", "1.6.4")
	if _, err := EnforcementConfigFromEnv(true, true, true); err == nil {
		t.Fatal("fully excluded pilot allowlist was accepted")
	}
}
