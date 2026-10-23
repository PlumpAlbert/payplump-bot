//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/broadcast"
)

func createCleanupTestJob(t *testing.T, s *Store, ctx context.Context, bot int64, p broadcast.Policy, updateID int64, body string) int64 {
	t.Helper()
	preview := broadcastTestAction(t, s, ctx, bot, p, updateID, "/broadcast_text "+body)
	textToken := broadcastResponseToken(t, preview, "Токен: ")
	frozen := broadcastTestAction(t, s, ctx, bot, p, updateID+1, "/broadcast_all "+textToken)
	confirmToken := broadcastResponseToken(t, frozen, "/broadcast_confirm ")
	broadcastTestAction(t, s, ctx, bot, p, updateID+2, "/broadcast_confirm "+confirmToken)
	var id int64
	if err := s.pool.QueryRow(ctx, `SELECT id FROM broadcasts WHERE bot_id=$1 ORDER BY id DESC LIMIT 1`, bot).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestBroadcastStalePolicyCleanupIsBoundedAndMonotonic(t *testing.T) {
	s, _ := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	const bot = int64(550099)
	const batch = 2
	oldPolicy := broadcastTestPolicy()
	if err := s.InstallBroadcastPolicy(ctx, bot, oldPolicy); err != nil {
		t.Fatal(err)
	}
	broadcastTestMembers(t, s, ctx, bot, 5)
	var oldJobs []int64
	for i := range 3 {
		oldJobs = append(oldJobs, createCleanupTestJob(t, s, ctx, bot, oldPolicy, int64(100+i*3), "old policy body"))
	}
	currentPolicy := oldPolicy
	currentPolicy.Revision++
	if err := s.InstallBroadcastPolicy(ctx, bot, currentPolicy); err != nil {
		t.Fatal(err)
	}
	currentJob := createCleanupTestJob(t, s, ctx, bot, currentPolicy, 200, "current policy body")

	for poll := range 20 {
		var before, cancelledBefore int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM broadcast_deliveries d JOIN broadcasts b ON b.id=d.job_id WHERE b.bot_id=$1 AND b.policy_revision<>$2 AND d.status='pending'`, bot, currentPolicy.Revision).Scan(&before); err != nil {
			t.Fatal(err)
		}
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM broadcasts WHERE id=ANY($1::bigint[]) AND state='cancelled'`, oldJobs).Scan(&cancelledBefore); err != nil {
			t.Fatal(err)
		}
		progress, err := s.ProcessBroadcastBatch(ctx, bot, currentPolicy, batch)
		if err != nil {
			t.Fatalf("cleanup poll %d: %v", poll, err)
		}
		if progress.Skipped > batch || progress.Enqueued > batch {
			t.Fatalf("poll exceeded bounded work: %+v", progress)
		}
		var after int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM broadcast_deliveries d JOIN broadcasts b ON b.id=d.job_id WHERE b.bot_id=$1 AND b.policy_revision<>$2 AND d.status='pending'`, bot, currentPolicy.Revision).Scan(&after); err != nil {
			t.Fatal(err)
		}
		var cancelledAfter int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM broadcasts WHERE id=ANY($1::bigint[]) AND state='cancelled'`, oldJobs).Scan(&cancelledAfter); err != nil {
			t.Fatal(err)
		}
		if cancelledAfter-cancelledBefore > batch {
			t.Fatalf("poll cancelled unbounded jobs: before=%d after=%d", cancelledBefore, cancelledAfter)
		}
		if before-after != progress.Skipped {
			t.Fatalf("cleanup did not report monotonic bounded progress: before=%d after=%d progress=%+v", before, after, progress)
		}
		if after > 0 && progress.Enqueued != 0 {
			t.Fatalf("new-policy fanout ran before stale deliveries drained: remaining=%d progress=%+v", after, progress)
		}
		if after == 0 {
			var uncancelled int
			if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM broadcasts WHERE id=ANY($1::bigint[]) AND state<>'cancelled'`, oldJobs).Scan(&uncancelled); err != nil {
				t.Fatal(err)
			}
			if uncancelled == 0 {
				break
			}
			// Cancellation has its own bounded phase; it must not skip pending
			// deliveries or prevent the current-policy job from being reached.
		}
		if poll == 19 {
			t.Fatal("stale cleanup did not drain within the bounded poll sequence")
		}
	}
	var staleState string
	if err := s.pool.QueryRow(ctx, `SELECT bool_and(b.state='cancelled')::text FROM broadcasts b WHERE b.id=ANY($1::bigint[])`, oldJobs).Scan(&staleState); err != nil || staleState != "true" {
		t.Fatalf("stale jobs were not cancelled after their deliveries: state=%s err=%v", staleState, err)
	}
	for _, job := range oldJobs {
		var pending, enqueued, skipped int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE status='pending'),count(*) FILTER(WHERE status='enqueued'),count(*) FILTER(WHERE status='skipped') FROM broadcast_deliveries WHERE job_id=$1`, job).Scan(&pending, &enqueued, &skipped); err != nil {
			t.Fatal(err)
		}
		if pending != 0 || enqueued != 0 || skipped != 5 {
			t.Fatalf("old job %d escaped cleanup: pending=%d enqueued=%d skipped=%d", job, pending, enqueued, skipped)
		}
	}
	progress, err := s.ProcessBroadcastBatch(ctx, bot, currentPolicy, batch)
	if err != nil || progress.Enqueued != batch {
		t.Fatalf("current-policy job became unreachable after cleanup: progress=%+v err=%v", progress, err)
	}
	var enqueued int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM broadcast_deliveries WHERE job_id=$1 AND status='enqueued'`, currentJob).Scan(&enqueued); err != nil || enqueued != batch {
		t.Fatalf("current job fanout count=%d err=%v", enqueued, err)
	}
}
