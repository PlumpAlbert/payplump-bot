package integrationhttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type putRoundTripFunc func(*http.Request) (*http.Response, error)

func (f putRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPutIsExplicitBoundedAndNonReplayable(t *testing.T) {
	writer, err := NewWriter("https://synthetic.example.test/prefix", http.Header{"X-API-TOKEN": {"synthetic-token"}}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	payload := map[string]string{"next_send_date": "2040-02-29"}
	calls := 0
	writer.client.http.Transport = putRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		payload["next_send_date"] = "changed-after-encoding"
		if r.Method != http.MethodPut || r.URL.Path != "/prefix/recurring_invoices/synthetic-id" || r.GetBody != nil || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("PUT lost explicit non-replayable wire contract")
		}
		raw, _ := io.ReadAll(r.Body)
		if string(raw) != `{"next_send_date":"2040-02-29"}` {
			t.Error("PUT did not freeze the payload before networking")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
	})
	var result map[string]bool
	if _, err := writer.Put(context.Background(), []string{"recurring_invoices", "synthetic-id"}, nil, payload, &result); err != nil || !result["ok"] || calls != 1 {
		t.Fatal("PUT was not exactly one transport invocation")
	}
	if _, err := writer.Put(context.Background(), nil, nil, struct{}{}, nil); err == nil || calls != 1 {
		t.Fatal("nil response permitted a PUT")
	}
	if _, err := writer.Put(context.Background(), nil, nil, make(chan int), &result); err == nil || calls != 1 {
		t.Fatal("invalid JSON payload permitted a PUT")
	}
}
func TestPutNeverRedirectsOrReplaysUnknownResponse(t *testing.T) {
	for _, status := range []int{200, 307, 308, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("PUT followed redirect") }))
			defer target.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPut {
					t.Error("PUT changed method")
				}
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Location", target.URL)
				w.WriteHeader(status)
				fmt.Fprint(w, `{"secret":"synthetic-private-body"`)
			}))
			defer server.Close()
			writer, err := NewWriter(server.URL, http.Header{"Token": {"synthetic-private-token"}}, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			var result any
			_, err = writer.Put(context.Background(), []string{"record"}, nil, struct{}{}, &result)
			if err == nil || calls.Load() != 1 || strings.Contains(err.Error(), server.URL) || strings.Contains(err.Error(), "synthetic-private") {
				t.Fatal("unknown PUT replayed, redirected or leaked diagnostics")
			}
		})
	}
}
func TestPutCancellationDoesNotReplayAcceptedWrite(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-release
	}))
	defer server.Close()
	defer close(release)
	writer, err := NewWriter(server.URL, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		var result any
		_, err := writer.Put(ctx, []string{"record"}, nil, struct{}{}, &result)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("PUT did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
			t.Fatal("canceled PUT was repeated")
		}
	case <-time.After(time.Second):
		t.Fatal("PUT did not cancel")
	}
}
