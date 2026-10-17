package telegram

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type webhookStoreFunc func(context.Context, int64, string, Update) error

func (f webhookStoreFunc) ApplyTelegramUpdate(ctx context.Context, id int64, name string, u Update) error {
	return f(ctx, id, name, u)
}

func activeWebhook(t *testing.T, store WebhookStore) *WebhookHandler {
	t.Helper()
	h, err := NewWebhookHandler("test-webhook-secret", store)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Activate(User{ID: 123, IsBot: true, Username: "PayplumpBot"}); err != nil {
		t.Fatal(err)
	}
	return h
}

func webhookRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, WebhookPath, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	r.Header.Set(WebhookSecretHeader, "test-webhook-secret")
	return r
}

func TestWebhookAcknowledgesOnlyAfterStoreSuccess(t *testing.T) {
	for _, fail := range []bool{false, true} {
		called := false
		h := activeWebhook(t, webhookStoreFunc(func(ctx context.Context, id int64, name string, u Update) error {
			called = true
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 5*time.Second {
				t.Error("database deadline missing")
			}
			if id != 123 || name != "PayplumpBot" || u.ID != 4503599627370497 {
				t.Error("update identity changed")
			}
			if fail {
				return errors.New("secret database details")
			}
			return nil
		}))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, webhookRequest(`{"update_id":4503599627370497,"unknown_field":"ignored"}`))
		want := 200
		if fail {
			want = 503
		}
		if !called || w.Code != want || strings.Contains(w.Body.String(), "secret database") {
			t.Fatalf("got %d %q", w.Code, w.Body.String())
		}
	}
}

func TestWebhookRejectsUnauthorizedAndMalformedRequests(t *testing.T) {
	calls := 0
	h := activeWebhook(t, webhookStoreFunc(func(context.Context, int64, string, Update) error { calls++; return nil }))
	for _, tc := range []struct {
		name, body string
		change     func(*http.Request)
		status     int
	}{
		{"missing secret", `{"update_id":1}`, func(r *http.Request) { r.Header.Del(WebhookSecretHeader) }, 403},
		{"wrong secret", `{"update_id":1}`, func(r *http.Request) { r.Header.Set(WebhookSecretHeader, "incorrect") }, 403},
		{"duplicate secret", `{"update_id":1}`, func(r *http.Request) { r.Header.Add(WebhookSecretHeader, "incorrect") }, 403},
		{"method", `{"update_id":1}`, func(r *http.Request) { r.Method = "GET" }, 405},
		{"content type", `{"update_id":1}`, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 415},
		{"missing id", `{}`, nil, 400}, {"null", `null`, nil, 400}, {"array", `[]`, nil, 400},
		{"negative id", `{"update_id":-1}`, nil, 400}, {"fractional id", `{"update_id":1.5}`, nil, 400},
		{"invalid json", `{"update_id":`, nil, 400}, {"trailing json", `{"update_id":1}{}`, nil, 400},
		{"oversized", `{"update_id":1,"padding":"` + strings.Repeat("x", maxWebhookBytes) + `"}`, nil, 413},
		{"oversized trailing whitespace", `{"update_id":1}` + strings.Repeat(" ", maxWebhookBytes), nil, 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := webhookRequest(tc.body)
			if tc.change != nil {
				tc.change(r)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("got %d, want %d", w.Code, tc.status)
			}
		})
	}
	if calls != 0 {
		t.Fatal("rejected request reached the database")
	}
}

func TestWebhookWaitsForActivationAndCancelsStore(t *testing.T) {
	store := webhookStoreFunc(func(ctx context.Context, _ int64, _ string, _ Update) error { <-ctx.Done(); return ctx.Err() })
	h, err := NewWebhookHandler("test-webhook-secret", store)
	if err != nil {
		t.Fatal(err)
	}
	if h.Ready() {
		t.Fatal("unconfigured webhook is ready")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, webhookRequest(`{"update_id":1}`))
	if w.Code != 503 {
		t.Fatal("unconfigured webhook accepted an update")
	}
	if err := h.Activate(User{ID: 123, IsBot: true, Username: "PayplumpBot"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w = httptest.NewRecorder()
	h.ServeHTTP(w, webhookRequest(`{"update_id":1}`).WithContext(ctx))
	if w.Code != 503 {
		t.Fatal("cancelled request was acknowledged")
	}
}

func TestWebhookRequiresValidConfiguration(t *testing.T) {
	if _, err := NewWebhookHandler("", webhookStoreFunc(nil)); err == nil {
		t.Fatal("empty secret accepted")
	}
	if _, err := NewWebhookHandler("secret", nil); err == nil {
		t.Fatal("nil store accepted")
	}
	h, err := NewWebhookHandler("secret", webhookStoreFunc(nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Activate(User{ID: 123, Username: "user"}); err == nil || h.Ready() {
		t.Fatal("non-bot identity activated")
	}
}
