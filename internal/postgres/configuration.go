package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/clientmenu"
	"github.com/PlumpAlbert/payplump-bot/internal/configdelivery"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
	"github.com/jackc/pgx/v5"
)

var _ configdelivery.Store = (*Store)(nil)

const configurationDeniedText = clientmenu.ConfigurationUnavailableText

// Every error crossing this boundary is static or an explicitly safe sentinel.
func configurationError(err error) error {
	if err == nil {
		return nil
	}
	for _, sentinel := range []error{queue.ErrLeaseLost, queue.ErrConflict, configdelivery.ErrDenied, configdelivery.ErrAlreadyDispatched, configdelivery.ErrStalePolicy, ErrServicePolicyRevoked, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, sentinel) {
			return fmt.Errorf("protected configuration storage: %w", sentinel)
		}
	}
	return errors.New("protected configuration storage unavailable")
}

// Policy transitions serialize before acquiring bot rows. The lock is scoped
// to the isolated application schema, not to a process or a configured bot.
const configurationPolicyLock = `SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || ':payplump-configuration-policy',0))`

// DisableConfigurationPolicies runs even when Telegram is disabled or its setup
// fails. Startup must not report ready while an older known policy remains on.
// Already-authorized HTTP sends still require sender quiescence; this revokes
// new permissions and never releases an unknown chat reservation.
func (s *Store) DisableConfigurationPolicies(ctx context.Context) (err error) {
	defer func() { err = configurationError(err) }()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if _, err = tx.Exec(ctx, configurationPolicyLock); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT bot_id FROM configuration_policies WHERE enabled ORDER BY bot_id`)
	if err != nil {
		return err
	}
	var bots []int64
	for rows.Next() {
		var bot int64
		if err = rows.Scan(&bot); err != nil {
			rows.Close()
			return err
		}
		bots = append(bots, bot)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, bot := range bots {
		if _, err = tx.Exec(ctx, `SELECT bot_id FROM telegram_bots WHERE bot_id=$1 FOR NO KEY UPDATE`, bot); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `SET LOCAL synchronous_commit = on`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE configuration_policies SET revision=revision+1,enabled=false WHERE enabled`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) InstallConfigurationPolicy(ctx context.Context, botID int64, policy configdelivery.Policy) error {
	return s.installConfigurationPolicy(ctx, botID, &policy, false)
}

func (s *Store) InstallServiceConfigurationPolicy(ctx context.Context, botID int64, policy *configdelivery.Policy) error {
	return s.installConfigurationPolicy(ctx, botID, policy, true)
}

func (s *Store) installConfigurationPolicy(ctx context.Context, botID int64, target *configdelivery.Policy, automatic bool) (err error) {
	defer func() { err = configurationError(err) }()
	if target == nil {
		return configdelivery.ErrStalePolicy
	}
	policy := *target
	if botID <= 0 || (policy.Enabled && (policy.Revision < 1 || policy.Hash == ([32]byte{}))) ||
		(!policy.Enabled && (policy.Revision != 0 || policy.Hash != ([32]byte{}))) {
		return configdelivery.ErrStalePolicy
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if _, err = tx.Exec(ctx, configurationPolicyLock); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO telegram_bots (bot_id) VALUES ($1) ON CONFLICT DO NOTHING`, botID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `SELECT bot_id FROM telegram_bots WHERE bot_id=$1 FOR NO KEY UPDATE`, botID); err != nil {
		return err
	}
	stored, found, err := readConfigurationPolicy(ctx, tx, botID, true)
	if err != nil {
		return err
	}
	if automatic {
		policy.Revision, err = servicePolicyRevision(found, stored.Enabled, stored.Revision, stored.Hash == policy.Hash)
		if err != nil {
			return err
		}
		*target = policy
	}
	next := policy
	if found {
		if !policy.Enabled {
			if !stored.Enabled {
				return tx.Commit(ctx)
			}
			if stored.Revision == 1<<63-1 {
				return configdelivery.ErrStalePolicy
			}
			next = configdelivery.Policy{Revision: stored.Revision + 1, Hash: stored.Hash}
		} else {
			if policy.Revision < stored.Revision || (policy.Revision == stored.Revision && policy != stored) {
				return configdelivery.ErrStalePolicy
			}
			if policy == stored {
				return tx.Commit(ctx)
			}
		}
	}
	if _, err = tx.Exec(ctx, `SET LOCAL synchronous_commit = on`); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO configuration_policies (bot_id,revision,hash,enabled) VALUES ($1,$2,$3,$4)
        ON CONFLICT (bot_id) DO UPDATE SET revision=EXCLUDED.revision,hash=EXCLUDED.hash,enabled=EXCLUDED.enabled`, botID, next.Revision, next.Hash[:], next.Enabled)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func readConfigurationPolicy(ctx context.Context, tx pgx.Tx, botID int64, lock bool) (configdelivery.Policy, bool, error) {
	var p configdelivery.Policy
	var hash []byte
	sql := `SELECT revision,hash,enabled FROM configuration_policies WHERE bot_id=$1`
	if lock {
		sql += ` FOR UPDATE`
	}
	err := tx.QueryRow(ctx, sql, botID).Scan(&p.Revision, &hash, &p.Enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, false, nil
	}
	if err != nil {
		return p, false, err
	}
	if len(hash) != 32 {
		return p, false, configdelivery.ErrDenied
	}
	copy(p.Hash[:], hash)
	return p, true, nil
}

// Finalizers use bot -> outbox -> dispatch -> chat, matching authorization.
func lockConfigurationOutbox(ctx context.Context, tx pgx.Tx, lease queue.Lease) (int64, int64, error) {
	if lease.ID <= 0 || lease.Attempt <= 0 {
		return 0, 0, queue.ErrLeaseLost
	}
	var bot, chat int64
	err := tx.QueryRow(ctx, `SELECT bot_id FROM outbox WHERE id=$1 AND protected_configuration`, lease.ID).Scan(&bot)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, queue.ErrLeaseLost
	}
	if err != nil {
		return 0, 0, err
	}
	if _, err := tx.Exec(ctx, `SELECT bot_id FROM telegram_bots WHERE bot_id=$1 FOR NO KEY UPDATE`, bot); err != nil {
		return 0, 0, err
	}
	err = tx.QueryRow(ctx, `SELECT chat_id FROM outbox WHERE id=$1 AND attempts=$2 AND status='running'
        AND locked_until>clock_timestamp() AND protected_configuration FOR UPDATE`, lease.ID, lease.Attempt).Scan(&chat)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, queue.ErrLeaseLost
	}
	return bot, chat, err
}
func (s *Store) completeConfigurationOutbox(ctx context.Context, lease queue.Lease, messageID int64) (err error) {
	defer func() { err = configurationError(err) }()
	if messageID <= 0 {
		return queue.ErrLeaseLost
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	_, _, err = lockConfigurationOutbox(ctx, tx, lease)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE configuration_dispatches SET outcome='sent',telegram_message_id=$3,error_code=0
        WHERE outbox_id=$1 AND attempt=$2 AND outcome='reserved'`, lease.ID, lease.Attempt, messageID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrLeaseLost
	}
	if _, err := tx.Exec(ctx, `UPDATE configuration_chat_guards SET outbox_id=NULL,attempt=NULL
        WHERE outbox_id=$1 AND attempt=$2 AND EXISTS(SELECT 1 FROM configuration_dispatches d WHERE d.outbox_id=$1 AND d.kind='secret')`, lease.ID, lease.Attempt); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SET LOCAL synchronous_commit = on`); err != nil {
		return err
	}
	tag, err = tx.Exec(ctx, `UPDATE outbox SET status='sent',telegram_message_id=$3,finished_at=clock_timestamp(),locked_until=NULL,last_error_code=0
        WHERE id=$1 AND attempts=$2 AND status='running' AND locked_until>clock_timestamp() AND protected_configuration`, lease.ID, lease.Attempt, messageID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrLeaseLost
	}
	return tx.Commit(ctx)
}

