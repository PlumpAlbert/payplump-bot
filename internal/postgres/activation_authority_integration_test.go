//go:build integration

package postgres

import (
	"encoding/json"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
)

// Compile-only in the authority rollout task; execution requires a separately
// authorized disposable PostgreSQL fixture and applies migration 028 forward.
func TestAuthorityActivationJournalCompletesWithoutSUIAudit(t *testing.T) {
	s, ctx := activationTestStore(t)
	ids := seedActivationAccounts(t, s, ctx, 123, 31000, 1)
	if n, err := s.seedActivations(ctx, 123, activationAllow(31000, 1), activationTestPolicy, activation.NinjaAuthorityMode); err != nil || n != 1 {
		t.Fatal("authority seed failed")
	}
	op, snapshot := claimOneActivation(t, s, ctx, 123)
	if snapshot.Mode != activation.NinjaAuthorityMode {
		t.Fatal("mode not persisted")
	}
	for _, step := range activation.StepsForMode(snapshot.Mode) {
		payload := activationPayload(t, snapshot, step)
		if granted, err := s.PrepareActivationWrite(ctx, op.Lease, snapshot, step, payload, activationToday); err != nil || !granted {
			t.Fatal("authority write not durably reserved")
		}
		if err := s.ConfirmRecurringActivationStep(ctx, op.Lease, step, "recurring-synthetic", json.Number("125.25"), snapshot.Proof.NextInvoiceDate+"T12:00:00Z"); err != nil {
			t.Fatal(err)
		}
		snapshot = loadActivation(t, s, ctx, op.Lease, 123)
	}
	if err := s.FinishActivation(ctx, op.Lease, snapshot, activationToday, nil); err != nil {
		t.Fatal(err)
	}
	paymentSQL(t, s, ctx, `SELECT o.status='completed' AND f.flow_mode='ninja_authority'
 AND c.sui_id IS NULL AND sub.recurring_id='recurring-synthetic'
 AND sub.state='awaiting_payment' AND NOT sub.desired_access
 AND sub.payment_date='2039-01-31' AND sub.next_invoice_date='2039-02-28'
 AND (SELECT count(*)=2 FROM activation_steps WHERE operation_id=o.id)
 AND NOT EXISTS(SELECT 1 FROM activation_steps WHERE operation_id=o.id AND step IN ('sui_create','sui_enable'))
 AND NOT EXISTS(SELECT 1 FROM activation_sui_bindings WHERE client_id=c.id)
 FROM clients c JOIN subscriptions sub ON sub.client_id=c.id JOIN operations o ON o.client_id=c.id
 JOIN activation_flows f ON f.operation_id=o.id WHERE c.id=$1`, ids[0])
	if n, err := s.seedActivations(ctx, 123, activationAllow(31000, 1), activationTestPolicy, activation.NinjaAuthorityMode); err != nil || n != 0 {
		t.Fatal("completed flow reseeded")
	}
	// Handoff is real observed state, not an invented SUI activation receipt.
	o := authorityObservation(31000)
	if err := s.pool.QueryRow(ctx, `SELECT c.invoice_ninja_id,s.current_invoice_id,s.recurring_id FROM clients c JOIN subscriptions s ON s.client_id=c.id WHERE c.id=$1`, ids[0]).Scan(&o.CustomerID, &o.InvoiceID, &o.RecurringID); err != nil {
		t.Fatal(err)
	}
	o.Binding.CustomerID = o.CustomerID
	if err := s.SaveAuthorityObservation(ctx, 123, o); err != nil {
		t.Fatal(err)
	}
	d := authorityDraft(t, "31000", false)
	r := authorityRecord(t, d, 777)
	o.Binding.SUIID = r.ID()
	o.Binding.Name = r.Name()
	o.Binding.CredentialsHash = r.CredentialsHash()
	o.SUIEnabled = false
	if err := s.SaveAuthorityObservation(ctx, 123, o); err != nil {
		t.Fatal(err)
	}
	paymentSQL(t, s, ctx, `SELECT NOT EXISTS(SELECT 1 FROM outbox WHERE message_key LIKE 'authority-change:%:configuration_ready')`)
	o.SUIEnabled = true
	for i := 0; i < 2; i++ {
		if err := s.SaveAuthorityObservation(ctx, 123, o); err != nil {
			t.Fatal(err)
		}
	}
	for _, enabled := range []bool{false, true} {
		o.Enabled = enabled
		if err := s.SaveAuthorityObservation(ctx, 123, o); err != nil {
			t.Fatal(err)
		}
	}
	paymentSQL(t, s, ctx, `SELECT (SELECT count(*)=1 FROM outbox WHERE message_key LIKE 'authority-change:%:configuration_ready') AND NOT EXISTS(SELECT 1 FROM activation_steps WHERE step IN ('sui_create','sui_enable'))`)
}

func TestAuthorityActivationOnlyEmptyLegacyJournalsTransition(t *testing.T) {
	for _, dispatched := range []bool{false, true} {
		name := "empty"
		if dispatched {
			name = "unknown SUI sender"
		}
		t.Run(name, func(t *testing.T) {
			s, ctx := activationTestStore(t)
			seedActivationAccounts(t, s, ctx, 123, 32000, 1)
			seedActivationBatch(t, s, ctx, 123, activationAllow(32000, 1), 1)
			op, original := claimOneActivation(t, s, ctx, 123)
			if dispatched {
				if granted, err := s.PrepareActivationWrite(ctx, op.Lease, original, activation.SUICreate, activationPayload(t, original, activation.SUICreate), activationToday); err != nil || !granted {
					t.Fatal("fixture did not dispatch old SUI permission")
				}
			}
			policy := activationTestPolicy
			policy[0] ^= 1
			authority := &AuthorityActivationStore{Store: s, policy: policy}
			loaded, err := authority.LoadActivation(ctx, op.Lease, 123)
			if err != nil {
				t.Fatal(err)
			}
			if dispatched {
				if loaded.Mode != "" || loaded.PolicyHash != original.PolicyHash || len(loaded.Steps) != 1 || loaded.Steps[activation.SUICreate].ExternalID != "" || !loaded.WriterBusy {
					t.Fatal("old unknown intent was reinterpreted/released")
				}
			} else if loaded.Mode != activation.NinjaAuthorityMode || loaded.PolicyHash != policy || len(loaded.Steps) != 0 || loaded.Token != original.Token || loaded.Proof.Hash != original.Proof.Hash {
				t.Fatal("empty flow did not preserve original identity and proof")
			}
		})
	}
}
