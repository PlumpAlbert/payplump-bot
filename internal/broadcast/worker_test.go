package broadcast

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type workerStore struct {
	mu                       sync.Mutex
	calls, active, maxActive int
	cancelObserved           chan struct{}
	fail                     bool
}

func (s *workerStore) ProcessBroadcastBatch(ctx context.Context, _ int64, _ Policy, _ int) (Progress, error) {
	s.mu.Lock()
	s.calls++
	s.active++
	if s.active > s.maxActive {
		s.maxActive = s.active
	}
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.active--; s.mu.Unlock() }()
	if s.fail {
		return Progress{}, context.DeadlineExceeded
	}
	if s.cancelObserved != nil {
		<-ctx.Done()
		close(s.cancelObserved)
		return Progress{}, ctx.Err()
	}
	return Progress{Enqueued: 1}, nil
}
func workerConfig(n int) Config {
	return Config{Enabled: true, Revision: 1, MaxRecipients: 1000, BatchSize: 1, Workers: n, PollInterval: 100 * time.Millisecond, AdminIDs: map[int64]struct{}{7: {}}}
}

func TestWorkerPollsWithinConcurrencyAndJoinsCancellation(t *testing.T) {
	store := &workerStore{}
	var logs bytes.Buffer
	w, err := New(store, 123, workerConfig(2), slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	deadline := time.After(time.Second)
	for {
		store.mu.Lock()
		calls := store.calls
		active := store.active
		store.mu.Unlock()
		if calls >= 2 && active == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("worker did not poll batches")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not join")
	}
	store.mu.Lock()
	max := store.maxActive
	store.mu.Unlock()
	if max > 2 {
		t.Fatalf("active calls %d", max)
	}
	dec := json.NewDecoder(strings.NewReader(logs.String()))
	for {
		var record map[string]any
		err := dec.Decode(&record)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal("invalid worker log")
		}
		for key := range record {
			switch key {
			case "time", "level", "msg", "enqueued", "skipped", "completed":
			default:
				t.Fatal("worker log contains a non-technical attribute")
			}
		}
	}
}
func TestWorkerDBErrorAndCancellationJoin(t *testing.T) {
	failed := &workerStore{fail: true}
	w, err := New(failed, 123, workerConfig(1), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	deadline := time.After(time.Second)
	for {
		failed.mu.Lock()
		n := failed.calls
		failed.mu.Unlock()
		if n > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("no batch attempted")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
	blocking := &workerStore{cancelObserved: make(chan struct{})}
	w, err = New(blocking, 123, workerConfig(1), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	done = make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	deadline = time.After(time.Second)
	for {
		blocking.mu.Lock()
		n := blocking.calls
		blocking.mu.Unlock()
		if n > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("no blocking batch")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("active DB call not joined")
	}
}
func TestWorkerRejectsDisabledOrInvalidConfig(t *testing.T) {
	if _, err := New(&workerStore{}, 123, Config{}, nil); err == nil {
		t.Fatal("disabled worker accepted")
	}
	if _, err := New(&workerStore{}, 0, workerConfig(1), nil); err == nil {
		t.Fatal("invalid bot accepted")
	}
}