func (s *Store) FailConfigurationOutbox(ctx context.Context, lease queue.Lease, errorCode int, cooldown time.Duration, rejected bool) (err error) {
	defer func() { err = configurationError(err) }()
	if lease.ID <= 0 || lease.Attempt <= 0 {
		return queue.ErrLeaseLost
	}
	if errorCode < 0 || int64(errorCode) > 1<<31-1 || cooldown < 0 {
		return configdelivery.ErrDenied
	}
	// A boolean without a Telegram rejection code is not validated evidence.
	rejected = rejected && errorCode >= 400 && errorCode < 500 && errorCode != 408
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	bot, _, err := lockConfigurationOutbox(ctx, tx, lease)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE configuration_dispatches SET outcome=CASE WHEN attempt=$2 AND $4 THEN 'rejected' ELSE 'unknown' END,error_code=$3
        WHERE outbox_id=$1 AND outcome IN ('reserved','unknown')`, lease.ID, lease.Attempt, errorCode, rejected)
	if err != nil {
		return err
	}
	if rejected && tag.RowsAffected() == 1 {
		if _, err := tx.Exec(ctx, `UPDATE configuration_chat_guards SET outbox_id=NULL,attempt=NULL WHERE outbox_id=$1 AND attempt=$2
            AND EXISTS(SELECT 1 FROM configuration_dispatches d WHERE d.outbox_id=$1 AND d.attempt=$2 AND d.kind='secret' AND d.outcome='rejected')`, lease.ID, lease.Attempt); err != nil {
			return err
		}
	}
	if errorCode == 429 {
		if cooldown < time.Minute {
			cooldown = time.Minute
		}
		// Do not cap provider Retry-After: the caller supplies its minimum.
		if _, err := tx.Exec(ctx, `UPDATE telegram_bots SET outbox_not_before=GREATEST(outbox_not_before,clock_timestamp()+$2*interval '1 millisecond') WHERE bot_id=$1`, bot, cooldown.Milliseconds()); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `SET LOCAL synchronous_commit = on`); err != nil {
		return err
	}
	tag, err = tx.Exec(ctx, `UPDATE outbox SET status='failed',locked_until=NULL,finished_at=clock_timestamp(),last_error_code=$3
        WHERE id=$1 AND attempts=$2 AND status='running' AND locked_until>clock_timestamp() AND protected_configuration`, lease.ID, lease.Attempt, errorCode)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrLeaseLost
	}
	return tx.Commit(ctx)
}

func configurationPolicyMatches(p configdelivery.Policy, r configdelivery.Request) bool {
	return p.Enabled && p.Revision == r.PolicyRevision && p.Hash == r.PolicyHash
}

// This is a completed-journal reader, deliberately NOT readActivationSnapshot:
// completed operations have no running lease. Caller holds the client lock or
// uses repeatable read, so all of these projections are one coherent account.
func readOwnedConfigurationEntitlement(ctx context.Context, tx pgx.Tx, botID, actor int64) (configdelivery.Entitlement, error) {
	var e configdelivery.Entitlement
	a, s := &e.Activation.Account, &e.Activation
	var policy, hash, bindingHash []byte
	var invoice, currentInvoice, currentPaymentDate, currentNextDate string
	var checkBot, bindingBot, bindingOperation int64
	var checkReview int
	var provenance bool
	err := tx.QueryRow(ctx, `SELECT c.id,c.telegram_id,c.chat_id,f.bot_id,COALESCE(c.invoice_ninja_id,''),
        COALESCE(s.current_invoice_id,''),COALESCE(s.first_invoice_id,''),COALESCE(s.recurring_id,''),COALESCE(c.sui_id,''),
        s.state,s.desired_access,COALESCE(s.payment_date::text,''),COALESCE(s.next_invoice_date::text,''),
        o.id,c.configuration_revision,f.token,f.policy_hash,f.proof_hash,f.invoice_id,f.review_code,
        p.bot_id,p.review_code,(`+paymentProvenance+`),b.sui_id,b.name,b.credentials_hash,b.bot_id,b.operation_id
        FROM clients c JOIN subscriptions s ON s.client_id=c.id
        JOIN operations o ON o.client_id=c.id AND o.kind='activate_first_subscription' AND o.status='completed'
        JOIN activation_flows f ON f.operation_id=o.id AND f.bot_id=$1
        JOIN payment_checks p ON p.client_id=c.id
        JOIN activation_sui_bindings b ON b.client_id=c.id
        WHERE c.telegram_id=$2 AND NOT EXISTS(SELECT 1 FROM registration_flows rf
            JOIN operations ro ON ro.id=rf.operation_id WHERE ro.client_id=c.id AND rf.review_code<>0)`, botID, actor).Scan(
		&a.ClientID, &a.TelegramID, &a.ChatID, &a.BotID, &a.CustomerID, &currentInvoice, &a.FirstInvoiceID, &a.RecurringID, &a.SUIID,
		&a.State, &a.DesiredAccess, &currentPaymentDate, &currentNextDate, &e.ActivationID, &e.Revision, &s.Token, &policy, &hash, &invoice, &s.ReviewCode,
		&checkBot, &checkReview, &provenance, &e.Binding.ID, &e.Binding.Name, &bindingHash, &bindingBot, &bindingOperation)
	if errors.Is(err, pgx.ErrNoRows) {
		return e, configdelivery.ErrDenied
	}
	if err != nil {
		return e, err
	}
	if !provenance || checkBot != botID || bindingBot != botID || bindingOperation != e.ActivationID || len(policy) != 32 || len(hash) != 32 || len(bindingHash) != 32 ||
		invoice != a.FirstInvoiceID || currentInvoice == "" || a.TelegramID != actor || a.ChatID != actor {
		return e, configdelivery.ErrDenied
	}
	copy(s.PolicyHash[:], policy)
	copy(e.Binding.CredentialsHash[:], bindingHash)
	a.InvoiceID = a.FirstInvoiceID
	confirmation, err := readActivationConfirmation(ctx, tx, invoice)
	if errors.Is(err, queue.ErrConflict) {
		return e, configdelivery.ErrDenied
	}
	if err != nil {
		return e, err
	}
	a.PaymentDate, a.NextInvoiceDate = confirmation.paid, confirmation.next
	if confirmation.clientID != a.ClientID || confirmation.botID != botID || confirmation.customerID != a.CustomerID {
		return e, configdelivery.ErrDenied
	}
	s.Proof, err = confirmation.proof()
	if err != nil {
		return e, configdelivery.ErrDenied
	}
	if !bytes.Equal(hash, s.Proof.Hash[:]) {
		return e, configdelivery.ErrDenied
	}
	s.PaymentReview = confirmation.review || confirmation.reviewCode != 0 || checkReview != 0
	rows, err := tx.Query(ctx, `SELECT step,request,COALESCE(external_id,''),dispatch_attempt,
        COALESCE(recurring_amount::text,''),COALESCE(recurring_datetime,''),confirmed_at IS NOT NULL
        FROM activation_steps WHERE operation_id=$1`, e.ActivationID)
	if err != nil {
		return e, err
	}
	s.Steps = make(map[activation.Step]activation.Intent)
	corrupt := false
	for rows.Next() {
		var step activation.Step
		var intent activation.Intent
		var confirmed bool
		if err := rows.Scan(&step, &intent.Payload, &intent.ExternalID, &intent.DispatchAttempt, &intent.RecurringAmount, &intent.RecurringDatetime, &confirmed); err != nil {
			rows.Close()
			return e, err
		}
		if !step.Valid() || !confirmed || intent.DispatchAttempt < 1 || intent.ExternalID == "" {
			corrupt = true
		}
		if step == activation.RecurringCreate || step == activation.RecurringStart {
			amount, instant, err := activation.NormalizeRecurringFacts(json.Number(intent.RecurringAmount), intent.RecurringDatetime)
			if err != nil {
				corrupt = true
			} else {
				intent.RecurringAmount, intent.RecurringDatetime = amount, instant
			}
		} else if intent.RecurringAmount != "" || intent.RecurringDatetime != "" {
			corrupt = true
		}
		s.Steps[step] = intent
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return e, err
	}
	e.Period = configdelivery.FirstPeriod
	if corrupt || len(s.Steps) != 4 || e.Validate() != nil {
		return e, configdelivery.ErrDenied
	}
	if currentInvoice != a.FirstInvoiceID {
		e.Period = configdelivery.RenewalPeriod
		e.Current = payments.Snapshot{ClientID: a.ClientID, TelegramID: actor, ChatID: actor, BotID: botID, CustomerID: a.CustomerID,
			InvoiceID: currentInvoice, FirstInvoiceID: a.FirstInvoiceID, RecurringID: a.RecurringID, SUIID: a.SUIID,
			State: a.State, DesiredAccess: a.DesiredAccess, PaymentDate: currentPaymentDate, NextInvoiceDate: currentNextDate}
		current, currentErr := readActivationConfirmation(ctx, tx, currentInvoice)
		if currentErr != nil {
			return e, configdelivery.ErrDenied
		}
		proof, proofErr := current.proof()
		if proofErr != nil || current.clientID != a.ClientID || current.botID != botID || current.customerID != a.CustomerID || current.invoiceID != currentInvoice || current.review || current.reviewCode != 0 || checkReview != 0 ||
			current.paid != currentPaymentDate || current.next != currentNextDate {
			return e, configdelivery.ErrDenied
		}
		e.CurrentProof = proof
		e.Renewal, err = readConfigurationRenewalAudit(ctx, tx, a.ClientID, botID, currentInvoice, a.RecurringID, a.SUIID, proof)
		if err != nil {
			return e, configdelivery.ErrDenied
		}
	}
	return e, nil
}

func validRenewalDate(value string) bool {
	date, err := time.Parse(time.DateOnly, value)
	return err == nil && date.Format(time.DateOnly) == value
}

func readConfigurationRenewalAudit(ctx context.Context, tx pgx.Tx, clientID, botID int64, invoiceID, recurringID, suiID string, proof payments.Proof) (configdelivery.RenewalAudit, error) {
	var audit configdelivery.RenewalAudit
	var target, amount string
	var next time.Time
	var baseline []byte
	var flags [4]bool
	err := tx.QueryRow(ctx, `SELECT a.operation_id,a.target_date::text,a.requires_pause,a.requires_schedule,a.requires_enable,a.requires_resume,
        a.baseline::text,a.recurring_amount::text,a.recurring_next_send_datetime
        FROM invoice_renewal_actions a JOIN operations o ON o.id=a.operation_id
        WHERE a.client_id=$1 AND a.bot_id=$2 AND a.invoice_id=$3 AND a.outcome='completed' AND a.review_code=0
          AND o.kind='invoice_paid_renewal' AND o.status='completed'
          AND EXISTS (SELECT 1 FROM invoice_lifecycle_invoices li WHERE li.client_id=a.client_id AND li.bot_id=a.bot_id
            AND li.invoice_id=a.invoice_id AND li.customer_id=$4 AND li.recurring_id=$5 AND li.invoice_number=$6 AND li.status='paid'
            AND li.notice_state='settled' AND li.amount=$7::numeric)
          AND NOT EXISTS (SELECT 1 FROM invoice_lifecycle_checks lc WHERE lc.client_id=a.client_id AND lc.bot_id=a.bot_id AND lc.review_code<>0)`, clientID, botID, invoiceID, proof.CustomerID, recurringID, proof.InvoiceNumber, proof.Amount).
		Scan(&audit.OperationID, &target, &flags[0], &flags[1], &flags[2], &flags[3], &baseline, &amount, &next)
	if err != nil || audit.OperationID <= 0 || target != proof.NextInvoiceDate || len(baseline) == 0 {
		return audit, configdelivery.ErrDenied
	}
	audit.TargetDate, audit.RequiresPause, audit.RequiresSchedule, audit.RequiresEnable, audit.RequiresResume = target, flags[0], flags[1], flags[2], flags[3]
	parsed := next.UTC()
	if parsed.IsZero() {
		return audit, configdelivery.ErrDenied
	}
	audit.Amount, _, err = activation.NormalizeRecurringFacts(json.Number(amount), parsed.Format(time.RFC3339Nano))
	if err != nil {
		return audit, configdelivery.ErrDenied
	}
	audit.NextSendUTC = parsed
	audit.PlanHash = sha256.Sum256(baseline)
	var baselineFields map[string]json.RawMessage
	if json.Unmarshal(baseline, &baselineFields) != nil || len(baselineFields) != 7 {
		return audit, configdelivery.ErrDenied
	}
	for _, name := range []string{"recurring_id", "status_id", "next_send_date", "amount", "next_send_datetime", "sui_id", "sui_enabled"} {
		if baselineFields[name] == nil {
			return audit, configdelivery.ErrDenied
		}
	}
	var frozen struct {
		RecurringID      string      `json:"recurring_id"`
		StatusID         string      `json:"status_id"`
		NextSendDate     string      `json:"next_send_date"`
		Amount           json.Number `json:"amount"`
		NextSendDatetime string      `json:"next_send_datetime"`
		SUIID            int64       `json:"sui_id"`
		SUIEnabled       bool        `json:"sui_enabled"`
	}
	if json.Unmarshal(baseline, &frozen) != nil || frozen.RecurringID != recurringID || strconv.FormatInt(frozen.SUIID, 10) != suiID ||
		(frozen.StatusID != "2" && frozen.StatusID != "3") || !validRenewalDate(frozen.NextSendDate) || frozen.SUIEnabled == flags[2] ||
		flags[1] != (frozen.NextSendDate != target) || flags[0] != (flags[1] && frozen.StatusID == "2") || flags[3] != (flags[1] || frozen.StatusID == "3" || flags[2]) {
		return audit, configdelivery.ErrDenied
	}
	baselineAmount, baselineUTC, err := activation.NormalizeRecurringFacts(frozen.Amount, frozen.NextSendDatetime)
	if err != nil {
		return audit, configdelivery.ErrDenied
	}
	finalAmount, finalUTC, err := activation.NormalizeRecurringFacts(json.Number(audit.Amount), audit.NextSendUTC.Format(time.RFC3339Nano))
	if err != nil || finalAmount != audit.Amount {
		return audit, configdelivery.ErrDenied
	}
	planned := []struct {
		name     string
		required bool
	}{{"pause_recurring", flags[0]}, {"schedule_recurring", flags[1]}, {"enable_sui", flags[2]}, {"resume_recurring", flags[3]}}
	rows, err := tx.Query(ctx, `SELECT step,request::text,COALESCE(external_id,''),COALESCE(receipt::text,''),confirmed_at IS NOT NULL FROM invoice_renewal_steps WHERE operation_id=$1 ORDER BY step`, audit.OperationID)
	if err != nil {
		return audit, err
	}
	defer rows.Close()
	seen := [4]bool{}
	for rows.Next() {
		var step, request, externalID, receipt string
		var confirmed bool
		if err := rows.Scan(&step, &request, &externalID, &receipt, &confirmed); err != nil {
			return audit, err
		}
		index := -1
		for i := range planned {
			if planned[i].name == step {
				index = i
				break
			}
		}
		if index < 0 || seen[index] || !planned[index].required || !confirmed || externalID == "" {
			return audit, configdelivery.ErrDenied
		}
		if (index == 0 || index == 1 || index == 3) && externalID != recurringID || index == 2 && externalID != suiID {
			return audit, configdelivery.ErrDenied
		}
		switch index {
		case 0:
			var intent invoiceninja.PauseDraft
			if json.Unmarshal([]byte(request), &intent) != nil || receipt != "" {
				return audit, configdelivery.ErrDenied
			}
		case 1:
			var intent invoiceninja.ScheduleDraft
			var requestFields map[string]json.RawMessage
			if json.Unmarshal([]byte(request), &intent) != nil || json.Unmarshal([]byte(request), &requestFields) != nil || len(requestFields) != 3 || requestFields["before"] == nil || requestFields["template"] == nil || requestFields["desired_date"] == nil {
				return audit, configdelivery.ErrDenied
			}
			var desired string
			if json.Unmarshal(requestFields["desired_date"], &desired) != nil || desired != target {
				return audit, configdelivery.ErrDenied
			}
			var receiptFacts invoiceninja.RecurringReceipt
			if json.Unmarshal([]byte(receipt), &receiptFacts) != nil {
				return audit, configdelivery.ErrDenied
			}
			receiptAmount, receiptUTC, receiptErr := activation.NormalizeRecurringFacts(receiptFacts.Amount, receiptFacts.NextSendDatetime)
			if receiptErr != nil || receiptAmount != finalAmount || receiptUTC != finalUTC {
				return audit, configdelivery.ErrDenied
			}
		case 2:
			var intent sui.EnableDraft
			if json.Unmarshal([]byte(request), &intent) != nil || receipt != "" {
				return audit, configdelivery.ErrDenied
			}
		case 3:
			var intent invoiceninja.ResumeDraft
			if json.Unmarshal([]byte(request), &intent) != nil || receipt != "" {
				return audit, configdelivery.ErrDenied
			}
		}
		seen[index] = true
		audit.StepIDs[index] = externalID
		audit.StepHashes[index] = sha256.Sum256([]byte(request + "\x00" + externalID + "\x00" + receipt))
	}
	if err := rows.Err(); err != nil {
		return audit, err
	}
	for i, item := range planned {
		if item.required != seen[i] {
			return audit, configdelivery.ErrDenied
		}
	}
	if !flags[1] && (baselineAmount != finalAmount || baselineUTC != finalUTC) {
		return audit, configdelivery.ErrDenied
	}
	if activation.ValidID(invoiceID) == false || audit.NextSendUTC.IsZero() || audit.Amount == "" {
		return audit, configdelivery.ErrDenied
	}
	return audit, nil
}

// Imported identity presence is authoritative for mode selection: an imported
// account never falls back to the owned activation journal reader.
func readConfigurationEntitlement(ctx context.Context, tx pgx.Tx, botID, actor int64) (configdelivery.Entitlement, error) {
	var imported bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM verified_import_origins io JOIN clients c ON c.id=io.client_id WHERE io.bot_id=$1 AND c.telegram_id=$2)`, botID, actor).Scan(&imported); err != nil {
		return configdelivery.Entitlement{}, err
	}
	if imported {
		return readImportedConfigurationEntitlement(ctx, tx, botID, actor)
	}
	return readOwnedConfigurationEntitlement(ctx, tx, botID, actor)
}

