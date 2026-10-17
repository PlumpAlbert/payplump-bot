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

func TestServiceCancelsPollingOnShutdown(t *testing.T) {
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
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("service did not stop polling")
	}
}

func TestServiceStopsOnFatalPollingError(t *testing.T) {
	want := errors.New("unauthorized")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := serve(context.Background(), logger, &http.Server{Addr: "127.0.0.1:0"}, func(context.Context) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("fatal polling error lost: %v", err)
	}
}

func TestServiceDoesNotStartPollingWhenListenFails(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	called := false
	err := serve(context.Background(), logger, &http.Server{Addr: "invalid address"}, func(context.Context) error { called = true; return nil })
	if err == nil || called {
		t.Fatal("poller started without HTTP listener")
	}
}
