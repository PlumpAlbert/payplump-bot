package postgres

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/importorigin"
	"github.com/PlumpAlbert/payplump-bot/internal/lifecycle"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var _ lifecycle.Store = (*Store)(nil)

// Registration provenance and the original activation journal jointly bind
// every candidate to this bot, Telegram client, first invoice and recurring ID.
const invoiceLifecycleManaged = paymentProvenance + `
    AND c.chat_id = c.telegram_id
    AND c.sui_id IS NOT NULL AND c.sui_id <> ''
    AND EXISTS (
        SELECT 1 FROM operations ao
        JOIN activation_flows af ON af.operation_id = ao.id
        JOIN payment_confirmations first_payment ON first_payment.invoice_id = af.invoice_id
            AND first_payment.client_id = c.id AND first_payment.bot_id = $1
            AND first_payment.customer_id = c.invoice_ninja_id
            AND first_payment.proof_hash = af.proof_hash AND first_payment.review_required = false AND first_payment.review_code = 0
        JOIN activation_steps created ON created.operation_id = ao.id
            AND created.step = 'recurring_create' AND created.confirmed_at IS NOT NULL
            AND created.external_id = s.recurring_id
        JOIN activation_steps started ON started.operation_id = ao.id
            AND started.step = 'recurring_start' AND started.confirmed_at IS NOT NULL
            AND started.external_id = s.recurring_id
        JOIN activation_sui_bindings binding ON binding.operation_id = ao.id
            AND binding.client_id = c.id AND binding.bot_id = $1 AND binding.sui_id::text = c.sui_id
        JOIN activation_steps panel_created ON panel_created.operation_id = ao.id
            AND panel_created.step = 'sui_create' AND panel_created.confirmed_at IS NOT NULL
            AND panel_created.external_id = binding.sui_id::text
        JOIN activation_steps panel_enabled ON panel_enabled.operation_id = ao.id
            AND panel_enabled.step = 'sui_enable' AND panel_enabled.confirmed_at IS NOT NULL
            AND panel_enabled.external_id = binding.sui_id::text
        WHERE ao.client_id = c.id AND ao.kind = 'activate_first_subscription'
            AND ao.status = 'completed' AND af.bot_id = $1 AND af.review_code = 0
            AND af.invoice_id = s.first_invoice_id
    )`

const invoiceLifecycleEligible = invoiceLifecycleManaged + `
    AND c.telegram_id = ANY($2::bigint[])
    AND s.state = 'active' AND s.desired_access = true`

const importedLifecycleTracking = `( (` + importedPaymentProvenance + `)
    AND NOT (` + paymentProvenance + `)
    AND s.state = 'active' AND s.desired_access = true )`
const invoiceLifecycleTrackingManaged = `( ((` + invoiceLifecycleManaged + `) AND NOT EXISTS (SELECT 1 FROM verified_import_origins io WHERE io.client_id=c.id)) OR (` + importedLifecycleTracking + `) )`
const invoiceLifecycleTrackingEligible = `(` + invoiceLifecycleTrackingManaged + `)
    AND c.telegram_id = ANY($2::bigint[])
    AND s.state = 'active' AND s.desired_access = true`

const invoiceLifecycleDue = `
    lc.review_code = 0
    AND (lc.next_check_at <= clock_timestamp() OR w.generation > lc.observed_generation)
    AND (lc.locked_until IS NULL OR lc.locked_until <= clock_timestamp())`

