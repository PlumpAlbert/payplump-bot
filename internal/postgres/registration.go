package postgres

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"unicode"
	"unicode/utf8"

	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/registration"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var _ registration.Store = (*Store)(nil)

// LoadRegistration initializes only local correlation state. Existing external
// associations are observations, not permission to adopt or overwrite them.
func (s *Store) LoadRegistration(ctx context.Context, lease queue.Lease, botID int64) (registration.Snapshot, error) {
	if botID <= 0 {
		return registration.Snapshot{}, errors.New("registration requires a positive bot identity")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return registration.Snapshot{}, registrationStageError("begin registration load", err)
	}
	defer rollback(tx)
	// Match the first-webhook lock order: its uncommitted bot insert may be
	// waiting for this client. Never take the client lock before this insert.
	if _, err := tx.Exec(ctx, `INSERT INTO telegram_bots (bot_id) VALUES ($1) ON CONFLICT DO NOTHING`, botID); err != nil {
		return registration.Snapshot{}, registrationStageError("prepare registration bot", err)
	}
	clientID, err := lockRegistrationLease(ctx, tx, lease)
	if err != nil {
		return registration.Snapshot{}, err
	}
	if _, err := tx.Exec(ctx, `
        INSERT INTO registration_flows (operation_id, bot_id, token) VALUES ($1, $2, $3)
        ON CONFLICT (operation_id) DO NOTHING`, lease.ID, botID, rand.Text()); err != nil {
		return registration.Snapshot{}, registrationStageError("initialize registration flow", err)
	}
	var snapshot registration.Snapshot
	if err := tx.QueryRow(ctx, `SELECT bot_id, token FROM registration_flows WHERE operation_id = $1`, lease.ID).
		Scan(&snapshot.BotID, &snapshot.Token); err != nil {
		return registration.Snapshot{}, registrationStageError("read registration flow", err)
	}
	if snapshot.BotID != botID {
		return registration.Snapshot{}, queue.ErrConflict
	}
	if err := tx.QueryRow(ctx, `
        SELECT c.id, c.telegram_id, c.chat_id, c.display_name,
            COALESCE(c.invoice_ninja_id, ''), COALESCE(c.sui_id, ''),
            COALESCE(s.first_invoice_id, ''), COALESCE(s.current_invoice_id, ''),
            COALESCE(s.recurring_id, ''), s.state, s.desired_access
        FROM clients c JOIN subscriptions s ON s.client_id = c.id
        WHERE c.id = $1`, clientID).Scan(
		&snapshot.ClientID, &snapshot.TelegramID, &snapshot.ChatID, &snapshot.DisplayName,
		&snapshot.ExistingCustomerID, &snapshot.ExistingSUIID,
		&snapshot.ExistingFirstInvoiceID, &snapshot.ExistingCurrentInvoiceID,
		&snapshot.ExistingRecurringID, &snapshot.SubscriptionState, &snapshot.DesiredAccess,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return registration.Snapshot{}, queue.ErrConflict
		}
		return registration.Snapshot{}, registrationStageError("read registration account", err)
	}
	rows, err := tx.Query(ctx, `
        SELECT step, request, COALESCE(external_id, '')
        FROM operation_steps WHERE operation_id = $1`, lease.ID)
	if err != nil {
		return registration.Snapshot{}, registrationStageError("read registration steps", err)
	}
	snapshot.Steps = make(map[registration.Step]registration.Intent)
	for rows.Next() {
		var step registration.Step
		var intent registration.Intent
		if err := rows.Scan(&step, &intent.Payload, &intent.ExternalID); err != nil {
			rows.Close()
			return registration.Snapshot{}, registrationStageError("decode registration step", err)
		}
		snapshot.Steps[step] = intent
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return registration.Snapshot{}, registrationStageError("read registration steps", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return registration.Snapshot{}, registrationStageError("commit registration load", err)
	}
	return snapshot, nil
}

