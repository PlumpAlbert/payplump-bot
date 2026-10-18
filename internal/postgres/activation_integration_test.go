//go:build integration

package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const activationToday = "2039-02-01"

var activationTestPolicy = sha256.Sum256([]byte("synthetic activation policy"))

// Reuse the isolated-schema fixture and the completed OWN registration fixture.
// No .env, live provider, credentials, or shared production schema is consulted.
func activationTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	if os.Getenv("TEST_DATABASE_URL") == "" && (os.Getenv("PGHOST") == "" || os.Getenv("PGUSER") == "" || os.Getenv("PGDATABASE") == "") {
		t.Skip("requires disposable TEST_DATABASE_URL or PGHOST/PGUSER/PGDATABASE")
	}
	return testStore(t)
}

func seedActivationAccounts(t *testing.T, s *Store, ctx context.Context, botID int64, start, count int) []int64 {
	t.Helper()
	ids := seedPaymentAccounts(t, s, ctx, botID, start, count)
	for remaining := count; remaining > 0; {
		n := min(50, remaining)
		seedPaymentChecks(t, s, ctx, botID, n)
		remaining -= n
	}
	hashes, evidence := make([][]byte, count), make([]string, count)
	for i := range count {
		proof := paymentProof(t, payments.Snapshot{InvoiceID: fmt.Sprintf("invoice-%d", start+i), CustomerID: fmt.Sprintf("customer-%d", start+i)}, "2039-01-31", "125.25")
		hashes[i], evidence[i] = proof.Hash[:], string(proof.JSON)
	}
	paymentExec(t, s, ctx, `WITH facts AS (
        SELECT * FROM unnest($1::bigint[], $2::bytea[], $3::text[]) AS f(client_id, hash, evidence)
    ), confirmed AS (
        INSERT INTO payment_confirmations (invoice_id, client_id, bot_id, customer_id, invoice_number,
            amount, currency_id, payment_date, next_invoice_date, proof_hash, evidence)
        SELECT s.current_invoice_id, c.id, $4, c.invoice_ninja_id, 'SYNTHETIC', 125.25, '1',
            '2039-01-31', '2039-02-28', f.hash, f.evidence::jsonb
        FROM facts f JOIN clients c ON c.id=f.client_id JOIN subscriptions s ON s.client_id=c.id
        RETURNING client_id, payment_date, next_invoice_date
    ) UPDATE subscriptions s SET payment_date=pc.payment_date, next_invoice_date=pc.next_invoice_date
        FROM confirmed pc WHERE s.client_id=pc.client_id`, ids, hashes, evidence, botID)
	return ids
}

func activationAllow(start, count int) []int64 {
	ids := make([]int64, count)
	for i := range count {
		ids[i] = int64(start + i)
	}
	return ids
}

func seedActivationBatch(t *testing.T, s *Store, ctx context.Context, botID int64, allowed []int64, want int) {
	t.Helper()
	if n, err := s.SeedActivations(ctx, botID, allowed, activationTestPolicy); err != nil || n != want {
		t.Fatalf("activation seed count=%d want=%d err=%v", n, want, err)
	}
}

func claimOneActivation(t *testing.T, s *Store, ctx context.Context, botID int64) (queue.Operation, activation.Snapshot) {
	t.Helper()
	claims, err := s.ClaimActivations(ctx, botID, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("activation claim count=%d err=%v", len(claims), err)
	}
	snapshot := loadActivation(t, s, ctx, claims[0].Lease, botID)
	return claims[0], snapshot
}

