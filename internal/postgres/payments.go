package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var _ payments.Store = (*Store)(nil)

// Only this application's completed registration journal proves ownership.
// Current invoices may advance, but the original customer/first invoice must
// still match all three confirmed registration steps. Imports are not adopted.
const paymentProvenance = `
    c.invoice_ninja_id IS NOT NULL AND c.invoice_ninja_id <> ''
    AND s.first_invoice_id IS NOT NULL AND s.first_invoice_id <> ''
    AND s.current_invoice_id IS NOT NULL AND s.current_invoice_id <> ''
    AND c.chat_id = c.telegram_id
    AND EXISTS (
        SELECT 1 FROM operations o
        JOIN registration_flows f ON f.operation_id = o.id
        JOIN operation_steps customer ON customer.operation_id = o.id
            AND customer.step = 'customer_create' AND customer.confirmed_at IS NOT NULL
            AND customer.external_id = c.invoice_ninja_id
        JOIN operation_steps invoice ON invoice.operation_id = o.id
            AND invoice.step = 'invoice_create' AND invoice.confirmed_at IS NOT NULL
            AND invoice.external_id = s.first_invoice_id
        JOIN operation_steps issued ON issued.operation_id = o.id
            AND issued.step = 'invoice_issue' AND issued.confirmed_at IS NOT NULL
            AND issued.external_id = s.first_invoice_id
        WHERE o.client_id = c.id AND o.kind = 'register_client'
            AND o.status = 'completed' AND f.bot_id = $1
    )`

const paymentDue = `
    (p.next_check_at <= clock_timestamp() OR (p.last_error_code = 0 AND p.observed_generation < w.generation))
    AND (p.locked_until IS NULL OR p.locked_until <= clock_timestamp())`

// RecordPaymentWake stores only a bot-scoped body digest. A duplicate does not
// wake the poller again, and acknowledgement is safe only after commit.
func (s *Store) RecordPaymentWake(ctx context.Context, botID int64, hash [32]byte) (fresh bool, err error) {
	defer func() { err = paymentStageError("record payment wake", err) }()
	if botID <= 0 {
		return false, errors.New("invalid payment bot")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer rollback(tx)
	if err := preparePaymentBot(ctx, tx, botID); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO payment_processed_events (bot_id, body_hash)
        VALUES ($1, $2) ON CONFLICT DO NOTHING`, botID, hash[:])
	if err != nil {
		return false, err
	}
	fresh = tag.RowsAffected() == 1
	if fresh {
		if _, err := tx.Exec(ctx, `UPDATE payment_wake_state SET generation = generation + 1
            WHERE bot_id = $1`, botID); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return fresh, nil
}

func preparePaymentBot(ctx context.Context, tx pgx.Tx, botID int64) error {
	if _, err := tx.Exec(ctx, `INSERT INTO telegram_bots (bot_id) VALUES ($1) ON CONFLICT DO NOTHING`, botID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO payment_wake_state (bot_id) VALUES ($1) ON CONFLICT DO NOTHING`, botID)
	return err
}

