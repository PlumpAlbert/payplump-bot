package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/queue"
)

func TestQueueBoundsRejectBeforeDatabaseAccess(t *testing.T) {
	s := &Store{}
	for _, tc := range []struct {
		limit int
		lease time.Duration
	}{
		{0, time.Minute}, {101, time.Minute}, {1, 999 * time.Millisecond}, {1, 6 * time.Minute},
	} {
		if _, err := s.ClaimOutbox(context.Background(), 123, tc.limit, tc.lease); err == nil {
			t.Fatal("invalid queue limits accepted")
		}
	}
	if _, err := s.ClaimOutbox(context.Background(), 0, 1, time.Minute); err == nil {
		t.Fatal("missing bot identity accepted")
	}
	if err := s.RetryOutbox(context.Background(), queue.Lease{}, -time.Second, 0); err == nil {
		t.Fatal("negative retry accepted")
	}
	if err := s.CompleteOutbox(context.Background(), queue.Lease{}, 0); err == nil {
		t.Fatal("invalid sent identity accepted")
	}
	if _, err := s.ClaimOperations(context.Background(), make([]string, 33), 1, time.Minute); err == nil {
		t.Fatal("unbounded operation kinds accepted")
	}
	if err := s.CompleteOperation(context.Background(), queue.Lease{}, make([]queue.Draft, 101)); err == nil {
		t.Fatal("unbounded notification batch accepted")
	}
}
