//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type activationQueryTrace struct {
	start func(context.Context, pgx.TraceQueryStartData)
}

func (t *activationQueryTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	t.start(ctx, data)
	return ctx
}
func (*activationQueryTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestActivationReservationOverridesAsyncPooledCommit(t *testing.T) {
	s, ctx := activationTestStore(t)
	seedActivationAccounts(t, s, ctx, 123, 21000, 1)
	seedActivationBatch(t, s, ctx, 123, activationAllow(21000, 1), 1)
	op, snapshot := claimOneActivation(t, s, ctx, 123)
	var mu sync.Mutex
	var statements []string
	trace := &activationQueryTrace{start: func(_ context.Context, data pgx.TraceQueryStartData) {
		// Never perform a recursive network query from a pgx callback, including
		// at COMMIT. A successful SET LOCAL before reservation pins durability.
		mu.Lock()
		defer mu.Unlock()
		statements = append(statements, strings.TrimSpace(strings.ToLower(data.SQL)))
	}}
	cfg := s.pool.Config()
	cfg.MaxConns, cfg.MinConns = 1, 0
	cfg.ConnConfig.RuntimeParams["synchronous_commit"] = "off"
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	// Neon may ignore startup GUCs, just as it can ignore startup search_path.
	// Establish the session fixture explicitly on this single-connection pool.
	if _, err := pool.Exec(ctx, `SET synchronous_commit = off`); err != nil {
		t.Fatal(err)
	}
	assertAsync := func() {
		t.Helper()
		var setting string
		if err := pool.QueryRow(ctx, `SELECT current_setting('synchronous_commit')`).Scan(&setting); err != nil || setting != "off" {
			t.Fatalf("fixture did not retain session async setting: setting=%s err=%v", setting, err)
		}
	}
	assertAsync()
	payload := activationPayload(t, snapshot, activation.SUICreate)
	if granted, err := (&Store{pool: pool}).PrepareActivationWrite(ctx, op.Lease, snapshot, activation.SUICreate, payload, activationToday); err != nil || !granted {
		t.Fatalf("durable permission granted=%v err=%v", granted, err)
	}
	assertAsync() // SET LOCAL must not silently change the next pooled transaction.
	mu.Lock()
	defer mu.Unlock()
	set, reservation, commit := -1, -1, -1
	for i, sql := range statements {
		switch {
		case sql == "set local synchronous_commit = on":
			set = i
		case strings.HasPrefix(sql, "insert into activation_steps"):
			reservation = i
		case sql == "commit":
			commit = i
		}
	}
	if set < 0 || reservation <= set || commit <= reservation {
		t.Fatal("acknowledged permission without a preceding local durable-commit override")
	}
}

func TestActivationRecurringFactsSurviveClaimsAndConstrainFinish(t *testing.T) {
	s, ctx := activationTestStore(t)
	seedActivationAccounts(t, s, ctx, 123, 22000, 1)
	seedActivationBatch(t, s, ctx, 123, activationAllow(22000, 1), 1)
	op, _ := claimOneActivation(t, s, ctx, 123)
	// A fixture spans several independent receipt checks and a reclaim; claim
	// duration itself is covered without extension in the scoped-claim suite.
	paymentExec(t, s, ctx, `UPDATE operations SET locked_until=clock_timestamp()+interval '4 minutes' WHERE id=$1`, op.ID)
	snapshot := finishActivationSteps(t, s, ctx, op)
	datetime := snapshot.Proof.NextInvoiceDate + "T12:00:00Z"
	for _, step := range []activation.Step{activation.RecurringCreate, activation.RecurringStart} {
		// Exact numeric equivalence and the provider's source-UTC format freeze
		// the same facts; no guessed local total or later GET is involved.
		if err := s.ConfirmRecurringActivationStep(ctx, op.Lease, step, "recurring-synthetic", json.Number("1.2525e2"), snapshot.Proof.NextInvoiceDate+" 12:00:00"); err != nil {
			t.Fatalf("canonical replay: %v", err)
		}
	}
	for _, change := range []struct{ set, undo string }{
		{`recurring_amount=125.26`, `recurring_amount=125.25`},
		{`recurring_datetime='2039-02-28T13:00:00Z'`, `recurring_datetime='2039-02-28T12:00:00Z'`},
	} {
		paymentExec(t, s, ctx, `UPDATE activation_steps SET `+change.set+` WHERE operation_id=$1 AND step='recurring_start'`, op.ID)
		if err := s.FinishActivation(ctx, op.Lease, snapshot, activationToday, nil); !errors.Is(err, queue.ErrConflict) {
			t.Fatalf("finish accepted divergent recurring response facts: %v", err)
		}
		paymentExec(t, s, ctx, `UPDATE activation_steps SET `+change.undo+` WHERE operation_id=$1 AND step='recurring_start'`, op.ID)
	}
	paymentExec(t, s, ctx, `UPDATE activation_steps SET recurring_amount=125.2500 WHERE operation_id=$1 AND step IN ('recurring_create','recurring_start')`, op.ID)
	paymentExec(t, s, ctx, `UPDATE operations SET locked_until=clock_timestamp()-interval '1 second' WHERE id=$1`, op.ID)
	fresh, loaded := claimOneActivation(t, s, ctx, 123)
	for _, step := range activation.OrderedSteps() {
		intent := loaded.Steps[step]
		if intent.DispatchAttempt != op.Attempt {
			t.Fatal("reclaim rewrote the original dispatch generation")
		}
		if step == activation.RecurringCreate || step == activation.RecurringStart {
			if intent.RecurringAmount != "125.25" || intent.RecurringDatetime != datetime {
				t.Fatal("reclaim lost canonical original response facts")
			}
			if err := s.ConfirmRecurringActivationStep(ctx, fresh.Lease, step, intent.ExternalID, json.Number("125.25"), datetime); !errors.Is(err, queue.ErrConflict) {
				t.Fatalf("new attempt confirmed an old sender's receipt: %v", err)
			}
		} else if intent.RecurringAmount != "" || intent.RecurringDatetime != "" {
			t.Fatal("SUI receipt acquired recurring facts")
		}
	}
	if err := s.FinishActivation(ctx, fresh.Lease, loaded, activationToday, nil); err != nil {
		t.Fatal(err)
	}
}

func TestActivationUnknownRecurringCannotBeConfirmedByLookupAfterReclaim(t *testing.T) {
	s, ctx := activationTestStore(t)
	seedActivationAccounts(t, s, ctx, 123, 23000, 1)
	seedActivationBatch(t, s, ctx, 123, activationAllow(23000, 1), 1)
	op, _ := claimOneActivation(t, s, ctx, 123)
	snapshot := advanceActivationSteps(t, s, ctx, op, []activation.Step{activation.SUICreate})
	payload := activationPayload(t, snapshot, activation.RecurringCreate)
	if granted, err := s.PrepareActivationWrite(ctx, op.Lease, snapshot, activation.RecurringCreate, payload, activationToday); err != nil || !granted {
		t.Fatal(err)
	}
	// Database constraints also prohibit inventing facts for unconfirmed/SUI
	// rows, missing facts for confirmed recurring rows, and unbounded schedules.
	for _, sql := range []string{
		`UPDATE activation_steps SET recurring_amount=125.25 WHERE step='recurring_create'`,
		`UPDATE activation_steps SET recurring_datetime='2039-02-28T12:00:00Z' WHERE step='sui_create'`,
		`UPDATE activation_steps SET external_id='invented', confirmed_at=clock_timestamp() WHERE step='recurring_create'`,
		`UPDATE activation_steps SET external_id='invented', confirmed_at=clock_timestamp(), recurring_amount=0, recurring_datetime='2039-02-28T12:00:00Z' WHERE step='recurring_create'`,
		`UPDATE activation_steps SET external_id='invented', confirmed_at=clock_timestamp(), recurring_amount=125.25, recurring_datetime='2039-02-28T12:00:00+00:00' WHERE step='recurring_create'`,
	} {
		if _, err := s.pool.Exec(ctx, sql); err == nil {
			t.Fatal("database accepted invalid recurring response facts")
		}
	}
	paymentExec(t, s, ctx, `UPDATE operations SET locked_until=clock_timestamp()-interval '1 second' WHERE id=$1`, op.ID)
	fresh, _ := claimOneActivation(t, s, ctx, 123)
	for _, attempt := range []struct {
		lease queue.Lease
		want  error
	}{{op.Lease, queue.ErrLeaseLost}, {fresh.Lease, queue.ErrConflict}} {
		err := s.ConfirmRecurringActivationStep(ctx, attempt.lease, activation.RecurringCreate, "lookup-result", json.Number("125.25"), snapshot.Proof.NextInvoiceDate+"T12:00:00Z")
		if !errors.Is(err, attempt.want) {
			t.Fatalf("lookup released unknown recurring barrier: %v", err)
		}
	}
	paymentSQL(t, s, ctx, `SELECT g.operation_id=$1 AND g.step='recurring_create' AND st.external_id IS NULL
        AND st.recurring_amount IS NULL AND st.recurring_datetime IS NULL AND st.dispatch_attempt=$2
        FROM activation_write_guard g JOIN activation_steps st ON st.operation_id=g.operation_id AND st.step=g.step`, op.ID, op.Attempt)
}

func TestActivationPastActualLocalMidnightCannotBeBypassedByLogicalToday(t *testing.T) {
	s, ctx := activationTestStore(t)
	ids := seedActivationAccounts(t, s, ctx, 123, 23500, 1)
	proof := paymentProof(t, payments.Snapshot{InvoiceID: "invoice-23500", CustomerID: "customer-23500"}, "2000-01-31", "125.25")
	paymentExec(t, s, ctx, `UPDATE payment_confirmations SET payment_date=$2, next_invoice_date=$3, proof_hash=$4, evidence=$5::jsonb
        WHERE client_id=$1`, ids[0], proof.PaymentDate, proof.NextInvoiceDate, proof.Hash[:], string(proof.JSON))
	paymentExec(t, s, ctx, `UPDATE subscriptions SET payment_date=$2, next_invoice_date=$3 WHERE client_id=$1`, ids[0], proof.PaymentDate, proof.NextInvoiceDate)
	seedActivationBatch(t, s, ctx, 123, activationAllow(23500, 1), 1)
	op, snapshot := claimOneActivation(t, s, ctx, 123)
	payload := activationPayload(t, snapshot, activation.SUICreate)
	if granted, err := s.PrepareActivationWrite(ctx, op.Lease, snapshot, activation.SUICreate, payload, "2000-02-01"); granted || !errors.Is(err, activation.ErrReviewRequired) {
		t.Fatalf("logical today bypassed actual local midnight: %v", err)
	}
	assertActivationDeadlineRollback(t, s, ctx, op, 0, snapshot.WriteEpoch)
}

func TestActivationPastActualUTCBlocksPrepareAndFinishButRetainsReceipt(t *testing.T) {
	for _, finish := range []bool{false, true} {
		t.Run(map[bool]string{false: "prepare", true: "finish"}[finish], func(t *testing.T) {
			s, ctx := activationTestStore(t)
			seedActivationAccounts(t, s, ctx, 123, 24000, 1)
			seedActivationBatch(t, s, ctx, 123, activationAllow(24000, 1), 1)
			op, _ := claimOneActivation(t, s, ctx, 123)
			var snapshot activation.Snapshot
			if finish {
				snapshot = finishActivationSteps(t, s, ctx, op)
				paymentExec(t, s, ctx, `UPDATE activation_steps SET recurring_datetime='2000-01-01T00:00:00Z'
                    WHERE operation_id=$1 AND step IN ('recurring_create','recurring_start')`, op.ID)
			} else {
				snapshot = advanceActivationSteps(t, s, ctx, op, []activation.Step{activation.SUICreate})
				payload := activationPayload(t, snapshot, activation.RecurringCreate)
				if granted, err := s.PrepareActivationWrite(ctx, op.Lease, snapshot, activation.RecurringCreate, payload, activationToday); err != nil || !granted {
					t.Fatal(err)
				}
				// Recording an original known response is not a new authorization.
				if err := s.ConfirmRecurringActivationStep(ctx, op.Lease, activation.RecurringCreate, "recurring-synthetic", json.Number("125.25"), "2000-01-01 00:00:00"); err != nil {
					t.Fatalf("expired receipt was discarded: %v", err)
				}
			}
			snapshot = loadActivation(t, s, ctx, op.Lease, 123)
			if finish {
				if err := s.FinishActivation(ctx, op.Lease, snapshot, activationToday, nil); !errors.Is(err, activation.ErrReviewRequired) {
					t.Fatalf("past actual UTC schedule authorized finish: %v", err)
				}
			} else {
				payload := activationPayload(t, snapshot, activation.SUIEnable)
				if granted, err := s.PrepareActivationWrite(ctx, op.Lease, snapshot, activation.SUIEnable, payload, activationToday); granted || !errors.Is(err, activation.ErrReviewRequired) {
					t.Fatalf("past actual UTC schedule authorized dispatch: %v", err)
				}
			}
			assertActivationDeadlineRollback(t, s, ctx, op, len(snapshot.Steps), snapshot.WriteEpoch)
		})
	}
}

func assertActivationDeadlineRollback(t *testing.T, s *Store, ctx context.Context, op queue.Operation, steps int, epoch int64) {
	t.Helper()
	paymentSQL(t, s, ctx, `SELECT (SELECT operation_id IS NULL AND epoch=$3 FROM activation_write_guard)
        AND (SELECT count(*)=$2 FROM activation_steps WHERE operation_id=$1)
        AND (SELECT count(*)=0 FROM activation_sui_bindings) AND (SELECT count(*)=0 FROM outbox)
        AND (SELECT count(*)=0 FROM telegram_bots WHERE bot_id=999)
        AND (SELECT sui_id IS NULL FROM clients WHERE id=$4)
        AND (SELECT state='awaiting_payment' AND NOT desired_access AND recurring_id IS NULL FROM subscriptions WHERE client_id=$4)
        AND (SELECT status='running' AND locked_until>clock_timestamp() FROM operations WHERE id=$1)`, op.ID, steps, epoch, op.ClientID)
}

func TestActivationAbsoluteDeadlineRecheckedAfterFinalTransactionWait(t *testing.T) {
	for _, finish := range []bool{false, true} {
		t.Run(map[bool]string{false: "prepare", true: "finish"}[finish], func(t *testing.T) {
			s, ctx := activationTestStore(t)
			ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			defer cancel()
			seedActivationAccounts(t, s, ctx, 123, 25000, 1)
			seedActivationBatch(t, s, ctx, 123, activationAllow(25000, 1), 1)
			op, _ := claimOneActivation(t, s, ctx, 123)
			// Keep unrelated lease age out of the absolute-deadline boundary
			// experiment; its explicit SQL clock guard remains the assertion.
			paymentExec(t, s, ctx, `UPDATE operations SET locked_until=clock_timestamp()+interval '4 minutes' WHERE id=$1`, op.ID)
			steps := activation.OrderedSteps()[:2]
			if finish {
				steps = activation.OrderedSteps()
			}
			advanceActivationSteps(t, s, ctx, op, steps)
			var now time.Time
			if err := s.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
				t.Fatal(err)
			}
			// Leave enough room for remote Neon lock setup before the deliberate
			// boundary wait; the test is still bounded by its three-minute context.
			deadline := now.Add(20 * time.Second).UTC()
			paymentExec(t, s, ctx, `UPDATE activation_steps SET recurring_datetime=$2
                WHERE operation_id=$1 AND step IN ('recurring_create','recurring_start')`, op.ID, deadline.Format(time.RFC3339Nano))
			snapshot := loadActivation(t, s, ctx, op.Lease, 123)
			ready, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			cfg := s.pool.Config()
			cfg.MaxConns, cfg.MinConns = 1, 0
			cfg.ConnConfig.Tracer = &activationQueryTrace{start: func(ctx context.Context, data pgx.TraceQueryStartData) {
				match := strings.Contains(data.SQL, "UPDATE activation_write_guard SET operation_id = $1")
				if finish {
					match = strings.Contains(data.SQL, "INSERT INTO outbox")
				}
				if match {
					once.Do(func() {
						// Both paths have acquired every authorization lock. Finish
						// has also tentatively changed local entitlement and binding.
						close(ready)
						select {
						case <-resume:
						case <-ctx.Done():
						}
					})
				}
			}}
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			// Cancel before pool.Close on any failure while the traced call waits.
			defer cancel()
			done := make(chan error, 1)
			payload := activationPayload(t, snapshot, activation.SUIEnable)
			go func() {
				store := &Store{pool: pool}
				if finish {
					draft := queue.Draft{BotID: 999, ChatID: snapshot.Account.ChatID, Key: "deadline-ready", Text: "Activated"}
					done <- store.FinishActivation(ctx, op.Lease, snapshot, activationToday, []queue.Draft{draft})
				} else {
					granted, err := store.PrepareActivationWrite(ctx, op.Lease, snapshot, activation.SUIEnable, payload, activationToday)
					if granted {
						err = errors.New("expired transaction granted dispatch")
					}
					done <- err
				}
			}()
			select {
			case <-ready:
			case err := <-done:
				t.Fatalf("transaction did not reach final wait: %v", err)
			case <-ctx.Done():
				t.Fatal("transaction did not reach final wait")
			}
			paymentSQL(t, s, ctx, `SELECT clock_timestamp() < $1::timestamptz`, deadline)
			// This is an explicit, bounded clock wait, not sleep/polling for a
			// race. The DB check below verifies the actual deadline was crossed.
			timer := time.NewTimer(time.Until(deadline) + 100*time.Millisecond)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
				t.Fatal("deadline clock wait timed out")
			}
			paymentSQL(t, s, ctx, `SELECT clock_timestamp() >= $1::timestamptz`, deadline)
			close(resume)
			select {
			case err := <-done:
				if !errors.Is(err, activation.ErrReviewRequired) {
					t.Fatalf("deadline crossed during final wait: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("expired transaction did not return")
			}
			assertActivationDeadlineRollback(t, s, ctx, op, len(snapshot.Steps), snapshot.WriteEpoch)
		})
	}
}

func TestActivationCompactSUIEnableJSONBRoundTrip(t *testing.T) {
	s, ctx := activationTestStore(t)
	seedActivationAccounts(t, s, ctx, 123, 26000, 1)
	seedActivationBatch(t, s, ctx, 123, activationAllow(26000, 1), 1)
	op, _ := claimOneActivation(t, s, ctx, 123)
	snapshot := advanceActivationSteps(t, s, ctx, op, activation.OrderedSteps()[:2])
	var before map[string]json.RawMessage
	if err := json.Unmarshal(snapshot.Steps[activation.SUICreate].Payload, &before); err != nil {
		t.Fatal(err)
	}
	before["id"] = json.RawMessage(`77`)
	links := make([]map[string]string, 800)
	for i := range links {
		links[i] = map[string]string{"type": "local", "uri": "synthetic"}
	}
	before["links"], _ = json.Marshal(links)
	raw, _ := json.Marshal(before)
	record, err := sui.ParseClientRecord(raw)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := activation.SUIBinding(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	draft, err := sui.PrepareEnable(record, binding)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(draft)
	if len(payload) < 28000 || len(payload) > 32768 || activation.ValidateIntent(snapshot, activation.SUIEnable, payload) != nil {
		t.Fatal("fixture is not a valid near-bound compact SUI enable intent")
	}
	if granted, err := s.PrepareActivationWrite(ctx, op.Lease, snapshot, activation.SUIEnable, payload, activationToday); err != nil || !granted {
		t.Fatalf("compact SUI before-image rejected: %v", err)
	}
	loaded := loadActivation(t, s, ctx, op.Lease, 123)
	expanded := loaded.Steps[activation.SUIEnable].Payload
	var decoded sui.EnableDraft
	if len(expanded) <= 32768 || activation.DecodeIntent(expanded, &decoded) != nil || activation.ValidateIntent(loaded, activation.SUIEnable, expanded) != nil {
		t.Fatal("JSONB spacing invalidated a valid compact before-image")
	}
	if err := s.ConfirmActivationStep(ctx, op.Lease, activation.SUIEnable, "77"); err != nil {
		t.Fatal(err)
	}
	loaded = advanceActivationSteps(t, s, ctx, op, []activation.Step{activation.RecurringStart})
	if err := s.FinishActivation(ctx, op.Lease, loaded, activationToday, nil); err != nil {
		t.Fatalf("JSONB round-trip prevented finalization: %v", err)
	}
}