func loadActivation(t *testing.T, s *Store, ctx context.Context, lease queue.Lease, botID int64) activation.Snapshot {
	t.Helper()
	snapshot, err := s.LoadActivation(ctx, lease, botID)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func activationPayload(t *testing.T, snapshot activation.Snapshot, step activation.Step) json.RawMessage {
	t.Helper()
	var value any
	switch step {
	case activation.SUICreate:
		draft, err := sui.NewDisabledDraft([]int64{1, 2})
		if err != nil {
			t.Fatal(err)
		}
		value = draft
	case activation.RecurringCreate:
		product := invoiceninja.Product{ID: "synthetic-product", ProductKey: "monthly", Notes: "Synthetic monthly service", Price: "125.25", TaxRate1: "0", TaxRate2: "0", TaxRate3: "0"}
		draft, err := invoiceninja.BuildRecurringDraft(snapshot.Account.CustomerID, snapshot.Proof.PaymentDate, activationToday, product, false, activation.Marker(snapshot.Token))
		if err != nil {
			t.Fatal(err)
		}
		value = draft
	case activation.SUIEnable:
		var record map[string]json.RawMessage
		if err := json.Unmarshal(snapshot.Steps[activation.SUICreate].Payload, &record); err != nil {
			t.Fatal(err)
		}
		record["id"] = json.RawMessage(snapshot.Steps[activation.SUICreate].ExternalID)
		raw, _ := json.Marshal(record)
		before, err := sui.ParseClientRecord(raw)
		if err != nil {
			t.Fatal(err)
		}
		binding, err := activation.SUIBinding(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		draft, err := sui.PrepareEnable(before, binding)
		if err != nil {
			t.Fatal(err)
		}
		value = draft
	case activation.RecurringStart:
		value = activation.StartRequest{IDs: []string{snapshot.Steps[activation.RecurringCreate].ExternalID}, Action: "start"}
	default:
		t.Fatal("invalid synthetic step")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := activation.ValidateIntent(snapshot, step, raw); err != nil {
		t.Fatalf("synthetic intent invalid for %s: %v", step, err)
	}
	return raw
}

func finishActivationSteps(t *testing.T, s *Store, ctx context.Context, operation queue.Operation) activation.Snapshot {
	t.Helper()
	return advanceActivationSteps(t, s, ctx, operation, activation.OrderedSteps())
}

func advanceActivationSteps(t *testing.T, s *Store, ctx context.Context, operation queue.Operation, steps []activation.Step) activation.Snapshot {
	t.Helper()
	for _, step := range steps {
		snapshot := loadActivation(t, s, ctx, operation.Lease, 123)
		payload := activationPayload(t, snapshot, step)
		if granted, err := s.PrepareActivationWrite(ctx, operation.Lease, snapshot, step, payload, activationToday); err != nil || !granted {
			t.Fatalf("new %s permit=%v err=%v", step, granted, err)
		}
		var object map[string]any
		_ = json.Unmarshal(payload, &object)
		equivalent, _ := json.MarshalIndent(object, "", "  ")
		if granted, err := s.PrepareActivationWrite(ctx, operation.Lease, snapshot, step, equivalent, activationToday); err != nil || granted {
			t.Fatalf("duplicate %s rearmed=%v err=%v", step, granted, err)
		}
		id, wrong := "77", "78"
		recurring := step == activation.RecurringCreate || step == activation.RecurringStart
		datetime := snapshot.Proof.NextInvoiceDate + "T12:00:00Z"
		if recurring {
			id, wrong = "recurring-synthetic", "recurring-other"
			if err := s.ConfirmActivationStep(ctx, operation.Lease, step, id); !errors.Is(err, queue.ErrConflict) {
				t.Fatalf("generic confirmation accepted recurring facts: %v", err)
			}
		}
		confirm := func(id string) error {
			if recurring {
				return s.ConfirmRecurringActivationStep(ctx, operation.Lease, step, id, json.Number("125.25"), datetime)
			}
			return s.ConfirmActivationStep(ctx, operation.Lease, step, id)
		}
		if step == activation.SUIEnable || step == activation.RecurringStart {
			if err := confirm(wrong); !errors.Is(err, queue.ErrConflict) {
				t.Fatalf("confirmed mismatched child identity: %v", err)
			}
		}
		if step == activation.RecurringStart {
			for _, facts := range [][2]string{{"125.26", datetime}, {"125.25", snapshot.Proof.NextInvoiceDate + "T13:00:00Z"}} {
				if err := s.ConfirmRecurringActivationStep(ctx, operation.Lease, step, id, json.Number(facts[0]), facts[1]); !errors.Is(err, queue.ErrConflict) {
					t.Fatalf("start changed frozen recurring facts: %v", err)
				}
			}
			paymentSQL(t, s, ctx, `SELECT operation_id=$1 AND step='recurring_start' FROM activation_write_guard`, operation.ID)
		}
		if err := confirm(id); err != nil {
			t.Fatal(err)
		}
		if err := confirm(id); err != nil {
			t.Fatalf("confirmed replay not idempotent: %v", err)
		}
		if err := confirm(wrong); !errors.Is(err, queue.ErrConflict) {
			t.Fatalf("changed confirmed identity: %v", err)
		}
		if recurring {
			for _, facts := range [][2]string{{"125.26", datetime}, {"125.25", snapshot.Proof.NextInvoiceDate + "T13:00:00Z"}} {
				if err := s.ConfirmRecurringActivationStep(ctx, operation.Lease, step, id, json.Number(facts[0]), facts[1]); !errors.Is(err, queue.ErrConflict) {
					t.Fatalf("replay changed frozen recurring facts: %v", err)
				}
			}
		}
		paymentSQL(t, s, ctx, `SELECT operation_id IS NULL AND step IS NULL FROM activation_write_guard WHERE id=1`)
	}
	return loadActivation(t, s, ctx, operation.Lease, 123)
}

func TestActivationSeedOwnershipAllowlistPolicyAndAntiJoin(t *testing.T) {
	s, ctx := activationTestStore(t)
	ids := seedActivationAccounts(t, s, ctx, 123, 10000, 65)
	seedActivationAccounts(t, s, ctx, 456, 20000, 2)
	// Fifteen independently ineligible original registrations. Account zero
	// is deliberately excluded from the allowlist, even though it is valid.
	paymentExec(t, s, ctx, `UPDATE operations SET status='pending', completed_at=NULL
        WHERE client_id=$1 AND kind='register_client'`, ids[1])
	paymentExec(t, s, ctx, `UPDATE operation_steps SET external_id='foreign-customer'
        WHERE operation_id=(SELECT id FROM operations WHERE client_id=$1) AND step='customer_create'`, ids[2])
	paymentExec(t, s, ctx, `UPDATE clients SET sui_id='preexisting' WHERE id=$1`, ids[3])
	paymentExec(t, s, ctx, `UPDATE subscriptions SET recurring_id='preexisting-recurring' WHERE client_id=$1`, ids[4])
	paymentExec(t, s, ctx, `UPDATE subscriptions SET state='active' WHERE client_id=$1`, ids[5])
	paymentExec(t, s, ctx, `UPDATE subscriptions SET desired_access=true WHERE client_id=$1`, ids[6])
	paymentExec(t, s, ctx, `UPDATE subscriptions SET current_invoice_id='later-invoice' WHERE client_id=$1`, ids[7])
	paymentExec(t, s, ctx, `UPDATE payment_confirmations SET review_required=true WHERE client_id=$1`, ids[8])
	paymentExec(t, s, ctx, `UPDATE payment_checks SET review_code=2007 WHERE client_id=$1`, ids[9])
	paymentExec(t, s, ctx, `UPDATE payment_confirmations SET customer_id='foreign-owner' WHERE client_id=$1`, ids[10])
	paymentExec(t, s, ctx, `UPDATE payment_confirmations SET bot_id=456 WHERE client_id=$1`, ids[11])
	paymentExec(t, s, ctx, `UPDATE subscriptions SET next_invoice_date='2039-03-01' WHERE client_id=$1`, ids[12])
	paymentExec(t, s, ctx, `UPDATE payment_confirmations SET proof_hash=$2 WHERE client_id=$1`, ids[13], make([]byte, 32))
	paymentExec(t, s, ctx, `UPDATE payment_confirmations SET review_code=2007 WHERE client_id=$1`, ids[14])
	seedActivationBatch(t, s, ctx, 456, activationAllow(10000, 20), 0)
	seedActivationBatch(t, s, ctx, 123, activationAllow(20000, 2), 0)
	seedActivationBatch(t, s, ctx, 123, activationAllow(10001, 19), 5)
	changed := sha256.Sum256([]byte("changed synthetic policy"))
	if n, err := s.SeedActivations(ctx, 123, activationAllow(10001, 19), changed); err != nil || n != 0 {
		t.Fatal("reseed rewrote existing flow")
	}
	paymentSQL(t, s, ctx, `SELECT count(*)=5 AND bool_and(policy_hash=$1) FROM activation_flows`, activationTestPolicy[:])
	seedActivationBatch(t, s, ctx, 123, activationAllow(10020, 20), 20)
	var wg sync.WaitGroup
	counts := make(chan int, 2)
	for range 2 {
		wg.Go(func() {
			n, err := s.SeedActivations(ctx, 123, activationAllow(10040, 20), activationTestPolicy)
			if err != nil {
				t.Error(err)
			}
			counts <- n
		})
	}
	wg.Wait()
	close(counts)
	total := 0
	for n := range counts {
		total += n
	}
	if total != 20 {
		t.Fatalf("concurrent seed duplicated or lost ownership: %d", total)
	}
	// Fifty older registrations/checks must not starve this final eligible set.
	seedActivationBatch(t, s, ctx, 123, activationAllow(10060, 5), 5)
	seedActivationBatch(t, s, ctx, 456, activationAllow(20000, 2), 2)
	paymentSQL(t, s, ctx, `SELECT count(*)=52 AND count(*) FILTER (WHERE f.bot_id=123)=50
        AND bool_and(f.proof_hash=pc.proof_hash AND f.invoice_id=pc.invoice_id AND f.bot_id=pc.bot_id)
        FROM activation_flows f JOIN payment_confirmations pc USING(invoice_id)`)
	paymentSQL(t, s, ctx, `SELECT NOT EXISTS (SELECT 1 FROM operations
        WHERE client_id=ANY($1::bigint[]) AND kind='activate_first_subscription')`, ids[:15])
}

func TestActivationClaimsScopedFIFOConcurrentAndExpiredFencing(t *testing.T) {
	s, ctx := activationTestStore(t)
	ids := seedActivationAccounts(t, s, ctx, 123, 11000, 8)
	seedActivationAccounts(t, s, ctx, 456, 21000, 2)
	paymentExec(t, s, ctx, `INSERT INTO operations (client_id, operation_key, kind, next_attempt_at)
        VALUES ($1, 'older-unsupported', 'unsupported', clock_timestamp()),
            ($2, 'older-held', 'test_notify', 'infinity')`, ids[0], ids[1])
	seedActivationBatch(t, s, ctx, 123, activationAllow(11000, 8), 8)
	seedActivationBatch(t, s, ctx, 456, activationAllow(21000, 2), 2)
	paymentExec(t, s, ctx, `UPDATE operations SET next_attempt_at='infinity'
        WHERE client_id=ANY($1::bigint[]) AND kind='activate_first_subscription'`, ids[2:4])
	var wg sync.WaitGroup
	results := make(chan []queue.Operation, 2)
	for range 2 {
		wg.Go(func() {
			claims, err := s.ClaimActivations(ctx, 123, 4)
			if err != nil {
				t.Error(err)
			}
			results <- claims
		})
	}
	wg.Wait()
	close(results)
	seen := map[int64]bool{}
	var owned queue.Operation
	for claims := range results {
		for _, op := range claims {
			if seen[op.ClientID] || op.ClientID < ids[4] || op.ClientID > ids[7] || op.Attempt != 1 || op.Kind != activation.Kind {
				t.Fatal("claimed blocked, duplicate, or foreign FIFO head")
			}
			seen[op.ClientID] = true
			owned = op
		}
	}
	if len(seen) != 4 {
		t.Fatalf("claimed %d eligible clients", len(seen))
	}
	other, err := s.ClaimActivations(ctx, 456, 4)
	if err != nil || len(other) != 2 {
		t.Fatalf("foreign bot claims=%d err=%v", len(other), err)
	}
	for _, op := range other {
		if seen[op.ClientID] {
			t.Fatal("bot claim crossed provenance")
		}
	}
	paymentSQL(t, s, ctx, `SELECT bool_and(locked_until>clock_timestamp()+interval '90 seconds'
        AND locked_until<=clock_timestamp()+interval '2 minutes') FROM operations WHERE kind='activate_first_subscription' AND status='running'`)
	if err := s.CompleteOperation(ctx, owned.Lease, nil); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("generic completion bypassed activation: %v", err)
	}
	if _, err := s.LoadActivation(ctx, owned.Lease, 456); !errors.Is(err, queue.ErrConflict) {
		t.Fatalf("loaded foreign source bot: %v", err)
	}
	paymentExec(t, s, ctx, `INSERT INTO operations (client_id, operation_key, kind) VALUES ($1, 'later-notice', 'test_notify')`, owned.ClientID)
	if later, err := s.ClaimOperations(ctx, []string{"test_notify"}, 4, time.Minute); err != nil || len(later) != 0 {
		t.Fatalf("general FIFO bypassed activation or held head: %v", err)
	}
	paymentExec(t, s, ctx, `UPDATE operations SET locked_until=clock_timestamp()-interval '1 second' WHERE id=$1`, owned.ID)
	if _, err := s.LoadActivation(ctx, owned.Lease, 123); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("expired load: %v", err)
	}
	fresh, _ := claimOneActivation(t, s, ctx, 123)
	if fresh.ID != owned.ID || fresh.Attempt != owned.Attempt+1 {
		t.Fatal("claim did not advance fencing token")
	}
	if err := s.HoldActivation(ctx, owned.Lease, activation.ReviewSUI, nil); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("stale hold accepted: %v", err)
	}
}