// SeedPaymentChecks has a hard batch cap. Anti-join before LIMIT is essential:
// the first fifty existing accounts must not starve later registrations.
func (s *Store) SeedPaymentChecks(ctx context.Context, botID int64) (count int, err error) {
	defer func() { err = paymentStageError("seed payment checks", err) }()
	if botID <= 0 {
		return 0, errors.New("invalid payment bot")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, err
	}
	defer rollback(tx)
	// Never wait for a first-webhook bot insertion while holding its client.
	if err := preparePaymentBot(ctx, tx, botID); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `SELECT c.id FROM clients c
        JOIN subscriptions s ON s.client_id = c.id
        WHERE `+paymentProvenance+`
            AND NOT EXISTS (SELECT 1 FROM payment_checks p WHERE p.client_id = c.id)
        ORDER BY c.id LIMIT 50 FOR NO KEY UPDATE OF c SKIP LOCKED`, botID)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return 0, err
	}
	if len(ids) != 0 {
		if err := lockPaymentSubscriptions(ctx, tx, ids); err != nil {
			return 0, err
		}
		// Fresh snapshot after acquiring the client/subscription locks.
		tag, err := tx.Exec(ctx, `INSERT INTO payment_checks (client_id, bot_id)
            SELECT c.id, $1 FROM clients c JOIN subscriptions s ON s.client_id = c.id
            WHERE c.id = ANY($2::bigint[]) AND `+paymentProvenance+`
                AND NOT EXISTS (SELECT 1 FROM payment_checks p WHERE p.client_id = c.id)
            ORDER BY c.id ON CONFLICT (client_id) DO NOTHING`, botID, ids)
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

// Claims use their own queue, not the general operation FIFO. The wake row is
// read through MVCC only: locking it after clients would invert webhook order.
func (s *Store) ClaimPaymentChecks(ctx context.Context, botID int64, limit int) (claims []payments.Claim, err error) {
	defer func() { err = paymentStageError("claim payment checks", err) }()
	if botID <= 0 || limit < 1 || limit > 4 {
		return nil, errors.New("payment claims require a bot and batch of 1-4")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	defer rollback(tx)
	rows, err := tx.Query(ctx, `SELECT c.id FROM clients c
        JOIN subscriptions s ON s.client_id = c.id
        JOIN payment_checks p ON p.client_id = c.id AND p.bot_id = $1
        JOIN payment_wake_state w ON w.bot_id = p.bot_id
        WHERE `+paymentProvenance+` AND `+paymentDue+`
        ORDER BY p.next_check_at, c.id LIMIT $2 FOR NO KEY UPDATE OF c SKIP LOCKED`, botID, limit)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, err
	}
	if len(ids) != 0 {
		if err := lockPaymentSubscriptions(ctx, tx, ids); err != nil {
			return nil, err
		}
		// A separate Read Committed statement is mandatory. A completion can
		// commit between this claimant's first snapshot and its client lock.
		rows, err := tx.Query(ctx, `UPDATE payment_checks p
            SET attempts = p.attempts + 1,
                locked_until = clock_timestamp() + interval '2 minutes',
                lease_generation = w.generation
            FROM clients c JOIN subscriptions s ON s.client_id = c.id,
                payment_wake_state w
            WHERE p.client_id = c.id AND c.id = ANY($2::bigint[])
                AND p.bot_id = $1 AND w.bot_id = p.bot_id
                AND `+paymentProvenance+` AND `+paymentDue+`
            RETURNING p.client_id, p.attempts, p.bot_id, p.lease_generation`, botID, ids)
		if err != nil {
			return nil, err
		}
		claims, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (payments.Claim, error) {
			var claim payments.Claim
			err := row.Scan(&claim.ID, &claim.Attempt, &claim.BotID, &claim.Generation)
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

func lockPaymentSubscriptions(ctx context.Context, tx pgx.Tx, ids []int64) error {
	_, err := tx.Exec(ctx, `SELECT client_id FROM subscriptions WHERE client_id = ANY($1::bigint[])
        ORDER BY client_id FOR NO KEY UPDATE`, ids)
	return err
}

func validPaymentClaim(claim payments.Claim) bool {
	return claim.ID > 0 && claim.BotID > 0 && claim.Attempt > 0 && claim.Generation >= 0
}

func (s *Store) LoadPaymentSnapshot(ctx context.Context, claim payments.Claim) (snapshot payments.Snapshot, err error) {
	defer func() { err = paymentStageError("load payment snapshot", err) }()
	if !validPaymentClaim(claim) {
		return payments.Snapshot{}, queue.ErrLeaseLost
	}
	return readPaymentSnapshot(ctx, s.pool, claim)
}

type paymentSnapshotReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func readPaymentSnapshot(ctx context.Context, reader paymentSnapshotReader, claim payments.Claim) (payments.Snapshot, error) {
	var snapshot payments.Snapshot
	var provenance bool
	err := reader.QueryRow(ctx, `SELECT c.id, c.telegram_id, c.chat_id, p.bot_id,
        COALESCE(c.invoice_ninja_id, ''), COALESCE(s.current_invoice_id, ''),
        COALESCE(s.first_invoice_id, ''), COALESCE(s.recurring_id, ''), COALESCE(c.sui_id, ''),
        s.state, s.desired_access, COALESCE(s.payment_date::text, ''),
        COALESCE(s.next_invoice_date::text, ''), (`+paymentProvenance+`)
        FROM payment_checks p JOIN clients c ON c.id = p.client_id
        JOIN subscriptions s ON s.client_id = c.id
        WHERE p.bot_id = $1 AND p.client_id = $2 AND p.attempts = $3
            AND p.lease_generation = $4 AND p.locked_until > clock_timestamp()`,
		claim.BotID, claim.ID, claim.Attempt, claim.Generation).Scan(
		&snapshot.ClientID, &snapshot.TelegramID, &snapshot.ChatID, &snapshot.BotID,
		&snapshot.CustomerID, &snapshot.InvoiceID, &snapshot.FirstInvoiceID,
		&snapshot.RecurringID, &snapshot.SUIID, &snapshot.State, &snapshot.DesiredAccess,
		&snapshot.PaymentDate, &snapshot.NextInvoiceDate, &provenance)
	if errors.Is(err, pgx.ErrNoRows) {
		return payments.Snapshot{}, queue.ErrLeaseLost
	}
	if err != nil {
		return payments.Snapshot{}, err
	}
	if !provenance {
		return payments.Snapshot{}, queue.ErrConflict
	}
	return snapshot, nil
}

// Client -> subscription -> check -> confirmation is the mutation lock order.
// No bot or wake-state lock may be acquired after entering this helper.
func lockPaymentClaim(ctx context.Context, tx pgx.Tx, claim payments.Claim) error {
	var id int64
	err := tx.QueryRow(ctx, `SELECT id FROM clients WHERE id = $1 FOR NO KEY UPDATE`, claim.ID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return queue.ErrLeaseLost
	}
	if err != nil {
		return err
	}
	err = tx.QueryRow(ctx, `SELECT client_id FROM subscriptions WHERE client_id = $1 FOR NO KEY UPDATE`, claim.ID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return queue.ErrConflict
	}
	if err != nil {
		return err
	}
	err = tx.QueryRow(ctx, `SELECT client_id FROM payment_checks
        WHERE client_id = $1 AND bot_id = $2 AND attempts = $3 AND lease_generation = $4
            AND locked_until > clock_timestamp() FOR UPDATE`,
		claim.ID, claim.BotID, claim.Attempt, claim.Generation).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return queue.ErrLeaseLost
	}
	return err
}

func (s *Store) DeferPaymentCheck(ctx context.Context, claim payments.Claim, delay time.Duration, code int) (err error) {
	defer func() { err = paymentStageError("defer payment check", err) }()
	if !validPaymentClaim(claim) {
		return queue.ErrLeaseLost
	}
	if delay < 0 || code < 0 || int64(code) > 1<<31-1 {
		return errors.New("invalid payment retry")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if err := lockPaymentClaim(ctx, tx, claim); err != nil {
		return err
	}
	// Do not acknowledge wakes received after this lease was claimed. A
	// transient failure never invalidates evidence or emits a notification.
	tag, err := tx.Exec(ctx, `UPDATE payment_checks SET locked_until = NULL,
        next_check_at = clock_timestamp() + $5::bigint * interval '1 microsecond',
        observed_generation = $4, last_error_code = $6
        WHERE client_id = $1 AND bot_id = $2 AND attempts = $3 AND lease_generation = $4
            AND locked_until > clock_timestamp()`,
		claim.ID, claim.BotID, claim.Attempt, claim.Generation, delay.Microseconds(), code)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrLeaseLost
	}
	return tx.Commit(ctx)
}

func (s *Store) FinishPaymentCheck(ctx context.Context, claim payments.Claim, expected payments.Snapshot, decision payments.Decision, notices payments.Notices, interval time.Duration) (err error) {
	defer func() { err = paymentStageError("finish payment check", err) }()
	if err := decision.Validate(); err != nil {
		return err
	}
	if len(notices.Confirmed) > 100 || len(notices.Review) > 100 || interval < time.Second || interval > time.Hour {
		return errors.New("invalid payment completion bounds")
	}
	messages := make([]queue.Draft, 0, len(notices.Confirmed)+len(notices.Review))
	messages = append(messages, notices.Confirmed...)
	messages = append(messages, notices.Review...)
	for _, message := range messages {
		if err := message.Validate(); err != nil {
			return err
		}
	}
	if !validPaymentClaim(claim) {
		return queue.ErrLeaseLost
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer rollback(tx)
	// Prepare BOTH groups, including the unused one, before any client lock.
	if err := prepareOutboxBots(ctx, tx, messages); err != nil {
		return err
	}
	if err := lockPaymentClaim(ctx, tx, claim); err != nil {
		return err
	}
	actual, err := readPaymentSnapshot(ctx, tx, claim)
	if err != nil {
		return err
	}
	if actual != expected {
		return queue.ErrConflict
	}
	if decision.Proof != nil && (decision.Proof.CustomerID != actual.CustomerID || decision.Proof.InvoiceID != actual.InvoiceID) {
		return queue.ErrConflict
	}

	var clientID, botID int64
	var customerID string
	var originalHash []byte
	var review bool
	err = tx.QueryRow(ctx, `SELECT client_id, bot_id, customer_id, proof_hash, review_required
        FROM payment_confirmations WHERE invoice_id = $1 FOR UPDATE`, actual.InvoiceID).
		Scan(&clientID, &botID, &customerID, &originalHash, &review)
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if exists && (clientID != actual.ClientID || botID != actual.BotID || customerID != actual.CustomerID) {
		return queue.ErrConflict
	}
	var drafts []queue.Draft
	reviewCode := 0
	switch {
	case exists:
		if review || decision.Kind != payments.Verified || !bytes.Equal(originalHash, decision.Proof.Hash[:]) {
			// Original financial facts are immutable. Once reviewed, even the
			// original proof returning cannot clear this sticky warning.
			reviewCode = payments.ReviewChangedProof
			if _, err := tx.Exec(ctx, `UPDATE payment_confirmations
                SET review_required = true, review_code = $2 WHERE invoice_id = $1`,
				actual.InvoiceID, reviewCode); err != nil {
				return err
			}
			drafts = notices.Review
		}
		// Matching proof: no date extension, evidence rewrite or second notice.
	case decision.Kind == payments.Verified:
		proof := decision.Proof
		if actual.PaymentDate != "" && proof.PaymentDate < actual.PaymentDate {
			reviewCode, drafts = payments.ReviewChangedProof, notices.Review
			break
		}
		if _, err := tx.Exec(ctx, `INSERT INTO payment_confirmations
            (invoice_id, client_id, bot_id, customer_id, invoice_number, amount, currency_id,
                payment_date, next_invoice_date, proof_hash, evidence)
            VALUES ($1, $2, $3, $4, $5, $6::numeric, $7, $8::date, $9::date, $10, $11::jsonb)`,
			proof.InvoiceID, actual.ClientID, actual.BotID, proof.CustomerID, proof.InvoiceNumber,
			proof.Amount, proof.CurrencyID, proof.PaymentDate, proof.NextInvoiceDate, proof.Hash[:], string(proof.JSON)); err != nil {
			return err
		}
		// Observation only: no access, recurring/SUI binding or operation write.
		if _, err := tx.Exec(ctx, `UPDATE subscriptions SET payment_date = $2::date,
            next_invoice_date = $3::date WHERE client_id = $1`, actual.ClientID, proof.PaymentDate, proof.NextInvoiceDate); err != nil {
			return err
		}
		drafts = notices.Confirmed
	case decision.Kind == payments.NeedsReview:
		reviewCode, drafts = decision.Code, notices.Review
	}
	for _, draft := range drafts {
		if _, err := enqueueOutbox(ctx, tx, draft); err != nil {
			return err
		}
	}
	// This final clock guard also fences repeat-success and expiry during an
	// outbox wait. Failure rolls back every preceding write and notification.
	tag, err := tx.Exec(ctx, `UPDATE payment_checks SET locked_until = NULL,
        next_check_at = clock_timestamp() + $5::bigint * interval '1 microsecond',
        observed_generation = $4, last_checked_at = clock_timestamp(),
        last_error_code = 0, review_code = $6
        WHERE client_id = $1 AND bot_id = $2 AND attempts = $3 AND lease_generation = $4
            AND locked_until > clock_timestamp()`,
		claim.ID, claim.BotID, claim.Attempt, claim.Generation, interval.Microseconds(), reviewCode)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrLeaseLost
	}
	return tx.Commit(ctx)
}

// Never expose SQL, driver details, evidence, or account/notification bindings.
func paymentStageError(stage string, err error) error {
	if err == nil {
		return nil
	}
	for _, sentinel := range []error{queue.ErrLeaseLost, queue.ErrConflict, context.Canceled, context.DeadlineExceeded} {
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
