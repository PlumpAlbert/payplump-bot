package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/billing"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/lifecycle"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/jackc/pgx/v5"
)

func (s *Store) FreezeRenewalPlan(ctx context.Context, claim queue.Operation, botID int64, expected lifecycle.RenewalSnapshot, plan lifecycle.RenewalPlan, baseline json.RawMessage, allowed []int64) (err error) {
	defer func() { err = invoiceLifecycleStageError("freeze paid renewal plan", err) }()
	if plan.TargetDate == "" || len(baseline) == 0 || len(baseline) > 64<<10 || !json.Valid(baseline) || !lifecycleIDs(allowed) {
		return errors.New("invalid paid renewal plan")
	}
	if date, e := time.Parse(time.DateOnly, plan.TargetDate); e != nil || date.Format(time.DateOnly) != plan.TargetDate {
		return errors.New("invalid paid renewal target date")
	}
	var facts struct {
		RecurringID      string      `json:"recurring_id"`
		StatusID         string      `json:"status_id"`
		NextSendDate     string      `json:"next_send_date"`
		Amount           json.Number `json:"amount"`
		NextSendDatetime string      `json:"next_send_datetime"`
		SUIID            int64       `json:"sui_id"`
		SUIEnabled       bool        `json:"sui_enabled"`
	}
	if json.Unmarshal(baseline, &facts) != nil || facts.RecurringID != expected.RecurringID || facts.SUIID != expected.SUIBinding.ID {
		return errors.New("invalid paid renewal before facts")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer rollback(tx)
	clientID, invoiceID, err := lockRenewalClaim(ctx, tx, claim, botID)
	if err != nil {
		return err
	}
	actual, err := readRenewalSnapshot(ctx, tx, claim, botID)
	if err != nil {
		return err
	}
	if clientID != expected.ClientID || invoiceID != expected.InvoiceID || !reflect.DeepEqual(actual, expected) {
		return queue.ErrConflict
	}
	if actual.Plan != nil || actual.WriterBusy || actual.PaymentLocked || actual.PaymentReview || actual.LifecycleReview || !validRenewalStoreFacts(actual, botID, allowed) || actual.Proof.Hash != expected.Proof.Hash {
		return queue.ErrConflict
	}
	next, e := billing.NextBillingDate(actual.Proof.PaymentDate)
	if e != nil || next != plan.TargetDate || next != actual.Proof.NextInvoiceDate || next <= time.Now().In(time.Local).Format(time.DateOnly) {
		return lifecycle.ErrRenewalTargetExpired
	}
	shouldSchedule := facts.NextSendDate != next
	shouldEnable := actual.State == "overdue"
	shouldPause := shouldSchedule && facts.StatusID == "2"
	shouldResume := shouldSchedule || shouldEnable
	if plan.Schedule != shouldSchedule || plan.Enable != shouldEnable || plan.Pause != shouldPause || plan.Resume != shouldResume || facts.SUIEnabled == shouldEnable || facts.StatusID != "2" && facts.StatusID != "3" {
		return queue.ErrConflict
	}
	if actual.State == "active" && (facts.StatusID != "2" || !facts.SUIEnabled) || actual.State == "overdue" && (facts.StatusID != "3" || facts.SUIEnabled || !hasRenewalOverduePrefix(actual)) {
		return queue.ErrConflict
	}
	when, parseErr := invoiceninja.ParseRecurringUTC(facts.NextSendDatetime)
	if parseErr != nil || !when.After(time.Now()) {
		return lifecycle.ErrRenewalTargetExpired
	}
	var epoch, owner int64
	var ownerStep string
	if err = tx.QueryRow(ctx, `SELECT epoch,COALESCE(operation_id,0),COALESCE(step,'') FROM activation_write_guard WHERE id=1 FOR UPDATE`).Scan(&epoch, &owner, &ownerStep); err != nil {
		return err
	}
	if owner != 0 {
		return lifecycle.ErrRenewalGuardBusy
	}
	if epoch != actual.WriteEpoch {
		return queue.ErrConflict
	}
	tag, err := tx.Exec(ctx, `UPDATE invoice_renewal_actions SET plan_frozen_at=clock_timestamp(),target_date=$2::date,requires_pause=$3,requires_schedule=$4,requires_enable=$5,requires_resume=$6,baseline=$7::jsonb WHERE operation_id=$1 AND outcome='pending' AND plan_frozen_at IS NULL`, claim.ID, plan.TargetDate, plan.Pause, plan.Schedule, plan.Enable, plan.Resume, string(baseline))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrConflict
	}
	return tx.Commit(ctx)
}