// PrepareRegistrationWrite grants permission only for a newly committed intent.
// Neither a new attempt nor manual resume can ever rearm an unresolved write.
func (s *Store) PrepareRegistrationWrite(ctx context.Context, lease queue.Lease, step registration.Step, payload json.RawMessage) (bool, error) {
	if !step.Valid() {
		return false, errors.New("invalid registration step")
	}
	trimmed := bytes.TrimSpace(payload)
	if len(payload) > 64*1024 || len(trimmed) == 0 || trimmed[0] != '{' || !utf8.Valid(payload) || !json.Valid(payload) {
		return false, errors.New("registration request must be a JSON object of at most 64 KiB")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, registrationStageError("begin registration intent", err)
	}
	defer rollback(tx)
	if _, err := lockRegistrationLease(ctx, tx, lease); err != nil {
		return false, err
	}
	if err := requireRegistrationFlow(ctx, tx, lease.ID); err != nil {
		return false, err
	}
	// A concurrent import/payment transition must not grant a new external
	// write based on an earlier snapshot. Registration owns only a fresh local
	// unpaid account, not any pre-existing billing or access association.
	var fresh bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
        SELECT 1 FROM operations o JOIN clients c ON c.id = o.client_id
            JOIN subscriptions s ON s.client_id = c.id
        WHERE o.id = $1 AND c.invoice_ninja_id IS NULL AND c.sui_id IS NULL
            AND s.current_invoice_id IS NULL AND s.first_invoice_id IS NULL
            AND s.recurring_id IS NULL AND s.state = 'awaiting_payment' AND s.desired_access = false
    )`, lease.ID).Scan(&fresh); err != nil {
		return false, registrationStageError("check fresh registration account", err)
	}
	if !fresh {
		return false, queue.ErrConflict
	}
	var previous registration.Step
	switch step {
	case registration.InvoiceCreate:
		previous = registration.CustomerCreate
	case registration.InvoiceIssue:
		previous = registration.InvoiceCreate
	}
	if previous != "" {
		var confirmed bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
            SELECT 1 FROM operation_steps WHERE operation_id = $1 AND step = $2
                AND external_id IS NOT NULL AND confirmed_at IS NOT NULL
        )`, lease.ID, previous).Scan(&confirmed); err != nil {
			return false, registrationStageError("check registration step order", err)
		}
		if !confirmed {
			return false, queue.ErrConflict
		}
	}
	tag, err := tx.Exec(ctx, `
        INSERT INTO operation_steps (operation_id, step, request) VALUES ($1, $2, $3::jsonb)
        ON CONFLICT (operation_id, step) DO NOTHING`, lease.ID, step, string(payload))
	if err != nil {
		return false, registrationStageError("save registration intent", err)
	}
	granted := tag.RowsAffected() == 1
	if !granted {
		var same bool
		if err := tx.QueryRow(ctx, `
            SELECT request = $3::jsonb FROM operation_steps WHERE operation_id = $1 AND step = $2`,
			lease.ID, step, string(payload)).Scan(&same); err != nil {
			return false, registrationStageError("check registration intent", err)
		}
		if !same {
			return false, queue.ErrConflict
		}
	}
	if err := tx.Commit(ctx); err != nil {
		// Even if COMMIT reached PostgreSQL, an uncertain acknowledgement never
		// grants permission to POST. The durable intent now requires review.
		return false, registrationStageError("commit registration intent", err)
	}
	return granted, nil
}

