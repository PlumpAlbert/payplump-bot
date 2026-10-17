package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := NewClient("test-only-token")
	c.baseURL = srv.URL
	return c
}

func TestClientRequestsAndResults(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("unexpected request")
		}
		switch r.URL.Path {
		case "/bottest-only-token/getMe":
			fmt.Fprint(w, `{"ok":true,"result":{"id":123,"is_bot":true,"username":"PayplumpBot"}}`)
		case "/bottest-only-token/setWebhook":
			var data struct {
				URL         string   `json:"url"`
				Secret      string   `json:"secret_token"`
				Allowed     []string `json:"allowed_updates"`
				Connections int      `json:"max_connections"`
				Drop        *bool    `json:"drop_pending_updates"`
			}
			if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
				t.Error(err)
			}
			if data.URL != "https://bot.example.com/telegram/webhook" || data.Secret != "test-webhook-secret" || len(data.Allowed) != 1 || data.Allowed[0] != "message" || data.Connections != 1 || data.Drop == nil || *data.Drop {
				t.Error("wrong webhook registration parameters")
			}
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		default:
			t.Error("unexpected method")
			w.WriteHeader(404)
		}
	})
	me, err := c.GetMe(context.Background())
	if err != nil || me.ID != 123 {
		t.Fatalf("getMe: %v", err)
	}
	if err := c.SetWebhook(context.Background(), "https://bot.example.com/telegram/webhook", "test-webhook-secret"); err != nil {
		t.Fatal(err)
	}
}

func TestClientErrorsAndRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		status, code int
		retry        int
		permanent    bool
	}{
		{200, 429, 17, false}, {429, 429, 5, false}, {401, 401, 0, true}, {409, 409, 0, true}, {500, 500, 0, false},
	} {
		t.Run(fmt.Sprint(tc.status, "/", tc.code), func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprintf(w, `{"ok":false,"error_code":%d,"description":"test-only-token private data","parameters":{"retry_after":%d}}`, tc.code, tc.retry)
			})
			err := c.SetWebhook(context.Background(), "https://bot.example.com/telegram/webhook", "test-webhook-secret")
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.RetryAfter != time.Duration(tc.retry)*time.Second || apiErr.Permanent() != tc.permanent {
				t.Fatalf("unexpected API error: %v", err)
			}
			if strings.Contains(err.Error(), "test-only-token") || strings.Contains(err.Error(), "private data") {
				t.Fatal("sensitive response leaked")
			}
		})
	}
}

func TestClientRejectsInvalidWebhookResults(t *testing.T) {
	for _, body := range []string{
		`{}`, `{"ok":true}`, `{"ok":true,"result":null}`, `{"ok":true,"result":{}}`,
		`{"ok":true,"result":false}`, `{"ok":true,"result":[]}`,
		`{"ok":false,"error_code":429,"parameters":{"retry_after":9223372036854775807}}`,
		strings.Repeat("x", maxResponseBytes+1),
	} {
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
		if err := c.SetWebhook(context.Background(), "https://bot.example.com/telegram/webhook", "test-webhook-secret"); err == nil {
			t.Fatal("invalid response accepted")
		}
	}
}

func TestClientCancellationAndTransportRedaction(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	t.Cleanup(func() { close(release) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		err := c.SetWebhook(ctx, "https://bot.example.com/telegram/webhook", "test-webhook-secret")
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("webhook setup request did not cancel")
	}
	c.baseURL = "http://127.0.0.1:0"
	_, err := c.GetMe(context.Background())
	if err == nil || strings.Contains(err.Error(), "test-only-token") {
		t.Fatal("transport failure must redact URL")
	}
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/unexpected", http.StatusTemporaryRedirect)
	})
	if _, err := c.GetMe(context.Background()); err == nil || strings.Contains(err.Error(), "test-only-token") {
		t.Fatal("redirect accepted or token leaked")
	}
}