func (s *Store) RecordInvoiceLifecycleWake(ctx context.Context, botID int64) (err error) {
	defer func() { err = invoiceLifecycleStageError("record invoice lifecycle wake", err) }()
	if botID <= 0 {
		return errors.New("invalid invoice lifecycle bot")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if err := prepareLifecycleBot(ctx, tx, botID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE invoice_lifecycle_wake_state
        SET generation = generation + 1 WHERE bot_id = $1`, botID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func prepareLifecycleBot(ctx context.Context, tx pgx.Tx, botID int64) error {
	if _, err := tx.Exec(ctx, `INSERT INTO telegram_bots (bot_id) VALUES ($1) ON CONFLICT DO NOTHING`, botID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO invoice_lifecycle_wake_state (bot_id) VALUES ($1) ON CONFLICT DO NOTHING`, botID)
	return err
}

func (s *Store) SeedInvoiceLifecycleChecks(ctx context.Context, botID int64, allowed []int64, limit int) (count int, err error) {
	defer func() { err = invoiceLifecycleStageError("seed invoice lifecycle checks", err) }()
	if botID <= 0 || !lifecycleIDs(allowed) || limit < 1 || limit > 1000 {
		return 0, errors.New("invalid invoice lifecycle seed bounds")
	}
	if len(allowed) == 0 {
		return 0, nil
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, err
	}
	defer rollback(tx)
	if err := prepareLifecycleBot(ctx, tx, botID); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `SELECT c.id FROM clients c JOIN subscriptions s ON s.client_id = c.id
        WHERE `+invoiceLifecycleTrackingEligible+`
          AND NOT EXISTS (SELECT 1 FROM invoice_lifecycle_checks lc WHERE lc.client_id = c.id)
        ORDER BY c.id LIMIT $3 FOR NO KEY UPDATE OF c SKIP LOCKED`, botID, allowed, limit)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return 0, err
	}
	if len(ids) > 0 {
		if err := lockPaymentSubscriptions(ctx, tx, ids); err != nil {
			return 0, err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO invoice_lifecycle_checks (client_id, bot_id)
            SELECT c.id, $1 FROM clients c JOIN subscriptions s ON s.client_id = c.id
            WHERE c.id = ANY($3::bigint[]) AND `+invoiceLifecycleTrackingEligible+`
              AND NOT EXISTS (SELECT 1 FROM invoice_lifecycle_checks lc WHERE lc.client_id = c.id)
            ORDER BY c.id ON CONFLICT (client_id) DO NOTHING`, botID, allowed, ids)
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

func (s *Store) ClaimInvoiceLifecycleChecks(ctx context.Context, botID int64, allowed []int64, limit int) (claims []lifecycle.Claim, err error) {
	defer func() { err = invoiceLifecycleStageError("claim invoice lifecycle checks", err) }()
	if botID <= 0 || !lifecycleIDs(allowed) || limit < 1 || limit > 4 {
		return nil, errors.New("invalid invoice lifecycle claim bounds")
	}
	if len(allowed) == 0 {
		return nil, nil
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	defer rollback(tx)
	rows, err := tx.Query(ctx, `SELECT c.id FROM clients c JOIN subscriptions s ON s.client_id = c.id
        JOIN invoice_lifecycle_checks lc ON lc.client_id = c.id AND lc.bot_id = $1
        JOIN invoice_lifecycle_wake_state w ON w.bot_id = lc.bot_id
        WHERE `+invoiceLifecycleTrackingEligible+` AND `+invoiceLifecycleDue+`
        ORDER BY lc.next_check_at, c.id LIMIT $3 FOR NO KEY UPDATE OF c SKIP LOCKED`, botID, allowed, limit)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, err
	}
	if len(ids) > 0 {
		if err := lockPaymentSubscriptions(ctx, tx, ids); err != nil {
			return nil, err
		}
		rows, err = tx.Query(ctx, `UPDATE invoice_lifecycle_checks lc
            SET attempts = lc.attempts + 1,
                locked_until = clock_timestamp() + $4::bigint * interval '1 microsecond',
                lease_generation = w.generation
            FROM clients c JOIN subscriptions s ON s.client_id = c.id,
                invoice_lifecycle_wake_state w
            WHERE lc.client_id = c.id AND c.id = ANY($3::bigint[])
                AND lc.bot_id = $1 AND w.bot_id = lc.bot_id
                AND `+invoiceLifecycleTrackingEligible+` AND `+invoiceLifecycleDue+`
            RETURNING lc.client_id, lc.attempts, lc.bot_id, lc.lease_generation`, botID, allowed, ids, lifecycle.LeaseDuration.Microseconds())
		if err != nil {
			return nil, err
		}
		claims, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (lifecycle.Claim, error) {
			var claim lifecycle.Claim
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

func (s *Store) LoadInvoiceLifecycleSnapshot(ctx context.Context, claim lifecycle.Claim) (snapshot lifecycle.Snapshot, err error) {
	defer func() { err = invoiceLifecycleStageError("load invoice lifecycle snapshot", err) }()
	if !validLifecycleClaim(claim) {
		return snapshot, queue.ErrLeaseLost
	}
	return readInvoiceLifecycleSnapshot(ctx, s.pool, claim)
}

type lifecycleSnapshotReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func readInvoiceLifecycleSnapshot(ctx context.Context, reader lifecycleSnapshotReader, claim lifecycle.Claim) (snapshot lifecycle.Snapshot, err error) {
	var provenance, hasOrigin bool
	var refClient, refBot, admissionRevision, runtimeRevision, configRevision int64
	var stableHash, policyHash, scopeHash []byte
	err = reader.QueryRow(ctx, `SELECT c.id, c.telegram_id, c.chat_id, lc.bot_id,
        COALESCE(c.invoice_ninja_id, ''), COALESCE(s.first_invoice_id, ''),
        COALESCE(s.current_invoice_id, ''), COALESCE(s.recurring_id, ''), COALESCE(c.sui_id, ''),
        COALESCE((SELECT rf.token FROM operations ro JOIN registration_flows rf ON rf.operation_id = ro.id
            WHERE ro.client_id = c.id AND ro.kind = 'register_client' AND ro.status = 'completed'
                AND rf.bot_id = $1), ''),
        COALESCE((SELECT af.token FROM operations ao JOIN activation_flows af ON af.operation_id = ao.id
            WHERE ao.client_id = c.id AND ao.kind = 'activate_first_subscription' AND ao.status = 'completed'
                AND af.bot_id = $1 AND af.review_code = 0 AND af.invoice_id = s.first_invoice_id), ''),
        s.state, s.desired_access,
        ARRAY(SELECT seen.invoice_id FROM (
            SELECT li.invoice_id FROM invoice_lifecycle_invoices li WHERE li.client_id=c.id
            UNION SELECT jsonb_array_elements_text(io.historical_invoice_ids) FROM verified_import_origins io
                WHERE io.client_id=c.id AND io.bot_id=$1 AND (`+importedLifecycleTracking+`)
        ) seen ORDER BY seen.invoice_id LIMIT 10001),
        (`+invoiceLifecycleTrackingManaged+`), (io.client_id IS NOT NULL),
        COALESCE(io.client_id,0), COALESCE(io.bot_id,0), COALESCE(io.admission_revision,0), COALESCE(rp.revision,0),
        c.configuration_revision, COALESCE(io.stable_hash,decode(repeat('00',32),'hex')),
        COALESCE(io.policy_hash,decode(repeat('00',32),'hex')), COALESCE(io.source_scope_hash,decode(repeat('00',32),'hex')),
        COALESCE(io.template->>'private_notes','')
        FROM invoice_lifecycle_checks lc JOIN clients c ON c.id = lc.client_id
        JOIN subscriptions s ON s.client_id = c.id
        LEFT JOIN verified_import_origins io ON io.client_id=c.id AND io.bot_id=lc.bot_id
        LEFT JOIN imported_runtime_permissions rp ON rp.bot_id=io.bot_id
        WHERE lc.bot_id = $1 AND lc.client_id = $2 AND lc.attempts = $3
            AND lc.lease_generation = $4 AND lc.locked_until > clock_timestamp()`,
		claim.BotID, claim.ID, claim.Attempt, claim.Generation).Scan(
		&snapshot.ClientID, &snapshot.TelegramID, &snapshot.ChatID, &snapshot.BotID,
		&snapshot.CustomerID, &snapshot.FirstInvoiceID, &snapshot.CurrentInvoiceID,
		&snapshot.RecurringID, &snapshot.SUIID, &snapshot.RegistrationToken, &snapshot.ActivationToken,
		&snapshot.State, &snapshot.DesiredAccess, &snapshot.SeenInvoiceIDs, &provenance, &hasOrigin,
		&refClient, &refBot, &admissionRevision, &runtimeRevision, &configRevision, &stableHash, &policyHash, &scopeHash, &snapshot.ImportedPrivateNotes)
	if errors.Is(err, pgx.ErrNoRows) {
		return lifecycle.Snapshot{}, queue.ErrLeaseLost
	}
	if err != nil {
		return lifecycle.Snapshot{}, err
	}
	snapshot.ManagedProvenance = provenance
	if hasOrigin {
		stable, okStable := importorigin.ParseHash(stableHash)
		policy, okPolicy := importorigin.ParseHash(policyHash)
		scope, okScope := importorigin.ParseHash(scopeHash)
		if okStable && okPolicy && okScope {
			snapshot.ImportedOrigin = importorigin.Ref{ClientID: refClient, BotID: refBot, AdmissionRevision: admissionRevision, RuntimeRevision: runtimeRevision, ConfigurationRevision: configRevision, StableHash: stable, PolicyHash: policy, SourceScopeHash: scope}
		}
	}
	snapshot.SeenInvoiceOverflow = len(snapshot.SeenInvoiceIDs) > 10000
	return snapshot, nil
}

func validLifecycleClaim(claim lifecycle.Claim) bool {
	return claim.ID > 0 && claim.BotID > 0 && claim.Attempt > 0 && claim.Generation >= 0
}

func (s *Store) DeferInvoiceLifecycleCheck(ctx context.Context, claim lifecycle.Claim, delay time.Duration, code int) (err error) {
	defer func() { err = invoiceLifecycleStageError("defer invoice lifecycle check", err) }()
	if !validLifecycleClaim(claim) {
		return queue.ErrLeaseLost
	}
	if delay < 0 || delay > time.Hour || code < 0 {
		return errors.New("invalid invoice lifecycle retry")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if err := lockInvoiceLifecycleClaim(ctx, tx, claim); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE invoice_lifecycle_checks SET locked_until = NULL,
        next_check_at = clock_timestamp() + $5::bigint * interval '1 microsecond',
        observed_generation = $4, last_error_code = $6
        WHERE client_id = $1 AND bot_id = $2 AND attempts = $3 AND lease_generation = $4
            AND locked_until > clock_timestamp()`, claim.ID, claim.BotID, claim.Attempt, claim.Generation, delay.Microseconds(), code)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrLeaseLost
	}
	return tx.Commit(ctx)
}

