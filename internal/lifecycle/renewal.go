package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/billing"
	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

var (
	ErrRenewalGuardBusy     = errors.New("shared external writer guard is busy")
	ErrRenewalIntentExists  = errors.New("paid renewal write intent already exists; do not replay")
	ErrRenewalPaymentLease  = errors.New("payment verifier lease is active")
	errRenewalAuthorization = errors.New("fresh payment proof no longer authorizes renewal")
	ErrRenewalTargetExpired = errors.New("paid renewal target date is not safely in the future")
)

const (
	renewalLease       = 5 * time.Minute
	renewalAttemptTime = 4 * time.Minute
	renewalDBTimeout   = 8 * time.Second
)

type RenewalWorker struct {
	store      RenewalStore
	invoices   RenewalInvoiceReader
	payments   RenewalPaymentReader
	recurring  RenewalRecurringWriter
	panel      RenewalSUIWriter
	botID      int64
	admins     []int64
	currencyID string
	cfg        RenewalConfig
	logger     *slog.Logger
	now        func() time.Time
}

func NewRenewalWorker(store RenewalStore, invoices RenewalInvoiceReader, paymentReader RenewalPaymentReader, recurring RenewalRecurringWriter, panel RenewalSUIWriter, botID int64, admins []int64, currencyID string, cfg RenewalConfig, logger *slog.Logger) (*RenewalWorker, error) {
	if !cfg.Enabled {
		return nil, errors.New("paid renewal is disabled")
	}
	if err := cfg.Validate(true, true, true); err != nil {
		return nil, err
	}
	if store == nil || invoices == nil || paymentReader == nil || recurring == nil || panel == nil || botID <= 0 || !validPaymentCurrency(currencyID) {
		return nil, errors.New("paid renewal requires verified store and bounded provider capabilities")
	}
	ids := sortedIDs(admins)
	ids = slices.Compact(ids)
	if len(ids) < 1 || len(ids) > 99 || ids[0] <= 0 {
		return nil, errors.New("paid renewal requires 1-99 positive administrator IDs")
	}
	cfg.AllowedTelegramIDs = slices.Clone(cfg.AllowedTelegramIDs)
	cfg.ExcludedTelegramIDs = slices.Clone(cfg.ExcludedTelegramIDs)
	if logger == nil {
		logger = slog.Default()
	}
	return &RenewalWorker{store: store, invoices: invoices, payments: paymentReader, recurring: recurring, panel: panel, botID: botID, admins: ids, currencyID: currencyID, cfg: cfg, logger: logger, now: time.Now}, nil
}

