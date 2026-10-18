//go:build integration

package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/billing"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Bulk fixtures keep the >50 account case inexpensive over a remote test DB.
func seedPaymentAccounts(t *testing.T, s *Store, ctx context.Context, botID int64, start, count int) []int64 {
	t.Helper()
	if _, err := s.pool.Exec(ctx, `WITH bot AS (
        INSERT INTO telegram_bots (bot_id) VALUES ($1) ON CONFLICT DO NOTHING
    ) INSERT INTO payment_wake_state (bot_id) VALUES ($1) ON CONFLICT DO NOTHING`, botID); err != nil {
		t.Fatal(err)
	}
	rows, err := s.pool.Query(ctx, `WITH c AS (
        INSERT INTO clients (telegram_id, chat_id, display_name, invoice_ninja_id)
        SELECT n, n, 'payment fixture', format('customer-%s', n)
        FROM generate_series($2::integer, $2::integer + $3::integer - 1) n RETURNING *
    ), sub AS (
        INSERT INTO subscriptions (client_id, first_invoice_id, current_invoice_id)
        SELECT id, format('invoice-%s', telegram_id), format('invoice-%s', telegram_id)
        FROM c RETURNING client_id, first_invoice_id
    ), op AS (
        INSERT INTO operations (client_id, operation_key, kind, status, completed_at)
        SELECT c.id, format('payment-registration:%s', c.id), 'register_client', 'completed', clock_timestamp()
        FROM c JOIN sub ON sub.client_id=c.id RETURNING id, client_id
    ), flow AS (
        INSERT INTO registration_flows (operation_id, bot_id, token)
        SELECT id, $1, repeat('p', 20) || id::text FROM op RETURNING operation_id
    ), steps AS (
        INSERT INTO operation_steps (operation_id, step, request, external_id, confirmed_at)
        SELECT op.id, step, '{}'::jsonb,
            CASE WHEN step='customer_create' THEN c.invoice_ninja_id ELSE sub.first_invoice_id END,
            clock_timestamp()
        FROM op JOIN flow ON flow.operation_id=op.id JOIN c ON c.id=op.client_id
            JOIN sub ON sub.client_id=c.id
            CROSS JOIN (VALUES ('customer_create'), ('invoice_create'), ('invoice_issue')) v(step)
        RETURNING operation_id
    ) SELECT c.id FROM c WHERE EXISTS (
        SELECT 1 FROM steps JOIN op ON op.id=steps.operation_id WHERE op.client_id=c.id
    ) ORDER BY c.id`, botID, start, count)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil || len(ids) != count {
		t.Fatalf("seed payment accounts: count=%d err=%v", len(ids), err)
	}
	return ids
}

func paymentSQL(t *testing.T, s *Store, ctx context.Context, sql string, args ...any) {
	t.Helper()
	var ok bool
	if err := s.pool.QueryRow(ctx, sql, args...).Scan(&ok); err != nil || !ok {
		t.Fatalf("payment invariant failed: ok=%v err=%v", ok, err)
	}
}