type activationClaimTrace struct {
	ready, resume chan struct{}
	once          sync.Once
}

func (t *activationClaimTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "WITH heads AS") {
		t.once.Do(func() {
			close(t.ready)
			select {
			case <-t.resume:
			case <-ctx.Done():
			}
		})
	}
	return ctx
}
func (*activationClaimTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestActivationClaimFreshReadCommittedSnapshot(t *testing.T) {
	s, ctx := activationTestStore(t)
	ids := seedActivationAccounts(t, s, ctx, 123, 12000, 1)
	seedActivationBatch(t, s, ctx, 123, activationAllow(12000, 1), 1)
	trace := &activationClaimTrace{ready: make(chan struct{}), resume: make(chan struct{})}
	cfg := s.pool.Config()
	cfg.MaxConns = 1
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	claimCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	type result struct {
		claims []queue.Operation
		err    error
	}
	done := make(chan result, 1)
	go func() {
		claims, err := (&Store{pool: pool}).ClaimActivations(claimCtx, 123, 1)
		done <- result{claims, err}
	}()
	select {
	case <-trace.ready:
	case <-claimCtx.Done():
		t.Fatal("claim did not separate lock and eligibility statements")
	}
	// Fixture intentionally bypasses the client lock to commit after the first
	// statement's snapshot, exactly like the existing queue freshness test.
	paymentExec(t, s, ctx, `UPDATE operations SET next_attempt_at='infinity'
        WHERE client_id=$1 AND kind='activate_first_subscription'`, ids[0])
	close(trace.resume)
	select {
	case got := <-done:
		if got.err != nil || len(got.claims) != 0 {
			t.Fatalf("reused stale claim snapshot: %v", got.err)
		}
	case <-claimCtx.Done():
		t.Fatal("claim did not finish")
	}
	paymentSQL(t, s, ctx, `SELECT attempts=0 AND status='pending' FROM operations WHERE kind='activate_first_subscription'`)
}

func TestActivationDurableGuardNoExpiryTakeoverOrReconciliation(t *testing.T) {
	s, ctx := activationTestStore(t)
	seedActivationAccounts(t, s, ctx, 123, 13000, 2)
	seedActivationBatch(t, s, ctx, 123, activationAllow(13000, 2), 2)
	first, snapshot := claimOneActivation(t, s, ctx, 123)
	second, other := claimOneActivation(t, s, ctx, 123)
	future := activationPayload(t, snapshot, activation.RecurringCreate)
	if granted, err := s.PrepareActivationWrite(ctx, first.Lease, snapshot, activation.RecurringCreate, future, activationToday); granted || !errors.Is(err, queue.ErrConflict) {
		t.Fatalf("out-of-order grant: %v", err)
	}
	payload := activationPayload(t, snapshot, activation.SUICreate)
	if granted, err := s.PrepareActivationWrite(ctx, first.Lease, snapshot, activation.SUICreate, payload, activationToday); !granted || err != nil {
		t.Fatal(err)
	}
	otherPayload := activationPayload(t, other, activation.SUICreate)
	if granted, err := s.PrepareActivationWrite(ctx, second.Lease, other, activation.SUICreate, otherPayload, activationToday); granted || !errors.Is(err, activation.ErrGuardBusy) {
		t.Fatalf("competing global writer authorized: %v", err)
	}
	paymentSQL(t, s, ctx, `SELECT count(*)=1 AND bool_and(operation_id=$1 AND dispatch_attempt=$2) FROM activation_steps`, first.ID, first.Attempt)
	if granted, err := s.PrepareActivationWrite(ctx, first.Lease, snapshot, activation.SUICreate, payload, activationToday); granted || err != nil {
		t.Fatalf("duplicate intent rearmed: %v", err)
	}
	if granted, err := s.PrepareActivationWrite(ctx, first.Lease, snapshot, activation.SUICreate, activationPayload(t, snapshot, activation.SUICreate), activationToday); granted || !errors.Is(err, queue.ErrConflict) {
		t.Fatalf("changed immutable intent: %v", err)
	}
	// Removing a guard is fixture-only corruption, never an application action.
	paymentExec(t, s, ctx, `UPDATE activation_write_guard SET operation_id=NULL, step=NULL`)
	if granted, err := s.PrepareActivationWrite(ctx, first.Lease, snapshot, activation.SUICreate, payload, activationToday); granted || !errors.Is(err, queue.ErrConflict) {
		t.Fatalf("unresolved intent without guard accepted: %v", err)
	}
	paymentExec(t, s, ctx, `UPDATE activation_write_guard SET operation_id=$1, step='sui_create'`, first.ID)
	paymentExec(t, s, ctx, `UPDATE operations SET locked_until=clock_timestamp()-interval '1 second' WHERE id=$1`, first.ID)
	if err := s.ConfirmActivationStep(ctx, first.Lease, activation.SUICreate, "77"); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("expired sender cleared barrier: %v", err)
	}
	fresh, replay := claimOneActivation(t, s, ctx, 123)
	if fresh.ID != first.ID || replay.Steps[activation.SUICreate].DispatchAttempt != first.Attempt {
		t.Fatal("dispatch generation was rewritten on reclaim")
	}
	if granted, err := s.PrepareActivationWrite(ctx, fresh.Lease, replay, activation.SUICreate, payload, activationToday); granted || err != nil {
		t.Fatalf("reclaim rearmed unresolved write: %v", err)
	}
	// A positive lookup by a later attempt cannot establish quiescence of the
	// original sender. It must not clear the barrier or adopt the discovered ID.
	if err := s.ConfirmActivationStep(ctx, fresh.Lease, activation.SUICreate, "77"); !errors.Is(err, queue.ErrConflict) {
		t.Fatalf("new owner reconciled unknown write: %v", err)
	}
	if err := s.HoldActivation(ctx, fresh.Lease, activation.ReviewUnknownWrite, nil); err != nil {
		t.Fatal(err)
	}
	if granted, err := s.PrepareActivationWrite(ctx, second.Lease, other, activation.SUICreate, otherPayload, activationToday); granted || !errors.Is(err, activation.ErrGuardBusy) {
		t.Fatalf("HOLD released barrier: %v", err)
	}
	paymentSQL(t, s, ctx, `SELECT g.operation_id=$1 AND g.step='sui_create' AND st.external_id IS NULL
        AND st.dispatch_attempt=$2 AND o.status='pending' AND o.next_attempt_at='infinity'::timestamptz
        AND f.review_code=3006 AND NOT pc.review_required AND p.review_code=0
        FROM activation_write_guard g JOIN activation_steps st ON st.operation_id=g.operation_id AND st.step=g.step
        JOIN operations o ON o.id=g.operation_id JOIN activation_flows f ON f.operation_id=o.id
        JOIN payment_confirmations pc ON pc.invoice_id=f.invoice_id JOIN payment_checks p ON p.client_id=o.client_id`, first.ID, first.Attempt)
}