func validPaymentCurrency(s string) bool {
	if s == "" || len(s) > 10 || s[0] < '1' || s[0] > '9' {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (w *RenewalWorker) Run(ctx context.Context) error {
	allowed := enforcementEffectiveIDs(w.cfg.AllowedTelegramIDs, w.cfg.ExcludedTelegramIDs)
	for ctx.Err() == nil {
		db, cancel := context.WithTimeout(ctx, renewalDBTimeout)
		_, err := w.store.SeedRenewalOperations(db, w.botID, allowed, w.cfg.MaxAccounts)
		cancel()
		if err != nil && ctx.Err() == nil {
			w.logger.Warn("paid renewal seed deferred")
		}
		if ctx.Err() != nil {
			return nil
		}
		db, cancel = context.WithTimeout(ctx, renewalDBTimeout)
		claims, err := w.store.ClaimOperations(db, []string{RenewalOperationKind}, w.cfg.Workers, renewalLease)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				w.logger.Warn("paid renewal claim deferred")
			}
		} else {
			var tasks sync.WaitGroup
			for _, claim := range claims {
				if ctx.Err() != nil {
					break
				}
				tasks.Add(1)
				go func(c queue.Operation) { defer tasks.Done(); w.handle(ctx, c) }(claim)
			}
			tasks.Wait()
		}
		timer := time.NewTimer(w.cfg.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	return nil
}

func (w *RenewalWorker) handle(service context.Context, claim queue.Operation) {
	if claim.ID <= 0 || claim.Attempt <= 0 || claim.ClientID <= 0 || claim.Kind != RenewalOperationKind {
		return
	}
	ctx, cancel := context.WithTimeout(service, renewalAttemptTime)
	defer cancel()
	for i := 0; i < 6 && ctx.Err() == nil; i++ {
		db, stop := context.WithTimeout(ctx, renewalDBTimeout)
		s, err := w.store.LoadRenewalSnapshot(db, claim, w.botID)
		stop()
		if err != nil {
			if !errors.Is(err, queue.ErrLeaseLost) && service.Err() == nil {
				w.retry(claim, err)
			}
			return
		}
		if s.ClientID != claim.ClientID || s.BotID != w.botID || !validRenewalAssociation(s, w.botID, w.cfg.AllowedTelegramIDs, w.cfg.ExcludedTelegramIDs) {
			w.review(claim, s, ReviewRenewalAssociation)
			return
		}
		if s.WriterBusy {
			if s.WriterOperationID == claim.ID && s.WriterStep != "" {
				w.review(claim, s, ReviewRenewalUnknownWrite)
			} else {
				w.retry(claim, ErrRenewalGuardBusy)
			}
			return
		}
		if hasPendingRenewalIntent(s.Steps) {
			w.review(claim, s, ReviewRenewalUnknownWrite)
			return
		}
		if s.PaymentLocked {
			w.retry(claim, ErrRenewalPaymentLease)
			return
		}
		if s.PaymentReview || s.LifecycleReview {
			w.review(claim, s, ReviewRenewalProof)
			return
		}
		decision, err := w.observe(ctx, s)
		if err != nil {
			if service.Err() == nil {
				w.retry(claim, err)
			}
			return
		}
		if decision.Kind != payments.Verified || decision.Proof == nil || decision.Proof.Hash != s.Proof.Hash {
			w.review(claim, s, ReviewRenewalProof)
			return
		}
		invoice, err := w.invoices.GetInvoice(ctx, s.InvoiceID)
		if err != nil {
			if service.Err() == nil {
				w.retry(claim, err)
			}
			return
		}
		if !renewalInvoiceMatches(invoice, s) {
			w.review(claim, s, ReviewRenewalProof)
			return
		}
		record, err := w.recurring.Get(ctx, s.RecurringID)
		if err != nil {
			if service.Err() == nil {
				w.retry(claim, err)
			}
			return
		}
		panelRecord, panelErr := w.panel.GetRecord(ctx, s.SUIBinding.ID)
		if panelErr != nil {
			if service.Err() == nil {
				w.retry(claim, panelErr)
			}
			return
		}
		if !suiRenewalBindingMatches(panelRecord, s.SUIBinding) {
			w.review(claim, s, ReviewRenewalAssociation)
			return
		}
		if code := validRenewalExternalState(s, record, panelRecord); code != 0 {
			w.review(claim, s, code)
			return
		}
		if s.Plan == nil {
			plan, baseline, code := makeRenewalPlan(s, record, panelRecord, w.now())
			if code != 0 {
				w.review(claim, s, code)
				return
			}
			db, stop = context.WithTimeout(ctx, renewalDBTimeout)
			err = w.store.FreezeRenewalPlan(db, claim, w.botID, s, plan, baseline, enforcementEffectiveIDs(w.cfg.AllowedTelegramIDs, w.cfg.ExcludedTelegramIDs))
			stop()
			if err != nil {
				if errors.Is(err, queue.ErrConflict) {
					continue
				}
				if errors.Is(err, ErrRenewalTargetExpired) {
					w.review(claim, s, ReviewRenewalTarget)
					return
				}
				if errors.Is(err, ErrRenewalGuardBusy) {
					w.retry(claim, err)
					return
				}
				if !errors.Is(err, queue.ErrLeaseLost) {
					w.retry(claim, err)
				}
				return
			}
			continue
		}
		next, pending, done, valid := NextRenewalStep(*s.Plan, s.Steps)
		if !valid || pending {
			w.review(claim, s, ReviewRenewalSequence)
			return
		}
		if done {
			if !renewalCompletionState(s, *s.Plan, record, panelRecord, w.now()) {
				w.review(claim, s, ReviewRenewalExternalState)
				return
			}
			drafts := w.successNotices(s, *s.Plan)
			db, stop = context.WithTimeout(ctx, renewalDBTimeout)
			err = w.store.FinishRenewal(db, claim, w.botID, s, drafts)
			stop()
			if errors.Is(err, queue.ErrConflict) {
				w.review(claim, s, ReviewRenewalProof)
			} else if errors.Is(err, ErrRenewalGuardBusy) {
				w.retry(claim, err)
			} else if !errors.Is(err, queue.ErrLeaseLost) && err != nil {
				w.logger.Warn("paid renewal completion deferred")
			}
			return
		}
		if !s.Plan.Required(next) {
			w.review(claim, s, ReviewRenewalSequence)
			return
		}
		if !w.dispatch(ctx, claim, s, next, record, panelRecord) {
			return
		}
	}
	if ctx.Err() == nil {
		db, stop := context.WithTimeout(context.Background(), renewalDBTimeout)
		if s, err := w.store.LoadRenewalSnapshot(db, claim, w.botID); err == nil {
			w.review(claim, s, ReviewRenewalSequence)
		}
		stop()
	}
}

func validRenewalAssociation(s RenewalSnapshot, botID int64, allowed, excluded []int64) bool {
	return s.BotID == botID && s.ClientID > 0 && s.TelegramID > 0 && s.ChatID == s.TelegramID && s.ManagedProvenance && slices.Contains(allowed, s.TelegramID) && !slices.Contains(excluded, s.TelegramID) && s.CustomerID != "" && s.InvoiceID != "" && s.CurrentInvoiceID == s.InvoiceID && s.FirstInvoiceID != "" && s.FirstInvoiceID != s.InvoiceID && s.RecurringID != "" && s.SUIID != "" && s.RegistrationToken != "" && s.ActivationToken != "" && s.Payment.ClientID == s.ClientID && s.Payment.BotID == botID && s.Payment.CustomerID == s.CustomerID && s.Payment.InvoiceID == s.InvoiceID && s.Payment.FirstInvoiceID == s.FirstInvoiceID && s.Payment.RecurringID == s.RecurringID && s.Proof.InvoiceID == s.InvoiceID && s.Proof.CustomerID == s.CustomerID && s.Proof.Validate() == nil && validRenewalLedger(s) && s.RecurringDraft.ClientID == s.CustomerID && s.RecurringDraft.PrivateNotes == activation.Marker(s.ActivationToken) && invoiceninja.ValidateRecurringDraft(s.RecurringDraft) == nil && s.SUIBinding.ID > 0 && strconv.FormatInt(s.SUIBinding.ID, 10) == s.SUIID && s.SUIBinding.Name != ""
}

func validRenewalLedger(s RenewalSnapshot) bool {
	if s.LedgerCustomerID != s.CustomerID || s.LedgerRecurringID != s.RecurringID || s.LedgerInvoiceNumber != s.Proof.InvoiceNumber || s.LedgerStatus != "sent" && s.LedgerStatus != "partial" && s.LedgerStatus != "paid" {
		return false
	}
	date, err := time.Parse(time.DateOnly, s.LedgerSentDate)
	if err != nil || date.Format(time.DateOnly) != s.LedgerSentDate {
		return false
	}
	amount, err := billing.FormatAmount(json.Number(s.LedgerAmount))
	if err != nil || amount != s.LedgerAmount || amount != s.Proof.Amount {
		return false
	}
	balance, err := billing.FormatAmount(json.Number(s.LedgerBalance))
	if err != nil || balance != s.LedgerBalance {
		return false
	}
	paid, err := billing.FormatAmount(json.Number(s.LedgerPaidToDate))
	if err != nil || paid != s.LedgerPaidToDate {
		return false
	}
	a, aok := new(big.Rat).SetString(amount)
	b, bok := new(big.Rat).SetString(balance)
	p, pok := new(big.Rat).SetString(paid)
	if !aok || !bok || !pok || new(big.Rat).Add(b, p).Cmp(a) != 0 {
		return false
	}
	switch s.LedgerStatus {
	case "sent":
		return s.LedgerBalance == s.LedgerAmount && s.LedgerPaidToDate == "0"
	case "partial":
		return s.LedgerBalance != "0" && s.LedgerPaidToDate != "0"
	case "paid":
		return s.LedgerBalance == "0" && s.LedgerPaidToDate == s.LedgerAmount
	}
	return false
}

func renewalInvoiceMatches(invoice invoiceninja.CycleInvoice, s RenewalSnapshot) bool {
	return invoice.ID == s.InvoiceID && invoice.ClientID == s.CustomerID && invoice.RecurringID == s.RecurringID && invoice.PrivateNotes == activation.Marker(s.ActivationToken) && invoice.StatusID == "4" && !invoice.IsDeleted && invoice.ArchivedAt == 0
}

func (w *RenewalWorker) observe(ctx context.Context, s RenewalSnapshot) (payments.Decision, error) {
	d, err := payments.Observe(ctx, w.payments, s.Payment, w.currencyID, w.now().In(time.Local).Format(time.DateOnly))
	if err == nil && d.Kind == payments.Verified && (d.Proof.Hash != s.Proof.Hash || d.Proof.InvoiceNumber != s.Proof.InvoiceNumber) {
		return payments.Decision{Kind: payments.NeedsReview, Code: payments.ReviewChangedProof}, nil
	}
	return d, err
}

// Own-overdue provenance is captured durably by the store; only this explicit marker allows restoration.
func validRenewalExternalState(s RenewalSnapshot, r invoiceninja.RecurringRecord, p sui.ClientRecord) int {
	if r.ID != s.RecurringID || r.ClientID != s.CustomerID || r.PrivateNotes != s.RecurringDraft.PrivateNotes || r.IsDeleted || r.ArchivedAt != 0 || !validRenewalRecurringRecord(r, s.RecurringDraft) {
		return ReviewRenewalAssociation
	}
	if s.State == "active" && s.DesiredAccess {
		if !p.Enabled() {
			return ReviewRenewalExternalState
		}
		if r.StatusID == "2" {
			return 0
		}
		pause, paused := s.Steps[RenewalPause]
		_, resumed := s.Steps[RenewalResume]
		if r.StatusID == "3" && s.Plan != nil && s.Plan.Pause && paused && pause.Confirmed && !resumed {
			return 0
		}
		return ReviewRenewalExternalState
	}
	if s.State == "overdue" && !s.DesiredAccess {
		if !hasConfirmedOverduePrefix(s) {
			return ReviewRenewalExternalState
		}
		statusOK := r.StatusID == "3"
		if step, ok := s.Steps[RenewalResume]; ok && step.Confirmed && r.StatusID == "2" {
			statusOK = true
		}
		panelOK := !p.Enabled()
		if step, ok := s.Steps[RenewalEnable]; ok && step.Confirmed && s.Plan != nil && s.Plan.Enable && p.Enabled() {
			panelOK = true
		}
		if !statusOK || !panelOK {
			return ReviewRenewalExternalState
		}
		return 0
	}
	return ReviewRenewalAssociation
}

func hasConfirmedOverduePrefix(s RenewalSnapshot) bool {
	if s.OverduePending {
		return false
	}
	pause, disable := false, false
	for name, step := range s.OverdueSteps {
		if step {
			if name == "pause_recurring" {
				pause = true
			}
			if name == "disable_sui" {
				disable = true
			}
		}
	}
	return pause && disable
}

func makeRenewalPlan(s RenewalSnapshot, r invoiceninja.RecurringRecord, p sui.ClientRecord, now time.Time) (RenewalPlan, json.RawMessage, int) {
	target, err := billing.NextBillingDate(s.Proof.PaymentDate)
	if err != nil || target != s.Proof.NextInvoiceDate || target <= now.In(time.Local).Format(time.DateOnly) {
		return RenewalPlan{}, nil, ReviewRenewalTarget
	}
	plan := RenewalPlan{TargetDate: target, BaselineReceipt: invoiceninja.RecurringReceipt{Amount: r.Amount, NextSendDatetime: r.NextSendDatetime}}
	if r.NextSendDate != target {
		plan.Schedule = true
		plan.Pause = r.StatusID == "2"
		plan.Resume = true
	}
	if s.State == "active" && r.StatusID != "2" {
		return RenewalPlan{}, nil, ReviewRenewalExternalState
	}
	if s.State == "overdue" && (r.StatusID != "3" || p.Enabled()) {
		return RenewalPlan{}, nil, ReviewRenewalExternalState
	}
	if s.State == "overdue" {
		plan.Enable = true
		plan.Resume = true
	}
	if s.State == "active" && !p.Enabled() {
		return RenewalPlan{}, nil, ReviewRenewalExternalState
	}
	if r.StatusID == "3" && !plan.Resume {
		return RenewalPlan{}, nil, ReviewRenewalExternalState
	}
	when, e := invoiceninja.ParseRecurringUTC(r.NextSendDatetime)
	if e != nil || !when.After(now) {
		return RenewalPlan{}, nil, ReviewRenewalTarget
	}
	baseline, err := json.Marshal(struct {
		RecurringID      string      `json:"recurring_id"`
		StatusID         string      `json:"status_id"`
		NextSendDate     string      `json:"next_send_date"`
		Amount           json.Number `json:"amount"`
		NextSendDatetime string      `json:"next_send_datetime"`
		SUIID            int64       `json:"sui_id"`
		SUIEnabled       bool        `json:"sui_enabled"`
	}{r.ID, r.StatusID, r.NextSendDate, r.Amount, r.NextSendDatetime, p.ID(), p.Enabled()})
	if err != nil {
		return RenewalPlan{}, nil, ReviewRenewalContract
	}
	return plan, baseline, 0
}

func NextRenewalStep(p RenewalPlan, steps map[RenewalStep]RenewalStepRecord) (next RenewalStep, pending, complete, valid bool) {
	order := [...]RenewalStep{RenewalPause, RenewalSchedule, RenewalEnable, RenewalResume}
	for i, step := range order {
		if !p.Required(step) {
			if _, exists := steps[step]; exists {
				return "", false, false, false
			}
			continue
		}
		r, exists := steps[step]
		if !exists {
			for _, later := range order[i+1:] {
				if _, found := steps[later]; found {
					return "", false, false, false
				}
			}
			return step, false, false, true
		}
		if !r.Confirmed {
			for _, later := range order[i+1:] {
				if _, found := steps[later]; found {
					return "", false, false, false
				}
			}
			return step, true, false, true
		}
	}
	if len(steps) != requiredRenewalSteps(p) {
		return "", false, false, false
	}
	return "", false, true, true
}

func requiredRenewalSteps(p RenewalPlan) int {
	n := 0
	for _, step := range []RenewalStep{RenewalPause, RenewalSchedule, RenewalEnable, RenewalResume} {
		if p.Required(step) {
			n++
		}
	}
	return n
}
func hasPendingRenewalIntent(steps map[RenewalStep]RenewalStepRecord) bool {
	for _, s := range steps {
		if !s.Confirmed {
			return true
		}
	}
	return false
}

func renewalCompletionState(s RenewalSnapshot, plan RenewalPlan, r invoiceninja.RecurringRecord, panel sui.ClientRecord, now time.Time) bool {
	if r.ID != s.RecurringID || r.ClientID != s.CustomerID || r.PrivateNotes != s.RecurringDraft.PrivateNotes || r.StatusID != "2" || r.NextSendDate != plan.TargetDate || r.IsDeleted || r.ArchivedAt != 0 || plan.TargetDate <= now.In(time.Local).Format(time.DateOnly) || !validRenewalRecurringRecord(r, s.RecurringDraft) || !panel.Enabled() || !suiRenewalBindingMatches(panel, s.SUIBinding) {
		return false
	}
	when, err := invoiceninja.ParseRecurringUTC(r.NextSendDatetime)
	return err == nil && when.After(now)
}

func validRenewalRecurringRecord(record invoiceninja.RecurringRecord, draft invoiceninja.RecurringDraft) bool {
	draft.NextSendDate = record.NextSendDate
	before := record
	before.LastSentDate = ""
	return invoiceninja.ValidateRecurringForDraft(before, draft) == nil
}

func suiRenewalBindingMatches(record sui.ClientRecord, binding sui.Binding) bool {
	return binding.ID > 0 && record.ID() == binding.ID && record.Name() == binding.Name && record.CredentialsHash() == binding.CredentialsHash
}

func (w *RenewalWorker) dispatch(ctx context.Context, claim queue.Operation, s RenewalSnapshot, step RenewalStep, record invoiceninja.RecurringRecord, panel sui.ClientRecord) bool {
	var raw []byte
	var err error
	var resultID string
	var receipt json.RawMessage
	granted := false
	authorize := func(authCtx context.Context) error {
		decision, e := w.observe(authCtx, s)
		if e != nil {
			return e
		}
		if decision.Kind != payments.Verified || decision.Proof == nil || decision.Proof.Hash != s.Proof.Hash {
			return errRenewalAuthorization
		}
		invoice, e := w.invoices.GetInvoice(authCtx, s.InvoiceID)
		if e != nil {
			return e
		}
		if !renewalInvoiceMatches(invoice, s) {
			return errRenewalAuthorization
		}
		db, cancel := context.WithTimeout(authCtx, renewalDBTimeout)
		defer cancel()
		ok, e := w.store.AuthorizeRenewalWrite(db, claim, w.botID, s, step, raw, w.now().In(time.Local).Format(time.DateOnly), enforcementEffectiveIDs(w.cfg.AllowedTelegramIDs, w.cfg.ExcludedTelegramIDs))
		if e != nil {
			return e
		}
		if !ok {
			return ErrRenewalIntentExists
		}
		granted = true
		return nil
	}
	switch step {
	case RenewalPause:
		draft, e := invoiceninja.PreparePause(record, s.RecurringDraft)
		if e != nil {
			w.review(claim, s, ReviewRenewalContract)
			return false
		}
		raw, err = json.Marshal(draft)
		if err != nil {
			w.review(claim, s, ReviewRenewalContract)
			return false
		}
		got, e := w.recurring.PauseAuthorized(ctx, draft, authorize)
		err = e
		resultID = got.ID
	case RenewalSchedule:
		today := w.now().In(time.Local).Format(time.DateOnly)
		draft, e := invoiceninja.PrepareSchedule(record, s.RecurringDraft, s.Plan.TargetDate, today)
		if e != nil {
			w.review(claim, s, ReviewRenewalTarget)
			return false
		}
		raw, err = json.Marshal(draft)
		if err != nil {
			w.review(claim, s, ReviewRenewalContract)
			return false
		}
		got, e := w.recurring.RescheduleAuthorized(ctx, draft, authorize)
		err = e
		resultID = got.ID
		if e == nil {
			receipt, err = json.Marshal(invoiceninja.RecurringReceipt{Amount: got.Amount, NextSendDatetime: got.NextSendDatetime})
		}
	case RenewalEnable:
		draft, e := sui.PrepareEnable(panel, s.SUIBinding)
		if e != nil {
			w.review(claim, s, ReviewRenewalExternalState)
			return false
		}
		raw, err = json.Marshal(draft)
		if err != nil {
			w.review(claim, s, ReviewRenewalContract)
			return false
		}
		got, e := w.panel.EnableAuthorized(ctx, draft, authorize)
		err = e
		resultID = strconv.FormatInt(got.ID(), 10)
	case RenewalResume:
		rec := s.Plan.BaselineReceipt
		if s.Plan.Schedule {
			var stepRecord RenewalStepRecord
			stepRecord, ok := s.Steps[RenewalSchedule]
			if !ok || !stepRecord.Confirmed || len(stepRecord.Receipt) == 0 {
				w.review(claim, s, ReviewRenewalSequence)
				return false
			}
			if json.Unmarshal(stepRecord.Receipt, &rec) != nil {
				w.review(claim, s, ReviewRenewalSequence)
				return false
			}
		}
		draft, e := invoiceninja.PrepareResume(record, s.RecurringDraft, rec, w.now().In(time.Local).Format(time.DateOnly))
		if e != nil {
			w.review(claim, s, ReviewRenewalTarget)
			return false
		}
		raw, err = json.Marshal(draft)
		if err != nil {
			w.review(claim, s, ReviewRenewalContract)
			return false
		}
		got, e := w.recurring.ResumeAuthorized(ctx, draft, authorize)
		err = e
		resultID = got.ID
	default:
		w.review(claim, s, ReviewRenewalSequence)
		return false
	}
	if err != nil {
		w.writeFailure(claim, s, granted, err)
		return false
	}
	if resultID == "" || step == RenewalSchedule && len(receipt) == 0 {
		w.holdCurrent(claim, ReviewRenewalUnknownWrite)
		return false
	}
	db, cancel := context.WithTimeout(context.Background(), renewalDBTimeout)
	err = w.store.ConfirmRenewalStep(db, claim, step, resultID, receipt)
	cancel()
	if err != nil {
		if !errors.Is(err, queue.ErrLeaseLost) {
			w.holdCurrent(claim, ReviewRenewalUnknownWrite)
		}
		return false
	}
	return true
}

func (w *RenewalWorker) writeFailure(claim queue.Operation, s RenewalSnapshot, granted bool, cause error) {
	if granted || errors.Is(cause, ErrRenewalIntentExists) || errors.Is(cause, invoiceninja.ErrRecurringUnknown) || errors.Is(cause, sui.ErrManagedUnknown) {
		w.holdCurrent(claim, ReviewRenewalUnknownWrite)
		return
	}
	if errors.Is(cause, errRenewalAuthorization) {
		w.review(claim, s, ReviewRenewalProof)
		return
	}
	if errors.Is(cause, ErrRenewalTargetExpired) {
		w.review(claim, s, ReviewRenewalTarget)
		return
	}
	if errors.Is(cause, invoiceninja.ErrRecurringReceipt) || errors.Is(cause, invoiceninja.ErrRecurringExpired) || errors.Is(cause, sui.ErrManagedConflict) || errors.Is(cause, sui.ErrManagedState) || isProviderContractError(cause) {
		w.review(claim, s, ReviewRenewalContract)
		return
	}
	if errors.Is(cause, ErrRenewalGuardBusy) {
		w.retry(claim, cause)
		return
	}
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		w.retry(claim, cause)
		return
	}
	w.retry(claim, cause)
}

func (w *RenewalWorker) retry(claim queue.Operation, cause error) {
	code := 9000
	var remote *integrationhttp.HTTPError
	if errors.As(cause, &remote) {
		code = remote.StatusCode
	}
	delay := time.Second
	for n := claim.Attempt; n > 1 && delay < 5*time.Minute; n-- {
		delay = min(delay*2, 5*time.Minute)
	}
	if code == 429 {
		delay = max(delay, time.Minute)
	}
	ctx, cancel := context.WithTimeout(context.Background(), renewalDBTimeout)
	defer cancel()
	if err := w.store.RetryOperation(ctx, claim.Lease, delay, code); err != nil && !errors.Is(err, queue.ErrLeaseLost) {
		w.logger.Warn("paid renewal retry scheduling failed")
	}
	w.logger.Warn("paid renewal deferred", "error_code", code)
}

func (w *RenewalWorker) review(claim queue.Operation, s RenewalSnapshot, code int) {
	ctx, cancel := context.WithTimeout(context.Background(), renewalDBTimeout)
	defer cancel()
	if err := w.store.ReviewRenewal(ctx, claim, w.botID, s, code, w.reviewNotices(s, code)); err != nil && !errors.Is(err, queue.ErrLeaseLost) {
		w.logger.Warn("paid renewal review persistence failed", "review_code", code)
	}
}

func (w *RenewalWorker) holdCurrent(claim queue.Operation, code int) {
	for n := 0; n < 3; n++ {
		ctx, cancel := context.WithTimeout(context.Background(), renewalDBTimeout)
		s, err := w.store.LoadRenewalSnapshot(ctx, claim, w.botID)
		if err == nil {
			err = w.store.ReviewRenewal(ctx, claim, w.botID, s, code, w.reviewNotices(s, code))
		}
		cancel()
		if err == nil || errors.Is(err, queue.ErrLeaseLost) {
			return
		}
		if !errors.Is(err, queue.ErrConflict) {
			w.logger.Warn("paid renewal review persistence deferred", "review_code", code)
			return
		}
	}
	w.logger.Warn("paid renewal review persistence deferred", "review_code", code)
}

func (w *RenewalWorker) reviewNotices(s RenewalSnapshot, code int) []queue.Draft {
	digest := sha256.Sum256([]byte(s.InvoiceID))
	prefix := "renewal:" + hex.EncodeToString(digest[:]) + ":review"
	body := fmt.Sprintf("Продление требует ручной проверки. Telegram ID %d, локальный клиент #%d, счёт %s. Автоматическое восстановление остановлено; проверьте состояние вручную. Код: %d.", s.TelegramID, s.ClientID, s.InvoiceID, code)
	out := make([]queue.Draft, 0, len(w.admins))
	for _, id := range w.admins {
		out = append(out, queue.Draft{BotID: w.botID, ChatID: id, Key: prefix + ":admin:" + strconv.FormatInt(id, 10), Text: body})
	}
	return out
}

func (w *RenewalWorker) successNotices(s RenewalSnapshot, plan RenewalPlan) []queue.Draft {
	digest := sha256.Sum256([]byte(s.InvoiceID))
	prefix := "renewal:" + hex.EncodeToString(digest[:])
	text := fmt.Sprintf("Платёж по счёту №%s подтверждён, срок обслуживания учётной записи продлён до %s. Настройки для подключения не пересылались, а само подключение не проверялось.", s.Proof.InvoiceNumber, plan.TargetDate)
	out := []queue.Draft{{BotID: w.botID, ChatID: s.ChatID, Key: prefix + ":client", Text: text}}
	for _, id := range w.admins {
		out = append(out, queue.Draft{BotID: w.botID, ChatID: id, Key: prefix + ":admin:" + strconv.FormatInt(id, 10), Text: fmt.Sprintf("Продление завершено: Telegram ID %d, счёт №%s, сумма %s %s, дата платежа %s, новый срок %s. Проверка подключения не выполнялась.", s.TelegramID, s.Proof.InvoiceNumber, s.Proof.Amount, s.Proof.CurrencyID, s.Proof.PaymentDate, plan.TargetDate)})
	}
	return out
}