// Called only after Telegram's private actor and the comparable menu snapshot
// have been checked. An ordinary denial is safe even if provenance is absent.
func enqueueConfigurationIntent(ctx context.Context, tx pgx.Tx, prepared clientmenu.Prepared) (err error) {
	defer func() { err = configurationError(err) }()
	r := prepared.Request
	if !prepared.ProtectedConfiguration || !r.Valid() || r.Action != clientmenu.Configuration || !prepared.Snapshot.Known || prepared.Text != queue.ConfigurationIntentBody {
		return configdelivery.ErrDenied
	}
	denied := func() error {
		_, err := enqueueOutbox(ctx, tx, queue.Draft{BotID: r.BotID, ChatID: r.ChatID, Key: r.Key(), Text: configurationDeniedText, ClientMenu: true})
		return err
	}
	p, _, err := readConfigurationPolicy(ctx, tx, r.BotID, false)
	if err != nil {
		return err
	}
	if !p.Enabled || p.Revision != prepared.ConfigurationPolicyRevision || p.Hash != prepared.ConfigurationPolicyHash {
		return denied()
	}
	e, err := readConfigurationEntitlement(ctx, tx, r.BotID, r.TelegramID)
	if errors.Is(err, configdelivery.ErrDenied) {
		return denied()
	}
	if err != nil {
		return err
	}
	clientID := e.Activation.Account.ClientID
	if e.Period == configdelivery.ImportedBaselinePeriod || (e.Period == configdelivery.RenewalPeriod && e.Imported != nil) {
		clientID = e.Current.ClientID
	}
	if clientID != prepared.Snapshot.ClientID || e.Revision != prepared.Snapshot.ConfigurationRevision {
		return denied()
	}
	deadline, err := e.Deadline()
	if err != nil {
		return denied()
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	if !now.Before(deadline) {
		return denied()
	}
	fingerprint, err := e.Fingerprint()
	if err != nil {
		return denied()
	}
	period, invoiceID, proofHash, renewalID := e.Period, e.Activation.Account.InvoiceID, e.Activation.Proof.Hash, int64(0)
	originKind := configdelivery.OwnedOrigin
	clientID, activationID := e.Activation.Account.ClientID, e.ActivationID
	var importedOriginID, admissionPermissionRevision, runtimeRevision int64
	if period == configdelivery.RenewalPeriod {
		invoiceID, proofHash, renewalID = e.Current.InvoiceID, e.CurrentProof.Hash, e.Renewal.OperationID
	}
	if period == configdelivery.ImportedBaselinePeriod || (period == configdelivery.RenewalPeriod && e.Imported != nil) {
		if e.Imported == nil || e.Imported.Origin.ClientID != e.Current.ClientID ||
			(period == configdelivery.ImportedBaselinePeriod && e.Imported.InvoiceID != e.Current.InvoiceID) {
			return denied()
		}
		originKind, clientID, activationID = configdelivery.ImportedOrigin, e.Current.ClientID, 0
		importedOriginID = e.Imported.Origin.ClientID
		admissionPermissionRevision = e.Imported.AdmissionPermissionRevision
		runtimeRevision = e.Imported.Origin.RuntimeRevision
		if period == configdelivery.ImportedBaselinePeriod {
			invoiceID, proofHash = e.Imported.InvoiceID, e.Imported.ProofHash
		} else {
			invoiceID, proofHash = e.Current.InvoiceID, e.CurrentProof.Hash
		}
	}
	id, err := insertOutbox(ctx, tx, queue.Draft{BotID: r.BotID, ChatID: r.ChatID, Key: r.Key(), Text: queue.ConfigurationIntentBody, ProtectedConfiguration: true})
	if err != nil {
		return err
	}
	// One database clock value supplies both timestamps; the request is immutable.
	_, err = tx.Exec(ctx, `INSERT INTO configuration_requests (outbox_id,bot_id,client_id,actor_id,chat_id,activation_operation_id,
        invoice_id,proof_hash,client_revision,policy_revision,policy_hash,entitlement_hash,created_at,expires_at,period_kind,renewal_operation_id,
        origin_kind,imported_origin_client_id,imported_admission_permission_revision,imported_runtime_revision)
        SELECT $1,$2,$3,$4,$4,NULLIF($5,0),$6,$7,$8,$9,$10,$11,t,t+interval '5 minutes',$12,NULLIF($13,0),$14,NULLIF($15,0),NULLIF($16,0),NULLIF($17,0) FROM (SELECT clock_timestamp() AS t) stamp`,
		id, r.BotID, clientID, r.TelegramID, activationID, invoiceID, proofHash[:], e.Revision, p.Revision, p.Hash[:], fingerprint[:], string(period), renewalID,
		string(originKind), importedOriginID, admissionPermissionRevision, runtimeRevision)
	return err
}

func validConfigurationMessage(m queue.Message) bool {
	return m.ID > 0 && m.Attempt > 0 && m.BotID > 0 && m.ChatID > 0 && m.ProtectedConfiguration && !m.ClientMenu && m.Text == queue.ConfigurationIntentBody
}
func readConfigurationRequest(ctx context.Context, tx pgx.Tx, m queue.Message) (configdelivery.Request, error) {
	var r configdelivery.Request
	var policy, proof, hash []byte
	var activationID, importedOriginID, importedAdmissionRevision, importedRuntimeRevision *int64
	var originKind string
	err := tx.QueryRow(ctx, `SELECT r.outbox_id,r.bot_id,r.client_id,r.actor_id,r.chat_id,r.activation_operation_id,
        r.invoice_id,r.proof_hash,r.client_revision,r.policy_revision,r.policy_hash,r.entitlement_hash,r.created_at,r.expires_at,r.period_kind,COALESCE(r.renewal_operation_id,0),
        r.origin_kind,r.imported_origin_client_id,r.imported_admission_permission_revision,r.imported_runtime_revision
        FROM configuration_requests r JOIN outbox o ON o.id=r.outbox_id
        WHERE o.id=$1 AND o.attempts=$2 AND o.status='running' AND o.locked_until>clock_timestamp()
        AND o.bot_id=$3 AND o.chat_id=$4 AND o.protected_configuration AND NOT o.client_menu AND o.body=$5
        AND r.bot_id=o.bot_id AND r.chat_id=o.chat_id AND r.actor_id=r.chat_id`, m.ID, m.Attempt, m.BotID, m.ChatID, queue.ConfigurationIntentBody).Scan(
		&r.OutboxID, &r.BotID, &r.ClientID, &r.ActorID, &r.ChatID, &activationID, &r.InvoiceID, &proof, &r.ClientRevision, &r.PolicyRevision, &policy, &hash, &r.CreatedAt, &r.ExpiresAt, &r.Period, &r.RenewalID,
		&originKind, &importedOriginID, &importedAdmissionRevision, &importedRuntimeRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, queue.ErrLeaseLost
	}
	if err != nil {
		return r, err
	}
	if len(policy) != 32 || len(proof) != 32 || len(hash) != 32 {
		return r, queue.ErrLeaseLost
	}
	copy(r.PolicyHash[:], policy)
	copy(r.ProofHash[:], proof)
	copy(r.EntitlementHash[:], hash)
	r.Origin = configdelivery.OriginKind(originKind)
	if activationID != nil {
		r.ActivationID = *activationID
	}
	if importedOriginID != nil {
		r.ImportedOriginClientID = *importedOriginID
	}
	if importedAdmissionRevision != nil {
		r.ImportedAdmissionPermissionRevision = *importedAdmissionRevision
	}
	if importedRuntimeRevision != nil {
		r.ImportedRuntimeRevision = *importedRuntimeRevision
	}
	if r.Validate() != nil || r.ExpiresAt.Sub(r.CreatedAt) != configdelivery.RequestTTL {
		return r, queue.ErrLeaseLost
	}
	return r, nil
}
func sameConfigurationRequest(a, b configdelivery.Request) bool {
	at, ae, bt, be := a.CreatedAt, a.ExpiresAt, b.CreatedAt, b.ExpiresAt
	a.CreatedAt, a.ExpiresAt, b.CreatedAt, b.ExpiresAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}
	return a == b && at.Equal(bt) && ae.Equal(be)
}
func readConfigurationView(ctx context.Context, tx pgx.Tx, m queue.Message) (configdelivery.View, error) {
	var v configdelivery.View
	r, err := readConfigurationRequest(ctx, tx, m)
	if err != nil {
		return v, err
	}
	v.Request = r
	v.DenialCode = configdelivery.DeniedEntitlement
	// A changed actor is never an account selector for a different client.
	var owner bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM clients WHERE id=$1 AND telegram_id=$2 AND chat_id=$2)`, r.ClientID, r.ActorID).Scan(&owner); err != nil {
		return v, err
	}
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM configuration_dispatches WHERE outbox_id=$1),
        EXISTS(SELECT 1 FROM configuration_chat_guards WHERE bot_id=$2 AND chat_id=$3 AND outbox_id IS NOT NULL)`, r.OutboxID, r.BotID, r.ChatID).Scan(&v.AlreadyDispatched, &v.ChatBlocked); err != nil {
		return v, err
	}
	if !owner {
		return v, nil
	}
	p, _, err := readConfigurationPolicy(ctx, tx, r.BotID, false)
	if err != nil {
		return v, err
	}
	if !configurationPolicyMatches(p, r) {
		v.DenialCode = configdelivery.DeniedPolicy
		return v, nil
	}
	e, err := readConfigurationEntitlement(ctx, tx, r.BotID, r.ActorID)
	if errors.Is(err, configdelivery.ErrDenied) {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	v.Entitlement = e
	v.Eligible = true
	if !v.Matches() {
		v.Eligible = false
		return v, nil
	}
	deadline, err := e.Deadline()
	if err != nil {
		v.Eligible = false
		return v, nil
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return v, err
	}
	if !now.Before(r.ExpiresAt) || !now.Before(deadline) {
		v.Eligible = false
		v.DenialCode = configdelivery.DeniedExpired
		return v, nil
	}
	v.DenialCode = 0
	return v, nil
}