func TestActivationOriginalConfirmationSurvivesReviewAndLocalChanges(t *testing.T) {
	s, ctx := activationTestStore(t)
	ids := seedActivationAccounts(t, s, ctx, 123, 14000, 1)
	seedActivationBatch(t, s, ctx, 123, activationAllow(14000, 1), 1)
	op, snapshot := claimOneActivation(t, s, ctx, 123)
	payload := activationPayload(t, snapshot, activation.SUICreate)
	if granted, err := s.PrepareActivationWrite(ctx, op.Lease, snapshot, activation.SUICreate, payload, activationToday); err != nil || !granted {
		t.Fatal(err)
	}
	paymentExec(t, s, ctx, `UPDATE payment_checks SET review_code=2007 WHERE client_id=$1`, ids[0])
	paymentExec(t, s, ctx, `UPDATE subscriptions SET state='overdue', desired_access=false WHERE client_id=$1`, ids[0])
	if err := s.ConfirmActivationStep(ctx, op.Lease, activation.SUICreate, "77"); err != nil {
		t.Fatalf("lost original sender's known response: %v", err)
	}
	if err := s.ConfirmActivationStep(ctx, op.Lease, activation.SUICreate, "77"); err != nil {
		t.Fatal(err)
	}
	current := loadActivation(t, s, ctx, op.Lease, 123)
	if !current.PaymentReview || current.Account.State != "overdue" || current.Steps[activation.SUICreate].ExternalID != "77" {
		t.Fatal("reviewed raw snapshot was hidden")
	}
	paymentSQL(t, s, ctx, `SELECT operation_id IS NULL FROM activation_write_guard`)
	if err := s.FinishActivation(ctx, op.Lease, current, activationToday, nil); err == nil {
		t.Fatal("known response became authorization")
	}
	if err := s.HoldActivation(ctx, op.Lease, activation.ReviewPayment, nil); err != nil {
		t.Fatal(err)
	}
}

