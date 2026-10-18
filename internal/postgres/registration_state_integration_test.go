//go:build integration

package postgres

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/registration"
)

func TestRegistrationCannotUseGenericCompletion(t *testing.T) {
	s, ctx := testStore(t)
	op := seedRegistrationWork(t, s, ctx, 42)
	if err := s.CompleteOperation(ctx, op.Lease, nil); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatal("generic completion bypassed the registration journal")
	}
	assertRegistrationSQL(t, s, ctx, `SELECT status = 'running' AND completed_at IS NULL FROM operations WHERE id=$1`, op.ID)
}

func TestRegistrationPrepareRechecksConcurrentAccountChanges(t *testing.T) {
	s, ctx := testStore(t)
	op := seedRegistrationWork(t, s, ctx, 42)
	seedRegistrationFlow(t, s, ctx, op.ID)
	if _, err := s.pool.Exec(ctx, `UPDATE subscriptions SET state='active', desired_access=true WHERE client_id=$1`, op.ClientID); err != nil {
		t.Fatal(err)
	}
	granted, err := s.PrepareRegistrationWrite(ctx, op.Lease, registration.CustomerCreate, json.RawMessage(`{"name":"synthetic"}`))
	if granted || !errors.Is(err, queue.ErrConflict) {
		t.Fatal("new external-write permission granted after account state changed")
	}
	assertRegistrationSQL(t, s, ctx, `SELECT count(*)=0 FROM operation_steps WHERE operation_id=$1`, op.ID)
}
