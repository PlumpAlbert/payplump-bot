package postgres

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var _ activation.Store = (*Store)(nil)

// Eligibility is rechecked in a separate statement after account locks. The
// anti-join is deliberately before LIMIT, including completed/held activations.
const activationSeedEligible = paymentProvenance + `
    AND c.telegram_id = ANY($2::bigint[])
    AND c.sui_id IS NULL AND s.recurring_id IS NULL
    AND s.state = 'awaiting_payment' AND NOT s.desired_access
    AND s.first_invoice_id = s.current_invoice_id
    AND p.bot_id = $1 AND p.review_code = 0
    AND pc.client_id = c.id AND pc.bot_id = $1
    AND pc.customer_id = c.invoice_ninja_id
    AND pc.payment_date = s.payment_date AND pc.next_invoice_date = s.next_invoice_date
    AND NOT pc.review_required AND pc.review_code = 0
    AND NOT EXISTS (SELECT 1 FROM operations existing WHERE existing.client_id = c.id
        AND existing.kind = 'activate_first_subscription')`

func prepareActivationBot(ctx context.Context, tx pgx.Tx, botID int64) error {
	_, err := tx.Exec(ctx, `INSERT INTO telegram_bots (bot_id) VALUES ($1) ON CONFLICT DO NOTHING`, botID)
	return err
}

