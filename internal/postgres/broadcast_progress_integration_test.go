//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/broadcast"
)

func TestBroadcastPolicyStartupDisableAndWorkerFence(t *testing.T) {
	s, ctx := testStore(t)
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	const botID = 912345
	if err := s.InstallBroadcastPolicy(ctx, botID, broadcast.Policy{}); err != nil {
		t.Fatal("install disabled startup policy")
	}
	cfg := broadcast.Config{Enabled: true, Revision: 1, MaxRecipients: 10, BatchSize: 50, Workers: 1, PollInterval: time.Second, AdminIDs: map[int64]struct{}{7: {}}}
	p := cfg.Policy()
	if err := s.InstallBroadcastPolicy(ctx, botID, p); err != nil {
		t.Fatal("install enabled policy")
	}
	if err := s.InstallBroadcastPolicy(ctx, botID, p); err != nil {
		t.Fatal("exact policy reinstall must be idempotent")
	}
	conflict := p
	conflict.AdminIDs = []int64{8}
	if err := s.InstallBroadcastPolicy(ctx, botID, conflict); !errors.Is(err, broadcast.ErrStalePolicy) {
		t.Fatalf("conflicting equal revision: %v", err)
	}
	if err := s.InstallBroadcastPolicy(ctx, botID, broadcast.Policy{}); err != nil {
		t.Fatal("disable policy")
	}
	progress, err := s.ProcessBroadcastBatch(ctx, botID, p, 50)
	if !errors.Is(err, broadcast.ErrStalePolicy) || progress != (broadcast.Progress{}) {
		t.Fatalf("stale worker policy processed: progress=%+v err=%v", progress, err)
	}
	var enabled bool
	var revision int64
	if err := s.pool.QueryRow(ctx, `SELECT enabled,revision FROM broadcast_policies WHERE bot_id=$1`, botID).Scan(&enabled, &revision); err != nil {
		t.Fatal(err)
	}
	if enabled || revision != 2 {
		t.Fatalf("unexpected disabled policy state: enabled=%v revision=%d", enabled, revision)
	}
	var jobs, deliveries int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM broadcasts WHERE bot_id=$1`, botID).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM broadcast_deliveries d JOIN broadcasts b ON b.id=d.job_id WHERE b.bot_id=$1`, botID).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if jobs != 0 || deliveries != 0 {
		t.Fatalf("disabled/stale worker created jobs=%d deliveries=%d", jobs, deliveries)
	}
}
