package lifecycle

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestShortWebhookSecretStillRequiresExactMatch(t *testing.T) {
	store := &wakeStore{}
	handler, err := NewWebhook("123", store)
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.Activate(42); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "124", "123"} {
		req := httptest.NewRequest(http.MethodPost, WebhookPath, strings.NewReader(`{}`))
		req.Header.Set(WebhookSecretHeader, value)
		req.Header.Set("Content-Type", "application/json")
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
			env := map[string]string{
				"INVOICE_LIFECYCLE_ENABLED":        "true",
				"INVOICE_NINJA_BASE_URL":           "https://billing.example.test",
				"INVOICE_NINJA_API_TOKEN":          "synthetic-api-token",
				"INVOICE_LIFECYCLE_WEBHOOK_SECRET": value,
			}
			cfg, err := ConfigFromEnvironment(func(key string) string { return env[key] }, true, true, value, value, value, true)
			if err != nil || cfg.WebhookSecret != value {
				t.Fatal("supplied secret rejected or changed")
			}
			env["INVOICE_NINJA_API_TOKEN"] = strings.Repeat("B", 40)
			env["INVOICE_LIFECYCLE_WEBHOOK_SECRET"] = env["INVOICE_NINJA_API_TOKEN"]
			if _, err := ConfigFromEnvironment(func(key string) string { return env[key] }, true, true, value, value, value, true); err != nil {
				t.Fatalf("shared API credential rejected: %v", err)
			}
		})
	}
}
