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
		case "/bottest-only-token/getUpdates":
			var data struct {
				Offset         int64 `json:"offset"`
				Timeout, Limit int
				Allowed        []string `json:"allowed_updates"`
			}
			if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
				t.Error(err)
			}
			if data.Offset != 37 || data.Timeout != 30 || data.Limit != 100 || len(data.Allowed) != 1 || data.Allowed[0] != "message" {
				t.Error("wrong polling parameters")
			}
			fmt.Fprint(w, `{"ok":true,"result":[{"update_id":37,"message":{"from":{"id":42,"first_name":"Client"},"chat":{"id":42,"type":"private"},"text":"/start","entities":[{"type":"bot_command","offset":0,"length":6}]}},{"update_id":38,"callback_query":{}}]}`)
		default:
			t.Error("unexpected method")
			w.WriteHeader(404)
		}
	})
	me, err := c.GetMe(context.Background())
	if err != nil || me.ID != 123 {
		t.Fatalf("getMe: %v", err)
	}
	updates, err := c.GetUpdates(context.Background(), 37)
	if err != nil || len(updates) != 2 {
		t.Fatalf("getUpdates: %v", err)
	}
	if _, ok := updates[0].PrivateStart(me.Username); !ok || updates[1].Message != nil {
		t.Fatal("update parsing failed")
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
			_, err := c.GetUpdates(context.Background(), 0)
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

func TestClientRejectsInvalidUpdates(t *testing.T) {
	for _, body := range []string{
		`{}`, `{"ok":true}`, `{"ok":true,"result":null}`, `{"ok":true,"result":{}}`,
		`{"ok":true,"result":[{}]}`, `{"ok":true,"result":[{"update_id":-1}]}`,
		`{"ok":true,"result":[{"update_id":9223372036854775807}]}`,
		`{"ok":true,"result":[{"update_id":2},{"update_id":1}]}`,
		`{"ok":true,"result":[{"update_id":1},{"update_id":1}]}`,
		`{"ok":false,"error_code":429,"parameters":{"retry_after":9223372036854775807}}`,
		strings.Repeat("x", maxResponseBytes+1),
	} {
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
		if _, err := c.GetUpdates(context.Background(), 0); err == nil {
			t.Fatal("invalid response accepted")
		}
	}
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"ok":true,"result":[]}`) })
	if updates, err := c.GetUpdates(context.Background(), 0); err != nil || len(updates) != 0 {
		t.Fatal("empty batch rejected")
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
	go func() { _, err := c.GetUpdates(ctx, 0); done <- err }()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("polling did not cancel")
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
