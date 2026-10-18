package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/payments"
)

func TestPaymentWebhookRouteIsExplicitlyOptional(t *testing.T) {
	calls := 0
	endpoint := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(202) })
	for _, enabled := range []bool{false, true} {
		var hook http.Handler
		if enabled {
			hook = endpoint
		}
		server := New("", func(context.Context) error { return nil }, nil, hook)
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, payments.WebhookPath, nil))
		want := 404
		if enabled {
			want = 202
		}
		if response.Code != want {
			t.Fatalf("enabled=%v status=%d", enabled, response.Code)
		}
		response = httptest.NewRecorder()
		server.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, payments.WebhookPath, nil))
		if response.Code != 404 && response.Code != 405 {
			t.Fatal("GET reached payment receiver")
		}
	}
	if calls != 1 {
		t.Fatal("disabled or GET request invoked payment receiver")
	}
}