func TestActivationFinishAtomicBindingsOutboxAndFinalClock(t *testing.T) {
	s, ctx := activationTestStore(t)
	ids := seedActivationAccounts(t, s, ctx, 123, 15000, 1)
	seedActivationBatch(t, s, ctx, 123, activationAllow(15000, 1), 1)
	op, snapshot := claimOneActivation(t, s, ctx, 123)
	if err := s.FinishActivation(ctx, op.Lease, snapshot, activationToday, nil); !errors.Is(err, queue.ErrConflict) {
		t.Fatalf("completed missing steps: %v", err)
	}
	snapshot = finishActivationSteps(t, s, ctx, op)
	for _, step := range activation.OrderedSteps() {
		if granted, err := s.PrepareActivationWrite(ctx, op.Lease, snapshot, step, snapshot.Steps[step].Payload, activationToday); granted || err != nil {
			t.Fatalf("confirmed historical intent rearmed: %v", err)
		}
	}
	binding, err := activation.SUIBinding(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	good := queue.Draft{BotID: 123, ChatID: snapshot.Account.ChatID, Key: "activation-ready", Text: "Subscription activated", ClientMenu: true}
	bad := queue.Draft{BotID: 999, ChatID: good.ChatID, Key: "activation-rejected", Text: "private synthetic notification"}
	paymentExec(t, s, ctx, `ALTER TABLE outbox ADD CONSTRAINT reject_activation_notice CHECK (message_key<>'activation-rejected')`)
	if err := s.FinishActivation(ctx, op.Lease, snapshot, activationToday, []queue.Draft{good, bad}); err == nil || strings.Contains(err.Error(), "private") {
		t.Fatalf("outbox failure ignored or exposed: %v", err)
	}
	paymentSQL(t, s, ctx, `SELECT (SELECT count(*)=0 FROM activation_sui_bindings)
        AND (SELECT count(*)=0 FROM outbox) AND (SELECT count(*)=0 FROM telegram_bots WHERE bot_id=999)
        AND (SELECT sui_id IS NULL FROM clients WHERE id=$1)
        AND (SELECT recurring_id IS NULL AND state='awaiting_payment' AND NOT desired_access FROM subscriptions WHERE client_id=$1)
        AND (SELECT status='running' AND locked_until>clock_timestamp() FROM operations WHERE id=$2)`, ids[0], op.ID)
	conflict := queue.Draft{BotID: 123, ChatID: good.ChatID, Key: "already-existing", Text: "unchanged"}
	if _, err := s.EnqueueOutbox(ctx, conflict); err != nil {
		t.Fatal(err)
	}
	conflict.Text = "changed"
	if err := s.FinishActivation(ctx, op.Lease, snapshot, activationToday, []queue.Draft{good, conflict}); !errors.Is(err, queue.ErrConflict) {
		t.Fatalf("outbox conflict accepted: %v", err)
	}
	paymentSQL(t, s, ctx, `SELECT (SELECT count(*)=0 FROM activation_sui_bindings) AND (SELECT count(*)=1 FROM outbox)
        AND (SELECT sui_id IS NULL FROM clients WHERE id=$1)`, ids[0])
	// Frozen intents and matching confirmed identities are independently checked.
	paymentExec(t, s, ctx, `UPDATE activation_steps SET external_id='wrong-recurring' WHERE operation_id=$1 AND step='recurring_start'`, op.ID)
	if err := s.FinishActivation(ctx, op.Lease, snapshot, activationToday, nil); !errors.Is(err, queue.ErrConflict) {
		t.Fatalf("mismatched confirmations accepted: %v", err)
	}
	paymentExec(t, s, ctx, `UPDATE activation_steps SET external_id='recurring-synthetic' WHERE operation_id=$1 AND step='recurring_start'`, op.ID)
	paymentExec(t, s, ctx, `UPDATE activation_steps SET request='{}'::jsonb WHERE operation_id=$1 AND step='recurring_create'`, op.ID)
	if err := s.FinishActivation(ctx, op.Lease, snapshot, activationToday, nil); !errors.Is(err, queue.ErrConflict) {
		t.Fatalf("invalid frozen intent accepted: %v", err)
	}
	paymentExec(t, s, ctx, `UPDATE activation_steps SET request=$2::jsonb WHERE operation_id=$1 AND step='recurring_create'`, op.ID, string(snapshot.Steps[activation.RecurringCreate].Payload))
	paymentExec(t, s, ctx, `CREATE FUNCTION expire_activation_notice() RETURNS trigger LANGUAGE plpgsql AS $$
        BEGIN UPDATE operations SET locked_until=clock_timestamp()-interval '1 second'
            WHERE kind='activate_first_subscription' AND status='running'; RETURN NEW; END $$`)
	paymentExec(t, s, ctx, `CREATE TRIGGER expire_activation_after_notice AFTER INSERT ON outbox
        FOR EACH ROW EXECUTE FUNCTION expire_activation_notice()`)
	if err := s.FinishActivation(ctx, op.Lease, snapshot, activationToday, []queue.Draft{good}); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("lease crossing committed entitlement: %v", err)
	}
	paymentSQL(t, s, ctx, `SELECT (SELECT count(*)=0 FROM activation_sui_bindings) AND (SELECT count(*)=1 FROM outbox)
        AND (SELECT sui_id IS NULL FROM clients) AND (SELECT state='awaiting_payment' AND NOT desired_access FROM subscriptions)
        AND (SELECT locked_until>clock_timestamp() FROM operations WHERE id=$1)`, op.ID)
	paymentExec(t, s, ctx, `DROP TRIGGER expire_activation_after_notice ON outbox`)
	// Exact idempotent mappings are permitted; pre-existing foreign mappings
	// are not adopted even when their panel ID happens to match.
	wrongName := "pp-" + strings.Repeat("a", 40)
	if wrongName == binding.Name {
		wrongName = "pp-" + strings.Repeat("b", 40)
	}
	paymentExec(t, s, ctx, `INSERT INTO activation_sui_bindings
        (client_id, bot_id, sui_id, name, credentials_hash, operation_id) VALUES ($1, 123, $2, $3, $4, $5)`, ids[0], binding.ID, wrongName, binding.CredentialsHash[:], op.ID)
	if err := s.FinishActivation(ctx, op.Lease, snapshot, activationToday, []queue.Draft{good}); !errors.Is(err, queue.ErrConflict) {
		t.Fatalf("adopted different binding: %v", err)
	}
	paymentExec(t, s, ctx, `UPDATE activation_sui_bindings SET name=$2 WHERE client_id=$1`, ids[0], binding.Name)
	// Finalization causes no provider effect: unlike dispatch, it does not
	// require a current idle preflight epoch.
	snapshot.WriteEpoch, snapshot.WriterBusy = 0, true
	if err := s.FinishActivation(ctx, op.Lease, snapshot, activationToday, []queue.Draft{good}); err != nil {
		t.Fatal(err)
	}
	paymentSQL(t, s, ctx, `SELECT b.sui_id=$2 AND b.name=$3 AND b.credentials_hash=$4 AND b.operation_id=$5
        AND c.sui_id=b.sui_id::text AND sub.recurring_id='recurring-synthetic' AND sub.state='active' AND sub.desired_access
        AND sub.first_invoice_id=pc.invoice_id AND sub.current_invoice_id=pc.invoice_id
        AND sub.payment_date=pc.payment_date AND sub.next_invoice_date=pc.next_invoice_date
        AND o.status='completed' AND o.locked_until IS NULL AND o.completed_at IS NOT NULL
        AND (SELECT count(*)=2 FROM outbox) AND (SELECT count(*)=4 FROM activation_steps WHERE confirmed_at IS NOT NULL)
        FROM activation_sui_bindings b JOIN clients c ON c.id=b.client_id JOIN subscriptions sub ON sub.client_id=c.id
        JOIN operations o ON o.id=b.operation_id JOIN activation_flows f ON f.operation_id=o.id
        JOIN payment_confirmations pc ON pc.invoice_id=f.invoice_id WHERE c.id=$1`, ids[0], binding.ID, binding.Name, binding.CredentialsHash[:], op.ID)
	if err := s.FinishActivation(ctx, op.Lease, snapshot, activationToday, []queue.Draft{good}); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("replayed completed lease: %v", err)
	}
	seedActivationBatch(t, s, ctx, 123, activationAllow(15000, 1), 0)
}

