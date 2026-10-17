package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"
)

func TestServiceStartsOutboxAfterSetupAndJoinsIt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	setupStarted := make(chan struct{})
	releaseSetup := make(chan struct{})
	workerStarted := make(chan struct{})
	workerCanceled := make(chan struct{})
	releaseWorker := make(chan struct{})
	defer func() {
		select {
		case <-releaseWorker:
		default:
			close(releaseWorker)
		}
	}()
	done := make(chan error, 1)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() {
		done <- serve(ctx, logger, &http.Server{Addr: "127.0.0.1:0"}, func(ctx context.Context) error {
			close(setupStarted)
			select {
			case <-releaseSetup:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}, func(ctx context.Context) error {
			close(workerStarted)
			<-ctx.Done()
			close(workerCanceled)
			<-releaseWorker // Simulate bounded persistence of a known delivery.
			return nil
		})
	}()
	select {
	case <-setupStarted:
	case <-time.After(time.Second):
		t.Fatal("setup did not start")
	}
	select {
	case <-workerStarted:
		t.Fatal("outbox started without verified bot identity")
	default:
	}
	close(releaseSetup)
	select {
	case <-workerStarted:
	case <-time.After(time.Second):
		t.Fatal("outbox did not start after setup")
	}
	cancel()
	select {
	case <-workerCanceled:
	case <-time.After(time.Second):
		t.Fatal("outbox was not canceled")
	}
	select {
	case err := <-done:
		t.Fatalf("service returned before joining outbox: %v", err)
	default:
	}
	close(releaseWorker)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("service did not join outbox")
	}
}

func TestServiceNeverStartsOutboxOnSetupFailure(t *testing.T) {
	want := errors.New("setup rejected")
	called := false
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := serve(context.Background(), logger, &http.Server{Addr: "127.0.0.1:0"},
		func(context.Context) error { return want },
		func(context.Context) error { called = true; return nil })
	if !errors.Is(err, want) || called {
		t.Fatal("outbox ran after failed setup or setup error was lost")
	}
}

func TestServiceStopsIfOutboxExitsUnexpectedly(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, result := range []error{nil, errors.New("worker failed")} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := serve(ctx, logger, &http.Server{Addr: "127.0.0.1:0"}, nil,
			func(context.Context) error { return result })
		cancel()
		if err == nil || (result != nil && !errors.Is(err, result)) {
			t.Fatalf("unexpected outbox exit ignored: %v", err)
		}
	}
}