func (s *Store) LoadConfiguration(ctx context.Context, m queue.Message) (v configdelivery.View, err error) {
	defer func() { err = configurationError(err) }()
	if !validConfigurationMessage(m) {
		return v, queue.ErrLeaseLost
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return v, err
	}
	defer rollback(tx)
	v, err = readConfigurationView(ctx, tx, m)
	if err != nil {
		return configdelivery.View{}, err
	}
	if err := checkConfigurationLease(ctx, tx, m, false); err != nil {
		return configdelivery.View{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return configdelivery.View{}, err
	}
	return v, nil
}

// All production mutators hold the client first. Lock the authorization rows in
// that same order; no remote calls are made while any of these locks are held.
func lockConfigurationAccount(ctx context.Context, tx pgx.Tx, r configdelivery.Request) error {
	if _, err := tx.Exec(ctx, `SELECT id FROM clients WHERE id=$1 FOR UPDATE`, r.ClientID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT client_id FROM subscriptions WHERE client_id=$1 FOR NO KEY UPDATE`, r.ClientID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT client_id FROM payment_checks WHERE client_id=$1 FOR UPDATE`, r.ClientID); err != nil {
		return err
	}
	// Lock both immutable first-period and requested current proofs in stable
	// invoice order, following subscription/payment_checks in the lock order.
	proofRows, err := tx.Query(ctx, `SELECT invoice_id FROM payment_confirmations WHERE invoice_id=$1 OR invoice_id=(SELECT invoice_id FROM activation_flows WHERE operation_id=$2) ORDER BY invoice_id FOR UPDATE`, r.InvoiceID, r.ActivationID)
	if err != nil {
		return err
	}
	for proofRows.Next() {
		var invoice string
		if err := proofRows.Scan(&invoice); err != nil {
			proofRows.Close()
			return err
		}
	}
	proofRows.Close()
	if err := proofRows.Err(); err != nil {
		return err
	}
	if r.Origin == configdelivery.ImportedOrigin {
		var lockedOrigin int64
		if err := tx.QueryRow(ctx, `SELECT client_id FROM verified_import_origins WHERE client_id=$1 AND bot_id=$2 FOR KEY SHARE`, r.ImportedOriginClientID, r.BotID).Scan(&lockedOrigin); err != nil {
			return err
		}
		if r.Period == configdelivery.RenewalPeriod {
			var operationID int64
			if err := tx.QueryRow(ctx, `SELECT id FROM operations WHERE id=$1 AND client_id=$2 AND kind='invoice_paid_renewal' AND status='completed' FOR UPDATE`, r.RenewalID, r.ClientID).Scan(&operationID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `SELECT operation_id FROM invoice_renewal_actions WHERE operation_id=$1 AND client_id=$2 AND bot_id=$3 AND invoice_id=$4 FOR UPDATE`, r.RenewalID, r.ClientID, r.BotID, r.InvoiceID); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, `SELECT step FROM invoice_renewal_steps WHERE operation_id=$1 ORDER BY step FOR UPDATE`, r.RenewalID)
			if err != nil {
				return err
			}
			for rows.Next() {
				var step string
				if err := rows.Scan(&step); err != nil {
					rows.Close()
					return err
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
		}
		rows, err := tx.Query(ctx, `SELECT rp.bot_id,ap.bot_id FROM imported_runtime_permissions rp
            JOIN import_admission_policy ap ON ap.bot_id=rp.bot_id WHERE rp.bot_id=$1 FOR SHARE OF rp,ap`, r.BotID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var runtimeBot, admissionBot int64
			if err := rows.Scan(&runtimeBot, &admissionBot); err != nil {
				rows.Close()
				return err
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	// Pipeline the remaining ordered locks over remote PostgreSQL links. The
	// backend still executes each statement serially, with the client held.
	batch := &pgx.Batch{}
	if r.Origin != configdelivery.ImportedOrigin {
		for _, q := range []struct {
			sql string
			arg any
		}{
			{`SELECT operation_id FROM activation_flows WHERE operation_id=$1 FOR UPDATE`, r.ActivationID},
			{`SELECT client_id FROM activation_sui_bindings WHERE client_id=$1 FOR UPDATE`, r.ClientID},
			{`SELECT step FROM activation_steps WHERE operation_id=$1 ORDER BY step FOR UPDATE`, r.ActivationID},
			{`SELECT id FROM operations WHERE client_id=$1 AND kind IN ('register_client','activate_first_subscription') ORDER BY id FOR UPDATE`, r.ClientID},
			{`SELECT f.operation_id FROM registration_flows f JOIN operations o ON o.id=f.operation_id WHERE o.client_id=$1 FOR UPDATE OF f`, r.ClientID},
			{`SELECT s.step FROM operation_steps s JOIN operations o ON o.id=s.operation_id WHERE o.client_id=$1 ORDER BY s.step FOR UPDATE OF s`, r.ClientID},
			{`SELECT operation_id FROM invoice_renewal_actions WHERE operation_id=$1 FOR UPDATE`, r.RenewalID},
			{`SELECT step FROM invoice_renewal_steps WHERE operation_id=$1 ORDER BY step FOR UPDATE`, r.RenewalID},
		} {
			batch.Queue(q.sql, q.arg)
		}
	}
	results := tx.SendBatch(ctx, batch)
	for range batch.Len() {
		if _, err := results.Exec(); err != nil {
			_ = results.Close()
			return err
		}
	}
	return results.Close()
}
func checkConfigurationLease(ctx context.Context, tx pgx.Tx, m queue.Message, cooldown bool, deadlines ...time.Time) error {
	var valid bool
	var deadline any
	if len(deadlines) != 0 {
		deadline = deadlines[0]
	}
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM outbox o JOIN telegram_bots b ON b.bot_id=o.bot_id
        WHERE o.id=$1 AND o.attempts=$2 AND o.bot_id=$3 AND o.chat_id=$4 AND o.status='running'
        AND o.locked_until>clock_timestamp() AND o.protected_configuration AND NOT o.client_menu AND o.body=$5
        AND (NOT $6 OR b.outbox_not_before<=clock_timestamp())
        AND ($7::timestamptz IS NULL OR clock_timestamp() < $7::timestamptz)
        AND NOT EXISTS(SELECT 1 FROM outbox head WHERE head.bot_id=o.bot_id AND head.chat_id=o.chat_id
            AND head.id<o.id AND head.status IN ('pending','running')))`, m.ID, m.Attempt, m.BotID, m.ChatID, queue.ConfigurationIntentBody, cooldown, deadline).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return queue.ErrLeaseLost
	}
	return nil
}

func (s *Store) AuthorizeConfiguration(ctx context.Context, m queue.Message, expected configdelivery.View, decision configdelivery.Decision) (permit configdelivery.Permit, err error) {
	defer func() {
		err = configurationError(err)
		if err != nil {
			permit = configdelivery.Permit{}
		}
	}()
	if !validConfigurationMessage(m) {
		return permit, queue.ErrLeaseLost
	}
	if decision.Validate() != nil || expected.Request.Validate() != nil {
		return permit, configdelivery.ErrDenied
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return permit, err
	}
	defer rollback(tx)
	// Never acquire the bot after a client or outbox lock.
	if _, err := tx.Exec(ctx, `SELECT bot_id FROM telegram_bots WHERE bot_id=$1 FOR NO KEY UPDATE`, m.BotID); err != nil {
		return permit, err
	}
	if _, _, err := readConfigurationPolicy(ctx, tx, m.BotID, true); err != nil {
		return permit, err
	}
	request, err := readConfigurationRequest(ctx, tx, m)
	if err != nil {
		return permit, err
	}
	if !sameConfigurationRequest(request, expected.Request) {
		return permit, configdelivery.ErrDenied
	}
	if err := lockConfigurationAccount(ctx, tx, request); err != nil {
		return permit, err
	}
	if _, err := tx.Exec(ctx, `SELECT id FROM outbox WHERE id=$1 FOR UPDATE`, m.ID); err != nil {
		return permit, err
	}
	fresh, err := readConfigurationView(ctx, tx, m)
	if err != nil {
		return permit, err
	}
	if fresh.AlreadyDispatched {
		return permit, configdelivery.ErrAlreadyDispatched
	}
	permit = configdelivery.Permit{Kind: configdelivery.Denial, Reason: decision.Reason}
	if permit.Reason == 0 {
		permit.Reason = fresh.DenialCode
	}
	if permit.Reason == 0 {
		permit.Reason = configdelivery.DeniedEntitlement
	}
	if decision.Kind == configdelivery.Secret && fresh.Eligible && expected.Eligible && expected.Matches() && fresh.Matches() {
		hash, e := expected.Entitlement.Fingerprint()
		if e == nil && hash == fresh.Request.EntitlementHash {
			permit.Kind = configdelivery.Secret
			permit.Reason = 0
		}
	}
	if decision.Kind == configdelivery.Denial && decision.Reason == configdelivery.DeniedPayment && decision.PaymentChanged {
		// A coherent expected original proof is required, never a newly selected
		// invoice or a GET error. Changes to another owner cannot become sticky.
		if request.Period == configdelivery.FirstPeriod {
			confirmation, readErr := readActivationConfirmation(ctx, tx, request.InvoiceID)
			if readErr != nil && !errors.Is(readErr, queue.ErrConflict) {
				return permit, readErr
			}
			proof, proofErr := confirmation.proof()
			a := expected.Entitlement.Activation.Account
			if readErr == nil && proofErr == nil && expected.Matches() && fresh.Eligible && fresh.Matches() && proof.Hash == request.ProofHash &&
				confirmation.clientID == request.ClientID && confirmation.botID == request.BotID && confirmation.customerID == a.CustomerID &&
				confirmation.paid == a.PaymentDate && confirmation.next == a.NextInvoiceDate {
				tag, err := tx.Exec(ctx, `UPDATE payment_confirmations pc SET review_required=true,review_code=2007
                WHERE pc.invoice_id=$1 AND pc.client_id=$2 AND pc.bot_id=$3 AND pc.customer_id=$4 AND pc.proof_hash=$5
                AND EXISTS(SELECT 1 FROM activation_flows f JOIN operations o ON o.id=f.operation_id
                    JOIN clients c ON c.id=o.client_id WHERE f.operation_id=$6 AND o.client_id=pc.client_id
                    AND f.bot_id=pc.bot_id AND f.invoice_id=pc.invoice_id AND f.proof_hash=pc.proof_hash
                    AND c.invoice_ninja_id=pc.customer_id AND c.telegram_id=$7 AND c.chat_id=$7)`, request.InvoiceID, request.ClientID, request.BotID, a.CustomerID, request.ProofHash[:], request.ActivationID, request.ActorID)
				if err != nil {
					return permit, err
				}
				if tag.RowsAffected() == 1 {
					if _, err := tx.Exec(ctx, `UPDATE payment_checks SET review_code=2007 WHERE client_id=$1 AND bot_id=$2`, request.ClientID, request.BotID); err != nil {
						return permit, err
					}
				}
			}
		} else if request.Period == configdelivery.ImportedBaselinePeriod && request.Origin == configdelivery.ImportedOrigin {
			confirmation, readErr := readActivationConfirmation(ctx, tx, request.InvoiceID)
			if readErr != nil && !errors.Is(readErr, queue.ErrConflict) {
				return permit, readErr
			}
			proof, proofErr := confirmation.proof()
			baseline := expected.Entitlement.Imported
			if readErr == nil && proofErr == nil && baseline != nil && expected.Matches() && fresh.Eligible && fresh.Matches() &&
				proof.Hash == request.ProofHash && proof.Hash == baseline.ProofHash && confirmation.clientID == request.ClientID &&
				confirmation.botID == request.BotID && confirmation.customerID == expected.Entitlement.Current.CustomerID &&
				confirmation.paid == expected.Entitlement.Current.PaymentDate && confirmation.next == expected.Entitlement.Current.NextInvoiceDate &&
				baseline.Origin.ClientID == request.ImportedOriginClientID && baseline.AdmissionPermissionRevision == request.ImportedAdmissionPermissionRevision &&
				baseline.Origin.RuntimeRevision == request.ImportedRuntimeRevision {
				tag, err := tx.Exec(ctx, `UPDATE payment_confirmations pc SET review_required=true,review_code=2007
                WHERE pc.invoice_id=$1 AND pc.client_id=$2 AND pc.bot_id=$3 AND pc.customer_id=$4 AND pc.proof_hash=$5
                  AND EXISTS(SELECT 1 FROM verified_import_origins io
                    JOIN imported_runtime_permissions rp ON rp.bot_id=io.bot_id
                    JOIN import_admission_policy ap ON ap.bot_id=io.bot_id
                    WHERE io.client_id=$6 AND io.client_id=pc.client_id AND io.bot_id=pc.bot_id
                      AND io.lifecycle='active' AND io.current_invoice_id=io.paid_anchor_invoice_id AND io.current_invoice_id=pc.invoice_id
                      AND io.evidence->>'proof_hash'=$7 AND rp.enabled AND ap.enabled
                      AND rp.revision=$8 AND ap.revision=$9 AND rp.policy_hash=io.policy_hash AND ap.policy_hash=io.policy_hash
                      AND rp.source_scope_hash=io.source_scope_hash AND ap.source_scope_hash=io.source_scope_hash)
                  AND EXISTS(SELECT 1 FROM clients c JOIN subscriptions s ON s.client_id=c.id
                    WHERE c.id=pc.client_id AND c.invoice_ninja_id=pc.customer_id AND c.telegram_id=$10 AND c.chat_id=$10
                      AND s.current_invoice_id=pc.invoice_id AND s.first_invoice_id IS NULL AND s.state='active' AND s.desired_access)`,
					request.InvoiceID, request.ClientID, request.BotID, expected.Entitlement.Current.CustomerID, request.ProofHash[:], request.ImportedOriginClientID,
					fmt.Sprintf("%x", request.ProofHash), request.ImportedRuntimeRevision, request.ImportedAdmissionPermissionRevision, request.ActorID)
				if err != nil {
					return permit, err
				}
				if tag.RowsAffected() == 1 {
					if _, err := tx.Exec(ctx, `UPDATE payment_checks SET review_code=2007 WHERE client_id=$1 AND bot_id=$2`, request.ClientID, request.BotID); err != nil {
						return permit, err
					}
				}
			}
		} else if request.Period == configdelivery.RenewalPeriod {
			confirmation, readErr := readActivationConfirmation(ctx, tx, request.InvoiceID)
			if readErr != nil && !errors.Is(readErr, queue.ErrConflict) {
				return permit, readErr
			}
			proof, proofErr := confirmation.proof()
			current := expected.Entitlement.Current
			if readErr == nil && proofErr == nil && expected.Matches() && fresh.Eligible && fresh.Matches() && proof.Hash == request.ProofHash && confirmation.clientID == request.ClientID &&
				confirmation.botID == request.BotID && confirmation.customerID == current.CustomerID && confirmation.paid == current.PaymentDate && confirmation.next == current.NextInvoiceDate {
				tag, err := tx.Exec(ctx, `UPDATE payment_confirmations pc SET review_required=true,review_code=2007
                WHERE pc.invoice_id=$1 AND pc.client_id=$2 AND pc.bot_id=$3 AND pc.customer_id=$4 AND pc.proof_hash=$5
                  AND EXISTS(SELECT 1 FROM invoice_renewal_actions a JOIN operations o ON o.id=a.operation_id
                    JOIN subscriptions s ON s.client_id=a.client_id JOIN clients c ON c.id=a.client_id
                    WHERE a.operation_id=$6 AND a.client_id=pc.client_id AND a.bot_id=pc.bot_id AND a.invoice_id=pc.invoice_id
                      AND a.outcome='completed' AND a.review_code=0 AND o.kind='invoice_paid_renewal' AND o.status='completed'
                      AND s.current_invoice_id=pc.invoice_id AND s.state='active' AND s.desired_access
                      AND ((a.origin_mode='ledger' AND $8='owned' AND a.imported_origin_client_id IS NULL)
                        OR ($8='imported' AND a.imported_origin_client_id=$9
                          AND a.imported_admission_permission_revision>0
                          AND a.imported_runtime_revision>0 AND a.imported_effect_revision>0
                          AND ((a.origin_mode='ledger' AND EXISTS(SELECT 1 FROM verified_import_origins io WHERE io.client_id=a.imported_origin_client_id AND io.client_id=a.client_id AND io.bot_id=a.bot_id AND (io.lifecycle='active' OR (io.lifecycle='paused' AND EXISTS(SELECT 1 FROM invoice_renewal_actions initial JOIN operations initial_op ON initial_op.id=initial.operation_id WHERE initial.client_id=io.client_id AND initial.bot_id=io.bot_id AND initial.imported_origin_client_id=io.client_id AND initial.origin_mode='imported_initial_paused' AND initial.outcome='completed' AND initial.review_code=0 AND initial_op.kind='invoice_paid_renewal' AND initial_op.status='completed')))))
                            OR (a.origin_mode='imported_initial_paused' AND a.imported_origin_client_id=$9 AND a.imported_admission_permission_revision>0 AND a.imported_runtime_revision>0 AND a.imported_effect_revision>0 AND EXISTS(SELECT 1 FROM verified_import_origins io WHERE io.client_id=a.imported_origin_client_id AND io.client_id=a.client_id AND io.bot_id=a.bot_id AND io.lifecycle='paused' AND io.current_invoice_id=a.invoice_id AND io.paid_anchor_invoice_id<>io.current_invoice_id)))))
                      AND c.invoice_ninja_id=pc.customer_id AND c.telegram_id=$7 AND c.chat_id=$7)`, request.InvoiceID, request.ClientID, request.BotID, current.CustomerID, request.ProofHash[:], request.RenewalID, request.ActorID, string(request.Origin), request.ImportedOriginClientID)
				if err != nil {
					return permit, err
				}
				if tag.RowsAffected() == 1 {
					if _, err := tx.Exec(ctx, `UPDATE payment_checks SET review_code=2007 WHERE client_id=$1 AND bot_id=$2`, request.ClientID, request.BotID); err != nil {
						return permit, err
					}
				}
			}
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO configuration_chat_guards (bot_id,chat_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, m.BotID, m.ChatID); err != nil {
		return permit, err
	}
	var blocked bool
	if err := tx.QueryRow(ctx, `SELECT outbox_id IS NOT NULL FROM configuration_chat_guards WHERE bot_id=$1 AND chat_id=$2 FOR UPDATE`, m.BotID, m.ChatID).Scan(&blocked); err != nil {
		return permit, err
	}
	if permit.Kind == configdelivery.Secret && blocked {
		permit.Kind = configdelivery.Denial
		permit.Reason = configdelivery.DeniedUnknownDelivery
	}
	var leaseDeadline time.Time
	if err := tx.QueryRow(ctx, `SELECT locked_until FROM outbox WHERE id=$1`, m.ID).Scan(&leaseDeadline); err != nil {
		return permit, err
	}
	permit.NotAfter = leaseDeadline
	if permit.Kind == configdelivery.Secret {
		deadline, err := fresh.Entitlement.Deadline()
		if err != nil {
			return permit, configdelivery.ErrDenied
		}
		if request.ExpiresAt.Before(permit.NotAfter) {
			permit.NotAfter = request.ExpiresAt
		}
		if deadline.Before(permit.NotAfter) {
			permit.NotAfter = deadline
		}
	}
	if _, err := tx.Exec(ctx, `SET LOCAL synchronous_commit = on`); err != nil {
		return permit, err
	}
	var snapshot any
	if permit.Kind == configdelivery.Secret {
		snapshot = request.EntitlementHash[:]
	}
	_, err = tx.Exec(ctx, `INSERT INTO configuration_dispatches (outbox_id,attempt,kind,snapshot_hash,not_after,authorized_at)
        VALUES ($1,$2,$3,$4,$5,clock_timestamp())`, m.ID, m.Attempt, string(permit.Kind), snapshot, permit.NotAfter)
	if err != nil {
		return permit, err
	}
	if permit.Kind == configdelivery.Secret {
		if _, err := tx.Exec(ctx, `UPDATE configuration_chat_guards SET outbox_id=$3,attempt=$4 WHERE bot_id=$1 AND chat_id=$2 AND outbox_id IS NULL`, m.BotID, m.ChatID, m.ID, m.Attempt); err != nil {
			return permit, err
		}
	}
	// This check is after ALL row waits and immediately before COMMIT. A late
	// secret deadline downgrades safely rather than returning an expired grant.
	if err := checkConfigurationLease(ctx, tx, m, true); err != nil {
		return permit, err
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return permit, err
	}
	if permit.Kind == configdelivery.Secret && !now.Before(permit.NotAfter) {
		if _, err := tx.Exec(ctx, `UPDATE configuration_chat_guards SET outbox_id=NULL,attempt=NULL WHERE outbox_id=$1 AND attempt=$2`, m.ID, m.Attempt); err != nil {
			return permit, err
		}
		permit.Kind, permit.Reason, permit.NotAfter = configdelivery.Denial, configdelivery.DeniedExpired, leaseDeadline
		if _, err := tx.Exec(ctx, `UPDATE configuration_dispatches SET kind='denial',snapshot_hash=NULL,not_after=$2 WHERE outbox_id=$1`, m.ID, leaseDeadline); err != nil {
			return permit, err
		}
	}
	if err := checkConfigurationLease(ctx, tx, m, true, permit.NotAfter); err != nil {
		return permit, err
	}
	if err := tx.Commit(ctx); err != nil {
		return permit, err
	}
	return permit, nil
}