func TestActivationHoldStickyOnlyForPaymentAndPreservesOriginalFacts(t *testing.T) {
	s, ctx := activationTestStore(t)
	ids := seedActivationAccounts(t, s, ctx, 123, 16000, 3)
	seedActivationBatch(t, s, ctx, 123, activationAllow(16000, 3), 3)
	for i, reason := range []int{activation.ReviewUnknownWrite, activation.ReviewPeriod, activation.ReviewPayment} {
		op, snapshot := claimOneActivation(t, s, ctx, 123)
		if i == 0 {
			payload := activationPayload(t, snapshot, activation.SUICreate)
			if granted, err := s.PrepareActivationWrite(ctx, op.Lease, snapshot, activation.SUICreate, payload, activationToday); err != nil || !granted {
				t.Fatal(err)
			}
		}
		if i == 2 {
			paymentExec(t, s, ctx, `UPDATE subscriptions SET current_invoice_id='later-current-invoice' WHERE client_id=$1`, op.ClientID)
		}
		draft := queue.Draft{BotID: 123, ChatID: snapshot.Account.ChatID, Key: fmt.Sprintf("hold:%d", i), Text: "Activation needs manual review"}
		if err := s.HoldActivation(ctx, op.Lease, reason, []queue.Draft{draft}); err != nil {
			t.Fatal(err)
		}
		paymentSQL(t, s, ctx, `SELECT o.status='pending' AND o.locked_until IS NULL AND o.next_attempt_at='infinity'::timestamptz
            AND f.review_code=$2 AND pc.review_required=$3 AND pc.review_code=CASE WHEN $3 THEN 2007 ELSE 0 END
            AND p.review_code=CASE WHEN $3 THEN 2007 ELSE 0 END AND pc.proof_hash=f.proof_hash
            AND pc.payment_date='2039-01-31' AND pc.next_invoice_date='2039-02-28' AND pc.amount=125.25
            AND NOT sub.desired_access AND sub.state='awaiting_payment'
            FROM operations o JOIN activation_flows f ON f.operation_id=o.id JOIN payment_confirmations pc ON pc.invoice_id=f.invoice_id
            JOIN payment_checks p ON p.client_id=o.client_id JOIN subscriptions sub ON sub.client_id=o.client_id WHERE o.id=$1`, op.ID, reason, reason == activation.ReviewPayment)
	}
	paymentSQL(t, s, ctx, `SELECT count(*)=3 FROM outbox`)
	paymentSQL(t, s, ctx, `SELECT count(*)=1 AND bool_and(external_id IS NULL) FROM activation_steps`)
	paymentSQL(t, s, ctx, `SELECT operation_id IS NOT NULL AND step='sui_create' FROM activation_write_guard`)
	paymentExec(t, s, ctx, `INSERT INTO operations (client_id, operation_key, kind)
        SELECT id, 'later-held:'||id::text, 'test_notify' FROM clients WHERE id=ANY($1::bigint[])`, ids)
	if claims, err := s.ClaimActivations(ctx, 123, 4); err != nil || len(claims) != 0 {
		t.Fatalf("held activation became claimable: %v", err)
	}
	if claims, err := s.ClaimOperations(ctx, []string{"test_notify"}, 4, time.Minute); err != nil || len(claims) != 0 {
		t.Fatalf("held activation head bypassed: %v", err)
	}
}

