package integrationhttp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTransportPreservesConfiguredHTTPTimeout(t *testing.T) {
	for _, timeout := range []time.Duration{500 * time.Millisecond, 30 * time.Second, 31 * time.Second, 2 * time.Minute} {
		t.Run(timeout.String(), func(t *testing.T) {
			c, err := New("https://synthetic.invalid", nil, timeout)
			if err != nil {
				t.Fatal(err)
			}
			if c.http.Timeout != timeout {
				t.Fatalf("timeout=%v want=%v", c.http.Timeout, timeout)
			}
			start := time.Now()
			calls := 0
			c.http.Transport = putRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				deadline, ok := r.Context().Deadline()
				// net/http must pass the configured timeout to the request, not clamp it.
				if !ok || deadline.Before(start.Add(timeout)) || deadline.After(time.Now().Add(timeout)) {
					t.Errorf("transport deadline=%v does not preserve timeout=%v", deadline, timeout)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
			})
			var result any
			if _, err := c.Get(context.Background(), []string{"read"}, nil, &result); err != nil || calls != 1 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestShortContextOverridesLongHTTPTimeoutWithoutWriteReplay(t *testing.T) {
	for _, write := range []bool{false, true} {
		name := "get"
		if write {
			name = "write"
		}
		t.Run(name, func(t *testing.T) {
			writer, err := NewWriter("https://synthetic.invalid", http.Header{"Token": {"synthetic-private-token"}}, 2*time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			var calls atomic.Int32
			release := make(chan struct{})
			defer close(release)
			writer.client.http.Transport = putRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				got, ok := r.Context().Deadline()
				want, _ := ctx.Deadline()
				if !ok || !got.Equal(want) {
					t.Error("caller deadline was replaced by HTTP timeout")
				}
				if write && r.GetBody != nil {
					t.Error("write body is replayable")
				}
				select {
				case <-r.Context().Done():
					return nil, r.Context().Err()
				case <-release:
					return nil, context.Canceled
				}
			})
			done := make(chan error, 1)
			go func() {
				var result any
				var err error
				if write {
					_, err = writer.Post(ctx, []string{"record"}, nil, struct{}{}, &result)
				} else {
					_, err = writer.client.Get(ctx, []string{"read"}, nil, &result)
				}
				done <- err
			}()
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
					t.Fatalf("calls=%d err=%v", calls.Load(), err)
				}
				if strings.Contains(err.Error(), "synthetic") {
					t.Fatal("deadline error leaked request details")
				}
			case <-time.After(time.Second):
				t.Fatal("short caller deadline did not stop request")
			}
		})
	}
}