func (s *Store) SeedActivations(ctx context.Context, botID int64, allowed []int64, policyHash [32]byte) (count int, err error) {
	defer func() { err = activationStageError("seed activations", err) }()
	if botID <= 0 || len(allowed) > 1000 || policyHash == ([32]byte{}) {
		return 0, errors.New("invalid activation seed policy")
	}
	for _, id := range allowed {
		if id <= 0 {
			return 0, errors.New("invalid activation allowlist")
		}
	}
	if len(allowed) == 0 {
		return 0, nil
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, err
	}
	defer rollback(tx)
	if err := prepareActivationBot(ctx, tx, botID); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `SELECT c.id FROM clients c
        JOIN subscriptions s ON s.client_id = c.id
        JOIN payment_checks p ON p.client_id = c.id
        JOIN payment_confirmations pc ON pc.invoice_id = s.current_invoice_id
        WHERE `+activationSeedEligible+`
        ORDER BY c.id LIMIT 50 FOR NO KEY UPDATE OF c SKIP LOCKED`, botID, allowed)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, tx.Commit(ctx)
	}
	if err := lockPaymentSubscriptions(ctx, tx, ids); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `SELECT client_id FROM payment_checks WHERE client_id = ANY($1::bigint[])
        ORDER BY client_id FOR UPDATE`, ids); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `SELECT pc.invoice_id FROM payment_confirmations pc
        JOIN subscriptions s ON s.current_invoice_id = pc.invoice_id
        WHERE s.client_id = ANY($1::bigint[]) ORDER BY pc.invoice_id FOR UPDATE OF pc`, ids); err != nil {
		return 0, err
	}
	rows, err = tx.Query(ctx, `SELECT c.id, pc.client_id, pc.bot_id, pc.invoice_id, pc.customer_id, pc.invoice_number,
        pc.amount::text, pc.currency_id, pc.payment_date::text, pc.next_invoice_date::text, pc.proof_hash,
        pc.evidence, pc.review_required, pc.review_code FROM clients c
        JOIN subscriptions s ON s.client_id = c.id
        JOIN payment_checks p ON p.client_id = c.id
        JOIN payment_confirmations pc ON pc.invoice_id = s.current_invoice_id
        WHERE c.id = ANY($3::bigint[]) AND `+activationSeedEligible+` ORDER BY c.id`, botID, allowed, ids)
	if err != nil {
		return 0, err
	}
	type candidate struct {
		clientID     int64
		confirmation activationConfirmation
	}
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
		var candidate candidate
		c := &candidate.confirmation
		err := row.Scan(&candidate.clientID, &c.clientID, &c.botID, &c.invoiceID, &c.customerID, &c.number,
			&c.amount, &c.currency, &c.paid, &c.next, &c.hash, &c.evidence, &c.review, &c.reviewCode)
		return candidate, err
	})
	if err != nil {
		return 0, err
	}
	var clientIDs []int64
	var invoices, tokens []string
	var hashes [][]byte
	for _, c := range candidates {
		proof, err := c.confirmation.proof()
		if err != nil {
			continue // Malformed evidence never creates a flow.
		}
		clientIDs = append(clientIDs, c.clientID)
		invoices = append(invoices, proof.InvoiceID)
		tokens = append(tokens, rand.Text())
		hashes = append(hashes, proof.Hash[:])
	}
	if len(clientIDs) != 0 {
		tag, err := tx.Exec(ctx, `WITH candidates AS (
            SELECT * FROM unnest($1::bigint[], $2::text[], $3::text[], $4::bytea[])
                AS c(client_id, invoice_id, token, proof_hash)
        ), inserted AS (
            INSERT INTO operations (client_id, operation_key, kind)
            SELECT client_id, 'first-activation:' || client_id::text, 'activate_first_subscription'
            FROM candidates ORDER BY client_id ON CONFLICT DO NOTHING RETURNING id, client_id
        ) INSERT INTO activation_flows (operation_id, bot_id, token, invoice_id, proof_hash, policy_hash)
            SELECT o.id, $5, c.token, c.invoice_id, c.proof_hash, $6
            FROM inserted o JOIN candidates c ON c.client_id=o.client_id`, clientIDs, invoices, tokens, hashes, botID, policyHash[:])
		if err != nil {
			return 0, err
		}
		count = int(tag.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return count, nil
}

// Claims serialize on exactly the same client locks and FIFO as ClaimOperations,
// but their source-bot join is mandatory. No held/unsupported head is bypassed.
func (s *Store) ClaimActivations(ctx context.Context, botID int64, limit int) (claims []queue.Operation, err error) {
	defer func() { err = activationStageError("claim activations", err) }()
	if botID <= 0 || limit < 1 || limit > 4 {
		return nil, errors.New("invalid activation claim bounds")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	defer rollback(tx)
	if err := prepareActivationBot(ctx, tx, botID); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT c.id FROM clients c
        JOIN LATERAL (
            SELECT id, kind, status, next_attempt_at, locked_until FROM operations
            WHERE client_id = c.id AND status IN ('pending', 'running') ORDER BY id LIMIT 1
        ) head ON true
        JOIN activation_flows f ON f.operation_id = head.id AND f.bot_id = $1
        WHERE head.kind = 'activate_first_subscription'
            AND ((head.status = 'pending' AND head.next_attempt_at <= clock_timestamp())
                OR (head.status = 'running' AND (head.locked_until IS NULL OR head.locked_until <= clock_timestamp())))
        ORDER BY head.id LIMIT $2 FOR NO KEY UPDATE OF c SKIP LOCKED`, botID, limit)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, err
	}
	if len(ids) != 0 {
		// Read Committed: this statement MUST start after all client locks.
		rows, err := tx.Query(ctx, `WITH heads AS (
            SELECT DISTINCT ON (client_id) id, kind, status, next_attempt_at, locked_until
            FROM operations WHERE client_id = ANY($2::bigint[]) AND status IN ('pending', 'running')
            ORDER BY client_id, id
        ) UPDATE operations o SET status = 'running', attempts = o.attempts + 1,
            locked_until = clock_timestamp() + interval '2 minutes'
        FROM heads head JOIN activation_flows f ON f.operation_id = head.id AND f.bot_id = $1
        WHERE o.id = head.id AND head.kind = 'activate_first_subscription'
            AND ((head.status = 'pending' AND head.next_attempt_at <= clock_timestamp())
                OR (head.status = 'running' AND (head.locked_until IS NULL OR head.locked_until <= clock_timestamp())))
        RETURNING o.id, o.attempts, o.client_id, o.kind`, botID, ids)
		if err != nil {
			return nil, err
		}
		claims, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (queue.Operation, error) {
			var claim queue.Operation
			err := row.Scan(&claim.ID, &claim.Attempt, &claim.ClientID, &claim.Kind)
			return claim, err
		})
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return claims, nil
}

// Lease reads use the database's actual clock, never transaction_timestamp().
// Call again as the last action before commit, including no-op replays.
func readActivationLease(ctx context.Context, tx pgx.Tx, lease queue.Lease) (int64, error) {
	var botID int64
	err := tx.QueryRow(ctx, `SELECT f.bot_id FROM operations o
        JOIN activation_flows f ON f.operation_id = o.id
        WHERE o.id = $1 AND o.attempts = $2 AND o.kind = 'activate_first_subscription'
            AND o.status = 'running' AND o.locked_until > clock_timestamp()
        FOR UPDATE OF o`, lease.ID, lease.Attempt).Scan(&botID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, queue.ErrLeaseLost
	}
	return botID, err
}

// All queues share client -> subscription -> payment check -> original payment
// confirmation -> operation -> flow/steps -> global external-write barrier.
// Initial MVCC lookups only locate locks; they never authorize a write.
func lockActivation(ctx context.Context, tx pgx.Tx, lease queue.Lease) (int64, error) {
	if lease.ID <= 0 || lease.Attempt <= 0 {
		return 0, queue.ErrLeaseLost
	}
	var clientID int64
	var invoiceID string
	err := tx.QueryRow(ctx, `SELECT o.client_id, f.invoice_id FROM operations o
        JOIN activation_flows f ON f.operation_id = o.id
        WHERE o.id = $1 AND o.kind = 'activate_first_subscription'`, lease.ID).Scan(&clientID, &invoiceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, queue.ErrLeaseLost
	}
	if err != nil {
		return 0, err
	}
	if err := lockOperationClient(ctx, tx, lease.ID); err != nil {
		return 0, err
	}
	for _, statement := range []string{
		`SELECT client_id FROM subscriptions WHERE client_id = $1 FOR NO KEY UPDATE`,
		`SELECT client_id FROM payment_checks WHERE client_id = $1 FOR UPDATE`,
	} {
		var id int64
		if err := tx.QueryRow(ctx, statement, clientID).Scan(&id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return 0, queue.ErrConflict
			}
			return 0, err
		}
	}
	var original string
	if err := tx.QueryRow(ctx, `SELECT invoice_id FROM payment_confirmations WHERE invoice_id = $1 FOR UPDATE`, invoiceID).Scan(&original); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, queue.ErrConflict
		}
		return 0, err
	}
	botID, err := readActivationLease(ctx, tx, lease)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `SELECT operation_id FROM activation_flows WHERE operation_id = $1 FOR UPDATE`, lease.ID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `SELECT step FROM activation_steps WHERE operation_id = $1 ORDER BY step FOR UPDATE`, lease.ID); err != nil {
		return 0, err
	}
	return botID, nil
}

type activationConfirmation struct {
	clientID, botID                                             int64
	invoiceID, customerID, number, amount, currency, paid, next string
	hash, evidence                                              []byte
	review                                                      bool
	reviewCode                                                  int
}

func readActivationConfirmation(ctx context.Context, reader paymentSnapshotReader, invoiceID string) (activationConfirmation, error) {
	var c activationConfirmation
	err := reader.QueryRow(ctx, `SELECT client_id, bot_id, invoice_id, customer_id, invoice_number,
        amount::text, currency_id, payment_date::text, next_invoice_date::text, proof_hash,
        evidence, review_required, review_code FROM payment_confirmations WHERE invoice_id = $1`, invoiceID).
		Scan(&c.clientID, &c.botID, &c.invoiceID, &c.customerID, &c.number, &c.amount, &c.currency,
			&c.paid, &c.next, &c.hash, &c.evidence, &c.review, &c.reviewCode)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, queue.ErrConflict
	}
	return c, err
}

// jsonb normalizes whitespace and key order. Hash ONLY the decoded, re-marshaled
// Evidence, not jsonb::text; also bind every duplicated financial/owner column.
func (c activationConfirmation) proof() (payments.Proof, error) {
	var p payments.Proof
	if json.Unmarshal(c.evidence, &p.Evidence) != nil {
		return p, queue.ErrConflict
	}
	canonical, err := json.Marshal(p.Evidence)
	if err != nil {
		return p, queue.ErrConflict
	}
	p.JSON, p.Hash, p.InvoiceNumber = canonical, sha256.Sum256(canonical), c.number
	if len(c.hash) != 32 || !bytes.Equal(p.Hash[:], c.hash) || p.InvoiceID != c.invoiceID || p.CustomerID != c.customerID ||
		p.Amount != c.amount || p.CurrencyID != c.currency || p.PaymentDate != c.paid || p.NextInvoiceDate != c.next || p.Validate() != nil {
		return p, queue.ErrConflict
	}
	return p, nil
}

func readActivationSnapshot(ctx context.Context, tx pgx.Tx, lease queue.Lease, botID int64) (activation.Snapshot, error) {
	var snapshot activation.Snapshot
	var provenance bool
	var invoiceID string
	var hash, policy []byte
	var checkBot int64
	var checkReview int
	a := &snapshot.Account
	err := tx.QueryRow(ctx, `SELECT c.id, c.telegram_id, c.chat_id, f.bot_id,
        COALESCE(c.invoice_ninja_id, ''), COALESCE(s.current_invoice_id, ''), COALESCE(s.first_invoice_id, ''),
        COALESCE(s.recurring_id, ''), COALESCE(c.sui_id, ''), s.state, s.desired_access,
        COALESCE(s.payment_date::text, ''), COALESCE(s.next_invoice_date::text, ''),
        f.token, f.policy_hash, f.proof_hash, f.invoice_id, f.review_code,
        p.bot_id, p.review_code, (`+paymentProvenance+`), g.epoch, (g.operation_id IS NOT NULL)
        FROM operations o JOIN activation_flows f ON f.operation_id = o.id
        JOIN clients c ON c.id = o.client_id JOIN subscriptions s ON s.client_id = c.id
        JOIN payment_checks p ON p.client_id = c.id
        CROSS JOIN activation_write_guard g
        WHERE g.id = 1 AND o.id = $2 AND o.attempts = $3 AND o.kind = 'activate_first_subscription'
            AND o.status = 'running' AND o.locked_until > clock_timestamp() AND f.bot_id = $1`, botID, lease.ID, lease.Attempt).
		Scan(&a.ClientID, &a.TelegramID, &a.ChatID, &a.BotID, &a.CustomerID, &a.InvoiceID, &a.FirstInvoiceID,
			&a.RecurringID, &a.SUIID, &a.State, &a.DesiredAccess, &a.PaymentDate, &a.NextInvoiceDate,
			&snapshot.Token, &policy, &hash, &invoiceID, &snapshot.ReviewCode, &checkBot, &checkReview, &provenance,
			&snapshot.WriteEpoch, &snapshot.WriterBusy)
	if errors.Is(err, pgx.ErrNoRows) {
		return snapshot, queue.ErrLeaseLost
	}
	if err != nil {
		return snapshot, err
	}
	if !provenance || checkBot != botID || len(policy) != 32 || len(hash) != 32 {
		return snapshot, queue.ErrConflict
	}
	copy(snapshot.PolicyHash[:], policy)
	confirmation, err := readActivationConfirmation(ctx, tx, invoiceID)
	if err != nil {
		return snapshot, err
	}
	if confirmation.clientID != a.ClientID || confirmation.botID != botID || confirmation.customerID != a.CustomerID {
		return snapshot, queue.ErrConflict
	}
	snapshot.Proof, err = confirmation.proof()
	if err != nil {
		return snapshot, err
	}
	if !bytes.Equal(hash, snapshot.Proof.Hash[:]) {
		return snapshot, queue.ErrConflict
	}
	snapshot.PaymentReview = confirmation.review || confirmation.reviewCode != 0 || checkReview != 0
	rows, err := tx.Query(ctx, `SELECT step, request, COALESCE(external_id, ''), dispatch_attempt,
        COALESCE(recurring_amount::text, ''), COALESCE(recurring_datetime, '')
        FROM activation_steps WHERE operation_id = $1`, lease.ID)
	if err != nil {
		return snapshot, err
	}
	snapshot.Steps = make(map[activation.Step]activation.Intent)
	for rows.Next() {
		var step activation.Step
		var intent activation.Intent
		if err := rows.Scan(&step, &intent.Payload, &intent.ExternalID, &intent.DispatchAttempt, &intent.RecurringAmount, &intent.RecurringDatetime); err != nil {
			rows.Close()
			return snapshot, err
		}
		if (step == activation.RecurringCreate || step == activation.RecurringStart) && intent.ExternalID != "" {
			amount, datetime, err := activation.NormalizeRecurringFacts(json.Number(intent.RecurringAmount), intent.RecurringDatetime)
			if err != nil {
				rows.Close()
				return snapshot, queue.ErrConflict
			}
			intent.RecurringAmount, intent.RecurringDatetime = amount, datetime
		} else if intent.RecurringAmount != "" || intent.RecurringDatetime != "" {
			rows.Close()
			return snapshot, queue.ErrConflict
		}
		snapshot.Steps[step] = intent
	}
	rows.Close()
	return snapshot, rows.Err()
}

func (s *Store) LoadActivation(ctx context.Context, lease queue.Lease, botID int64) (snapshot activation.Snapshot, err error) {
	defer func() { err = activationStageError("load activation", err) }()
	if botID <= 0 || lease.ID <= 0 || lease.Attempt <= 0 {
		return snapshot, queue.ErrLeaseLost
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return snapshot, err
	}
	defer rollback(tx)
	if err := prepareActivationBot(ctx, tx, botID); err != nil {
		return snapshot, err
	}
	sourceBot, err := lockActivation(ctx, tx, lease)
	if err != nil {
		return snapshot, err
	}
	if sourceBot != botID {
		return snapshot, queue.ErrConflict
	}
	snapshot, err = readActivationSnapshot(ctx, tx, lease, botID)
	if err != nil {
		return snapshot, err
	}
	if _, err := readActivationLease(ctx, tx, lease); err != nil {
		return snapshot, err
	}
	if err := tx.Commit(ctx); err != nil {
		return snapshot, err
	}
	// Do not reject changed local state or review flags here: the worker needs
	// the original proof and journal to place a durable, classified HOLD.
	return snapshot, nil
}

func authorizeActivation(actual, expected activation.Snapshot, today string) error {
	if actual.Account != expected.Account || actual.Token != expected.Token || actual.PolicyHash != expected.PolicyHash || actual.Proof.Hash != expected.Proof.Hash {
		return queue.ErrConflict
	}
	if activation.ValidateSnapshot(actual) != nil || actual.PaymentReview || actual.ReviewCode != 0 || !activation.ValidDate(today) ||
		actual.Proof.PaymentDate > today || actual.Proof.NextInvoiceDate <= today {
		return activation.ErrReviewRequired
	}
	return nil
}

// Logical test dates cannot authorize a period that expired on the database's
// actual clock. The next local billing midnight and a known provider UTC
// schedule are independent upper bounds; neither can extend the other.
func activationDeadline(actual activation.Snapshot) (time.Time, error) {
	deadline, err := time.ParseInLocation(time.DateOnly, actual.Proof.NextInvoiceDate, time.Local)
	if err != nil || !activation.ValidDate(actual.Proof.NextInvoiceDate) {
		return time.Time{}, activation.ErrReviewRequired
	}
	if intent := actual.Steps[activation.RecurringCreate]; intent.ExternalID != "" {
		_, datetime, err := activation.NormalizeRecurringFacts(json.Number(intent.RecurringAmount), intent.RecurringDatetime)
		if err != nil {
			return time.Time{}, activation.ErrReviewRequired
		}
		instant, err := time.Parse(time.RFC3339Nano, datetime)
		if err != nil {
			return time.Time{}, activation.ErrReviewRequired
		}
		if instant.Before(deadline) {
			deadline = instant
		}
	}
	return deadline, nil
}

func checkActivationDeadline(ctx context.Context, tx pgx.Tx, actual activation.Snapshot) error {
	deadline, err := activationDeadline(actual)
	if err != nil {
		return err
	}
	var future bool
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp() < $1::timestamptz`, deadline).Scan(&future); err != nil {
		return err
	}
	if !future {
		return activation.ErrReviewRequired
	}
	return nil
}

type activationGuard struct {
	operationID *int64
	step        *string
	epoch       int64
}

func lockActivationGuard(ctx context.Context, tx pgx.Tx) (activationGuard, error) {
	var guard activationGuard
	err := tx.QueryRow(ctx, `SELECT operation_id, step, epoch FROM activation_write_guard WHERE id = 1 FOR UPDATE`).Scan(&guard.operationID, &guard.step, &guard.epoch)
	return guard, err
}
func (g activationGuard) owns(operationID int64, step activation.Step) bool {
	return g.operationID != nil && g.step != nil && *g.operationID == operationID && *g.step == string(step)
}

func (s *Store) PrepareActivationWrite(ctx context.Context, lease queue.Lease, expected activation.Snapshot, step activation.Step, payload json.RawMessage, today string) (granted bool, err error) {
	defer func() { err = activationStageError("prepare activation write", err) }()
	if !step.Valid() || !activation.ValidDate(today) {
		return false, activation.ErrReviewRequired
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, err
	}
	defer rollback(tx)
	// Permission is acknowledged only after the reservation reaches primary
	// WAL durability, even when the pooled session/DSN requests async commits.
	// This is not a backup guarantee or provider-side exactly-once delivery.
	if _, err := tx.Exec(ctx, `SET LOCAL synchronous_commit = on`); err != nil {
		return false, err
	}
	botID, err := lockActivation(ctx, tx, lease)
	if err != nil {
		return false, err
	}
	actual, err := readActivationSnapshot(ctx, tx, lease, botID)
	if err != nil {
		return false, err
	}
	if err := authorizeActivation(actual, expected, today); err != nil {
		return false, err
	}
	if _, exists := actual.Steps[step]; exists {
		var object map[string]json.RawMessage
		if activation.DecodeIntent(payload, &object) != nil {
			return false, queue.ErrConflict
		}
		var same bool
		if err := tx.QueryRow(ctx, `SELECT request = $3::jsonb FROM activation_steps WHERE operation_id = $1 AND step = $2`, lease.ID, step, string(payload)).Scan(&same); err != nil {
			return false, err
		}
		if !same {
			return false, queue.ErrConflict
		}
	}
	if activation.ValidateIntent(actual, step, payload) != nil {
		return false, activation.ErrReviewRequired
	}
	next, err := activation.NextStep(actual)
	if err != nil {
		return false, queue.ErrConflict
	}
	// NextStep establishes one confirmed prefix, with at most one unresolved
	// tail and no gaps/future intents. Exact older intents may be observed
	// again without permission; only the next absent step can be inserted.
	guard, err := lockActivationGuard(ctx, tx)
	if err != nil {
		return false, err
	}
	if intent, exists := actual.Steps[step]; exists {
		if intent.ExternalID == "" && !guard.owns(lease.ID, step) {
			return false, queue.ErrConflict
		}
		if err := checkActivationDeadline(ctx, tx, actual); err != nil {
			return false, err
		}
		if _, err := readActivationLease(ctx, tx, lease); err != nil {
			return false, err
		}
		return false, tx.Commit(ctx) // An observation NEVER rearms transmission.
	}
	if next != step {
		return false, queue.ErrConflict
	}
	// The preflight must have started with an idle guard at this exact epoch.
	// An intervening dispatch invalidates old inventories even if its sender
	// has already confirmed and released the barrier by the time we get here.
	if guard.operationID != nil || expected.WriterBusy || guard.epoch != expected.WriteEpoch {
		return false, activation.ErrGuardBusy
	}
	if _, err := tx.Exec(ctx, `INSERT INTO activation_steps (operation_id, step, request, dispatch_attempt)
        VALUES ($1, $2, $3::jsonb, $4)`, lease.ID, step, string(payload), lease.Attempt); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE activation_write_guard SET operation_id = $1, step = $2, epoch = epoch + 1 WHERE id = 1`, lease.ID, step); err != nil {
		return false, err
	}
	if err := checkActivationDeadline(ctx, tx, actual); err != nil {
		return false, err
	}
	if _, err := readActivationLease(ctx, tx, lease); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	// Only an acknowledged COMMIT of a NEW insertion grants one transmission.
	return true, nil
}

func (s *Store) ConfirmActivationStep(ctx context.Context, lease queue.Lease, step activation.Step, externalID string) (err error) {
	defer func() { err = activationStageError("confirm activation step", err) }()
	if (step != activation.SUICreate && step != activation.SUIEnable) || !activation.ValidSUIID(externalID) {
		return queue.ErrConflict
	}
	return s.confirmActivationStep(ctx, lease, step, externalID, "", "")
}

func (s *Store) ConfirmRecurringActivationStep(ctx context.Context, lease queue.Lease, step activation.Step, externalID string, amount json.Number, datetime string) (err error) {
	defer func() { err = activationStageError("confirm recurring activation step", err) }()
	if (step != activation.RecurringCreate && step != activation.RecurringStart) || !activation.ValidID(externalID) {
		return queue.ErrConflict
	}
	canonicalAmount, canonicalDatetime, err := activation.NormalizeRecurringFacts(amount, datetime)
	if err != nil {
		return queue.ErrConflict
	}
	return s.confirmActivationStep(ctx, lease, step, externalID, canonicalAmount, canonicalDatetime)
}

func (s *Store) confirmActivationStep(ctx context.Context, lease queue.Lease, step activation.Step, externalID, amount, datetime string) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer rollback(tx)
	if _, err := lockActivation(ctx, tx, lease); err != nil {
		return err
	}
	var attempt int
	var existing, existingAmount, existingDatetime string
	err = tx.QueryRow(ctx, `SELECT dispatch_attempt, COALESCE(external_id, ''),
        COALESCE(recurring_amount::text, ''), COALESCE(recurring_datetime, '') FROM activation_steps
        WHERE operation_id = $1 AND step = $2`, lease.ID, step).Scan(&attempt, &existing, &existingAmount, &existingDatetime)
	if errors.Is(err, pgx.ErrNoRows) {
		return queue.ErrConflict
	}
	if err != nil {
		return err
	}
	if existing != "" && (step == activation.RecurringCreate || step == activation.RecurringStart) {
		existingAmount, existingDatetime, err = activation.NormalizeRecurringFacts(json.Number(existingAmount), existingDatetime)
		if err != nil {
			return queue.ErrConflict
		}
	}
	if attempt != lease.Attempt || (existing != "" && (existing != externalID || existingAmount != amount || existingDatetime != datetime)) {
		return queue.ErrConflict
	}
	if step == activation.SUIEnable || step == activation.RecurringStart {
		create := activation.SUICreate
		if step == activation.RecurringStart {
			create = activation.RecurringCreate
		}
		var matches bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM activation_steps WHERE operation_id = $1
            AND step = $2 AND confirmed_at IS NOT NULL AND external_id = $3
            AND recurring_amount IS NOT DISTINCT FROM NULLIF($4, '')::numeric
            AND recurring_datetime IS NOT DISTINCT FROM NULLIF($5, ''))`, lease.ID, create, externalID, amount, datetime).Scan(&matches); err != nil {
			return err
		}
		if !matches {
			return queue.ErrConflict
		}
	}
	guard, err := lockActivationGuard(ctx, tx)
	if err != nil {
		return err
	}
	if existing == "" {
		if !guard.owns(lease.ID, step) {
			return queue.ErrConflict
		}
		if _, err := tx.Exec(ctx, `UPDATE activation_steps SET external_id = $3, confirmed_at = clock_timestamp(),
            recurring_amount = NULLIF($4, '')::numeric, recurring_datetime = NULLIF($5, '')
            WHERE operation_id = $1 AND step = $2`, lease.ID, step, externalID, amount, datetime); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE activation_write_guard SET operation_id = NULL, step = NULL WHERE id = 1`); err != nil {
			return err
		}
	} else if guard.owns(lease.ID, step) {
		return queue.ErrConflict // A corrupt barrier is never silently repaired.
	}
	// This only records a known response from the ORIGINAL sender. Payment or
	// local-state changes do not authorize access, but must not lose that fact.
	if _, err := readActivationLease(ctx, tx, lease); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func validateActivationDrafts(drafts []queue.Draft) error {
	if len(drafts) > 100 {
		return errors.New("too many activation notifications")
	}
	for _, draft := range drafts {
		if draft.Validate() != nil || strings.Contains(draft.Text, "://") {
			return errors.New("invalid activation notification")
		}
	}
	return nil
}

func (s *Store) HoldActivation(ctx context.Context, lease queue.Lease, reason int, drafts []queue.Draft) (err error) {
	defer func() { err = activationStageError("hold activation", err) }()
	if reason < activation.ReviewAssociation || reason > activation.ReviewPolicy {
		return errors.New("invalid activation review code")
	}
	if err := validateActivationDrafts(drafts); err != nil {
		return err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer rollback(tx)
	if err := prepareOutboxBots(ctx, tx, drafts); err != nil {
		return err
	}
	if _, err := lockActivation(ctx, tx, lease); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE activation_flows SET review_code = $2 WHERE operation_id = $1`, lease.ID, reason); err != nil {
		return err
	}
	if reason == activation.ReviewPayment {
		// Always mark the ORIGINAL confirmation, even if current_invoice_id has
		// changed. A read outage or an expired period must never become sticky.
		if _, err := tx.Exec(ctx, `UPDATE payment_confirmations pc SET review_required = true, review_code = 2007
            FROM activation_flows f WHERE f.operation_id = $1 AND pc.invoice_id = f.invoice_id`, lease.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_checks p SET review_code = 2007 FROM operations o
            WHERE o.id = $1 AND p.client_id = o.client_id`, lease.ID); err != nil {
			return err
		}
	}
	for _, draft := range drafts {
		if _, err := enqueueOutbox(ctx, tx, draft); err != nil {
			return err
		}
	}
	if _, err := readActivationLease(ctx, tx, lease); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE operations SET status = 'pending', next_attempt_at = 'infinity',
        locked_until = NULL, last_error_code = $3 WHERE id = $1 AND attempts = $2
        AND kind = 'activate_first_subscription' AND status = 'running' AND locked_until > clock_timestamp()`, lease.ID, lease.Attempt, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrLeaseLost
	}
	return tx.Commit(ctx)
}

