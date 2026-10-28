package payments

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestShortWebhookSecretStillRequiresExactMatch(t *testing.T) {
	store := &wakeStoreFake{}
	handler, err := NewWebhook("123", store)
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.Activate(42); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "124", "123"} {
		req := webhookRequest(`{}`)
		req.Header.Set(WebhookSecretHeader, value)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		want := http.StatusForbidden
		if value == "123" {
			want = http.StatusOK
		}
		if response.Code != want {
			t.Fatalf("unexpected authentication status: %d", response.Code)
		}
	}
}

func TestWebhookSecretIsUsedAsProvided(t *testing.T) {
	for name, value := range map[string]string{
		"short":   "123",
		"base64":  strings.Repeat("B", 40) + "+/=",
		"long":    strings.Repeat("B", 300),
		"unicode": "секрет",
		"spaces":  " supplied as is ",
	} {
		t.Run(name, func(t *testing.T) {
			paymentEnv(t, true)
			t.Setenv("INVOICE_NINJA_WEBHOOK_SECRET", value)
			cfg, err := ConfigFromEnv(true)
			if err != nil || cfg.WebhookSecret != value {
				t.Fatal("supplied secret rejected or changed")
			}
		})
	}
}

func TestConfigAllowsSharedWebhookAndAPICredentials(t *testing.T) {
	paymentEnv(t, true)
	t.Setenv("INVOICE_NINJA_API_TOKEN", testPaymentSecret)
	t.Setenv("TELEGRAM_BOT_TOKEN", testPaymentSecret)
	t.Setenv("TELEGRAM_WEBHOOK_SECRET", testPaymentSecret)
	if _, err := ConfigFromEnv(true); err != nil {
		t.Fatalf("shared credentials rejected: %v", err)
	}
}
