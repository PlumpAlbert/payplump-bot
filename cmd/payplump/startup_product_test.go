package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
)

type startupProductScript struct {
	errors []error
	calls  int
	cancel context.CancelFunc
}

func (s *startupProductScript) ProductKey(context.Context, string) (string, error) {
	s.calls++
	if s.cancel != nil {
		s.cancel()
	}
	if s.calls <= len(s.errors) {
		return "", s.errors[s.calls-1]
	}
	return "monthly", nil
}

func TestServiceVerificationRecoversFromTransientProductRead(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/products/product-a" {
			t.Error("startup retried something other than the configured read-only product lookup")
		}
		if calls.Add(1) == 1 {
			http.Error(w, "private provider error", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"data":{"id":"product-a","product_key":"monthly","is_deleted":false,"archived_at":0}}`)
	}))
	defer server.Close()
	values := normalServiceValues()
	values["INVOICE_NINJA_BASE_URL"] = server.URL
	get, _, err := serviceEnvironment(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := activation.ConfigFromEnvironment(get, true, true, true)
	if err != nil {
		t.Fatal(err)
	}
	verification, err := serviceVerificationConfig(context.Background(), get, 42, cfg)
	if err != nil || verification.policy.ProductKey != "monthly" || calls.Load() != 2 {
		t.Fatalf("startup read did not recover: calls=%d err=%v", calls.Load(), err)
	}
}

func TestStartupProductRetriesOnlyTransientReadFailures(t *testing.T) {
	for _, err := range []error{context.DeadlineExceeded, &integrationhttp.HTTPError{StatusCode: 503}} {
		reader := &startupProductScript{errors: []error{err}}
		key, got := startupProductKey(context.Background(), reader, "synthetic-product")
		if got != nil || key != "monthly" || reader.calls != 2 {
			t.Fatalf("transient read did not recover: calls=%d err=%v", reader.calls, got)
		}
	}
}

func TestStartupProductDoesNotRetryPermanentFailures(t *testing.T) {
	for _, err := range []error{
		&integrationhttp.HTTPError{StatusCode: 401}, &integrationhttp.HTTPError{StatusCode: 403},
		&integrationhttp.HTTPError{StatusCode: 404}, integrationhttp.ErrInvalidResponse,
		errors.New("invalid product data"), context.Canceled,
	} {
		reader := &startupProductScript{errors: []error{err}}
		key, got := startupProductKey(context.Background(), reader, "synthetic-product")
		if !errors.Is(got, err) || key != "" || reader.calls != 1 {
			t.Fatalf("permanent failure retried or lost: calls=%d err=%v", reader.calls, got)
		}
	}
}

func TestStartupProductRetriesAreBounded(t *testing.T) {
	reader := &startupProductScript{errors: []error{context.DeadlineExceeded, context.DeadlineExceeded, context.DeadlineExceeded}}
	_, err := startupProductKey(context.Background(), reader, "synthetic-product")
	if !errors.Is(err, context.DeadlineExceeded) || reader.calls != 3 {
		t.Fatalf("retry bound lost: calls=%d err=%v", reader.calls, err)
	}
}

func TestStartupProductCancellationStopsBeforeAnotherRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := &startupProductScript{errors: []error{context.DeadlineExceeded}, cancel: cancel}
	_, err := startupProductKey(ctx, reader, "synthetic-product")
	if !errors.Is(err, context.Canceled) || reader.calls != 1 {
		t.Fatalf("cancellation did not stop retry: calls=%d err=%v", reader.calls, err)
	}
	_, err = startupProductKey(ctx, reader, "synthetic-product")
	if !errors.Is(err, context.Canceled) || reader.calls != 1 {
		t.Fatal("request made on canceled startup")
	}
}

func TestTransientStartupReadClassification(t *testing.T) {
	for _, tc := range []struct {
		err   error
		retry bool
	}{
		{context.DeadlineExceeded, true},
		{integrationhttp.ErrConnection, true},
		{&integrationhttp.HTTPError{StatusCode: 408}, true},
		{&integrationhttp.HTTPError{StatusCode: 429}, true},
		{&integrationhttp.HTTPError{StatusCode: 500}, true},
		{&integrationhttp.HTTPError{StatusCode: 504}, true},
		{&integrationhttp.HTTPError{StatusCode: 400}, false},
		{&integrationhttp.HTTPError{StatusCode: 404}, false},
		{integrationhttp.ErrInvalidResponse, false},
	} {
		if transientStartupRead(fmt.Errorf("wrapped: %w", tc.err)) != tc.retry {
			t.Fatalf("incorrect classification for %T", tc.err)
		}
	}
}