func lockInvoiceLifecycleClaim(ctx context.Context, tx pgx.Tx, claim lifecycle.Claim) error {
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
	err = tx.QueryRow(ctx, `SELECT client_id FROM invoice_lifecycle_checks
        WHERE client_id = $1 AND bot_id = $2 AND attempts = $3 AND lease_generation = $4
            AND locked_until > clock_timestamp() FOR UPDATE`, claim.ID, claim.BotID, claim.Attempt, claim.Generation).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return queue.ErrLeaseLost
	}
	return err
}

func (s *Store) FinishInvoiceLifecycleCheck(ctx context.Context, claim lifecycle.Claim, expected lifecycle.Snapshot, invoice *lifecycle.Invoice, reviewCode int, notices lifecycle.Notices, interval time.Duration) (err error) {
	defer func() { err = invoiceLifecycleStageError("finish invoice lifecycle check", err) }()
	if !validLifecycleClaim(claim) || interval < time.Second || interval > time.Hour || reviewCode != 0 && (reviewCode < lifecycle.ReviewAssociation || reviewCode > lifecycle.ReviewPreviousPayment) {
		return errors.New("invalid invoice lifecycle completion")
	}
	if reviewCode == 0 && invoice != nil {
		if err := invoice.Validate(); err != nil {
			return err
		}
	} else if reviewCode != 0 && invoice == nil {
		return errors.New("invoice lifecycle review requires an identity")
	}
	if reviewCode == 0 && invoice == nil && (notices.Client != nil || len(notices.Admins) != 0 || len(notices.Review) != 0 || len(notices.PredecessorReview) != 0) {
		return errors.New("empty lifecycle observation cannot notify")
	}
	if invoice != nil && reviewCode == 0 {
		if invoice.Status == "paid" && (notices.Client != nil || len(notices.Admins) != 0) {
			return errors.New("paid invoice cannot receive an unpaid notice")
		}
		if invoice.Status != "paid" && (notices.Client == nil || len(notices.Admins) == 0) {
			return errors.New("sent invoice requires client and admin notices")
		}
	}
	if len(notices.Admins) > 100 || len(notices.Review) > 100 || len(notices.PredecessorReview) > 100 {
		return errors.New("invoice lifecycle notice limit exceeded")
	}
	messages := make([]queue.Draft, 0, len(notices.Admins)+len(notices.Review)+len(notices.PredecessorReview)+1)
	if notices.Client != nil {
		messages = append(messages, *notices.Client)
	}
	messages = append(messages, notices.Admins...)
	messages = append(messages, notices.Review...)
	messages = append(messages, notices.PredecessorReview...)
	if expected.ClientID != claim.ID || expected.BotID != claim.BotID {
		return errors.New("invoice lifecycle snapshot scope mismatch")
	}
	for _, message := range messages {
		if err := message.Validate(); err != nil {
			return err
		}
		if message.BotID != claim.BotID {
			return errors.New("invoice lifecycle notice bot mismatch")
		}
	}
	if notices.Client != nil && (expected.ChatID != expected.TelegramID || notices.Client.ChatID != expected.ChatID) {
		return errors.New("invoice lifecycle client notice scope mismatch")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer rollback(tx)
	if err := prepareLifecycleBot(ctx, tx, claim.BotID); err != nil {
		return err
	}
	if invoice != nil && reviewCode == 0 {
		if err := preparePaymentBot(ctx, tx, claim.BotID); err != nil {
			return err
		}
	}
	if err := prepareOutboxBots(ctx, tx, messages); err != nil {
		return err
	}
	if err := lockInvoiceLifecycleClaim(ctx, tx, claim); err != nil {
		return err
	}
	actual, err := readInvoiceLifecycleSnapshot(ctx, tx, claim)
	if err != nil {
		return err
	}
	if !sameLifecycleSnapshot(actual, expected) {
		return queue.ErrConflict
	}
	if actual.ImportedOrigin != (importorigin.Ref{}) {
		if err := lockImportedPaymentPermissions(ctx, tx, claim.ID, claim.BotID); err != nil {
			return err
		}
		current, err := readInvoiceLifecycleSnapshot(ctx, tx, claim)
		if err != nil {
			return err
		}
		if !sameLifecycleSnapshot(current, expected) || !current.ManagedProvenance {
			return queue.ErrConflict
		}
		actual = current
	}

	if reviewCode != 0 {
		if len(notices.Review) == 0 {
			return errors.New("invoice lifecycle review requires admin notice")
		}
		for _, draft := range notices.Review {
			if _, err := enqueueOutbox(ctx, tx, draft); err != nil {
				return err
			}
		}
		return finishInvoiceLifecycleLease(ctx, tx, claim, interval, reviewCode)
	}
	if invoice == nil {
		return finishInvoiceLifecycleLease(ctx, tx, claim, interval, 0)
	}

	if invoice.ID == actual.CurrentInvoiceID || invoice.ID == actual.FirstInvoiceID {
		return queue.ErrConflict
	}
	if actual.ImportedOrigin != (importorigin.Ref{}) && slices.Contains(actual.SeenInvoiceIDs, invoice.ID) {
		return queue.ErrConflict
	}
	var paymentBotID int64
	var paymentReview int
	if _, err := tx.Exec(ctx, `INSERT INTO payment_checks (client_id, bot_id) VALUES ($1, $2) ON CONFLICT (client_id) DO NOTHING`, claim.ID, claim.BotID); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT bot_id, review_code FROM payment_checks WHERE client_id = $1 FOR UPDATE`, claim.ID).Scan(&paymentBotID, &paymentReview); err != nil {
		return err
	}
	if paymentBotID != claim.BotID {
		if len(notices.PredecessorReview) == 0 {
			return errors.New("payment scope review requires admin notice")
		}
		for _, draft := range notices.PredecessorReview {
			if _, err := enqueueOutbox(ctx, tx, draft); err != nil {
				return err
			}
		}
		return finishInvoiceLifecycleLease(ctx, tx, claim, interval, lifecycle.ReviewPreviousPayment)
	}
	var confirmationClient, confirmationBot int64
	var confirmationCustomer string
	var confirmationReview bool
	var confirmationReviewCode int
	confirmErr := tx.QueryRow(ctx, `SELECT client_id, bot_id, customer_id, review_required, review_code
        FROM payment_confirmations WHERE invoice_id = $1 FOR UPDATE`, actual.CurrentInvoiceID).
		Scan(&confirmationClient, &confirmationBot, &confirmationCustomer, &confirmationReview, &confirmationReviewCode)
	if errors.Is(confirmErr, pgx.ErrNoRows) {
		return deferInvoiceLifecycleLease(ctx, tx, claim, time.Minute)
	}
	if confirmErr != nil {
		return confirmErr
	}
	if paymentReview != 0 || confirmationReview || confirmationReviewCode != 0 || confirmationClient != claim.ID || confirmationBot != claim.BotID || confirmationCustomer != actual.CustomerID {
		if len(notices.PredecessorReview) == 0 {
			return errors.New("predecessor payment review requires admin notice")
		}
		for _, draft := range notices.PredecessorReview {
			if _, err := enqueueOutbox(ctx, tx, draft); err != nil {
				return err
			}
		}
		return finishInvoiceLifecycleLease(ctx, tx, claim, interval, lifecycle.ReviewPreviousPayment)
	}

	var foreignBinding bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM subscriptions
        WHERE client_id <> $1 AND (first_invoice_id = $2 OR current_invoice_id = $2))`, claim.ID, invoice.ID).Scan(&foreignBinding); err != nil {
		return err
	}
	if foreignBinding {
		if len(notices.Review) == 0 {
			return errors.New("invoice association review requires admin notice")
		}
		for _, draft := range notices.Review {
			if _, err := enqueueOutbox(ctx, tx, draft); err != nil {
				return err
			}
		}
		return finishInvoiceLifecycleLease(ctx, tx, claim, interval, lifecycle.ReviewAssociation)
	}
	var existingClient int64
	lookupErr := tx.QueryRow(ctx, `SELECT client_id FROM invoice_lifecycle_invoices WHERE invoice_id = $1 FOR UPDATE`, invoice.ID).Scan(&existingClient)
	if lookupErr == nil {
		if len(notices.Review) == 0 {
			return errors.New("invoice collision review requires admin notice")
		}
		for _, draft := range notices.Review {
			if _, err := enqueueOutbox(ctx, tx, draft); err != nil {
				return err
			}
		}
		return finishInvoiceLifecycleLease(ctx, tx, claim, interval, lifecycle.ReviewAssociation)
	}
	if !errors.Is(lookupErr, pgx.ErrNoRows) {
		return lookupErr
	}
	noticeState := "issued"
	if invoice.Status == "paid" {
		noticeState = "settled"
	}
	if _, err := tx.Exec(ctx, `INSERT INTO invoice_lifecycle_invoices
        (invoice_id, client_id, bot_id, customer_id, recurring_id, invoice_number, status, sent_date, amount, balance, paid_to_date, notice_state)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8::date, $9::numeric, $10::numeric, $11::numeric, $12)`,
		invoice.ID, claim.ID, claim.BotID, actual.CustomerID, actual.RecurringID,
		invoice.Number, invoice.Status, invoice.SentDate, invoice.Amount, invoice.Balance, invoice.PaidToDate, noticeState); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE subscriptions SET current_invoice_id = $2
        WHERE client_id = $1 AND current_invoice_id = $3 AND first_invoice_id IS NOT DISTINCT FROM NULLIF($4, '')
            AND recurring_id = $5 AND state = 'active' AND desired_access = true`,
		claim.ID, invoice.ID, actual.CurrentInvoiceID, actual.FirstInvoiceID, actual.RecurringID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrConflict
	}
	if _, err := tx.Exec(ctx, `UPDATE payment_checks SET next_check_at = clock_timestamp(), last_error_code = 0
        WHERE client_id = $1 AND bot_id = $2 AND review_code = 0`, claim.ID, claim.BotID); err != nil {
		return err
	}
	if notices.Client != nil {
		if _, err := enqueueOutbox(ctx, tx, *notices.Client); err != nil {
			return err
		}
	}
	for _, draft := range notices.Admins {
		if _, err := enqueueOutbox(ctx, tx, draft); err != nil {
			return err
		}
	}
	return finishInvoiceLifecycleLease(ctx, tx, claim, interval, 0)
}

func sameLifecycleSnapshot(a, b lifecycle.Snapshot) bool {
	return a.ClientID == b.ClientID && a.TelegramID == b.TelegramID && a.ChatID == b.ChatID && a.BotID == b.BotID &&
		a.CustomerID == b.CustomerID && a.FirstInvoiceID == b.FirstInvoiceID && a.CurrentInvoiceID == b.CurrentInvoiceID &&
		a.RecurringID == b.RecurringID && a.SUIID == b.SUIID && a.RegistrationToken == b.RegistrationToken && a.ActivationToken == b.ActivationToken &&
		a.State == b.State && a.DesiredAccess == b.DesiredAccess && a.ManagedProvenance == b.ManagedProvenance && a.ImportedOrigin == b.ImportedOrigin && a.ImportedPrivateNotes == b.ImportedPrivateNotes &&
		a.SeenInvoiceOverflow == b.SeenInvoiceOverflow && slices.Equal(a.SeenInvoiceIDs, b.SeenInvoiceIDs)
}

func finishInvoiceLifecycleLease(ctx context.Context, tx pgx.Tx, claim lifecycle.Claim, interval time.Duration, reviewCode int) error {
	tag, err := tx.Exec(ctx, `UPDATE invoice_lifecycle_checks SET locked_until = NULL,
        next_check_at = clock_timestamp() + $5::bigint * interval '1 microsecond',
        observed_generation = $4, last_checked_at = clock_timestamp(), last_error_code = 0, review_code = $6
        WHERE client_id = $1 AND bot_id = $2 AND attempts = $3 AND lease_generation = $4
            AND locked_until > clock_timestamp()`, claim.ID, claim.BotID, claim.Attempt, claim.Generation, interval.Microseconds(), reviewCode)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrLeaseLost
	}
	return tx.Commit(ctx)
}

func deferInvoiceLifecycleLease(ctx context.Context, tx pgx.Tx, claim lifecycle.Claim, delay time.Duration) error {
	tag, err := tx.Exec(ctx, `UPDATE invoice_lifecycle_checks SET locked_until = NULL,
        next_check_at = clock_timestamp() + $5::bigint * interval '1 microsecond',
        observed_generation = $4, last_checked_at = clock_timestamp(), last_error_code = 0
        WHERE client_id = $1 AND bot_id = $2 AND attempts = $3 AND lease_generation = $4
            AND locked_until > clock_timestamp()`, claim.ID, claim.BotID, claim.Attempt, claim.Generation, delay.Microseconds())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrLeaseLost
	}
	return tx.Commit(ctx)
}

func invoiceLifecycleStageError(stage string, err error) error {
	if err == nil {
		return nil
	}
	for _, sentinel := range []error{queue.ErrLeaseLost, queue.ErrConflict, lifecycle.ErrOverduePaid, lifecycle.ErrOverdueNotUnpaid, lifecycle.ErrOverdueGuardBusy, lifecycle.ErrOverdueIntentExists, lifecycle.ErrOverduePaymentLease, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, sentinel) {
			return fmt.Errorf("%s: %w", stage, sentinel)
		}
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if pgErr.Code == "23505" {
			return fmt.Errorf("%s: %w", stage, queue.ErrConflict)
		}
		// SQLSTATE is a public error category, unlike Message/Detail/Where,
		// which can contain SQL parameters and private client data.
		if len(pgErr.Code) == 5 && strings.Trim(pgErr.Code, "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ") == "" {
			return fmt.Errorf("%s: PostgreSQL SQLSTATE %s", stage, pgErr.Code)
		}
	}
	return errors.New(stage)
}

func lifecycleIDs(ids []int64) bool {
	if len(ids) > 1000 {
		return false
	}
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id <= 0 || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}
