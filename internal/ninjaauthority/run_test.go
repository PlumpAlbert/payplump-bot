package ninjaauthority

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type wakeStore struct {
	*memoryStore
	wakeMu            sync.Mutex
	generation        WakeGeneration
	fail              bool
	reads             int
	started           chan struct{}
	release           chan struct{}
	active, maxActive int
}

func newWakeStore() *wakeStore {
	return &wakeStore{memoryStore: newMemoryStore(), started: make(chan struct{}, 10), release: make(chan struct{}, 10)}
}
func (s *wakeStore) LoadAuthorityWake(ctx context.Context, _ int64) (WakeGeneration, error) {
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	s.reads++
	if _, ok := ctx.Deadline(); !ok {
		return WakeGeneration{}, errors.New("missing bounded read context")
	}
	if s.fail {
		return WakeGeneration{}, errors.New("synthetic failure")
	}
	return s.generation, nil
}
func (s *wakeStore) WithAuthorityScan(ctx context.Context, _ int64, _ func(context.Context) error) error {
	s.wakeMu.Lock()
	s.active++
	if s.active > s.maxActive {
		s.maxActive = s.active
	}
	s.wakeMu.Unlock()
	defer func() { s.wakeMu.Lock(); s.active--; s.wakeMu.Unlock() }()
	s.started <- struct{}{}
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *wakeStore) wake(invoices, payments int64) {
	s.wakeMu.Lock()
	s.generation = WakeGeneration{invoices, payments}
	s.wakeMu.Unlock()
}
func awaitScan(t *testing.T, st *wakeStore) {
	t.Helper()
	select {
	case <-st.started:
	case <-time.After(time.Second):
		t.Fatal("scan did not start promptly")
	}
}
func assertNoScan(t *testing.T, st *wakeStore) {
	t.Helper()
	select {
	case <-st.started:
		t.Fatal("unexpected duplicate/overlapping scan")
	case <-time.After(40 * time.Millisecond):
	}
}

func TestRunCoalescesWakesAndPreservesEventsDuringScan(t *testing.T) {
	s, _, _, _ := fixture(t, "2", false)
	st := newWakeStore()
	s.store = st
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.run(ctx, time.Hour, 5*time.Millisecond) }()
	awaitScan(t, st)
	// Both generations advance during startup. Neither may be marked observed
	// using a post-scan read; they must cause the second scan.
	st.wake(2, 3)
	assertNoScan(t, st)
	st.release <- struct{}{}
	awaitScan(t, st)
	st.wake(4, 3) // an invoice event while the wake-triggered scan is running
	assertNoScan(t, st)
	st.release <- struct{}{}
	awaitScan(t, st)
	st.release <- struct{}{}
	assertNoScan(t, st)
	st.wake(4, 4) // payment-only wake
	awaitScan(t, st)
	st.release <- struct{}{}
	assertNoScan(t, st)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("run ignored cancellation")
	}
	st.wakeMu.Lock()
	defer st.wakeMu.Unlock()
	if st.maxActive != 1 {
		t.Fatal("overlapping scans")
	}
}

func TestRunFallbackOnWakeReadFailureAndCancelScan(t *testing.T) {
	s, _, _, _ := fixture(t, "2", false)
	st := newWakeStore()
	st.fail = true
	s.store = st
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.run(ctx, 60*time.Millisecond, 5*time.Millisecond) }()
	awaitScan(t, st)
	st.release <- struct{}{}
	awaitScan(t, st) // timer fallback despite repeated wake read failures
	cancel()         // cancellation while scan is still running
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scan ignored cancellation")
	}
	st.wakeMu.Lock()
	defer st.wakeMu.Unlock()
	if st.reads > 30 || st.maxActive != 1 {
		t.Fatal("busy retry or overlapping scans")
	}
}