func (s *Store) FinishActivation(ctx context.Context, lease queue.Lease, expected activation.Snapshot, today string, drafts []queue.Draft) (err error) {
	defer func() { err = activationStageError("finish activation", err) }()
	if err := validateActivationDrafts(drafts); err != nil {
		return err
	}
	if !activation.ValidDate(today) {
		return activation.ErrReviewRequired
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer rollback(tx)
	if err := prepareOutboxBots(ctx, tx, drafts); err != nil {
		return err
	}
	botID, err := lockActivation(ctx, tx, lease)
	if err != nil {
		return err
	}
	actual, err := readActivationSnapshot(ctx, tx, lease, botID)
	if err != nil {
		return err
	}
	if err := authorizeActivation(actual, expected, today); err != nil {
		return err
	}
	if next, err := activation.NextStep(actual); err != nil || next != "" {
		return queue.ErrConflict
	}
	for _, step := range activation.OrderedSteps() {
		intent, ok := actual.Steps[step]
		if !ok || intent.ExternalID == "" || intent.DispatchAttempt <= 0 || activation.ValidateIntent(actual, step, intent.Payload) != nil {
			return queue.ErrConflict
		}
	}
	created, started := actual.Steps[activation.RecurringCreate], actual.Steps[activation.RecurringStart]
	if actual.Steps[activation.SUIEnable].ExternalID != actual.Steps[activation.SUICreate].ExternalID ||
		started.ExternalID != created.ExternalID || started.RecurringAmount != created.RecurringAmount ||
		started.RecurringDatetime != created.RecurringDatetime ||
		!activation.ValidSUIID(actual.Steps[activation.SUICreate].ExternalID) {
		return queue.ErrConflict
	}
	binding, err := activation.SUIBinding(actual)
	if err != nil {
		return queue.ErrConflict
	}
	guard, err := lockActivationGuard(ctx, tx)
	if err != nil {
		return err
	}
	if guard.operationID != nil && *guard.operationID == lease.ID {
		return queue.ErrConflict
	}
	if _, err := tx.Exec(ctx, `INSERT INTO activation_sui_bindings
        (client_id, bot_id, sui_id, name, credentials_hash, operation_id) VALUES ($1, $2, $3, $4, $5, $6)
        ON CONFLICT (client_id) DO NOTHING`, actual.Account.ClientID, botID, binding.ID, binding.Name, binding.CredentialsHash[:], lease.ID); err != nil {
		return err
	}
	var exact bool
	if err := tx.QueryRow(ctx, `SELECT bot_id = $2 AND sui_id = $3 AND name = $4 AND credentials_hash = $5
        AND operation_id = $6 FROM activation_sui_bindings WHERE client_id = $1`, actual.Account.ClientID,
		botID, binding.ID, binding.Name, binding.CredentialsHash[:], lease.ID).Scan(&exact); err != nil {
		return err
	}
	if !exact {
		return queue.ErrConflict
	}
	tag, err := tx.Exec(ctx, `UPDATE clients SET sui_id = $2 WHERE id = $1 AND sui_id IS NULL`, actual.Account.ClientID, actual.Steps[activation.SUICreate].ExternalID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrConflict
	}
	tag, err = tx.Exec(ctx, `UPDATE subscriptions SET recurring_id = $2, state = 'active', desired_access = true
        WHERE client_id = $1 AND recurring_id IS NULL AND state = 'awaiting_payment' AND NOT desired_access`, actual.Account.ClientID, actual.Steps[activation.RecurringCreate].ExternalID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrConflict
	}
	for _, draft := range drafts {
		if _, err := enqueueOutbox(ctx, tx, draft); err != nil {
			return err
		}
	}
	if err := checkActivationDeadline(ctx, tx, actual); err != nil {
		return err
	}
	if _, err := readActivationLease(ctx, tx, lease); err != nil {
		return err
	}
	deadline, err := activationDeadline(actual)
	if err != nil {
		return err
	}
	tag, err = tx.Exec(ctx, `UPDATE operations SET status = 'completed', completed_at = clock_timestamp(),
        locked_until = NULL, last_error_code = 0 WHERE id = $1 AND attempts = $2
        AND kind = 'activate_first_subscription' AND status = 'running' AND locked_until > clock_timestamp()
        AND clock_timestamp() < $3::timestamptz`, lease.ID, lease.Attempt, deadline)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		if _, err := readActivationLease(ctx, tx, lease); err != nil {
			return err
		}
		return activation.ErrReviewRequired
	}
	return tx.Commit(ctx)
}

// Driver errors may contain SQL, JSON credentials, URLs and account identities.
// Keep only these explicitly safe classifications and a constant local stage.
func activationStageError(stage string, err error) error {
	if err == nil {
		return nil
	}
	for _, sentinel := range []error{queue.ErrLeaseLost, queue.ErrConflict, activation.ErrGuardBusy, activation.ErrReviewRequired, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, sentinel) {
			return fmt.Errorf("%s: %w", stage, sentinel)
		}
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fmt.Errorf("%s: %w", stage, queue.ErrConflict)
	}
	return errors.New(stage)
}
