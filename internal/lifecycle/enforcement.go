package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/billing"
	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

var (
	ErrOverduePaid         = errors.New("invoice became paid before overdue action")
	ErrOverdueNotUnpaid    = errors.New("invoice is not authoritatively unpaid")
	ErrOverdueGuardBusy    = errors.New("shared external writer guard is busy")
	ErrOverdueIntentExists = errors.New("overdue write intent already exists; do not replay")
	ErrOverduePaymentLease = errors.New("payment verifier lease is active")
)

const (
	enforcementLease       = 5 * time.Minute
	enforcementAttemptTime = 4 * time.Minute
	enforcementDBTimeout   = 8 * time.Second
)

type EnforcementWorker struct {
	store     EnforcementStore
	invoices  EnforcementInvoiceReader
	recurring EnforcementRecurringWriter
	sui       EnforcementSUIWriter
	botID     int64
	admins    []int64
	cfg       EnforcementConfig
	logger    *slog.Logger
	now       func() time.Time
}

func NewEnforcementWorker(store EnforcementStore, invoices EnforcementInvoiceReader, recurring EnforcementRecurringWriter, panel EnforcementSUIWriter, botID int64, admins []int64, cfg EnforcementConfig, logger *slog.Logger) (*EnforcementWorker, error) {
	if !cfg.Enabled {
		return nil, errors.New("invoice overdue enforcement is disabled")
	}
	if err := cfg.validateSettings(); err != nil {
		return nil, err
	}
	if store == nil || invoices == nil || recurring == nil || panel == nil || botID <= 0 {
		return nil, errors.New("invoice overdue enforcement requires verified store and provider capabilities")
	}
	ids := sortedIDs(admins)
	ids = slices.Compact(ids)
	if len(ids) < 1 || len(ids) > 99 || ids[0] <= 0 {
		return nil, errors.New("invoice overdue enforcement requires 1-99 positive administrator IDs")
	}
	cfg.AllowedTelegramIDs = slices.Clone(cfg.AllowedTelegramIDs)
	cfg.ExcludedTelegramIDs = slices.Clone(cfg.ExcludedTelegramIDs)
	if logger == nil {
		logger = slog.Default()
	}
	return &EnforcementWorker{store: store, invoices: invoices, recurring: recurring, sui: panel, botID: botID, admins: ids, cfg: cfg, logger: logger, now: time.Now}, nil
}