func TestActivationSnapshotOptimisticAndReviewGuards(t *testing.T) {
	s, ctx := activationTestStore(t)
	ids := seedActivationAccounts(t, s, ctx, 123, 17000, 1)
	seedActivationBatch(t, s, ctx, 123, activationAllow(17000, 1), 1)
	op, snapshot := claimOneActivation(t, s, ctx, 123)
	payload := activationPayload(t, snapshot, activation.SUICreate)
	for _, mutate := range []func(*activation.Snapshot){
		func(s *activation.Snapshot) { s.Account.ClientID++ }, func(s *activation.Snapshot) { s.Account.TelegramID++ },
		func(s *activation.Snapshot) { s.Account.ChatID++ }, func(s *activation.Snapshot) { s.Account.BotID++ },
		func(s *activation.Snapshot) { s.Account.CustomerID += "other" }, func(s *activation.Snapshot) { s.Account.InvoiceID += "other" },
		func(s *activation.Snapshot) { s.Account.FirstInvoiceID += "other" }, func(s *activation.Snapshot) { s.Account.RecurringID = "other" },
		func(s *activation.Snapshot) { s.Account.SUIID = "other" }, func(s *activation.Snapshot) { s.Account.State = "active" },
		func(s *activation.Snapshot) { s.Account.DesiredAccess = true }, func(s *activation.Snapshot) { s.Account.PaymentDate = "2039-01-30" },
		func(s *activation.Snapshot) { s.Account.NextInvoiceDate = "2039-03-01" }, func(s *activation.Snapshot) { s.Token += "other" },
		func(s *activation.Snapshot) { s.PolicyHash[0] ^= 1 }, func(s *activation.Snapshot) { s.Proof.Hash[0] ^= 1 },
	} {
		wrong := snapshot
		mutate(&wrong)
		if granted, err := s.PrepareActivationWrite(ctx, op.Lease, wrong, activation.SUICreate, payload, activationToday); granted || !errors.Is(err, queue.ErrConflict) {
			t.Fatalf("incomplete expected snapshot fence: %v", err)
		}
		if err := s.FinishActivation(ctx, op.Lease, wrong, activationToday, nil); !errors.Is(err, queue.ErrConflict) {
			t.Fatalf("incomplete finish fence: %v", err)
		}
	}
	for _, today := range []string{"2039-01-30", "2039-02-28", "2039-03-01"} {
		if granted, err := s.PrepareActivationWrite(ctx, op.Lease, snapshot, activation.SUICreate, payload, today); granted || !errors.Is(err, activation.ErrReviewRequired) {
			t.Fatalf("future/expired payment accepted: %v", err)
		}
	}
	for _, change := range []struct{ set, undo string }{
		{`UPDATE subscriptions SET state='active', desired_access=true`, `UPDATE subscriptions SET state='awaiting_payment', desired_access=false`},
		{`UPDATE subscriptions SET current_invoice_id='changed-current'`, `UPDATE subscriptions SET current_invoice_id=first_invoice_id`},
		{`UPDATE subscriptions SET payment_date='2039-01-30'`, `UPDATE subscriptions SET payment_date='2039-01-31'`},
		{`UPDATE clients SET sui_id='preexisting-panel'`, `UPDATE clients SET sui_id=NULL`},
		{`UPDATE subscriptions SET recurring_id='preexisting-recurring'`, `UPDATE subscriptions SET recurring_id=NULL`},
		{`UPDATE payment_confirmations SET review_required=true`, `UPDATE payment_confirmations SET review_required=false`},
		{`UPDATE payment_checks SET review_code=2007`, `UPDATE payment_checks SET review_code=0`},
		{`UPDATE activation_flows SET review_code=3004`, `UPDATE activation_flows SET review_code=0`},
	} {
		paymentExec(t, s, ctx, change.set)
		actual := loadActivation(t, s, ctx, op.Lease, 123)
		if granted, err := s.PrepareActivationWrite(ctx, op.Lease, actual, activation.SUICreate, payload, activationToday); granted || err == nil {
			t.Fatal("changed local state/review authorized write")
		}
		if err := s.FinishActivation(ctx, op.Lease, actual, activationToday, nil); err == nil {
			t.Fatal("changed local state/review authorized completion")
		}
		paymentExec(t, s, ctx, change.undo)
	}
	for _, change := range []struct {
		set, undo string
		args      []any
	}{
		{`UPDATE payment_confirmations SET proof_hash=$1`, `UPDATE payment_confirmations SET proof_hash=$1`, []any{snapshot.Proof.Hash[:]}},
		{`UPDATE activation_flows SET proof_hash=$1`, `UPDATE activation_flows SET proof_hash=$1`, []any{snapshot.Proof.Hash[:]}},
	} {
		paymentExec(t, s, ctx, change.set, make([]byte, 32))
		if _, err := s.LoadActivation(ctx, op.Lease, 123); !errors.Is(err, queue.ErrConflict) {
			t.Fatalf("changed immutable proof accepted: %v", err)
		}
		paymentExec(t, s, ctx, change.undo, change.args...)
	}
	paymentExec(t, s, ctx, `UPDATE payment_confirmations SET client_id=$1, customer_id='foreign-customer'`, ids[0])
	if _, err := s.LoadActivation(ctx, op.Lease, 123); !errors.Is(err, queue.ErrConflict) {
		t.Fatalf("foreign proof owner accepted: %v", err)
	}
	paymentExec(t, s, ctx, `UPDATE payment_confirmations SET customer_id=$1`, snapshot.Account.CustomerID)
	paymentSQL(t, s, ctx, `SELECT (SELECT count(*)=0 FROM activation_steps) AND (SELECT operation_id IS NULL FROM activation_write_guard)`)
}

