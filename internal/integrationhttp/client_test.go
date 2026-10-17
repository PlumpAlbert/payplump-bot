package integrationhttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetPreservesPrefixEscapesQueryAndNumbers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Token") != "synthetic-token" || r.Header.Get("Accept") != "application/json" {
			t.Error("unexpected request method or headers")
		}
		if r.URL.EscapedPath() != "/panel/prefix/api/products/item%3Fkey=value" || r.URL.Query().Get("filter") != "a&b" {
			t.Error("path prefix or escaping was lost")
		}
		w.Header().Set("X-App-Version", "5.13.47")
		fmt.Fprint(w, `{"id":9007199254740993,"amount":123456789.123456789}`)
	}))
	defer server.Close()
	headers := http.Header{"Token": {"synthetic-token"}}
	c, err := New(server.URL+"/panel/prefix/", headers, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	headers.Set("Token", "changed-after-construction")
	var result map[string]any
	response, err := c.Get(context.Background(), []string{"api", "products", "item?key=value"}, url.Values{"filter": {"a&b"}}, &result)
	if err != nil {
		t.Fatal(err)
	}
	if result["id"].(json.Number).String() != "9007199254740993" || result["amount"].(json.Number).String() != "123456789.123456789" || response.Get("X-App-Version") != "5.13.47" {
		t.Fatal("numeric precision or response metadata lost")
	}
}

func TestGetPreservesEncodedTrailingPrefixSlash(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/panel%2F/api/clients" {
			t.Error("encoded prefix slash was lost")
		}
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	c, err := New(server.URL+"/panel%2F/", nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var result any
	if _, err := c.Get(context.Background(), []string{"api", "clients"}, nil, &result); err != nil {
		t.Fatal(err)
	}
}

func TestConstructorRejectsUnsafeConfigurationWithoutEcho(t *testing.T) {
	for _, raw := range []string{
		"", "ftp://example.com", "https://user:private-token@example.com", "https://example.com?private-token=value",
		"https://example.com#private-token", "https://example.com/a/../b", "https://example.com/%2e%2e/b", " https://example.com",
	} {
		if _, err := New(raw, nil, time.Second); err == nil || strings.Contains(err.Error(), "private-token") {
			t.Fatal("unsafe URL accepted or leaked into diagnostics")
		}
	}
	for _, timeout := range []time.Duration{0, -time.Second, 31 * time.Second} {
		if _, err := New("https://example.com", nil, timeout); err == nil {
			t.Fatal("invalid timeout accepted")
		}
	}
	if _, err := New("http://example.com", http.Header{"Token": {"private-token\nnext-header"}}, time.Second); err == nil || strings.Contains(err.Error(), "private-token") {
		t.Fatal("invalid header accepted or leaked")
	}
}

func TestGetRejectsUnsafeSegmentsBeforeRequest(t *testing.T) {
	c, err := New("https://example.invalid", nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, segment := range []string{"", ".", "..", "a/b", "a\\b", "a\x00b"} {
		var result any
		if _, err := c.Get(context.Background(), []string{segment}, nil, &result); err == nil {
			t.Fatal("unsafe segment accepted")
		}
	}
}

func TestGetNeverFollowsTokenBearingRedirect(t *testing.T) {
	var forwarded atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { forwarded.Store(true) }))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, http.StatusFound) }))
	defer server.Close()
	c, err := New(server.URL, http.Header{"Token": {"private-token"}}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var result any
	_, err = c.Get(context.Background(), []string{"clients"}, nil, &result)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusFound || forwarded.Load() || strings.Contains(err.Error(), "private-token") || strings.Contains(err.Error(), server.URL) {
		t.Fatal("redirect was followed or sensitive request details leaked")
	}
}

func TestGetRejectsMalformedLargeAndErrorResponses(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
	}{
		{"bad JSON", 200, "private-body"},
		{"trailing JSON", 200, `{} {"private-body":true}`},
		{"too large", 200, strings.Repeat("x", MaxResponseBytes+1)},
		{"HTTP failure", 403, `{"error":"private-token private-body"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.code); fmt.Fprint(w, tc.body) }))
			defer server.Close()
			c, err := New(server.URL, nil, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			var result any
			_, err = c.Get(context.Background(), []string{"read"}, nil, &result)
			if err == nil || strings.Contains(err.Error(), "private-body") || strings.Contains(err.Error(), "private-token") || strings.Contains(err.Error(), server.URL) {
				t.Fatal("invalid response accepted or leaked")
			}
		})
	}
}

func TestGetCancellationAndTimeoutAreRedacted(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprint(timeout), func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer server.Close()
			defer close(release)
			duration := time.Second
			if timeout {
				duration = 100 * time.Millisecond
			}
			c, err := New(server.URL, http.Header{"Token": {"private-token"}}, duration)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { var result any; _, err := c.Get(ctx, []string{"clients"}, nil, &result); done <- err }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("request did not start")
			}
			if !timeout {
				cancel()
			}
			select {
			case err := <-done:
				want := context.Canceled
				if timeout {
					want = context.DeadlineExceeded
				}
				if !errors.Is(err, want) || strings.Contains(err.Error(), "private-token") || strings.Contains(err.Error(), server.URL) {
					t.Fatalf("cancellation/deadline lost or sensitive URL leaked: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("request did not stop")
			}
		})
	}
}
