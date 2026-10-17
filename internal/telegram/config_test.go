package telegram

import (
	"strings"
	"testing"
)

func TestConfigOptInAndAdmins(t *testing.T) {
	t.Setenv("TELEGRAM_ENABLED", "")
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-only-token")
	cfg, err := ConfigFromEnv()
	if err != nil || cfg.Enabled || cfg.Token != "" || cfg.IsAdmin(42) {
		t.Fatal("Telegram must be disabled by default")
	}
	t.Setenv("TELEGRAM_ENABLED", "true")
	t.Setenv("TELEGRAM_ADMIN_IDS", "42, 4503599627370497,42")
	t.Setenv("TELEGRAM_WEBHOOK_URL", "https://bot.example.com/telegram/webhook")
	t.Setenv("TELEGRAM_WEBHOOK_SECRET", "test-webhook-secret")
	cfg, err = ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled || !cfg.IsAdmin(42) || !cfg.IsAdmin(4503599627370497) || cfg.IsAdmin(7) || len(cfg.AdminIDs) != 2 {
		t.Fatal("admin whitelist not preserved")
	}
}

func TestConfigRejectsInvalidValuesWithoutLeakingToken(t *testing.T) {
	t.Setenv("TELEGRAM_WEBHOOK_URL", "https://bot.example.com/telegram/webhook")
	t.Setenv("TELEGRAM_WEBHOOK_SECRET", "test-webhook-secret")
	for _, tc := range []struct{ enabled, token, admins string }{
		{"not-bool", "test-only-token", "42"}, {"true", "", "42"},
		{"true", " test-only-token ", "42"}, {"true", "test-only-token", ""},
		{"true", "test-only-token", "-1"}, {"true", "test-only-token", "0"},
		{"true", "test-only-token", "42,"}, {"true", "test-only-token", "9223372036854775808"},
	} {
		t.Setenv("TELEGRAM_ENABLED", tc.enabled)
		t.Setenv("TELEGRAM_BOT_TOKEN", tc.token)
		t.Setenv("TELEGRAM_ADMIN_IDS", tc.admins)
		_, err := ConfigFromEnv()
		if err == nil || strings.Contains(err.Error(), "test-only-token") {
			t.Fatal("expected a sanitized configuration error")
		}
	}
}

func TestWebhookConfigValidation(t *testing.T) {
	t.Setenv("TELEGRAM_ENABLED", "true")
	t.Setenv("TELEGRAM_BOT_TOKEN", "test-only-token")
	t.Setenv("TELEGRAM_ADMIN_IDS", "42")
	for _, tc := range []struct{ url, secret string }{
		{"", "test-webhook-secret"},
		{"http://bot.example.com/telegram/webhook", "test-webhook-secret"},
		{"https://bot.example.com/wrong", "test-webhook-secret"},
		{"https://user:private@bot.example.com/telegram/webhook", "test-webhook-secret"},
		{"https://bot.example.com/telegram/webhook?secret=private", "test-webhook-secret"},
		{"https://bot.example.com/telegram/webhook#private", "test-webhook-secret"},
		{"https://bot.example.com/telegram/webhook", ""},
		{"https://bot.example.com/telegram/webhook", "bad private secret"},
		{"https://bot.example.com/telegram/webhook", strings.Repeat("a", 257)},
	} {
		t.Setenv("TELEGRAM_WEBHOOK_URL", tc.url)
		t.Setenv("TELEGRAM_WEBHOOK_SECRET", tc.secret)
		if _, err := ConfigFromEnv(); err == nil || strings.Contains(err.Error(), "private") {
			t.Fatal("expected sanitized webhook configuration error")
		}
	}
}