func (w *EnforcementWorker) Run(ctx context.Context) error {
	allowed := enforcementEffectiveIDs(w.cfg.AllowedTelegramIDs, w.cfg.ExcludedTelegramIDs)
	for ctx.Err() == nil {
		db, cancel := context.WithTimeout(ctx, enforcementDBTimeout)
		_, err := w.store.SeedOverdueOperations(db, w.botID, allowed, w.now().In(time.Local).Format(time.DateOnly), w.cfg.MaxAccounts)
		cancel()
		if err != nil && ctx.Err() == nil {
			w.logger.Warn("invoice overdue seed deferred")
		}
		if ctx.Err() != nil {
			return nil
		}
		db, cancel = context.WithTimeout(ctx, enforcementDBTimeout)
		claims, err := w.store.ClaimOperations(db, []string{OverdueOperationKind}, w.cfg.Workers, enforcementLease)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				w.logger.Warn("invoice overdue claim deferred")
			}
		} else {
			var tasks sync.WaitGroup
			for _, claim := range claims {
				if ctx.Err() != nil {
					break
				}
				tasks.Add(1)
				go func(claim queue.Operation) { defer tasks.Done(); w.handle(ctx, claim) }(claim)
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

func (w *EnforcementWorker) handle(service context.Context, claim queue.Operation) {
	if claim.ID <= 0 || claim.Attempt <= 0 || claim.ClientID <= 0 || claim.Kind != OverdueOperationKind {
		return
	}
	ctx, cancel := context.WithTimeout(service, enforcementAttemptTime)
	defer cancel()
	for completed := 0; completed <= 3; completed++ {
		db, stop := context.WithTimeout(ctx, enforcementDBTimeout)
		s, err := w.store.LoadOverdueSnapshot(db, claim, w.botID)
		stop()
		if err != nil {
			if !errors.Is(err, queue.ErrLeaseLost) && service.Err() == nil {
				w.retry(claim, err)
			}
			return
		}
		if s.ClientID != claim.ClientID || s.BotID != w.botID {
			w.hold(claim, s, ReviewOverdueAssociation)
			return
		}
		if s.WriterBusy {
			if s.WriterOperationID == claim.ID && s.WriterStep != "" {
				w.hold(claim, s, ReviewOverdueUnknownWrite)
			} else {
				w.retry(claim, ErrOverdueGuardBusy)
			}
			return
		}
		if _, pending, _, valid := NextOverdueStep(s.Steps); !valid {
			w.hold(claim, s, ReviewOverdueSequence)
			return
		} else if pending {
			w.hold(claim, s, ReviewOverdueUnknownWrite)
			return
		}
		if code := validateEnforcementSnapshot(s, w.botID, w.cfg.AllowedTelegramIDs, w.cfg.ExcludedTelegramIDs, w.now()); code != 0 {
			w.hold(claim, s, code)
			return
		}
		if s.PaymentLocked {
			w.retry(claim, ErrOverduePaymentLease)
			return
		}
		if s.PaymentReview || s.LifecycleReview {
			w.hold(claim, s, ReviewOverduePayment)
			return
		}
		if s.PaymentConfirmed {
			if hasAnyEnforcementStep(s.Steps) {
				w.hold(claim, s, ReviewOverduePayment)
			} else {
				w.finishPaid(claim, s)
			}
			return
		}

		_, err = w.readUnpaid(ctx, s)
		if err != nil {
			if errors.Is(err, ErrOverduePaid) {
				if hasAnyEnforcementStep(s.Steps) {
					w.hold(claim, s, ReviewOverduePayment)
				} else {
					w.finishPaid(claim, s)
				}
				return
			}
			if errors.Is(err, ErrOverdueNotUnpaid) {
				w.hold(claim, s, ReviewOverduePayment)
				return
			}
			if errors.Is(err, queue.ErrConflict) || isProviderContractError(err) {
				w.hold(claim, s, ReviewOverdueInvoice)
				return
			}
			if service.Err() == nil {
				w.retry(claim, err)
			}
			return
		}
		next, _, complete, valid := NextOverdueStep(s.Steps)
		if !valid {
			w.hold(claim, s, ReviewOverdueSequence)
			return
		}
		if complete {
			drafts := overdueSuccessDrafts(w.botID, w.admins, s)
			db, stop := context.WithTimeout(ctx, enforcementDBTimeout)
			err := w.store.FinishOverdue(db, claim, w.botID, s, drafts)
			stop()
			if errors.Is(err, queue.ErrConflict) {
				w.hold(claim, s, ReviewOverduePayment)
			} else if errors.Is(err, ErrOverduePaymentLease) {
				w.retry(claim, err)
			} else if err != nil && !errors.Is(err, queue.ErrLeaseLost) {
				w.logger.Warn("invoice overdue finalization deferred")
			}
			return
		}
		switch next {
		case OverduePauseRecurring:
			if !w.pauseRecurring(ctx, claim, s) {
				return
			}
		case OverdueDisableSUI:
			if !w.disableSUI(ctx, claim, s) {
				return
			}
		case OverdueCloseSessions:
			if !w.closeSessions(ctx, claim, s) {
				return
			}
		default:
			w.hold(claim, s, ReviewOverdueSequence)
			return
		}
	}
	// More than three write steps means the stored action sequence is corrupt.
	db, stop := context.WithTimeout(context.Background(), enforcementDBTimeout)
	if s, err := w.store.LoadOverdueSnapshot(db, claim, w.botID); err == nil {
		w.hold(claim, s, ReviewOverdueSequence)
	}
	stop()
}

// readUnpaid performs the canonical exact-ID GET used both before an action and
// inside every adapter's final authorizer. Partial/unknown states are never
// translated into an unpaid decision.
func (w *EnforcementWorker) readUnpaid(ctx context.Context, s EnforcementSnapshot) (Invoice, error) {
	fresh, err := w.invoices.GetInvoice(ctx, s.InvoiceID)
	if err != nil {
		return Invoice{}, err
	}
	if fresh.ID != s.InvoiceID || fresh.ClientID != s.CustomerID || fresh.RecurringID != s.RecurringID || fresh.PrivateNotes != activation.Marker(s.ActivationToken) {
		return Invoice{}, queue.ErrConflict
	}
	if fresh.StatusID == "4" {
		return Invoice{ID: fresh.ID, Number: fresh.Number, Status: "paid"}, ErrOverduePaid
	}
	if fresh.StatusID != "2" {
		return Invoice{}, ErrOverdueNotUnpaid
	}
	candidate, code := makeCandidate(fresh, time.Local)
	if code != 0 || candidate.Status != "sent" || candidate.PaidToDate != "0" || candidate.Balance != candidate.Amount || candidate.ID != s.Invoice.ID || candidate.Number != s.Invoice.Number || candidate.SentDate != s.Invoice.SentDate || candidate.Amount != s.Invoice.Amount || candidate.Balance != s.Invoice.Balance || candidate.PaidToDate != s.Invoice.PaidToDate {
		return Invoice{}, ErrOverdueNotUnpaid
	}
	if fresh.IsDeleted || fresh.ArchivedAt != 0 {
		return Invoice{}, queue.ErrConflict
	}
	return candidate, nil
}

func validateEnforcementSnapshot(s EnforcementSnapshot, botID int64, allowed, excluded []int64, now time.Time) int {
	if s.BotID != botID || s.ClientID <= 0 || s.TelegramID <= 0 || s.ChatID != s.TelegramID || !s.ManagedProvenance || !slices.Contains(allowed, s.TelegramID) || slices.Contains(excluded, s.TelegramID) || s.CustomerID == "" || s.InvoiceID == "" || s.Invoice.ID != s.InvoiceID || s.CurrentInvoiceID != s.InvoiceID || s.FirstInvoiceID == s.InvoiceID || s.RecurringID == "" || s.SUIID == "" || s.RegistrationToken == "" || s.ActivationToken == "" {
		return ReviewOverdueAssociation
	}
	if !validOverdueInvoice(s.Invoice) || s.Invoice.Status != "sent" || s.Invoice.PaidToDate != "0" || s.Invoice.Balance != s.Invoice.Amount {
		return ReviewOverdueInvoice
	}
	if due, err := billing.IsOverdue(s.Invoice.SentDate, now.In(time.Local)); err != nil || !due {
		return ReviewOverdueInvoice
	}
	if s.RecurringDraft.ClientID != s.CustomerID || s.RecurringDraft.PrivateNotes != activation.Marker(s.ActivationToken) || invoiceninja.ValidateRecurringDraft(s.RecurringDraft) != nil || s.SUIBinding.ID <= 0 || strconv.FormatInt(s.SUIBinding.ID, 10) != s.SUIID || s.SUIBinding.Name == "" {
		return ReviewOverdueAssociation
	}
	if s.State == "active" && s.DesiredAccess {
		if confirmed(s.Steps, OverdueDisableSUI) {
			return ReviewOverdueSequence
		}
		return 0
	}
	if s.State == "overdue" && !s.DesiredAccess && confirmed(s.Steps, OverdueDisableSUI) {
		return 0
	}
	return ReviewOverdueAssociation
}

func confirmed(steps map[EnforcementStep]OverdueStep, step EnforcementStep) bool {
	v, ok := steps[step]
	return ok && v.Confirmed
}

func hasAnyEnforcementStep(steps map[EnforcementStep]OverdueStep) bool { return len(steps) != 0 }

func validOverdueInvoice(invoice Invoice) bool {
	if invoice.ID == "" || invoice.Number == "" || !printable(invoice.Number, 128) || invoice.Status != "sent" || invoice.SentDate == "" {
		return false
	}
	if _, err := time.Parse(time.DateOnly, invoice.SentDate); err != nil {
		return false
	}
	amount, amountValue, err := exactAmount(json.Number(invoice.Amount))
	if err != nil || amount != invoice.Amount || amountValue.Sign() <= 0 {
		return false
	}
	balance, balanceValue, err := exactAmount(json.Number(invoice.Balance))
	if err != nil || balance != invoice.Balance || balance != amount {
		return false
	}
	paid, paidValue, err := exactAmount(json.Number(invoice.PaidToDate))
	return err == nil && paid == invoice.PaidToDate && paidValue.Sign() == 0 && balanceValue.Cmp(amountValue) == 0
}

func (w *EnforcementWorker) pauseRecurring(ctx context.Context, claim queue.Operation, s EnforcementSnapshot) bool {
	record, err := w.recurring.Get(ctx, s.RecurringID)
	if err != nil {
		if isProviderContractError(err) {
			w.hold(claim, s, ReviewOverdueContract)
		} else {
			w.retry(claim, err)
		}
		return false
	}
	draft, err := invoiceninja.PreparePause(record, s.RecurringDraft)
	if err != nil {
		w.hold(claim, s, ReviewOverdueContract)
		return false
	}
	raw, err := json.Marshal(draft)
	if err != nil {
		w.hold(claim, s, ReviewOverdueContract)
		return false
	}
	granted := false
	authorize := func(ctx context.Context) error {
		return w.authorize(ctx, claim, s, OverduePauseRecurring, raw, &granted)
	}
	result, err := w.recurring.PauseAuthorized(ctx, draft, authorize)
	if err != nil {
		w.finishWriteAttempt(claim, s, granted, err)
		return false
	}
	if err := w.confirm(claim, OverduePauseRecurring, result.ID); err != nil {
		if !errors.Is(err, queue.ErrLeaseLost) {
			w.holdCurrent(claim, ReviewOverdueUnknownWrite)
		}
		return false
	}
	return true
}

func (w *EnforcementWorker) disableSUI(ctx context.Context, claim queue.Operation, s EnforcementSnapshot) bool {
	record, err := w.sui.GetRecord(ctx, s.SUIBinding.ID)
	if err != nil {
		if isProviderContractError(err) {
			w.hold(claim, s, ReviewOverdueContract)
		} else {
			w.retry(claim, err)
		}
		return false
	}
	draft, err := sui.PrepareDisable(record, s.SUIBinding)
	if err != nil {
		w.hold(claim, s, ReviewOverdueContract)
		return false
	}
	raw, err := json.Marshal(draft)
	if err != nil {
		w.hold(claim, s, ReviewOverdueContract)
		return false
	}
	granted := false
	authorize := func(ctx context.Context) error { return w.authorize(ctx, claim, s, OverdueDisableSUI, raw, &granted) }
	result, err := w.sui.DisableAuthorized(ctx, draft, authorize)
	if err != nil {
		w.finishWriteAttempt(claim, s, granted, err)
		return false
	}
	if err := w.confirm(claim, OverdueDisableSUI, strconv.FormatInt(result.ID(), 10)); err != nil {
		if !errors.Is(err, queue.ErrLeaseLost) {
			w.holdCurrent(claim, ReviewOverdueUnknownWrite)
		}
		return false
	}
	return true
}

func (w *EnforcementWorker) closeSessions(ctx context.Context, claim queue.Operation, s EnforcementSnapshot) bool {
	record, err := w.sui.GetRecord(ctx, s.SUIBinding.ID)
	if err != nil {
		if isProviderContractError(err) {
			w.hold(claim, s, ReviewOverdueContract)
		} else {
			w.retry(claim, err)
		}
		return false
	}
	draft, err := sui.PrepareCloseSessions(record, s.SUIBinding)
	if err != nil {
		w.hold(claim, s, ReviewOverdueContract)
		return false
	}
	raw, err := json.Marshal(draft)
	if err != nil {
		w.hold(claim, s, ReviewOverdueContract)
		return false
	}
	granted := false
	authorize := func(ctx context.Context) error {
		return w.authorize(ctx, claim, s, OverdueCloseSessions, raw, &granted)
	}
	err = w.sui.CloseSessionsAuthorized(ctx, draft, authorize)
	if err != nil {
		w.finishWriteAttempt(claim, s, granted, err)
		return false
	}
	if err := w.confirm(claim, OverdueCloseSessions, strconv.FormatInt(s.SUIBinding.ID, 10)); err != nil {
		if !errors.Is(err, queue.ErrLeaseLost) {
			w.holdCurrent(claim, ReviewOverdueUnknownWrite)
		}
		return false
	}
	return true
}

func (w *EnforcementWorker) authorize(ctx context.Context, claim queue.Operation, s EnforcementSnapshot, step EnforcementStep, raw []byte, granted *bool) error {
	if _, err := w.readUnpaid(ctx, s); err != nil {
		return err
	}
	db, cancel := context.WithTimeout(ctx, enforcementDBTimeout)
	defer cancel()
	ok, err := w.store.AuthorizeOverdueWrite(db, claim, w.botID, s, step, raw, w.now().In(time.Local).Format(time.DateOnly), enforcementEffectiveIDs(w.cfg.AllowedTelegramIDs, w.cfg.ExcludedTelegramIDs))
	if err != nil {
		return err
	}
	if !ok {
		return ErrOverdueIntentExists
	}
	*granted = true
	return nil
}

func (w *EnforcementWorker) confirm(claim queue.Operation, step EnforcementStep, externalID string) error {
	db, cancel := context.WithTimeout(context.Background(), enforcementDBTimeout)
	defer cancel()
	if err := w.store.ConfirmOverdueStep(db, claim, step, externalID); err != nil {
		if !errors.Is(err, queue.ErrLeaseLost) {
			w.logger.Warn("invoice overdue write confirmation deferred", "step", string(step))
		}
		return err
	}
	return nil
}

func (w *EnforcementWorker) finishWriteAttempt(claim queue.Operation, s EnforcementSnapshot, granted bool, cause error) {
	if granted || errors.Is(cause, invoiceninja.ErrRecurringUnknown) || errors.Is(cause, sui.ErrManagedUnknown) || errors.Is(cause, ErrOverdueIntentExists) {
		w.holdCurrent(claim, ReviewOverdueUnknownWrite)
		return
	}
	if errors.Is(cause, ErrOverduePaid) {
		if hasAnyEnforcementStep(s.Steps) {
			w.hold(claim, s, ReviewOverduePayment)
		} else {
			w.finishPaid(claim, s)
		}
		return
	}
	if errors.Is(cause, ErrOverdueNotUnpaid) {
		w.hold(claim, s, ReviewOverduePayment)
		return
	}
	if errors.Is(cause, ErrOverdueGuardBusy) {
		w.retry(claim, cause)
		return
	}
	if errors.Is(cause, invoiceninja.ErrRecurringReceipt) || errors.Is(cause, sui.ErrManagedConflict) || errors.Is(cause, sui.ErrManagedState) || errors.Is(cause, invoiceninja.ErrRecurringExpired) || isProviderContractError(cause) {
		w.hold(claim, s, ReviewOverdueContract)
		return
	}
	w.retry(claim, cause)
}

func isProviderContractError(err error) bool {
	if errors.Is(err, invoiceninja.ErrSettlementContract) || errors.Is(err, invoiceninja.ErrSettlementVersion) || errors.Is(err, invoiceninja.ErrWriteVersion) || errors.Is(err, integrationhttp.ErrInvalidResponse) || errors.Is(err, sui.ErrManagedResponse) || errors.Is(err, sui.ErrManagedVersion) || errors.Is(err, sui.ErrManagedConflict) || errors.Is(err, sui.ErrManagedState) {
		return true
	}
	var remote *integrationhttp.HTTPError
	return errors.As(err, &remote) && remote.StatusCode >= 400 && remote.StatusCode < 500 && remote.StatusCode != 429
}

func (w *EnforcementWorker) retry(claim queue.Operation, cause error) {
	code := 9000
	var remote *integrationhttp.HTTPError
	if errors.As(cause, &remote) {
		code = remote.StatusCode
	}
	delay := time.Second
	for attempt := claim.Attempt; attempt > 1 && delay < 5*time.Minute; attempt-- {
		delay = min(delay*2, 5*time.Minute)
	}
	if code == 429 {
		delay = max(delay, time.Minute)
	}
	ctx, cancel := context.WithTimeout(context.Background(), enforcementDBTimeout)
	defer cancel()
	if err := w.store.RetryOperation(ctx, claim.Lease, delay, code); err != nil && !errors.Is(err, queue.ErrLeaseLost) {
		w.logger.Warn("invoice overdue retry scheduling failed")
	}
	w.logger.Warn("invoice overdue action deferred", "error_code", code)
}

func (w *EnforcementWorker) hold(claim queue.Operation, s EnforcementSnapshot, code int) {
	ctx, cancel := context.WithTimeout(context.Background(), enforcementDBTimeout)
	defer cancel()
	if err := w.store.HoldOverdue(ctx, claim, w.botID, s, code, overdueReviewDrafts(w.botID, w.admins, s, code)); err != nil && !errors.Is(err, queue.ErrLeaseLost) {
		w.logger.Warn("invoice overdue review persistence failed", "error_code", code)
	}
}

// holdCurrent refreshes durable intent evidence under the same lease before
// recording review. Retries are DB-only; no provider call or guard release occurs.
func (w *EnforcementWorker) holdCurrent(claim queue.Operation, code int) {
	for attempt := 0; attempt < 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), enforcementDBTimeout)
		snapshot, err := w.store.LoadOverdueSnapshot(ctx, claim, w.botID)
		if err == nil {
			err = w.store.HoldOverdue(ctx, claim, w.botID, snapshot, code, overdueReviewDrafts(w.botID, w.admins, snapshot, code))
		}
		cancel()
		if err == nil || errors.Is(err, queue.ErrLeaseLost) {
			return
		}
		if !errors.Is(err, queue.ErrConflict) {
			w.logger.Warn("invoice overdue review persistence failed", "error_code", code)
			return
		}
	}
	w.logger.Warn("invoice overdue review persistence deferred", "error_code", code)
}

