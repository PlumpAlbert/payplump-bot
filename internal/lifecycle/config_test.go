package lifecycle

import (
	"testing"
)

func clearLifecycleEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"INVOICE_LIFECYCLE_ENABLED", "INVOICE_LIFECYCLE_WEBHOOK_SECRET", "INVOICE_LIFECYCLE_ALLOWED_TELEGRAM_IDS",
		"INVOICE_LIFECYCLE_WORKERS", "INVOICE_LIFECYCLE_POLL_INTERVAL", "INVOICE_LIFECYCLE_RECHECK_INTERVAL",
		"INVOICE_LIFECYCLE_HTTP_TIMEOUT", "INVOICE_LIFECYCLE_MAX_ACCOUNTS", "INTEGRATION_PAGE_SIZE", "INTEGRATION_MAX_PAGES",
		"REGISTRATION_EXCLUDED_TELEGRAM_IDS", "INVOICE_NINJA_BASE_URL", "INVOICE_NINJA_API_TOKEN", "INVOICE_NINJA_VERSION",
	} {
		t.Setenv(key, "")
	}
}

func TestConfigDefaultsOffAndIgnoresInactiveCredentials(t *testing.T) {
	clearLifecycleEnv(t)
	cfg, err := ConfigFromEnv(false, false, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Enabled {
		t.Fatal("invoice lifecycle is not default-off")
	}
}

func TestConfigRequiresExplicitPilotAndSeparateSecret(t *testing.T) {
	clearLifecycleEnv(t)
	t.Setenv("INVOICE_LIFECYCLE_ENABLED", "true")
	t.Setenv("INVOICE_LIFECYCLE_WEBHOOK_SECRET", "synthetic_lifecycle_webhook_secret_123456")
	t.Setenv("INVOICE_LIFECYCLE_ALLOWED_TELEGRAM_IDS", "101")
	t.Setenv("INVOICE_NINJA_BASE_URL", "https://billing.example.test")
	t.Setenv("INVOICE_NINJA_API_TOKEN", "synthetic-api-token")
	cfg, err := ConfigFromEnv(true, true, "synthetic_payment_webhook_secret_123456", "synthetic_telegram_webhook_secret_123456", "synthetic-telegram-token")
	if err != nil || !cfg.Enabled || len(cfg.AllowedTelegramIDs) != 1 || cfg.AllowedTelegramIDs[0] != 101 {
		t.Fatalf("config rejected safe synthetic pilot: %#v %v", cfg, err)
	}
	if _, err := ConfigFromEnv(true, true, cfg.WebhookSecret, "synthetic_telegram_webhook_secret_123456", "synthetic-telegram-token"); err == nil {
		t.Fatal("lifecycle secret was reused for payment webhook")
	}
	if _, err := ConfigFromEnv(true, true, "synthetic_payment_webhook_secret_123456", "synthetic_telegram_webhook_secret_123456", "synthetic-telegram-token"); err != nil {
		t.Fatalf("unexpected config error: %v", err)
	}
}

func TestConfigRejectsUnboundedOrAmbiguousScope(t *testing.T) {
	for _, tc := range []struct{ name, allowlist string }{
		{"empty", ""}, {"duplicate", "101,101"}, {"noncanonical", "0101"}, {"too many", "1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearLifecycleEnv(t)
			t.Setenv("INVOICE_LIFECYCLE_ENABLED", "true")
			t.Setenv("INVOICE_LIFECYCLE_WEBHOOK_SECRET", "synthetic_lifecycle_webhook_secret_123456")
			t.Setenv("INVOICE_LIFECYCLE_ALLOWED_TELEGRAM_IDS", tc.allowlist)
			t.Setenv("INVOICE_NINJA_BASE_URL", "https://billing.example.test")
			t.Setenv("INVOICE_NINJA_API_TOKEN", "synthetic-api-token")
			if _, err := ConfigFromEnv(true, true, "synthetic_payment_webhook_secret_123456", "synthetic_telegram_webhook_secret_123456", "synthetic-telegram-token"); err == nil {
				t.Fatal("invalid lifecycle scope accepted")
			}
		})
	}
}