func (s *Store) AuthorizeRenewalWrite(ctx context.Context, claim queue.Operation, botID int64, expected lifecycle.RenewalSnapshot, step lifecycle.RenewalStep, request json.RawMessage, today string, allowed []int64) (authorized bool, err error) {
	defer func() { err = invoiceLifecycleStageError("authorize paid renewal write", err) }()
	if !step.Valid() || len(request) == 0 || len(request) > 4<<20 || !json.Valid(request) || !lifecycleIDs(allowed) {
		return false, errors.New("invalid paid renewal intent")
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(request, &object) != nil || len(object) == 0 {
		return false, errors.New("renewal intent must be a JSON object")
	}
	date, e := time.Parse(time.DateOnly, today)
	if e != nil || date.Format(time.DateOnly) != today {
		return false, errors.New("invalid paid renewal local date")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, err
	}
	defer rollback(tx)
	if _, err = tx.Exec(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		return false, err
	}
	clientID, invoiceID, err := lockRenewalClaim(ctx, tx, claim, botID)
	if err != nil {
		return false, err
	}
	actual, err := readRenewalSnapshot(ctx, tx, claim, botID)
	if err != nil {
		return false, err
	}
	if clientID != expected.ClientID || invoiceID != expected.InvoiceID || !reflect.DeepEqual(actual, expected) {
		return false, queue.ErrConflict
	}
	if actual.Plan == nil || !actual.Plan.Required(step) || actual.WriterBusy || actual.PaymentLocked || actual.PaymentReview || actual.LifecycleReview || !validRenewalStoreFacts(actual, botID, allowed) || actual.Proof.Hash != expected.Proof.Hash {
		return false, queue.ErrConflict
	}
	if actual.Plan.TargetDate <= today {
		return false, lifecycle.ErrRenewalTargetExpired
	}
	target, e := billing.NextBillingDate(actual.Proof.PaymentDate)
	if e != nil || target != actual.Plan.TargetDate || target != actual.Proof.NextInvoiceDate {
		return false, queue.ErrConflict
	}
	if actual.State == "overdue" && !hasRenewalOverduePrefix(actual) {
		return false, queue.ErrConflict
	}
	next, pending, done, valid := lifecycle.NextRenewalStep(*actual.Plan, actual.Steps)
	if !valid {
		return false, queue.ErrConflict
	}
	if pending || done || next != step {
		return false, lifecycle.ErrRenewalIntentExists
	}
	if !renewalStepStateAllowed(actual, step) {
		return false, queue.ErrConflict
	}
	var epoch, owner int64
	var ownerStep string
	if err = tx.QueryRow(ctx, `SELECT epoch,COALESCE(operation_id,0),COALESCE(step,'') FROM activation_write_guard WHERE id=1 FOR UPDATE`).Scan(&epoch, &owner, &ownerStep); err != nil {
		return false, err
	}
	if owner != 0 {
		return false, lifecycle.ErrRenewalGuardBusy
	}
	if epoch != expected.WriteEpoch {
		return false, queue.ErrConflict
	}
	if _, err = tx.Exec(ctx, `INSERT INTO invoice_renewal_steps(operation_id,step,request,dispatch_attempt) VALUES($1,$2,$3::jsonb,$4)`, claim.ID, string(step), string(request), claim.Attempt); err != nil {
		if isUniqueViolation(err) {
			return false, lifecycle.ErrRenewalIntentExists
		}
		return false, err
	}
	tag, err := tx.Exec(ctx, `UPDATE activation_write_guard SET operation_id=$1,step=$2,epoch=epoch+1 WHERE id=1 AND operation_id IS NULL AND epoch=$3`, claim.ID, string(step), epoch)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() != 1 {
		return false, lifecycle.ErrRenewalGuardBusy
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func renewalStepStateAllowed(s lifecycle.RenewalSnapshot, step lifecycle.RenewalStep) bool {
	switch step {
	case lifecycle.RenewalPause:
		return s.State == "active" && s.DesiredAccess && s.Plan != nil && s.Plan.Schedule
	case lifecycle.RenewalSchedule:
		return s.State == "active" && s.DesiredAccess || s.State == "overdue" && !s.DesiredAccess
	case lifecycle.RenewalEnable:
		return s.State == "overdue" && !s.DesiredAccess
	case lifecycle.RenewalResume:
		return s.State == "active" && s.DesiredAccess || s.State == "overdue" && !s.DesiredAccess
	default:
		return false
	}
}

func (s *Store) ConfirmRenewalStep(ctx context.Context, claim queue.Operation, step lifecycle.RenewalStep, externalID string, receipt json.RawMessage) (err error) {
	defer func() { err = invoiceLifecycleStageError("confirm paid renewal write", err) }()
	if !step.Valid() || externalID == "" || len(externalID) > 256 || len(receipt) > 4096 || len(receipt) > 0 && !json.Valid(receipt) {
		return errors.New("invalid paid renewal confirmation")
	}
	var botID int64
	if err = s.pool.QueryRow(ctx, `SELECT bot_id FROM invoice_renewal_actions WHERE operation_id=$1`, claim.ID).Scan(&botID); errors.Is(err, pgx.ErrNoRows) {
		return queue.ErrLeaseLost
	} else if err != nil {
		return err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer rollback(tx)
	_, _, err = lockRenewalClaim(ctx, tx, claim, botID)
	if err != nil {
		return err
	}
	actual, err := readRenewalSnapshot(ctx, tx, claim, botID)
	if err != nil {
		return err
	}
	if actual.Plan == nil {
		return queue.ErrConflict
	}
	if step == lifecycle.RenewalEnable {
		if externalID != actual.SUIID {
			return queue.ErrConflict
		}
	} else if externalID != actual.RecurringID {
		return queue.ErrConflict
	}
	next, pending, _, valid := lifecycle.NextRenewalStep(*actual.Plan, actual.Steps)
	if !valid || !pending || next != step {
		return queue.ErrConflict
	}
	var owner int64
	var ownerStep string
	if err = tx.QueryRow(ctx, `SELECT COALESCE(operation_id,0),COALESCE(step,'') FROM activation_write_guard WHERE id=1 FOR UPDATE`).Scan(&owner, &ownerStep); err != nil {
		return err
	}
	if owner != claim.ID || ownerStep != string(step) {
		return queue.ErrConflict
	}
	var attempt int
	var confirmed bool
	if err = tx.QueryRow(ctx, `SELECT dispatch_attempt,confirmed_at IS NOT NULL FROM invoice_renewal_steps WHERE operation_id=$1 AND step=$2 FOR UPDATE`, claim.ID, string(step)).Scan(&attempt, &confirmed); errors.Is(err, pgx.ErrNoRows) {
		return queue.ErrConflict
	} else if err != nil {
		return err
	}
	if attempt != claim.Attempt || confirmed {
		return queue.ErrConflict
	}
	if step == lifecycle.RenewalSchedule {
		var v struct {
			Amount           json.Number `json:"Amount"`
			NextSendDatetime string      `json:"NextSendDatetime"`
		}
		if json.Unmarshal(receipt, &v) != nil || v.Amount == "" || v.NextSendDatetime == "" {
			return queue.ErrConflict
		}
		amount, e := billing.FormatAmount(v.Amount)
		if e != nil || amount != v.Amount.String() || amount == "0" {
			return queue.ErrConflict
		}
		when, parseErr := invoiceninja.ParseRecurringUTC(v.NextSendDatetime)
		if parseErr != nil || !when.After(time.Now()) {
			return queue.ErrConflict
		}
	} else if len(receipt) > 0 {
		return queue.ErrConflict
	}
	var receiptArg any
	if len(receipt) > 0 {
		receiptArg = string(receipt)
	}
	tag, err := tx.Exec(ctx, `UPDATE invoice_renewal_steps SET external_id=$3,receipt=$4::jsonb,confirmed_at=clock_timestamp() WHERE operation_id=$1 AND step=$2 AND confirmed_at IS NULL`, claim.ID, string(step), externalID, receiptArg)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrConflict
	}
	tag, err = tx.Exec(ctx, `UPDATE activation_write_guard SET operation_id=NULL,step=NULL WHERE id=1 AND operation_id=$1 AND step=$2`, claim.ID, string(step))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrConflict
	}
	return tx.Commit(ctx)
}

func pinUnknownRenewalGuard(ctx context.Context, tx pgx.Tx, operationID int64) error {
	var step string
	err := tx.QueryRow(ctx, `SELECT step FROM invoice_renewal_steps WHERE operation_id=$1 AND confirmed_at IS NULL ORDER BY CASE step WHEN 'pause_recurring' THEN 1 WHEN 'schedule_recurring' THEN 2 WHEN 'enable_sui' THEN 3 ELSE 4 END LIMIT 1 FOR UPDATE`, operationID).Scan(&step)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var owner int64
	var ownerStep string
	if err = tx.QueryRow(ctx, `SELECT COALESCE(operation_id,0),COALESCE(step,'') FROM activation_write_guard WHERE id=1 FOR UPDATE`).Scan(&owner, &ownerStep); err != nil {
		return err
	}
	if owner != 0 {
		return nil
	}
	tag, err := tx.Exec(ctx, `UPDATE activation_write_guard SET epoch=epoch+1,operation_id=$1,step=$2 WHERE id=1 AND operation_id IS NULL`, operationID, step)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return lifecycle.ErrRenewalGuardBusy
	}
	return nil
}

func (s *Store) ReviewRenewal(ctx context.Context, claim queue.Operation, botID int64, expected lifecycle.RenewalSnapshot, code int, drafts []queue.Draft) (err error) {
	defer func() { err = invoiceLifecycleStageError("review paid renewal action", err) }()
	if botID <= 0 || code <= 0 || len(drafts) == 0 || len(drafts) > 99 {
		return errors.New("invalid paid renewal review")
	}
	for _, d := range drafts {
		if d.BotID != botID || d.Validate() != nil {
			return errors.New("invalid paid renewal review notice")
		}
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer rollback(tx)
	if err = prepareOutboxBots(ctx, tx, drafts); err != nil {
		return err
	}
	clientID, invoiceID, err := lockRenewalClaim(ctx, tx, claim, botID)
	if err != nil {
		return err
	}
	actual, err := readRenewalSnapshot(ctx, tx, claim, botID)
	if err != nil {
		return err
	}
	if clientID != expected.ClientID || invoiceID != expected.InvoiceID || actual.ClientID != expected.ClientID || actual.InvoiceID != expected.InvoiceID {
		return queue.ErrConflict
	}
	if err = pinUnknownRenewalGuard(ctx, tx, claim.ID); err != nil {
		return err
	}
	if err = finishRenewalOperation(ctx, tx, claim, "review", code); err != nil {
		return err
	}
	for _, d := range drafts {
		if _, err = enqueueOutbox(ctx, tx, d); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) FinishRenewal(ctx context.Context, claim queue.Operation, botID int64, expected lifecycle.RenewalSnapshot, drafts []queue.Draft) (err error) {
	defer func() { err = invoiceLifecycleStageError("finish paid renewal action", err) }()
	if err = validateRenewalDrafts(drafts, botID); err != nil {
		return err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer rollback(tx)
	if err = prepareOutboxBots(ctx, tx, drafts); err != nil {
		return err
	}
	clientID, invoiceID, err := lockRenewalClaim(ctx, tx, claim, botID)
	if err != nil {
		return err
	}
	actual, err := readRenewalSnapshot(ctx, tx, claim, botID)
	if err != nil {
		return err
	}
	if clientID != expected.ClientID || invoiceID != expected.InvoiceID || !sameRenewalCompletionFacts(actual, expected) || actual.PaymentLocked || actual.PaymentReview || actual.LifecycleReview || actual.WriterBusy {
		return queue.ErrConflict
	}
	if !validRenewalStoreFacts(actual, botID, nil) || actual.CurrentInvoiceID != actual.InvoiceID || actual.FirstInvoiceID == actual.InvoiceID || actual.Proof.Hash != expected.Proof.Hash || actual.Plan == nil {
		return queue.ErrConflict
	}
	target, e := billing.NextBillingDate(actual.Proof.PaymentDate)
	if e != nil || target != actual.Plan.TargetDate || target != actual.Proof.NextInvoiceDate || target <= time.Now().In(time.Local).Format(time.DateOnly) {
		return queue.ErrConflict
	}
	_, pending, complete, valid := lifecycle.NextRenewalStep(*actual.Plan, actual.Steps)
	if !valid || pending || !complete {
		return queue.ErrConflict
	}
	if actual.State != "active" && !(actual.State == "overdue" && !actual.DesiredAccess) || actual.Plan.Enable != (actual.State == "overdue") {
		return queue.ErrConflict
	}
	if err = renewalCompletionReceipt(actual); err != nil {
		return err
	}
	var guardOwner int64
	if err = tx.QueryRow(ctx, `SELECT COALESCE(operation_id,0) FROM activation_write_guard WHERE id=1 FOR UPDATE`).Scan(&guardOwner); err != nil {
		return err
	}
	if guardOwner != 0 {
		return lifecycle.ErrRenewalGuardBusy
	}
	receipt := actual.Plan.BaselineReceipt
	if step, ok := actual.Steps[lifecycle.RenewalSchedule]; ok {
		var v invoiceninja.RecurringReceipt
		if !step.Confirmed || json.Unmarshal(step.Receipt, &v) != nil {
			return queue.ErrConflict
		}
		receipt = v
	}
	if _, err = tx.Exec(ctx, `UPDATE invoice_renewal_actions SET recurring_amount=$2::numeric,recurring_next_send_datetime=$3::timestamptz WHERE operation_id=$1 AND outcome='pending'`, claim.ID, receipt.Amount, receipt.NextSendDatetime); err != nil {
		return err
	}
	if err = finishRenewalOperation(ctx, tx, claim, "completed", 0); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE subscriptions SET state='active',desired_access=true,payment_date=$2::date,next_invoice_date=$3::date WHERE client_id=$1`, clientID, actual.Proof.PaymentDate, actual.Plan.TargetDate)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrConflict
	}
	tag, err = tx.Exec(ctx, `UPDATE invoice_lifecycle_invoices SET status='paid',balance=0,paid_to_date=amount,notice_state='settled',observed_at=clock_timestamp() WHERE client_id=$1 AND invoice_id=$2 AND amount=$3::numeric AND customer_id=$4 AND recurring_id=$5`, clientID, invoiceID, actual.Proof.Amount, actual.CustomerID, actual.RecurringID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrConflict
	}
	for _, d := range drafts {
		if _, err = enqueueOutbox(ctx, tx, d); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func renewalCompletionReceipt(s lifecycle.RenewalSnapshot) error {
	if s.Plan == nil {
		return queue.ErrConflict
	}
	receipt := s.Plan.BaselineReceipt
	if r, ok := s.Steps[lifecycle.RenewalSchedule]; ok {
		var v invoiceninja.RecurringReceipt
		if !r.Confirmed || json.Unmarshal(r.Receipt, &v) != nil {
			return queue.ErrConflict
		}
		receipt = v
	}
	amount, err := billing.FormatAmount(receipt.Amount)
	if err != nil || amount == "0" || amount != receipt.Amount.String() {
		return queue.ErrConflict
	}
	when, err := invoiceninja.ParseRecurringUTC(receipt.NextSendDatetime)
	if err != nil || !when.After(time.Now()) {
		return queue.ErrConflict
	}
	return nil
}

func finishRenewalOperation(ctx context.Context, tx pgx.Tx, claim queue.Operation, outcome string, code int) error {
	tag, err := tx.Exec(ctx, `UPDATE invoice_renewal_actions SET outcome=$2,review_code=$3,finished_at=clock_timestamp() WHERE operation_id=$1 AND outcome='pending'`, claim.ID, outcome, code)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrLeaseLost
	}
	tag, err = tx.Exec(ctx, `UPDATE operations SET status='completed',locked_until=NULL,completed_at=clock_timestamp(),last_error_code=$3 WHERE id=$1 AND attempts=$2 AND kind=$4 AND status='running' AND locked_until>clock_timestamp()`, claim.ID, claim.Attempt, code, lifecycle.RenewalOperationKind)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrLeaseLost
	}
	return nil
}

func lockRenewalClaim(ctx context.Context, tx pgx.Tx, claim queue.Operation, botID int64) (int64, string, error) {
	if claim.ID <= 0 || claim.Attempt <= 0 || claim.ClientID <= 0 || claim.Kind != lifecycle.RenewalOperationKind || botID <= 0 {
		return 0, "", queue.ErrLeaseLost
	}
	var clientID int64
	var invoiceID, first string
	err := tx.QueryRow(ctx, `SELECT a.client_id,a.invoice_id,COALESCE(s.first_invoice_id,'') FROM invoice_renewal_actions a JOIN subscriptions s ON s.client_id=a.client_id WHERE a.operation_id=$1 AND a.bot_id=$2`, claim.ID, botID).Scan(&clientID, &invoiceID, &first)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", queue.ErrLeaseLost
	}
	if err != nil {
		return 0, "", err
	}
	if clientID != claim.ClientID {
		return 0, "", queue.ErrConflict
	}
	if err = lockOperationClient(ctx, tx, claim.ID); err != nil {
		return 0, "", err
	}
	var id int64
	if err = tx.QueryRow(ctx, `SELECT client_id FROM subscriptions WHERE client_id=$1 FOR NO KEY UPDATE`, clientID).Scan(&id); errors.Is(err, pgx.ErrNoRows) {
		return 0, "", queue.ErrConflict
	} else if err != nil {
		return 0, "", err
	}
	if err = tx.QueryRow(ctx, `SELECT client_id FROM payment_checks WHERE client_id=$1 AND bot_id=$2 FOR UPDATE`, clientID, botID).Scan(&id); errors.Is(err, pgx.ErrNoRows) {
		return 0, "", queue.ErrConflict
	} else if err != nil {
		return 0, "", err
	}
	confirmationIDs := []string{invoiceID}
	if first != "" && first != invoiceID {
		confirmationIDs = append(confirmationIDs, first)
	}
	rows, err := tx.Query(ctx, `SELECT invoice_id FROM payment_confirmations WHERE invoice_id=ANY($1::text[]) ORDER BY invoice_id FOR UPDATE`, confirmationIDs)
	if err != nil {
		return 0, "", err
	}
	if _, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return 0, "", err
	}
	if err = tx.QueryRow(ctx, `SELECT invoice_id FROM invoice_lifecycle_invoices WHERE client_id=$1 AND invoice_id=$2 FOR UPDATE`, clientID, invoiceID).Scan(&invoiceID); errors.Is(err, pgx.ErrNoRows) {
		return 0, "", queue.ErrConflict
	} else if err != nil {
		return 0, "", err
	}
	if err = tx.QueryRow(ctx, `SELECT id FROM operations WHERE id=$1 AND client_id=$2 AND kind=$3 AND status='running' AND attempts=$4 AND locked_until>clock_timestamp() FOR UPDATE`, claim.ID, clientID, lifecycle.RenewalOperationKind, claim.Attempt).Scan(&id); errors.Is(err, pgx.ErrNoRows) {
		return 0, "", queue.ErrLeaseLost
	} else if err != nil {
		return 0, "", err
	}
	if err = tx.QueryRow(ctx, `SELECT client_id FROM invoice_renewal_actions WHERE operation_id=$1 AND bot_id=$2 AND outcome='pending' AND review_code=0 FOR UPDATE`, claim.ID, botID).Scan(&id); errors.Is(err, pgx.ErrNoRows) {
		return 0, "", queue.ErrLeaseLost
	} else if err != nil {
		return 0, "", err
	}
	rows, err = tx.Query(ctx, `SELECT step FROM invoice_renewal_steps WHERE operation_id=$1 ORDER BY step FOR UPDATE`, claim.ID)
	if err != nil {
		return 0, "", err
	}
	if _, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return 0, "", err
	}
	return clientID, invoiceID, nil
}

func sameRenewalCompletionFacts(actual, expected lifecycle.RenewalSnapshot) bool {
	actual.WriteEpoch = 0
	actual.WriterBusy = false
	actual.WriterOperationID = 0
	actual.WriterStep = ""
	expected.WriteEpoch = 0
	expected.WriterBusy = false
	expected.WriterOperationID = 0
	expected.WriterStep = ""
	return reflect.DeepEqual(actual, expected)
}
func validateRenewalDrafts(drafts []queue.Draft, botID int64) error {
	if len(drafts) < 2 || len(drafts) > 100 {
		return errors.New("paid renewal completion requires client and administrator notices")
	}
	for _, d := range drafts {
		if d.BotID != botID || d.Validate() != nil {
			return errors.New("invalid paid renewal notice set")
		}
	}
	return nil
}