func (w *EnforcementWorker) finishPaid(claim queue.Operation, s EnforcementSnapshot) {
	ctx, cancel := context.WithTimeout(context.Background(), enforcementDBTimeout)
	defer cancel()
	if err := w.store.FinishPaidOverdue(ctx, claim, w.botID, s); err != nil && !errors.Is(err, queue.ErrLeaseLost) {
		w.logger.Warn("paid invoice overdue action completion deferred")
	}
}

func overdueSuccessDrafts(botID int64, admins []int64, s EnforcementSnapshot) []queue.Draft {
	prefix := overdueMessagePrefix(s.InvoiceID)
	client := fmt.Sprintf("Счёт №%s не был отмечен как оплаченный после окончания дня отправки %s. Расписание продления приостановлено, доступ отключён. Система получила запрос закрыть активные сессии; это не подтверждает, что все соединения уже завершены. Если вы оплатили счёт, свяжитесь с администратором для проверки и восстановления доступа.", s.Invoice.Number, s.Invoice.SentDate)
	out := []queue.Draft{{BotID: botID, ChatID: s.ChatID, Key: prefix + ":overdue:client", Text: client}}
	for _, id := range sortedIDs(admins) {
		out = append(out, queue.Draft{BotID: botID, ChatID: id, Key: prefix + ":overdue:admin:" + strconv.FormatInt(id, 10), Text: fmt.Sprintf("Просрочка обработана: Telegram ID %d, счёт №%s. Recurring приостановлен, S-UI отключён; closeSessions принял запрос, фактическое завершение всех соединений не подтверждено.", s.TelegramID, s.Invoice.Number)})
	}
	return out
}

