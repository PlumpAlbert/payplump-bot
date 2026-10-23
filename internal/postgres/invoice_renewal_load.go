package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/lifecycle"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/jackc/pgx/v5"
)

var _ lifecycle.RenewalStore = (*Store)(nil)

func (s *Store) SeedRenewalOperations(ctx context.Context, botID int64, allowed []int64, limit int) (count int, err error) {
	defer func() { err = invoiceLifecycleStageError("seed paid renewal actions", err) }()
	if botID <= 0 || !lifecycleIDs(allowed) || len(allowed) == 0 || limit < 1 || limit > 1000 {
		return 0, errors.New("invalid paid renewal seed bounds")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, err
	}
	defer rollback(tx)
	if err = prepareLifecycleBot(ctx, tx, botID); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `SELECT c.id, v.invoice_id, s.first_invoice_id
        FROM clients c JOIN subscriptions s ON s.client_id = c.id
        JOIN invoice_lifecycle_invoices v ON v.client_id = c.id AND v.invoice_id = s.current_invoice_id
        JOIN payment_confirmations pc ON pc.invoice_id = v.invoice_id AND pc.client_id = c.id AND pc.bot_id = $1
        WHERE `+invoiceLifecycleManaged+`
          AND c.telegram_id = ANY($2::bigint[]) AND s.state IN ('active', 'overdue')
          AND ((s.state = 'active' AND s.desired_access = true) OR (s.state = 'overdue' AND s.desired_access = false))
          AND v.bot_id = $1 AND v.customer_id = c.invoice_ninja_id AND v.recurring_id = s.recurring_id
          AND v.invoice_id <> s.first_invoice_id AND pc.customer_id = c.invoice_ninja_id
          AND NOT pc.review_required AND pc.review_code = 0
          AND NOT EXISTS (SELECT 1 FROM invoice_renewal_actions a WHERE a.client_id = c.id AND a.invoice_id = v.invoice_id)
        ORDER BY pc.payment_date, c.id LIMIT $3 FOR NO KEY UPDATE OF c SKIP LOCKED`, botID, allowed, limit)
	if err != nil {
		return 0, err
	}
	type candidate struct {
		clientID       int64
		invoiceID      string
		firstInvoiceID string
	}
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
		var c candidate
		e := row.Scan(&c.clientID, &c.invoiceID, &c.firstInvoiceID)
		return c, e
	})
	if err != nil {
		return 0, err
	}
	if len(candidates) == 0 {
		return 0, tx.Commit(ctx)
	}
	clientIDs := make([]int64, 0, len(candidates))
	invoiceIDs := make([]string, 0, len(candidates)*2)
	for _, c := range candidates {
		clientIDs = append(clientIDs, c.clientID)
		invoiceIDs = append(invoiceIDs, c.invoiceID)
		if c.firstInvoiceID != c.invoiceID {
			invoiceIDs = append(invoiceIDs, c.firstInvoiceID)
		}
	}
	if err = lockPaymentSubscriptions(ctx, tx, clientIDs); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `SELECT client_id FROM payment_checks WHERE client_id = ANY($1::bigint[]) AND bot_id = $2 ORDER BY client_id FOR UPDATE`, clientIDs, botID); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `SELECT invoice_id FROM payment_confirmations WHERE invoice_id = ANY($1::text[]) ORDER BY invoice_id FOR UPDATE`, invoiceIDs); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `SELECT invoice_id FROM invoice_lifecycle_invoices WHERE invoice_id = ANY($1::text[]) ORDER BY invoice_id FOR UPDATE`, invoiceIDs); err != nil {
		return 0, err
	}
	for _, c := range candidates {
		var eligible bool
		err = tx.QueryRow(ctx, `SELECT `+invoiceLifecycleManaged+`
            AND c.telegram_id = ANY($2::bigint[]) AND s.state IN ('active', 'overdue')
            AND ((s.state = 'active' AND s.desired_access = true) OR (s.state = 'overdue' AND s.desired_access = false))
            AND s.current_invoice_id = $3 AND s.current_invoice_id <> s.first_invoice_id
            AND v.bot_id = $1 AND v.customer_id = c.invoice_ninja_id AND v.recurring_id = s.recurring_id
            AND pc.customer_id = c.invoice_ninja_id AND NOT pc.review_required AND pc.review_code = 0
            AND NOT EXISTS (SELECT 1 FROM invoice_renewal_actions a WHERE a.client_id = c.id AND a.invoice_id = v.invoice_id)
            FROM clients c JOIN subscriptions s ON s.client_id = c.id
            JOIN invoice_lifecycle_invoices v ON v.client_id = c.id AND v.invoice_id = $3
            JOIN payment_confirmations pc ON pc.invoice_id = $3 AND pc.client_id = c.id AND pc.bot_id = $1
            WHERE c.id = $4`, botID, allowed, c.invoiceID, c.clientID).Scan(&eligible)
		if errors.Is(err, pgx.ErrNoRows) || !eligible {
			continue
		}
		if err != nil {
			return 0, err
		}
		digest := sha256.Sum256([]byte(c.invoiceID))
		key := "invoice-renewal:" + hex.EncodeToString(digest[:])
		var operationID int64
		err = tx.QueryRow(ctx, `INSERT INTO operations (client_id, operation_key, kind, status)
            VALUES ($1, $2, $3, 'pending') ON CONFLICT (operation_key) DO NOTHING RETURNING id`, c.clientID, key, lifecycle.RenewalOperationKind).Scan(&operationID)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO invoice_renewal_actions (operation_id, client_id, bot_id, invoice_id) VALUES ($1, $2, $3, $4)`, operationID, c.clientID, botID, c.invoiceID); err != nil {
			return 0, err
		}
		count++
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) LoadRenewalSnapshot(ctx context.Context, claim queue.Operation, botID int64) (snapshot lifecycle.RenewalSnapshot, err error) {
	defer func() { err = invoiceLifecycleStageError("load paid renewal snapshot", err) }()
	if claim.ID <= 0 || claim.Attempt <= 0 || claim.ClientID <= 0 || claim.Kind != lifecycle.RenewalOperationKind || botID <= 0 {
		return snapshot, queue.ErrLeaseLost
	}
	snapshot, err = readRenewalSnapshot(ctx, s.pool, claim, botID)
	if err != nil {
		return lifecycle.RenewalSnapshot{}, err
	}
	return snapshot, nil
}

type renewalSnapshotReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func readRenewalSnapshot(ctx context.Context, reader renewalSnapshotReader, claim queue.Operation, botID int64) (lifecycle.RenewalSnapshot, error) {
	var s lifecycle.RenewalSnapshot
	var recurringRequest, evidence, proofHash, credentialHash, baseline []byte
	var amount, currencyID, paymentDate, nextDate, planDate string
	var actionOutcome string
	var reviewCode int
	var planFrozen, planPause, planSchedule, planEnable, planResume bool
	var suiID int64
	var paymentCheckReview, paymentConfirmationReview bool
	err := reader.QueryRow(ctx, `SELECT c.id, c.telegram_id, c.chat_id, a.bot_id, COALESCE(a.invoice_id, ''),
        COALESCE(c.invoice_ninja_id, ''), COALESCE(s.first_invoice_id, ''), COALESCE(s.current_invoice_id, ''),
        COALESCE(s.recurring_id, ''), COALESCE(c.sui_id, ''), COALESCE(reg.token, ''), COALESCE(act.token, ''),
        s.state, s.desired_access, v.customer_id, v.recurring_id, v.invoice_number, v.status, v.amount::text, v.balance::text, v.paid_to_date::text, v.sent_date::text,
        pc.customer_id, pc.invoice_id, pc.invoice_number, pc.amount::text, pc.currency_id, pc.payment_date::text, pc.next_invoice_date::text, pc.proof_hash, pc.evidence::text,
        COALESCE(rec.request, '{}'::jsonb)::text, COALESCE(binding.sui_id, 0), COALESCE(binding.name, ''), COALESCE(binding.credentials_hash, ''::bytea),
        (`+invoiceLifecycleManaged+`), p.review_code <> 0, (p.locked_until IS NOT NULL AND p.locked_until > clock_timestamp()),
        (pc.review_required OR pc.review_code <> 0), (lc.client_id IS NULL OR lc.review_code <> 0),
        g.epoch, (g.operation_id IS NOT NULL), COALESCE(g.operation_id, 0), COALESCE(g.step, ''),
        a.outcome, a.review_code, (a.plan_frozen_at IS NOT NULL), COALESCE(a.target_date::text, ''),
        COALESCE(a.requires_pause, false), COALESCE(a.requires_schedule, false), COALESCE(a.requires_enable, false), COALESCE(a.requires_resume, false), COALESCE(a.baseline, '{}'::jsonb)::text
        FROM invoice_renewal_actions a JOIN operations o ON o.id = a.operation_id
        JOIN clients c ON c.id = a.client_id JOIN subscriptions s ON s.client_id = c.id
        JOIN invoice_lifecycle_invoices v ON v.client_id = a.client_id AND v.invoice_id = a.invoice_id
        JOIN payment_checks p ON p.client_id = c.id AND p.bot_id = a.bot_id
        JOIN payment_confirmations pc ON pc.invoice_id = a.invoice_id AND pc.client_id = a.client_id AND pc.bot_id = a.bot_id
        LEFT JOIN invoice_lifecycle_checks lc ON lc.client_id = c.id AND lc.bot_id = a.bot_id
        LEFT JOIN LATERAL (SELECT rf.token FROM operations ro JOIN registration_flows rf ON rf.operation_id = ro.id WHERE ro.client_id = c.id AND ro.kind = 'register_client' AND ro.status = 'completed' AND rf.bot_id = a.bot_id ORDER BY ro.id DESC LIMIT 1) reg ON true
        LEFT JOIN LATERAL (SELECT af.token, af.operation_id FROM operations ao JOIN activation_flows af ON af.operation_id = ao.id WHERE ao.client_id = c.id AND ao.kind = 'activate_first_subscription' AND ao.status = 'completed' AND af.bot_id = a.bot_id ORDER BY ao.id DESC LIMIT 1) act ON true
        LEFT JOIN activation_steps rec ON rec.operation_id = act.operation_id AND rec.step = 'recurring_create' AND rec.confirmed_at IS NOT NULL
        LEFT JOIN activation_sui_bindings binding ON binding.client_id = c.id AND binding.bot_id = a.bot_id AND binding.sui_id::text = c.sui_id
        CROSS JOIN activation_write_guard g
        WHERE g.id = 1 AND a.operation_id = $2 AND a.bot_id = $1 AND a.client_id = $3 AND a.outcome = 'pending' AND a.review_code = 0
          AND o.kind = $4 AND o.status = 'running' AND o.attempts = $5 AND o.locked_until > clock_timestamp()`,
		botID, claim.ID, claim.ClientID, lifecycle.RenewalOperationKind, claim.Attempt).Scan(
		&s.ClientID, &s.TelegramID, &s.ChatID, &s.BotID, &s.InvoiceID, &s.CustomerID, &s.FirstInvoiceID, &s.CurrentInvoiceID,
		&s.RecurringID, &s.SUIID, &s.RegistrationToken, &s.ActivationToken, &s.State, &s.DesiredAccess,
		&s.LedgerCustomerID, &s.LedgerRecurringID, &s.LedgerInvoiceNumber, &s.LedgerStatus, &s.LedgerAmount, &s.LedgerBalance, &s.LedgerPaidToDate, &s.LedgerSentDate,
		&s.Payment.CustomerID, &s.Payment.InvoiceID, &s.Proof.InvoiceNumber, &amount, &currencyID, &paymentDate, &nextDate, &proofHash, &evidence,
		&recurringRequest, &suiID, &s.SUIBinding.Name, &credentialHash, &s.ManagedProvenance, &paymentCheckReview, &s.PaymentLocked,
		&paymentConfirmationReview, &s.LifecycleReview, &s.WriteEpoch, &s.WriterBusy, &s.WriterOperationID, &s.WriterStep,
		&actionOutcome, &reviewCode, &planFrozen, &planDate, &planPause, &planSchedule, &planEnable, &planResume, &baseline)
	if errors.Is(err, pgx.ErrNoRows) {
		return lifecycle.RenewalSnapshot{}, queue.ErrLeaseLost
	}
	if err != nil {
		return lifecycle.RenewalSnapshot{}, err
	}
	if actionOutcome != "pending" || reviewCode != 0 || len(proofHash) != 32 || len(credentialHash) != 32 {
		return lifecycle.RenewalSnapshot{}, queue.ErrConflict
	}
	if json.Unmarshal(evidence, &s.Proof.Evidence) != nil {
		return lifecycle.RenewalSnapshot{}, queue.ErrConflict
	}
	s.Proof.Amount = amount
	s.Proof.CurrencyID = currencyID
	s.Proof.PaymentDate = paymentDate
	s.Proof.NextInvoiceDate = nextDate
	copy(s.Proof.Hash[:], proofHash)
	s.Proof.JSON, err = json.Marshal(s.Proof.Evidence)
	if err != nil || s.Proof.Validate() != nil {
		return lifecycle.RenewalSnapshot{}, queue.ErrConflict
	}
	s.Payment.ClientID = s.ClientID
	s.Payment.TelegramID = s.TelegramID
	s.Payment.ChatID = s.ChatID
	s.Payment.BotID = botID
	s.Payment.CustomerID = s.CustomerID
	s.Payment.InvoiceID = s.CurrentInvoiceID
	s.Payment.FirstInvoiceID = s.FirstInvoiceID
	s.Payment.RecurringID = s.RecurringID
	s.Payment.State = s.State
	s.Payment.DesiredAccess = s.DesiredAccess
	s.Payment.PaymentDate = paymentDate
	s.Payment.NextInvoiceDate = nextDate
	s.PaymentReview = paymentCheckReview || paymentConfirmationReview
	s.SUIBinding.ID = suiID
	copy(s.SUIBinding.CredentialsHash[:], credentialHash)
	if json.Unmarshal(recurringRequest, &s.RecurringDraft) != nil || invoiceninja.ValidateRecurringDraft(s.RecurringDraft) != nil {
		return lifecycle.RenewalSnapshot{}, queue.ErrConflict
	}
	if planFrozen {
		var facts struct {
			Amount           json.Number `json:"amount"`
			NextSendDatetime string      `json:"next_send_datetime"`
		}
		if planDate == "" || json.Unmarshal(baseline, &facts) != nil || facts.Amount == "" || facts.NextSendDatetime == "" {
			return lifecycle.RenewalSnapshot{}, queue.ErrConflict
		}
		when, parseErr := invoiceninja.ParseRecurringUTC(facts.NextSendDatetime)
		if parseErr != nil || !planSchedule && !when.After(time.Now()) {
			return lifecycle.RenewalSnapshot{}, queue.ErrConflict
		}
		s.Plan = &lifecycle.RenewalPlan{TargetDate: planDate, Pause: planPause, Schedule: planSchedule, Enable: planEnable, Resume: planResume, BaselineReceipt: invoiceninja.RecurringReceipt{Amount: facts.Amount, NextSendDatetime: facts.NextSendDatetime}}
		s.PlanBaseline = append([]byte(nil), baseline...)
	}
	s.Steps = make(map[lifecycle.RenewalStep]lifecycle.RenewalStepRecord, 4)
	rows, err := reader.Query(ctx, `SELECT step, request::text, COALESCE(external_id, ''), COALESCE(receipt::text, ''), dispatch_attempt, confirmed_at IS NOT NULL FROM invoice_renewal_steps WHERE operation_id = $1 ORDER BY step`, claim.ID)
	if err != nil {
		return lifecycle.RenewalSnapshot{}, err
	}
	for rows.Next() {
		var name string
		var request, receipt []byte
		var step lifecycle.RenewalStepRecord
		if err = rows.Scan(&name, &request, &step.ExternalID, &receipt, &step.DispatchAttempt, &step.Confirmed); err != nil {
			rows.Close()
			return lifecycle.RenewalSnapshot{}, err
		}
		step.Request = append([]byte(nil), request...)
		step.Receipt = append([]byte(nil), receipt...)
		key := lifecycle.RenewalStep(name)
		if !key.Valid() {
			rows.Close()
			return lifecycle.RenewalSnapshot{}, queue.ErrConflict
		}
		s.Steps[key] = step
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return lifecycle.RenewalSnapshot{}, err
	}
	rows.Close()
	s.OverdueSteps = make(map[string]bool)
	rows, err = reader.Query(ctx, `SELECT st.step, st.external_id FROM invoice_overdue_actions a JOIN invoice_overdue_steps st ON st.operation_id = a.operation_id WHERE a.client_id = $1 AND a.invoice_id = $2 AND st.confirmed_at IS NOT NULL`, s.ClientID, s.InvoiceID)
	if err != nil {
		return lifecycle.RenewalSnapshot{}, err
	}
	for rows.Next() {
		var name, external string
		if err = rows.Scan(&name, &external); err != nil {
			rows.Close()
			return lifecycle.RenewalSnapshot{}, err
		}
		if name == "pause_recurring" && external == s.RecurringID || name == "disable_sui" && external == s.SUIID {
			s.OverdueSteps[name] = true
		} else {
			s.OverdueOtherSteps = true
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return lifecycle.RenewalSnapshot{}, err
	}
	rows.Close()
	if err = reader.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM invoice_overdue_actions a JOIN invoice_overdue_steps st ON st.operation_id=a.operation_id WHERE a.client_id=$1 AND a.invoice_id=$2 AND st.confirmed_at IS NULL)`, s.ClientID, s.InvoiceID).Scan(&s.OverduePending); err != nil {
		return lifecycle.RenewalSnapshot{}, err
	}
	if err = reader.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM invoice_overdue_actions a WHERE a.client_id=$1 AND a.invoice_id=$2 AND a.outcome='review' AND a.review_code=$3)`, s.ClientID, s.InvoiceID, lifecycle.ReviewOverduePayment).Scan(&s.OverdueReviewPayment); err != nil {
		return lifecycle.RenewalSnapshot{}, err
	}
	return s, nil
}
