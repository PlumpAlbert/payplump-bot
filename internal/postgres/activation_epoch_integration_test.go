//go:build integration

package postgres

import (
	"errors"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
)

func TestActivationEpochRejectsStaleInventoryAfterWriterReleasesGuard(t *testing.T) {
	s, ctx := activationTestStore(t)
	seedActivationAccounts(t, s, ctx, 123, 19000, 2)
	seedActivationBatch(t, s, ctx, 123, activationAllow(19000, 2), 2)
	a, beforeA := claimOneActivation(t, s, ctx, 123)
	b, beforeB := claimOneActivation(t, s, ctx, 123)
	if beforeA.WriteEpoch != beforeB.WriteEpoch || beforeA.WriterBusy || beforeB.WriterBusy {
		t.Fatal("preflight snapshots did not capture the same idle epoch")
	}
	payloadA := activationPayload(t, beforeA, activation.SUICreate)
	payloadB := activationPayload(t, beforeB, activation.SUICreate)
	if granted, err := s.PrepareActivationWrite(ctx, a.Lease, beforeA, activation.SUICreate, payloadA, activationToday); err != nil || !granted {
		t.Fatalf("first dispatch: permit=%v err=%v", granted, err)
	}
	busyB := loadActivation(t, s, ctx, b.Lease, 123)
	if !busyB.WriterBusy || busyB.WriteEpoch != beforeB.WriteEpoch+1 {
		t.Fatal("reservation did not atomically advance the visible busy epoch")
	}
	if err := s.ConfirmActivationStep(ctx, a.Lease, activation.SUICreate, "77"); err != nil {
		t.Fatal(err)
	}
	paymentSQL(t, s, ctx, `SELECT operation_id IS NULL AND step IS NULL AND epoch=$1 FROM activation_write_guard`, beforeB.WriteEpoch+1)
	// B's provider inventory could predate A's actual POST. A released guard
	// alone is insufficient: the dispatch epoch must invalidate B's preflight.
	for _, stale := range []activation.Snapshot{beforeB, busyB} {
		if granted, err := s.PrepareActivationWrite(ctx, b.Lease, stale, activation.SUICreate, payloadB, activationToday); granted || !errors.Is(err, activation.ErrGuardBusy) {
			t.Fatalf("stale/busy preflight granted=%v err=%v", granted, err)
		}
	}
	paymentSQL(t, s, ctx, `SELECT count(*)=0 FROM activation_steps WHERE operation_id=$1`, b.ID)
	freshB := loadActivation(t, s, ctx, b.Lease, 123)
	if freshB.WriterBusy || freshB.WriteEpoch != beforeB.WriteEpoch+1 {
		t.Fatal("confirmation reset the epoch or left the barrier occupied")
	}
	if granted, err := s.PrepareActivationWrite(ctx, b.Lease, freshB, activation.SUICreate, payloadB, activationToday); err != nil || !granted {
		t.Fatalf("fresh preflight granted=%v err=%v", granted, err)
	}
	if err := s.ConfirmActivationStep(ctx, b.Lease, activation.SUICreate, "88"); err != nil {
		t.Fatal(err)
	}
	paymentSQL(t, s, ctx, `SELECT operation_id IS NULL AND epoch=$1 FROM activation_write_guard`, beforeB.WriteEpoch+2)
}