func overdueReviewDrafts(botID int64, admins []int64, s EnforcementSnapshot, code int) []queue.Draft {
	prefix := overdueMessagePrefix(s.InvoiceID)
	confirmedSteps := make([]string, 0, 3)
	for _, step := range []EnforcementStep{OverduePauseRecurring, OverdueDisableSUI, OverdueCloseSessions} {
		if confirmed(s.Steps, step) {
			confirmedSteps = append(confirmedSteps, string(step))
		}
	}
	message := fmt.Sprintf("Просроченный счёт требует ручной проверки. Telegram ID %d, счёт №%s, review_code=%d. Подтверждённые шаги: %s. Автоматическое восстановление и повтор неизвестной записи не выполняются.", s.TelegramID, s.Invoice.Number, code, strings.Join(confirmedSteps, ", "))
	out := make([]queue.Draft, 0, len(admins))
	for _, id := range sortedIDs(admins) {
		out = append(out, queue.Draft{BotID: botID, ChatID: id, Key: prefix + ":review:" + strconv.Itoa(code) + ":admin:" + strconv.FormatInt(id, 10), Text: message})
	}
	return out
}

func overdueMessagePrefix(invoiceID string) string {
	digest := sha256.Sum256([]byte(invoiceID))
	return "invoice-overdue:" + hex.EncodeToString(digest[:])
}