func (s *Store) ConfirmRegistrationStep(ctx context.Context, lease queue.Lease, step registration.Step, externalID string) error {
	if !step.Valid() {
		return errors.New("invalid registration step")
	}
	if !validRegistrationID(externalID) {
		return errors.New("invalid registration external identity")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return registrationStageError("begin registration confirmation", err)
	}
	defer rollback(tx)
	if _, err := lockRegistrationLease(ctx, tx, lease); err != nil {
		return err
	}
	if err := requireRegistrationFlow(ctx, tx, lease.ID); err != nil {
		return err
	}
	if step == registration.InvoiceIssue {
		var matches bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
            SELECT 1 FROM operation_steps WHERE operation_id = $1 AND step = 'invoice_create'
                AND confirmed_at IS NOT NULL AND external_id = $2
        )`, lease.ID, externalID).Scan(&matches); err != nil {
			return registrationStageError("check issued invoice identity", err)
		}
		if !matches {
			return queue.ErrConflict
		}
	}
	tag, err := tx.Exec(ctx, `
        UPDATE operation_steps SET external_id = $3, confirmed_at = COALESCE(confirmed_at, clock_timestamp())
        WHERE operation_id = $1 AND step = $2 AND (external_id IS NULL OR external_id = $3)`,
		lease.ID, step, externalID)
	if err != nil {
		return registrationStageError("confirm registration step", err)
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return registrationStageError("commit registration confirmation", err)
	}
	return nil
}

// HoldRegistration leaves an unclaimable FIFO head, preserving every intent and
// atomically recording any manual-review notifications.
func (s *Store) HoldRegistration(ctx context.Context, lease queue.Lease, reason int, drafts []queue.Draft) error {
	if reason <= 0 || int64(reason) > 1<<31-1 {
		return errors.New("registration hold requires a positive review code")
	}
	if len(drafts) > 100 {
		return errors.New("registration supports at most 100 notifications")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return registrationStageError("begin registration hold", err)
	}
	defer rollback(tx)
	if err := prepareOutboxBots(ctx, tx, drafts); err != nil {
		return registrationStageError("prepare registration notifications", err)
	}
	if _, err := lockRegistrationLease(ctx, tx, lease); err != nil {
		return err
	}
	if err := requireRegistrationFlow(ctx, tx, lease.ID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `
        UPDATE operations SET status = 'pending', locked_until = NULL,
            next_attempt_at = 'infinity', last_error_code = $3
        WHERE id = $1 AND attempts = $2 AND kind = 'register_client' AND status = 'running'
            AND locked_until > clock_timestamp()`, lease.ID, lease.Attempt, reason)
	if err != nil {
		return registrationStageError("hold registration operation", err)
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrLeaseLost
	}
	if _, err := tx.Exec(ctx, `UPDATE registration_flows SET review_code = $2 WHERE operation_id = $1`, lease.ID, reason); err != nil {
		return registrationStageError("record registration review", err)
	}
	for _, draft := range drafts {
		if _, err := enqueueOutbox(ctx, tx, draft); err != nil {
			return registrationStageError("enqueue registration review", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return registrationStageError("commit registration hold", err)
	}
	return nil
}

// ResumeRegistration only makes a held operation claimable. In particular it
// neither deletes intents nor grants new external-write permission.
func (s *Store) ResumeRegistration(ctx context.Context, operationID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return registrationStageError("begin registration resume", err)
	}
	defer rollback(tx)
	if err := lockOperationClient(ctx, tx, operationID); err != nil {
		if errors.Is(err, queue.ErrLeaseLost) {
			return queue.ErrConflict
		}
		return registrationStageError("lock registration client", err)
	}
	tag, err := tx.Exec(ctx, `
        UPDATE operations o SET next_attempt_at = clock_timestamp()
        FROM registration_flows f
        WHERE o.id = $1 AND f.operation_id = o.id AND f.review_code > 0
            AND o.kind = 'register_client' AND o.status = 'pending'
            AND o.locked_until IS NULL AND o.next_attempt_at = 'infinity'::timestamptz`, operationID)
	if err != nil {
		return registrationStageError("resume registration operation", err)
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrConflict
	}
	if _, err := tx.Exec(ctx, `UPDATE registration_flows SET review_code = 0 WHERE operation_id = $1`, operationID); err != nil {
		return registrationStageError("clear registration review", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return registrationStageError("commit registration resume", err)
	}
	return nil
}

// FinishRegistration associates only a fully confirmed, unpaid first invoice.
// This transition never enables access or creates a recurring invoice.
func (s *Store) FinishRegistration(ctx context.Context, lease queue.Lease, customerID, invoiceID string, drafts []queue.Draft) error {
	if !validRegistrationID(customerID) || !validRegistrationID(invoiceID) {
		return errors.New("invalid registration external identity")
	}
	if len(drafts) > 100 {
		return errors.New("registration supports at most 100 notifications")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return registrationStageError("begin registration finish", err)
	}
	defer rollback(tx)
	if err := prepareOutboxBots(ctx, tx, drafts); err != nil {
		return registrationStageError("prepare registration notifications", err)
	}
	clientID, err := lockRegistrationLease(ctx, tx, lease)
	if err != nil {
		return err
	}
	if err := requireRegistrationFlow(ctx, tx, lease.ID); err != nil {
		return err
	}
	var confirmed bool
	if err := tx.QueryRow(ctx, `SELECT count(*) = 3 FROM operation_steps
        WHERE operation_id = $1 AND confirmed_at IS NOT NULL
            AND ((step = 'customer_create' AND external_id = $2)
                OR (step IN ('invoice_create', 'invoice_issue') AND external_id = $3))`,
		lease.ID, customerID, invoiceID).Scan(&confirmed); err != nil {
		return registrationStageError("check registration confirmations", err)
	}
	if !confirmed {
		return queue.ErrConflict
	}
	tag, err := tx.Exec(ctx, `
        UPDATE clients SET invoice_ninja_id = $2
        WHERE id = $1 AND sui_id IS NULL AND (invoice_ninja_id IS NULL OR invoice_ninja_id = $2)`, clientID, customerID)
	if err != nil {
		return registrationStageError("associate registration customer", err)
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrConflict
	}
	tag, err = tx.Exec(ctx, `
        UPDATE subscriptions SET first_invoice_id = $2, current_invoice_id = $2
        WHERE client_id = $1 AND state = 'awaiting_payment' AND desired_access = false AND recurring_id IS NULL
            AND (first_invoice_id IS NULL OR first_invoice_id = $2)
            AND (current_invoice_id IS NULL OR current_invoice_id = $2)`, clientID, invoiceID)
	if err != nil {
		return registrationStageError("associate registration invoice", err)
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrConflict
	}
	tag, err = tx.Exec(ctx, `
        UPDATE operations SET status = 'completed', locked_until = NULL,
            completed_at = clock_timestamp(), last_error_code = 0
        WHERE id = $1 AND attempts = $2 AND kind = 'register_client' AND status = 'running'
            AND locked_until > clock_timestamp()`, lease.ID, lease.Attempt)
	if err != nil {
		return registrationStageError("complete registration operation", err)
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrLeaseLost
	}
	for _, draft := range drafts {
		if _, err := enqueueOutbox(ctx, tx, draft); err != nil {
			return registrationStageError("enqueue registration notification", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return registrationStageError("commit registration finish", err)
	}
	return nil
}

// All registration methods fence before reading or mutating flow/step state,
// including repeated confirmations and repeated intent preparation.
func lockRegistrationLease(ctx context.Context, tx pgx.Tx, lease queue.Lease) (int64, error) {
	if err := lockOperationClient(ctx, tx, lease.ID); err != nil {
		return 0, registrationStageError("lock registration client", err)
	}
	var clientID int64
	err := tx.QueryRow(ctx, `
        SELECT client_id FROM operations
        WHERE id = $1 AND attempts = $2 AND kind = 'register_client' AND status = 'running'
            AND locked_until > clock_timestamp()
        FOR UPDATE`, lease.ID, lease.Attempt).Scan(&clientID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, queue.ErrLeaseLost
	}
	if err != nil {
		return 0, registrationStageError("check registration lease", err)
	}
	return clientID, nil
}

func requireRegistrationFlow(ctx context.Context, tx pgx.Tx, operationID int64) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM registration_flows WHERE operation_id = $1)`, operationID).Scan(&exists); err != nil {
		return registrationStageError("check registration flow", err)
	}
	if !exists {
		return queue.ErrConflict
	}
	return nil
}

func validRegistrationID(id string) bool {
	if id == "" || len(id) > 256 || !utf8.ValidString(id) {
		return false
	}
	for _, r := range id {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// PostgreSQL errors can include request JSON, remote identities, or draft text
// in Detail. Expose only the local stage and safe classifications to callers.
func registrationStageError(stage string, err error) error {
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, queue.ErrLeaseLost):
		return fmt.Errorf("%s: %w", stage, queue.ErrLeaseLost)
	case errors.Is(err, queue.ErrConflict), errors.As(err, &pgErr) && pgErr.Code == "23505":
		return fmt.Errorf("%s: %w", stage, queue.ErrConflict)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%s: %w", stage, context.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%s: %w", stage, context.DeadlineExceeded)
	default:
		return errors.New(stage)
	}
}
