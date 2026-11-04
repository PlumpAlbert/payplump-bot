package invoiceninja

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientsAcceptPositiveHTTPTimeouts(t *testing.T) {
	for _, timeout := range []time.Duration{500 * time.Millisecond, 30 * time.Second, 31 * time.Second, 2 * time.Minute} {
		t.Run(timeout.String(), func(t *testing.T) {
			const base = "https://synthetic.invalid/prefix"
			if err := ValidateClientConfiguration(base, recurringTestToken, timeout); err != nil {
				t.Fatal(err)
			}
			if _, err := New(base, recurringTestToken, timeout); err != nil {
				t.Fatal(err)
			}
			if _, err := NewRecurringClient(base, recurringTestToken, timeout, 1, 1); err != nil {
				t.Fatal(err)
			}
			if _, err := NewRecurringReader(base, recurringTestToken, timeout); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRecurringLongHTTPTimeoutStillHoldsDispatchedWriteOnCancellation(t *testing.T) {
	var posts atomic.Int32
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		started <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	c, err := NewRecurringClient(server.URL, recurringTestToken, 2*time.Minute, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		var result any
		done <- c.post(ctx, []string{"api", "v1", "recurring_invoices"}, struct{}{}, &result)
	}()
	// Cancel only after the server has received the POST: otherwise a slow
	// runner could expire the context before dispatch and miss the unknown path.
	select {
	case <-started:
		cancel()
	case <-time.After(5 * time.Second):
		t.Fatal("synthetic POST did not reach the server")
	}
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("caller cancellation did not stop dispatched write")
	}
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrRecurringUnknown) || posts.Load() != 1 {
		t.Fatalf("dispatched write not held exactly once: posts=%d err=%v", posts.Load(), err)
	}
	if strings.Contains(err.Error(), recurringTestToken) || strings.Contains(err.Error(), server.URL) {
		t.Fatal("cancellation error leaked request details")
	}
}
