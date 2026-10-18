package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestRunWorkersCancelsAndJoinsEveryWorker(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, 2)
	canceled := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	worker := func(ctx context.Context) error {
		started <- struct{}{}
		<-ctx.Done()
		canceled <- struct{}{}
		<-release
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- runWorkers(ctx, worker, worker) }()
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("worker did not start")
		}
	}
	cancel()
	for range 2 {
		select {
		case <-canceled:
		case <-time.After(time.Second):
			t.Fatal("worker did not cancel")
		}
	}
	select {
	case <-done:
		t.Fatal("returned without joining workers")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("workers did not join")
	}
}

func TestRunWorkersStopsPeersOnUnexpectedExit(t *testing.T) {
	for _, failure := range []error{nil, errors.New("synthetic failure")} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		stopped := make(chan struct{})
		err := runWorkers(ctx, func(context.Context) error { return failure }, func(ctx context.Context) error { <-ctx.Done(); close(stopped); return ctx.Err() })
		cancel()
		if err == nil || (failure != nil && !errors.Is(err, failure)) {
			t.Fatal("unexpected worker exit was ignored")
		}
		select {
		case <-stopped:
		default:
			t.Fatal("peer was not joined")
		}
	}
	if runWorkers(context.Background()) == nil || runWorkers(context.Background(), nil) == nil {
		t.Fatal("invalid worker set accepted")
	}
}
