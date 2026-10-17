package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHealthcheck(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
		err := healthcheck(srv.URL)
		srv.Close()
		if (err == nil) != (status == http.StatusOK) {
			t.Fatalf("status %d: %v", status, err)
		}
		if err := healthcheck(srv.URL); err == nil {
			t.Fatal("stopped service reported healthy")
		}
	}
}

func TestServiceCancelsWebhookSetupOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	done := make(chan error, 1)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() {
		done <- serve(ctx, logger, &http.Server{Addr: "127.0.0.1:0"}, func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return nil
		}, nil)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("webhook setup did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("service did not stop webhook setup")
	}
}

func TestServiceStopsOnFatalWebhookSetupError(t *testing.T) {
	want := errors.New("unauthorized")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := serve(context.Background(), logger, &http.Server{Addr: "127.0.0.1:0"}, func(context.Context) error { return want }, nil)
	if !errors.Is(err, want) {
		t.Fatalf("fatal webhook setup error lost: %v", err)
	}
}

func TestServiceDoesNotStartWebhookSetupWhenListenFails(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	called := false
	err := serve(context.Background(), logger, &http.Server{Addr: "invalid address"}, func(context.Context) error { called = true; return nil }, nil)
	if err == nil || called {
		t.Fatal("webhook setup started without HTTP listener")
	}
}

func TestServiceKeepsServingAfterWebhookSetup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	setup := make(chan struct{})
	done := make(chan error, 1)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() {
		done <- serve(ctx, logger, &http.Server{Addr: "127.0.0.1:0"}, func(context.Context) error { close(setup); return nil }, nil)
	}()
	select {
	case <-setup:
	case <-time.After(time.Second):
		t.Fatal("setup did not complete")
	}
	select {
	case err := <-done:
		t.Fatalf("service stopped after successful setup: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("service did not stop")
	}
}