func paymentExec(t *testing.T, s *Store, ctx context.Context, sql string, args ...any) {
	t.Helper()
	if _, err := s.pool.Exec(ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
}

func seedPaymentChecks(t *testing.T, s *Store, ctx context.Context, botID int64, want int) {
	t.Helper()
	if n, err := s.SeedPaymentChecks(ctx, botID); err != nil || n != want {
		t.Fatalf("seed checks: got=%d want=%d err=%v", n, want, err)
	}
}

func paymentWake(t *testing.T, s *Store, ctx context.Context, botID int64, body string, want bool) {
	t.Helper()
	if fresh, err := s.RecordPaymentWake(ctx, botID, sha256.Sum256([]byte(body))); err != nil || fresh != want {
		t.Fatalf("record wake: fresh=%v want=%v err=%v", fresh, want, err)
	}
}

func claimSinglePayment(t *testing.T, s *Store, ctx context.Context, botID int64) (payments.Claim, payments.Snapshot) {
	t.Helper()
	claims, err := s.ClaimPaymentChecks(ctx, botID, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("single payment claim: count=%d err=%v", len(claims), err)
	}
	snapshot, err := s.LoadPaymentSnapshot(ctx, claims[0])
	if err != nil {
		t.Fatal(err)
	}
	return claims[0], snapshot
}

func duePayment(t *testing.T, s *Store, ctx context.Context, clientID int64) {
	t.Helper()
	paymentExec(t, s, ctx, `UPDATE payment_checks SET next_check_at=clock_timestamp()-interval '1 second'
        WHERE client_id=$1`, clientID)
}

// Evidence is marshaled in exactly the same canonical form checked by Proof.
// Allocations sum exactly, and NextBillingDate handles short calendar months.
func paymentProof(t *testing.T, snapshot payments.Snapshot, date, amount string) *payments.Proof {
	t.Helper()
	next, err := billing.NextBillingDate(date)
	if err != nil {
		t.Fatal(err)
	}
	p := &payments.Proof{Evidence: payments.Evidence{
		InvoiceID: snapshot.InvoiceID, CustomerID: snapshot.CustomerID,
		Amount: amount, CurrencyID: "1", PaymentDate: date, NextInvoiceDate: next,
		Allocations: []payments.Allocation{{PaymentID: "payment-1", AllocationID: "allocation-1", Amount: amount, Date: date}},
	}, InvoiceNumber: "PAY-001"}
	p.JSON, err = json.Marshal(p.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	p.Hash = sha256.Sum256(p.JSON)
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	return p
}

func paymentNotices(snapshot payments.Snapshot) payments.Notices {
	return payments.Notices{
		Confirmed: []queue.Draft{{BotID: snapshot.BotID, ChatID: snapshot.ChatID, Key: "confirmed:" + snapshot.InvoiceID, Text: "Payment recorded"}},
		Review:    []queue.Draft{{BotID: snapshot.BotID, ChatID: snapshot.ChatID, Key: "review:" + snapshot.InvoiceID, Text: "Payment needs manual review"}},
	}
}

// Pause after client selection, before the fresh eligibility statement. This
// lets the fixture commit a changed queue row after the first MVCC snapshot.
type paymentClaimTrace struct {
	ready, resume chan struct{}
	once          sync.Once
}

func (t *paymentClaimTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "SELECT client_id FROM subscriptions WHERE client_id = ANY") {
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

func (*paymentClaimTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestPaymentsValidateWithoutDatabase(t *testing.T) {
	s := &Store{}
	ctx := context.Background()
	claim := payments.Claim{Lease: queue.Lease{ID: 1, Attempt: 1}, BotID: 123}
	for _, bot := range []int64{0, -1} {
		if _, err := s.RecordPaymentWake(ctx, bot, [32]byte{}); err == nil {
			t.Fatal("accepted invalid wake bot")
		}
		if _, err := s.SeedPaymentChecks(ctx, bot); err == nil {
			t.Fatal("accepted invalid seed bot")
		}
	}
	for _, limit := range []int{-1, 0, 5, 100} {
		if _, err := s.ClaimPaymentChecks(ctx, 123, limit); err == nil {
			t.Fatal("accepted unbounded claim")
		}
	}
	if err := s.DeferPaymentCheck(ctx, claim, -time.Nanosecond, 503); err == nil {
		t.Fatal("accepted negative retry")
	}
	for _, interval := range []time.Duration{0, time.Second - 1, time.Hour + 1} {
		if err := s.FinishPaymentCheck(ctx, claim, payments.Snapshot{}, payments.Decision{Kind: payments.Pending}, payments.Notices{}, interval); err == nil {
			t.Fatal("accepted invalid interval")
		}
	}
	for _, notices := range []payments.Notices{
		{Confirmed: make([]queue.Draft, 101)}, {Review: make([]queue.Draft, 101)},
		{Review: []queue.Draft{{BotID: 123, ChatID: 1, Key: "unused", Text: ""}}},
	} {
		if err := s.FinishPaymentCheck(ctx, claim, payments.Snapshot{}, payments.Decision{Kind: payments.Pending}, notices, time.Minute); err == nil {
			t.Fatal("accepted invalid unused notice group")
		}
	}
	if err := s.FinishPaymentCheck(ctx, claim, payments.Snapshot{}, payments.Decision{Kind: payments.Verified}, payments.Notices{}, time.Minute); err == nil {
		t.Fatal("accepted missing proof")
	}
	for _, sentinel := range []error{queue.ErrLeaseLost, queue.ErrConflict, context.Canceled, context.DeadlineExceeded} {
		if !errors.Is(paymentStageError("local stage", fmt.Errorf("private detail: %w", sentinel)), sentinel) {
			t.Fatal("safe error classification lost")
		}
	}
	redacted := paymentStageError("local stage", &pgconn.PgError{Code: "23514", Message: "private message", Detail: "private evidence"})
	if redacted.Error() != "local stage" {
		t.Fatal("driver details exposed")
	}
	proof := paymentProof(t, payments.Snapshot{CustomerID: "customer", InvoiceID: "invoice"}, "2026-01-31", "125.25")
	proof.Allocations[0].Amount = "1"
	proof.JSON, _ = json.Marshal(proof.Evidence)
	proof.Hash = sha256.Sum256(proof.JSON)
	if proof.Validate() == nil {
		t.Fatal("proof fixture validation omitted allocation coverage")
	}
}

func TestPaymentsWakeSeedProvenanceAndConcurrentClaims(t *testing.T) {
	s, ctx := testStore(t)
	valid := seedPaymentAccounts(t, s, ctx, 123, 1000, 56)
	invalid := seedPaymentAccounts(t, s, ctx, 123, 2000, 6)
	other := seedPaymentAccounts(t, s, ctx, 456, 3000, 2)
	paymentExec(t, s, ctx, `WITH changed_flow AS (
        UPDATE registration_flows f SET bot_id=456 FROM operations o
        WHERE o.id=f.operation_id AND o.client_id=$1
    ), changed_steps AS (
        UPDATE operation_steps step SET external_id='wrong-provenance' FROM operations o
        WHERE o.id=step.operation_id AND ((o.client_id=$2 AND step.step='customer_create')
            OR (o.client_id=$3 AND step.step='invoice_create') OR (o.client_id=$4 AND step.step='invoice_issue'))
    ), changed_op AS (
        UPDATE operations SET status='pending', completed_at=NULL, next_attempt_at='infinity' WHERE client_id=$5
    ) UPDATE subscriptions SET current_invoice_id='' WHERE client_id=$6`,
		invalid[0], invalid[1], invalid[2], invalid[3], invalid[4], invalid[5])
	// An automatic import has no confirmed registration journal at all.
	paymentExec(t, s, ctx, `WITH c AS (
        INSERT INTO clients (telegram_id, chat_id, display_name, invoice_ninja_id)
        VALUES (4000,4000,'import','import-customer') RETURNING id
    ) INSERT INTO subscriptions (client_id, first_invoice_id, current_invoice_id)
        SELECT id, 'import-first', 'import-invoice' FROM c`)
	paymentWake(t, s, ctx, 123, "one", true)
	paymentWake(t, s, ctx, 123, "one", false)
	paymentWake(t, s, ctx, 456, "one", true)
	paymentWake(t, s, ctx, 123, "two", true)
	seedPaymentChecks(t, s, ctx, 123, 50)
	seedPaymentChecks(t, s, ctx, 123, 6)
	seedPaymentChecks(t, s, ctx, 123, 0)
	paymentSQL(t, s, ctx, `SELECT count(*)=56 AND bool_and(client_id=ANY($1::bigint[])) FROM payment_checks`, valid)
	paymentSQL(t, s, ctx, `SELECT (SELECT generation=2 FROM payment_wake_state WHERE bot_id=123)
        AND (SELECT generation=1 FROM payment_wake_state WHERE bot_id=456)
        AND (SELECT count(*)=3 FROM payment_processed_events)`)
	// Existing checks never migrate to another bot, even if a flow is changed.
	paymentExec(t, s, ctx, `UPDATE registration_flows f SET bot_id=456 FROM operations o
        WHERE o.id=f.operation_id AND o.client_id=$1`, valid[55])
	seedPaymentChecks(t, s, ctx, 456, 3) // two real accounts plus the changed invalid flow
	paymentSQL(t, s, ctx, `SELECT bot_id=123 FROM payment_checks WHERE client_id=$1`, valid[55])
	if claims, err := s.ClaimPaymentChecks(ctx, 789, 4); err != nil || len(claims) != 0 {
		t.Fatalf("cross-bot claims: %v", err)
	}
	// A held operation FIFO for this account cannot block payment observations.
	paymentExec(t, s, ctx, `INSERT INTO operations (client_id,operation_key,kind,next_attempt_at,last_error_code)
        VALUES ($1,'held-registration-fifo','held_registration','infinity',1001)`, valid[0])
	locked, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(locked)
	if _, err := locked.Exec(ctx, `SELECT id FROM clients WHERE id=$1 FOR NO KEY UPDATE`, valid[0]); err != nil {
		t.Fatal(err)
	}
	claims, err := s.ClaimPaymentChecks(ctx, 123, 1)
	if err != nil || len(claims) != 1 || claims[0].ID != valid[1] {
		t.Fatalf("did not skip locked payment client: claims=%v err=%v", claims, err)
	}
	if err := locked.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	type result struct {
		claims []payments.Claim
		err    error
	}
	results := make(chan result, 4)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			claims, err := s.ClaimPaymentChecks(ctx, 123, 4)
			results <- result{claims, err}
		})
	}
	wg.Wait()
	close(results)
	seen := map[int64]bool{claims[0].ID: true}
	for result := range results {
		if result.err != nil || len(result.claims) > 4 {
			t.Fatalf("competing claims: %v", result.err)
		}
		for _, claim := range result.claims {
			if seen[claim.ID] || claim.Attempt != 1 || claim.Generation != 2 || claim.BotID != 123 {
				t.Fatalf("duplicated or misbound lease: %+v", claim)
			}
			seen[claim.ID] = true
		}
	}
	if !seen[valid[0]] {
		t.Fatal("held operation FIFO blocked a payment claim")
	}
	paymentSQL(t, s, ctx, `SELECT bool_and(locked_until > clock_timestamp()+interval '90 seconds'
        AND locked_until <= clock_timestamp()+interval '2 minutes') FROM payment_checks WHERE attempts=1`)
	cross, err := s.ClaimPaymentChecks(ctx, 456, 4)
	if err != nil || len(cross) != 3 {
		t.Fatalf("bot-scoped claims: %v", err)
	}
	for _, claim := range cross {
		if claim.ID != invalid[0] && claim.ID != other[0] && claim.ID != other[1] {
			t.Fatal("claimed another bot's existing check")
		}
	}
}

func TestPaymentsAtomicProofImmutableDatesAndStickyReview(t *testing.T) {
	s, ctx := testStore(t)
	ids := seedPaymentAccounts(t, s, ctx, 123, 5000, 1)
	seedPaymentChecks(t, s, ctx, 123, 1)
	claim, snapshot := claimSinglePayment(t, s, ctx, 123)
	proof := paymentProof(t, snapshot, "2026-01-31", "125.25")
	verified := payments.Decision{Kind: payments.Verified, Proof: proof}
	notices := paymentNotices(snapshot)
	// The second draft fails after financial writes and the first insert.
	paymentExec(t, s, ctx, `ALTER TABLE outbox ADD CONSTRAINT reject_payment_notice CHECK (message_key <> 'rejected')`)
	rejected := notices
	rejected.Confirmed = append(append([]queue.Draft{}, notices.Confirmed...), queue.Draft{BotID: 999, ChatID: snapshot.ChatID, Key: "rejected", Text: "private notification"})
	if err := s.FinishPaymentCheck(ctx, claim, snapshot, verified, rejected, 5*time.Minute); err == nil || strings.Contains(err.Error(), "private") {
		t.Fatalf("database failure was ignored or exposed: %v", err)
	}
	paymentSQL(t, s, ctx, `SELECT (SELECT count(*)=0 FROM payment_confirmations)
        AND (SELECT count(*)=0 FROM outbox) AND (SELECT count(*)=0 FROM telegram_bots WHERE bot_id=999)
        AND (SELECT payment_date IS NULL AND next_invoice_date IS NULL FROM subscriptions WHERE client_id=$1)
        AND (SELECT locked_until>clock_timestamp() AND last_checked_at IS NULL FROM payment_checks WHERE client_id=$1)`, ids[0])
	// Idempotency conflicts must also roll back the earlier good draft.
	conflict := queue.Draft{BotID: 123, ChatID: snapshot.ChatID, Key: "existing", Text: "unchanged"}
	if _, err := s.EnqueueOutbox(ctx, conflict); err != nil {
		t.Fatal(err)
	}
	conflict.Text = "changed"
	conflicting := notices
	conflicting.Confirmed = append(append([]queue.Draft{}, notices.Confirmed...), conflict)
	if err := s.FinishPaymentCheck(ctx, claim, snapshot, verified, conflicting, time.Minute); !errors.Is(err, queue.ErrConflict) {
		t.Fatalf("outbox conflict accepted: %v", err)
	}
	paymentSQL(t, s, ctx, `SELECT (SELECT count(*)=0 FROM payment_confirmations)
        AND (SELECT count(*)=1 FROM outbox) AND (SELECT payment_date IS NULL FROM subscriptions)`)
	wrong := snapshot
	wrong.CustomerID = "other-customer"
	if err := s.FinishPaymentCheck(ctx, claim, snapshot, payments.Decision{Kind: payments.Verified, Proof: paymentProof(t, wrong, "2026-01-31", "125.25")}, notices, time.Minute); !errors.Is(err, queue.ErrConflict) {
		t.Fatalf("unbound customer proof accepted: %v", err)
	}
	wrong = snapshot
	wrong.InvoiceID = "other-invoice"
	if err := s.FinishPaymentCheck(ctx, claim, snapshot, payments.Decision{Kind: payments.Verified, Proof: paymentProof(t, wrong, "2026-01-31", "125.25")}, notices, time.Minute); !errors.Is(err, queue.ErrConflict) {
		t.Fatalf("unbound invoice proof accepted: %v", err)
	}
	if err := s.FinishPaymentCheck(ctx, claim, snapshot, verified, notices, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	paymentSQL(t, s, ctx, `SELECT pc.proof_hash=$2 AND pc.evidence=$3::jsonb
        AND pc.amount=125.25 AND pc.currency_id='1' AND pc.payment_date='2026-01-31'
        AND pc.next_invoice_date='2026-02-28' AND NOT pc.review_required
        AND s.payment_date=pc.payment_date AND s.next_invoice_date=pc.next_invoice_date
        AND s.state='awaiting_payment' AND NOT s.desired_access AND s.recurring_id IS NULL
        AND c.sui_id IS NULL AND (SELECT count(*)=1 AND bool_and(kind='register_client') FROM operations)
        AND (SELECT count(*)=2 FROM outbox)
        FROM payment_confirmations pc JOIN subscriptions s USING(client_id) JOIN clients c ON c.id=pc.client_id
        WHERE pc.client_id=$1`, ids[0], proof.Hash[:], string(proof.JSON))
	// Matching proof does not rewrite even locally advanced billing dates, nor
	// enqueue a second message when given an entirely new notification key.
	paymentExec(t, s, ctx, `UPDATE subscriptions SET payment_date='2026-03-01', next_invoice_date='2026-04-01' WHERE client_id=$1`, ids[0])
	duePayment(t, s, ctx, ids[0])
	claim, snapshot = claimSinglePayment(t, s, ctx, 123)
	repeat := paymentNotices(snapshot)
	repeat.Confirmed[0].Key = "must-not-be-enqueued"
	if err := s.FinishPaymentCheck(ctx, claim, snapshot, verified, repeat, time.Minute); err != nil {
		t.Fatal(err)
	}
	paymentSQL(t, s, ctx, `SELECT (SELECT count(*)=2 FROM outbox)
        AND (SELECT payment_date='2026-03-01' AND next_invoice_date='2026-04-01' FROM subscriptions)`)
	changed := payments.Decision{Kind: payments.Verified, Proof: paymentProof(t, snapshot, "2026-02-01", "150")}
	for i, decision := range []payments.Decision{
		{Kind: payments.Pending}, {Kind: payments.NeedsReview, Code: payments.ReviewInvoice}, changed, verified,
	} {
		if i > 0 && i < 3 {
			// Reset the fixture only, isolating each independent regression from
			// a previously clean confirmation. No application API clears review.
			paymentExec(t, s, ctx, `UPDATE payment_confirmations SET review_required=false, review_code=0`)
		}
		duePayment(t, s, ctx, ids[0])
		claim, snapshot = claimSinglePayment(t, s, ctx, 123)
		if err := s.FinishPaymentCheck(ctx, claim, snapshot, decision, notices, time.Minute); err != nil {
			t.Fatal(err)
		}
		paymentSQL(t, s, ctx, `SELECT review_required AND review_code=2007 FROM payment_confirmations`)
	}
	paymentSQL(t, s, ctx, `SELECT pc.review_required AND pc.review_code=2007
        AND pc.proof_hash=$2 AND pc.evidence=$3::jsonb AND pc.payment_date='2026-01-31'
        AND pc.next_invoice_date='2026-02-28' AND pc.amount=125.25
        AND s.payment_date='2026-03-01' AND s.next_invoice_date='2026-04-01'
        AND s.state='awaiting_payment' AND NOT s.desired_access AND s.recurring_id IS NULL
        AND c.sui_id IS NULL AND (SELECT count(*)=1 FROM operations)
        AND (SELECT count(*)=3 FROM outbox) AND (SELECT review_code=2007 AND last_error_code=0 FROM payment_checks)
        FROM payment_confirmations pc JOIN subscriptions s USING(client_id) JOIN clients c ON c.id=pc.client_id
        WHERE pc.client_id=$1`, ids[0], proof.Hash[:], string(proof.JSON))
	// First proof for a new current invoice must not roll local dates backward.
	paymentExec(t, s, ctx, `UPDATE subscriptions SET current_invoice_id='new-current-invoice' WHERE client_id=$1`, ids[0])
	duePayment(t, s, ctx, ids[0])
	claim, snapshot = claimSinglePayment(t, s, ctx, 123)
	newNotices := paymentNotices(snapshot)
	older := payments.Decision{Kind: payments.Verified, Proof: paymentProof(t, snapshot, "2026-02-15", "125.25")}
	if err := s.FinishPaymentCheck(ctx, claim, snapshot, older, newNotices, time.Minute); err != nil {
		t.Fatal(err)
	}
	paymentSQL(t, s, ctx, `SELECT (SELECT count(*)=1 FROM payment_confirmations)
        AND (SELECT review_code=2007 FROM payment_checks)
        AND (SELECT payment_date='2026-03-01' AND next_invoice_date='2026-04-01' FROM subscriptions)
        AND (SELECT count(*)=4 FROM outbox)`)
	for _, decision := range []payments.Decision{{Kind: payments.NeedsReview, Code: payments.ReviewCurrency}, {Kind: payments.Pending}} {
		duePayment(t, s, ctx, ids[0])
		claim, snapshot = claimSinglePayment(t, s, ctx, 123)
		if err := s.FinishPaymentCheck(ctx, claim, snapshot, decision, newNotices, time.Minute); err != nil {
			t.Fatal(err)
		}
		paymentSQL(t, s, ctx, `SELECT review_code=$1 AND locked_until IS NULL AND isfinite(next_check_at)
            AND last_checked_at IS NOT NULL AND last_error_code=0
            AND (SELECT count(*)=1 FROM payment_confirmations) AND (SELECT count(*)=4 FROM outbox)
            FROM payment_checks`, decision.Code)
	}
	// An invoice-global confirmation may never be adopted from another account.
	paymentExec(t, s, ctx, `WITH changed AS (UPDATE subscriptions SET current_invoice_id=first_invoice_id WHERE client_id=$1)
        UPDATE payment_confirmations SET customer_id='another-customer' WHERE client_id=$1`, ids[0])
	duePayment(t, s, ctx, ids[0])
	claim, snapshot = claimSinglePayment(t, s, ctx, 123)
	if err := s.FinishPaymentCheck(ctx, claim, snapshot, verified, notices, time.Minute); !errors.Is(err, queue.ErrConflict) {
		t.Fatalf("adopted another customer confirmation: %v", err)
	}
}

func TestPaymentsFencingWakeRacesSnapshotAndFinalClock(t *testing.T) {
	s, ctx := testStore(t)
	ids := seedPaymentAccounts(t, s, ctx, 123, 6000, 1)
	seedPaymentChecks(t, s, ctx, 123, 1)
	paymentWake(t, s, ctx, 123, "first", true)
	claim, snapshot := claimSinglePayment(t, s, ctx, 123)
	notices := paymentNotices(snapshot)
	pending := payments.Decision{Kind: payments.Pending}
	for _, wrong := range []payments.Claim{
		{Lease: queue.Lease{ID: claim.ID, Attempt: claim.Attempt + 1}, BotID: claim.BotID, Generation: claim.Generation},
		{Lease: claim.Lease, BotID: 456, Generation: claim.Generation},
		{Lease: claim.Lease, BotID: claim.BotID, Generation: claim.Generation + 1},
	} {
		if _, err := s.LoadPaymentSnapshot(ctx, wrong); !errors.Is(err, queue.ErrLeaseLost) {
			t.Fatalf("snapshot accepted wrong lease: %v", err)
		}
		if err := s.DeferPaymentCheck(ctx, wrong, time.Minute, 503); !errors.Is(err, queue.ErrLeaseLost) {
			t.Fatalf("retry accepted wrong lease: %v", err)
		}
		if err := s.FinishPaymentCheck(ctx, wrong, snapshot, pending, notices, time.Minute); !errors.Is(err, queue.ErrLeaseLost) {
			t.Fatalf("finish accepted wrong lease: %v", err)
		}
	}
	paymentExec(t, s, ctx, `UPDATE payment_checks SET locked_until=clock_timestamp()-interval '1 second' WHERE client_id=$1`, ids[0])
	if _, err := s.LoadPaymentSnapshot(ctx, claim); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("expired snapshot accepted: %v", err)
	}
	if err := s.DeferPaymentCheck(ctx, claim, time.Minute, 503); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("expired retry accepted: %v", err)
	}
	if err := s.FinishPaymentCheck(ctx, claim, snapshot, pending, notices, time.Minute); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("expired finish accepted: %v", err)
	}
	old := claim
	claim, snapshot = claimSinglePayment(t, s, ctx, 123)
	if claim.Attempt != old.Attempt+1 {
		t.Fatal("attempt fencing token reset on reclaim")
	}
	if err := s.DeferPaymentCheck(ctx, old, time.Minute, 503); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("superseded owner accepted: %v", err)
	}
	paymentWake(t, s, ctx, 123, "during-retry", true)
	if claims, err := s.ClaimPaymentChecks(ctx, 123, 1); err != nil || len(claims) != 0 {
		t.Fatalf("wake stole live lease: %v", err)
	}
	if err := s.DeferPaymentCheck(ctx, claim, time.Hour, 503); err != nil {
		t.Fatal(err)
	}
	paymentSQL(t, s, ctx, `SELECT observed_generation=1 AND locked_until IS NULL
        AND last_error_code=503 AND review_code=0 AND isfinite(next_check_at)
        AND next_check_at>clock_timestamp()+interval '50 minutes'
        AND (SELECT count(*)=0 FROM outbox) AND (SELECT count(*)=0 FROM payment_confirmations)
        FROM payment_checks`)
	if waiting, err := s.ClaimPaymentChecks(ctx, 123, 1); err != nil || len(waiting) != 0 {
		t.Fatal("new wake bypassed retry cooldown")
	}
	duePayment(t, s, ctx, claim.ID)
	claim, snapshot = claimSinglePayment(t, s, ctx, 123)
	if claim.Generation != 2 {
		t.Fatal("new wake lost by deferred old generation")
	}
	paymentWake(t, s, ctx, 123, "during-finish", true)
	if err := s.FinishPaymentCheck(ctx, claim, snapshot, pending, notices, time.Hour); err != nil {
		t.Fatal(err)
	}
	paymentSQL(t, s, ctx, `SELECT observed_generation=2 AND last_error_code=0
        AND last_checked_at IS NOT NULL AND isfinite(next_check_at) FROM payment_checks`)
	claim, snapshot = claimSinglePayment(t, s, ctx, 123)
	if claim.Generation != 3 {
		t.Fatal("new wake lost by completion of old generation")
	}
	// Every comparable snapshot field participates in the optimistic fence.
	for _, mutate := range []func(*payments.Snapshot){
		func(s *payments.Snapshot) { s.ClientID++ }, func(s *payments.Snapshot) { s.TelegramID++ },
		func(s *payments.Snapshot) { s.ChatID++ }, func(s *payments.Snapshot) { s.BotID++ },
		func(s *payments.Snapshot) { s.CustomerID += "changed" }, func(s *payments.Snapshot) { s.InvoiceID += "changed" },
		func(s *payments.Snapshot) { s.FirstInvoiceID += "changed" }, func(s *payments.Snapshot) { s.RecurringID += "changed" },
		func(s *payments.Snapshot) { s.SUIID += "changed" }, func(s *payments.Snapshot) { s.State = "active" },
		func(s *payments.Snapshot) { s.DesiredAccess = !s.DesiredAccess },
		func(s *payments.Snapshot) { s.PaymentDate = "2026-01-01" }, func(s *payments.Snapshot) { s.NextInvoiceDate = "2026-02-01" },
	} {
		stale := snapshot
		mutate(&stale)
		if err := s.FinishPaymentCheck(ctx, claim, stale, pending, payments.Notices{}, time.Minute); !errors.Is(err, queue.ErrConflict) {
			t.Fatalf("incomplete snapshot comparison: %v", err)
		}
	}
	paymentExec(t, s, ctx, `WITH c AS (UPDATE clients SET sui_id='existing-sui' WHERE id=$1)
        UPDATE subscriptions SET state='active', desired_access=true, recurring_id='existing-recurring' WHERE client_id=$1`, ids[0])
	if err := s.FinishPaymentCheck(ctx, claim, snapshot, pending, notices, time.Minute); !errors.Is(err, queue.ErrConflict) {
		t.Fatalf("stale actual account accepted: %v", err)
	}
	snapshot, err := s.LoadPaymentSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	proof := paymentProof(t, snapshot, "2026-04-30", "125.25")
	verified := payments.Decision{Kind: payments.Verified, Proof: proof}
	// Deterministically move the DB deadline behind the clock after an outbox
	// insertion. The final guard must undo the proof, dates and inserted notice.
	paymentExec(t, s, ctx, `CREATE FUNCTION expire_payment_during_notice() RETURNS trigger LANGUAGE plpgsql AS $$
        BEGIN UPDATE payment_checks SET locked_until=clock_timestamp()-interval '1 second'; RETURN NEW; END $$`)
	paymentExec(t, s, ctx, `CREATE TRIGGER expire_payment_notice AFTER INSERT ON outbox
        FOR EACH ROW EXECUTE FUNCTION expire_payment_during_notice()`)
	if err := s.FinishPaymentCheck(ctx, claim, snapshot, verified, notices, time.Minute); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("deadline crossing appended a notification: %v", err)
	}
	paymentSQL(t, s, ctx, `SELECT (SELECT count(*)=0 FROM payment_confirmations)
        AND (SELECT count(*)=0 FROM outbox)
        AND (SELECT payment_date IS NULL AND next_invoice_date IS NULL AND state='active'
            AND desired_access AND recurring_id='existing-recurring' FROM subscriptions)
        AND (SELECT locked_until>clock_timestamp() FROM payment_checks)`)
	paymentExec(t, s, ctx, `DROP TRIGGER expire_payment_notice ON outbox`)
	// An unused review bot must be prepared before locking the client as well.
	trace := &botInsertTrace{started: make(chan struct{})}
	cfg := s.pool.Config()
	cfg.MaxConns = 1
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	firstWebhook, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(firstWebhook)
	if _, err := firstWebhook.Exec(ctx, `INSERT INTO telegram_bots (bot_id) VALUES (999)`); err != nil {
		t.Fatal(err)
	}
	notices.Review = []queue.Draft{{BotID: 999, ChatID: snapshot.ChatID, Key: "unused-review", Text: "Manual review"}}
	finishCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- (&Store{pool: pool}).FinishPaymentCheck(finishCtx, claim, snapshot, verified, notices, time.Minute)
	}()
	select {
	case <-trace.started:
	case <-finishCtx.Done():
		t.Fatal("finish never prepared both notification bot groups")
	}
	if _, err := firstWebhook.Exec(ctx, `SELECT id FROM clients WHERE id=$1 FOR NO KEY UPDATE NOWAIT`, ids[0]); err != nil {
		t.Fatalf("finish locked client before unused review bot: %v", err)
	}
	if err := firstWebhook.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-finishCtx.Done():
		t.Fatal("finish did not resume after first webhook commit")
	}
	paymentSQL(t, s, ctx, `SELECT (SELECT count(*)=1 FROM payment_confirmations)
        AND (SELECT count(*)=1 FROM outbox)
        AND (SELECT state='active' AND desired_access AND recurring_id='existing-recurring'
            AND payment_date='2026-04-30' AND next_invoice_date='2026-05-30' FROM subscriptions)
        AND (SELECT sui_id='existing-sui' FROM clients)
        AND (SELECT observed_generation=3 AND isfinite(next_check_at) AND next_check_at>clock_timestamp()
            AND locked_until IS NULL FROM payment_checks)`)
	if err := s.FinishPaymentCheck(ctx, claim, snapshot, verified, notices, time.Minute); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("repeat completion escaped fencing: %v", err)
	}
	if claims, err := s.ClaimPaymentChecks(ctx, 123, 1); err != nil || len(claims) != 0 {
		t.Fatalf("finite periodic delay ignored: %v", err)
	}
	duePayment(t, s, ctx, ids[0])
	next, _ := claimSinglePayment(t, s, ctx, 123)
	if next.Attempt != claim.Attempt+1 || next.Generation != claim.Generation {
		t.Fatal("periodic polling requires a new wake or reset attempts")
	}
	if err := s.DeferPaymentCheck(ctx, next, 0, 503); err != nil {
		t.Fatal(err)
	}
	freshTrace := &paymentClaimTrace{ready: make(chan struct{}), resume: make(chan struct{})}
	freshCfg := s.pool.Config()
	freshCfg.MaxConns = 1
	freshCfg.ConnConfig.Tracer = freshTrace
	freshPool, err := pgxpool.NewWithConfig(ctx, freshCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer freshPool.Close()
	freshCtx, freshCancel := context.WithTimeout(ctx, 10*time.Second)
	defer freshCancel()
	type freshResult struct {
		claims []payments.Claim
		err    error
	}
	freshDone := make(chan freshResult, 1)
	go func() {
		claims, err := (&Store{pool: freshPool}).ClaimPaymentChecks(freshCtx, 123, 1)
		freshDone <- freshResult{claims, err}
	}()
	select {
	case <-freshTrace.ready:
	case <-freshCtx.Done():
		t.Fatal("claim did not separate client locking from eligibility update")
	}
	paymentExec(t, s, ctx, `UPDATE payment_checks SET next_check_at=clock_timestamp()+interval '1 hour'
        WHERE client_id=$1`, ids[0])
	close(freshTrace.resume)
	select {
	case result := <-freshDone:
		if result.err != nil || len(result.claims) != 0 {
			t.Fatalf("claim reused stale eligibility snapshot: %v", result.err)
		}
	case <-freshCtx.Done():
		t.Fatal("claim failed to finish fresh eligibility check")
	}
	paymentSQL(t, s, ctx, `SELECT locked_until IS NULL AND attempts=$1 FROM payment_checks`, next.Attempt)
}