func TestActivationEveryWriteFinalLeaseFenceRollsBack(t *testing.T) {
	s, ctx := activationTestStore(t)
	seedActivationAccounts(t, s, ctx, 123, 18000, 1)
	seedActivationBatch(t, s, ctx, 123, activationAllow(18000, 1), 1)
	op, snapshot := claimOneActivation(t, s, ctx, 123)
	payload := activationPayload(t, snapshot, activation.SUICreate)
	paymentExec(t, s, ctx, `CREATE FUNCTION expire_activation_step() RETURNS trigger LANGUAGE plpgsql AS $$
        BEGIN UPDATE operations SET locked_until=clock_timestamp()-interval '1 second' WHERE id=NEW.operation_id; RETURN NEW; END $$`)
	paymentExec(t, s, ctx, `CREATE TRIGGER expire_activation_step_insert AFTER INSERT ON activation_steps
        FOR EACH ROW EXECUTE FUNCTION expire_activation_step()`)
	if granted, err := s.PrepareActivationWrite(ctx, op.Lease, snapshot, activation.SUICreate, payload, activationToday); granted || !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("expired intent granted permit: %v", err)
	}
	paymentSQL(t, s, ctx, `SELECT (SELECT count(*)=0 FROM activation_steps) AND (SELECT operation_id IS NULL FROM activation_write_guard)
        AND (SELECT locked_until>clock_timestamp() FROM operations WHERE id=$1)`, op.ID)
	paymentExec(t, s, ctx, `DROP TRIGGER expire_activation_step_insert ON activation_steps`)
	if granted, err := s.PrepareActivationWrite(ctx, op.Lease, snapshot, activation.SUICreate, payload, activationToday); err != nil || !granted {
		t.Fatal(err)
	}
	paymentExec(t, s, ctx, `CREATE TRIGGER expire_activation_step_confirm AFTER UPDATE ON activation_steps
        FOR EACH ROW EXECUTE FUNCTION expire_activation_step()`)
	if err := s.ConfirmActivationStep(ctx, op.Lease, activation.SUICreate, "77"); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("expired confirmation cleared barrier: %v", err)
	}
	paymentSQL(t, s, ctx, `SELECT g.operation_id=$1 AND st.external_id IS NULL FROM activation_write_guard g
        JOIN activation_steps st ON st.operation_id=g.operation_id AND st.step=g.step`, op.ID)
	paymentExec(t, s, ctx, `DROP TRIGGER expire_activation_step_confirm ON activation_steps`)
	paymentExec(t, s, ctx, `CREATE FUNCTION expire_activation_hold() RETURNS trigger LANGUAGE plpgsql AS $$
        BEGIN UPDATE operations SET locked_until=clock_timestamp()-interval '1 second' WHERE kind='activate_first_subscription'; RETURN NEW; END $$`)
	paymentExec(t, s, ctx, `CREATE TRIGGER expire_activation_hold_notice AFTER INSERT ON outbox
        FOR EACH ROW EXECUTE FUNCTION expire_activation_hold()`)
	draft := queue.Draft{BotID: 999, ChatID: snapshot.Account.ChatID, Key: "held", Text: "Needs manual review"}
	if err := s.HoldActivation(ctx, op.Lease, activation.ReviewPayment, []queue.Draft{draft}); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("expired HOLD committed: %v", err)
	}
	paymentSQL(t, s, ctx, `SELECT (SELECT count(*)=0 FROM outbox) AND (SELECT count(*)=0 FROM telegram_bots WHERE bot_id=999)
        AND (SELECT review_code=0 FROM activation_flows) AND (SELECT NOT review_required FROM payment_confirmations)
        AND (SELECT review_code=0 FROM payment_checks) AND (SELECT operation_id=$1 FROM activation_write_guard)
        AND (SELECT locked_until>clock_timestamp() FROM operations WHERE id=$1)`, op.ID)
	paymentExec(t, s, ctx, `DROP TRIGGER expire_activation_hold_notice ON outbox`)
	if err := s.ConfirmActivationStep(ctx, op.Lease, activation.SUICreate, "77"); err != nil {
		t.Fatal(err)
	}
	paymentExec(t, s, ctx, `UPDATE operations SET locked_until=clock_timestamp()-interval '1 second' WHERE id=$1`, op.ID)
	if err := s.ConfirmActivationStep(ctx, op.Lease, activation.SUICreate, "77"); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("confirmed replay ignored expired lease: %v", err)
	}
	if granted, err := s.PrepareActivationWrite(ctx, op.Lease, snapshot, activation.SUICreate, payload, activationToday); granted || !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("intent replay ignored expired lease: %v", err)
	}
	paymentSQL(t, s, ctx, `SELECT external_id=$2 FROM activation_steps WHERE operation_id=$1`, op.ID, strconv.Itoa(77))
}
