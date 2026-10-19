package lifecycle

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type wakeStore struct {
	botID int64
	calls int
	err   error
}

func (s *wakeStore) RecordInvoiceLifecycleWake(_ context.Context, botID int64) error {
	s.botID, s.calls = botID, s.calls+1
	return s.err
}

const testWebhookSecret = "synthetic_lifecycle_webhook_secret_123456"

func TestWebhookIsBoundedAuthenticatedWakeOnly(t *testing.T) {
	store := &wakeStore{}
	handler, err := NewWebhook(testWebhookSecret, store)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	post := func(body string, secret string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, server.URL+WebhookPath, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if secret != "" {
			req.Header.Set(WebhookSecretHeader, secret)
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp := post(`{"event":"untrusted","invoice_id":"synthetic"}`, testWebhookSecret); resp.StatusCode != http.StatusServiceUnavailable {
		resp.Body.Close()
		t.Fatalf("unready status=%d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if err := handler.Activate(71); err != nil {
		t.Fatal(err)
	}
	resp := post(`{"event":"untrusted","invoice_id":"synthetic"}`, testWebhookSecret)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || store.calls != 1 || store.botID != 71 {
		t.Fatalf("wake not recorded: status=%d calls=%d bot=%d", resp.StatusCode, store.calls, store.botID)
	}
	for _, tc := range []struct {
		name, body, secret string
		status             int
	}{
		{"wrong secret", `{}`, "synthetic_wrong_webhook_secret_123456", http.StatusForbidden},
		{"invalid JSON", `{`, testWebhookSecret, http.StatusBadRequest},
		{"not object", `[]`, testWebhookSecret, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := post(tc.body, tc.secret)
			r.Body.Close()
			if r.StatusCode != tc.status {
				t.Fatalf("status=%d want=%d", r.StatusCode, tc.status)
			}
		})
	}
	req, _ := http.NewRequest(http.MethodPost, server.URL+WebhookPath, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set(WebhookSecretHeader, testWebhookSecret)
	resp, err = server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("content type status=%d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodGet, server.URL+WebhookPath, nil)
	resp, err = server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("method status=%d", resp.StatusCode)
	}
	resp = post(strings.Repeat(" ", maxWebhookBytes+1), testWebhookSecret)
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status=%d", resp.StatusCode)
	}
	if store.calls != 1 {
		t.Fatalf("invalid requests caused wakes: %d", store.calls)
	}
}

func TestWebhookStorageFailureRequestsRetryWithoutExposingBody(t *testing.T) {
	store := &wakeStore{err: fmt.Errorf("synthetic database failure")}
	handler, err := NewWebhook(testWebhookSecret, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.Activate(71); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, WebhookPath, strings.NewReader(`{"private_notes":"synthetic-private-marker"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(WebhookSecretHeader, testWebhookSecret)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "synthetic-private-marker") {
		t.Fatalf("unsafe storage failure response: %d %q", response.Code, response.Body.String())
	}
}
