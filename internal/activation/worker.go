package activation

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/managedscope"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

type ProductReader interface {
	GetProduct(context.Context, string) (invoiceninja.Product, error)
}

type PanelAPI interface {
	GetRecord(context.Context, int64) (sui.ClientRecord, error)
	CreateDisabledAuthorized(context.Context, sui.CreateDraft, func(context.Context) error) (sui.ClientRecord, error)
	EnableAuthorized(context.Context, sui.EnableDraft, func(context.Context) error) (sui.ClientRecord, error)
}

type RecurringAPI interface {
	Get(context.Context, string) (invoiceninja.RecurringRecord, error)
	Find(context.Context, string) ([]invoiceninja.RecurringRecord, error)
	CreateDraftAuthorized(context.Context, invoiceninja.RecurringDraft, func(context.Context) error) (invoiceninja.RecurringRecord, error)
	StartFirstBoundAuthorized(context.Context, string, invoiceninja.RecurringDraft, invoiceninja.RecurringReceipt, func(context.Context) error) (invoiceninja.RecurringRecord, error)
}

type Providers struct {
	Payments  payments.Reader
	Products  ProductReader
	SUI       PanelAPI
	Recurring RecurringAPI
}

type Worker struct {
	store     Store
	providers Providers
	botID     int64
	admins    []int64
	cfg       Config
	logger    *slog.Logger
	now       func() time.Time
}

const (
	attemptTimeout  = time.Minute
	databaseTimeout = 8 * time.Second
	// ClaimActivations owns the two-minute lease; the attempt leaves room for
	// one bounded database finalization even during service shutdown.
)

type reviewError struct{ code int }

func (e reviewError) Error() string { return "activation requires manual review" }
func (e reviewError) Unwrap() error { return ErrReviewRequired }
func review(code int) error         { return reviewError{code: code} }

func New(store Store, providers Providers, botID int64, admins []int64, cfg Config, logger *slog.Logger) (*Worker, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, errors.New("activation worker is disabled")
	}
	if store == nil || providers.Payments == nil || providers.Products == nil || providers.SUI == nil || providers.Recurring == nil || botID <= 0 {
		return nil, errors.New("activation requires a store, providers and verified bot identity")
	}
	ids := slices.Clone(admins)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) < 1 || len(ids) > 99 || ids[0] <= 0 {
		return nil, errors.New("activation requires 1-99 positive administrator IDs")
	}
	cfg.InboundIDs = slices.Clone(cfg.InboundIDs)
	cfg.AllowedTelegramIDs = slices.Clone(cfg.AllowedTelegramIDs)
	cfg.ExcludedTelegramIDs = slices.Clone(cfg.ExcludedTelegramIDs)
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{store: store, providers: providers, botID: botID, admins: ids, cfg: cfg, logger: logger, now: time.Now}, nil
}

func (w *Worker) Run(service context.Context) error {
	ctx, cancel := context.WithCancel(service)
	defer cancel()
	for ctx.Err() == nil {
		dbCtx, dbCancel := context.WithTimeout(ctx, databaseTimeout)
		allowed, err := managedscope.Load(dbCtx, w.store, w.botID, w.cfg.ManagedClients, w.cfg.AllowedTelegramIDs, w.cfg.ExcludedTelegramIDs)
		if err == nil {
			_, err = w.store.SeedActivations(dbCtx, w.botID, allowed, w.cfg.PolicyHash())
		}
		dbCancel()
		if err != nil && ctx.Err() == nil {
			w.logger.Warn("activation seed deferred")
		}
		if ctx.Err() != nil {
			break
		}
		dbCtx, dbCancel = context.WithTimeout(ctx, databaseTimeout)
		work, err := w.store.ClaimActivations(dbCtx, w.botID, w.cfg.Workers)
		dbCancel()
		if err != nil {
			if ctx.Err() == nil {
				w.logger.Warn("activation claim deferred")
			}
		} else {
			var tasks sync.WaitGroup
			for _, op := range work {
				if ctx.Err() != nil {
					break
				}
				tasks.Add(1)
				go func(op queue.Operation) { defer tasks.Done(); w.handle(ctx, op) }(op)
			}
			// Never leave a permit holder running after Run returns.
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

// One lazy shutdown budget belongs to the entire handle, not each database
// operation. A failed/timed-out Confirm must not buy another eight seconds.
type activationAttempt struct {
	w           *Worker
	op          queue.Operation
	finalCtx    context.Context
	finalCancel context.CancelFunc
}

func (a *activationAttempt) finalization() context.Context {
	if a.finalCtx == nil {
		a.finalCtx, a.finalCancel = context.WithTimeout(context.Background(), databaseTimeout)
	}
	return a.finalCtx
}
func (a *activationAttempt) close() {
	if a.finalCancel != nil {
		a.finalCancel()
	}
}

func (w *Worker) handle(service context.Context, op queue.Operation) {
	ctx, cancel := context.WithTimeout(service, attemptTimeout)
	defer cancel()
	a := &activationAttempt{w: w, op: op}
	defer a.close()
	dbCtx, dbCancel := context.WithTimeout(ctx, databaseTimeout)
	s, err := w.store.LoadActivation(dbCtx, op.Lease, w.botID)
	dbCancel()
	if err == nil {
		err = a.advance(ctx, s)
	}
	if err == nil || errors.Is(err, queue.ErrLeaseLost) {
		return
	}
	var held reviewError
	if errors.As(err, &held) {
		a.hold(held.code)
		return
	}
	if errors.Is(err, ErrIntentExists) {
		a.hold(ReviewUnknownWrite)
		return
	}
	if errors.Is(err, ErrReviewRequired) || errors.Is(err, queue.ErrConflict) {
		a.hold(ReviewAssociation)
		return
	}
	if service.Err() != nil {
		return
	}
	code := errorCode(err)
	delay := backoff(op.Attempt)
	if errors.Is(err, ErrGuardBusy) {
		delay = 5 * time.Second
	} else if code == 429 {
		delay = max(delay, time.Minute)
	}
	w.logger.Warn("activation attempt deferred", "work_id", op.ID, "error_code", code)
	_ = w.store.RetryOperation(a.finalization(), op.Lease, delay, code)
}

func (a *activationAttempt) hold(code int) {
	if err := a.w.store.HoldActivation(a.finalization(), a.op.Lease, code, reviewMessages(a.w.botID, a.op.ID, a.w.admins)); err != nil {
		a.w.logger.Error("activation review persistence failed", "work_id", a.op.ID, "review_code", code)
	}
}

func (a *activationAttempt) confirm(step Step, result postResult) error {
	var err error
	if step == RecurringCreate || step == RecurringStart {
		err = a.w.store.ConfirmRecurringActivationStep(a.finalization(), a.op.Lease, step, result.id, result.amount, result.datetime)
	} else {
		err = a.w.store.ConfirmActivationStep(a.finalization(), a.op.Lease, step, result.id)
	}
	if err != nil {
		if errors.Is(err, queue.ErrLeaseLost) {
			return err
		}
		return review(ReviewUnknownWrite)
	}
	return a.w.store.RetryOperation(a.finalization(), a.op.Lease, 0, 0)
}

func (w *Worker) validate(s Snapshot, op queue.Operation) (Step, error) {
	// Scope/review fences precede EVERY provider read, not just writes.
	if !managedscope.Allows(s.Account.TelegramID, w.cfg.ManagedClients, w.cfg.AllowedTelegramIDs, w.cfg.ExcludedTelegramIDs) {
		return "", review(ReviewExcluded)
	}
	if s.PaymentReview {
		return "", review(ReviewPayment)
	}
	if s.ReviewCode != 0 {
		code := s.ReviewCode
		if code < ReviewAssociation || code > ReviewPolicy {
			code = ReviewAssociation
		}
		return "", review(code)
	}
	if ValidateSnapshot(s) != nil || s.Account.ClientID != op.ClientID || s.Account.BotID != w.botID || op.Kind != Kind {
		return "", review(ReviewAssociation)
	}
	if s.PolicyHash != w.cfg.PolicyHash() {
		return "", review(ReviewPolicy)
	}
	// An old sender may still be alive. No GET/positive lookup can release its
	// sticky global barrier or authorize a replacement dispatch.
	for _, intent := range s.Steps {
		if intent.ExternalID == "" {
			return "", review(ReviewUnknownWrite)
		}
	}
	next, err := NextStep(s)
	if err != nil {
		return "", review(ReviewContract)
	}
	for _, step := range OrderedSteps() {
		intent, ok := s.Steps[step]
		if !ok {
			continue
		}
		if err := w.validateIntent(s, step, intent.Payload); err != nil {
			return "", err
		}
		if step == SUIEnable && intent.ExternalID != s.Steps[SUICreate].ExternalID || step == RecurringStart && intent.ExternalID != s.Steps[RecurringCreate].ExternalID {
			return "", review(ReviewAssociation)
		}
		if step == RecurringStart && !sameRecurringFacts(s.Steps[RecurringCreate], json.Number(intent.RecurringAmount), intent.RecurringDatetime) {
			return "", review(ReviewRecurring)
		}
	}
	if err := w.period(s); err != nil {
		return "", err
	}
	if next != "" && s.WriterBusy {
		return "", ErrGuardBusy
	}
	return next, nil
}

func (w *Worker) validateIntent(s Snapshot, step Step, raw []byte) error {
	if ValidateIntent(s, step, raw) != nil {
		return review(ReviewContract)
	}
	switch step {
	case SUICreate:
		var draft sui.CreateDraft
		if DecodeIntent(raw, &draft) != nil || !slices.Equal(draft.InboundIDs(), w.cfg.InboundIDs) {
			return review(ReviewPolicy)
		}
	case RecurringCreate:
		var draft invoiceninja.RecurringDraft
		if DecodeIntent(raw, &draft) != nil || draft.UsesInclusiveTaxes != w.cfg.InclusiveTaxes {
			return review(ReviewPolicy)
		}
	}
	return nil
}

func (w *Worker) today() string           { return w.now().In(time.Local).Format(time.DateOnly) }
func (w *Worker) period(s Snapshot) error { return periodAt(s, w.today()) }
func periodAt(s Snapshot, today string) error {
	if s.Proof.PaymentDate > today || s.Proof.NextInvoiceDate <= today {
		return review(ReviewPeriod)
	}
	return nil
}

// A read failure is inconclusive, never an authoritative negative payment.
func (w *Worker) freshPayment(ctx context.Context, s Snapshot) error {
	decision, err := payments.Observe(ctx, w.providers.Payments, s.Account, w.cfg.CurrencyID, w.today())
	if err != nil {
		return err
	}
	if decision.Validate() != nil || decision.Kind != payments.Verified || decision.Proof.Hash != s.Proof.Hash {
		return review(ReviewPayment)
	}
	return w.period(s)
}

type priorRecords struct {
	panel          sui.ClientRecord
	panelDraft     sui.CreateDraft
	recurring      invoiceninja.RecurringRecord
	recurringDraft invoiceninja.RecurringDraft
}

func (w *Worker) prior(ctx context.Context, s Snapshot) (priorRecords, error) {
	var records priorRecords
	if intent, ok := s.Steps[SUICreate]; ok {
		if DecodeIntent(intent.Payload, &records.panelDraft) != nil {
			return records, review(ReviewContract)
		}
		binding, err := SUIBinding(s)
		if err != nil {
			return records, review(ReviewSUI)
		}
		records.panel, err = w.providers.SUI.GetRecord(ctx, binding.ID)
		if err != nil {
			return records, err
		}
		if records.panel.ID() != binding.ID || sui.ValidateRecordForDraft(records.panel, records.panelDraft, s.Steps[SUIEnable].ExternalID != "") != nil {
			return records, review(ReviewSUI)
		}
	}
	if intent, ok := s.Steps[RecurringCreate]; ok {
		if DecodeIntent(intent.Payload, &records.recurringDraft) != nil {
			return records, review(ReviewContract)
		}
		var err error
		records.recurring, err = w.providers.Recurring.Get(ctx, intent.ExternalID)
		if err != nil {
			return records, err
		}
		status := "1"
		if s.Steps[RecurringStart].ExternalID != "" {
			status = "2"
		}
		if err := w.validateRecurring(records.recurring, records.recurringDraft, intent.ExternalID, status); err != nil {
			return records, err
		}
		if !sameRecurringFacts(intent, records.recurring.Amount, records.recurring.NextSendDatetime) {
			return records, review(ReviewRecurring)
		}
	}
	return records, nil
}

func (w *Worker) validateRecurring(record invoiceninja.RecurringRecord, draft invoiceninja.RecurringDraft, id, status string) error {
	if !ValidID(record.ID) || id != "" && record.ID != id || record.StatusID != status || record.IsDeleted || record.ArchivedAt != 0 || invoiceninja.ValidateRecurringForDraft(record, draft) != nil {
		return review(ReviewRecurring)
	}
	return w.futureRecurring(record)
}
func (w *Worker) futureRecurring(record invoiceninja.RecurringRecord) error {
	now := w.now()
	scheduled, err := time.Parse(time.RFC3339Nano, record.NextSendDatetime)
	if err != nil {
		scheduled, err = time.ParseInLocation("2006-01-02 15:04:05", record.NextSendDatetime, time.UTC)
	}
	if err != nil || !scheduled.After(now) || record.NextSendDate <= now.In(time.Local).Format(time.DateOnly) {
		return review(ReviewPeriod)
	}
	return nil
}

func (a *activationAttempt) advance(ctx context.Context, s Snapshot) error {
	w := a.w
	if err := ctx.Err(); err != nil {
		return err
	}
	step, err := w.validate(s, a.op)
	if err != nil {
		return err
	}
	prior, err := w.prior(ctx, s)
	if err != nil {
		return err
	}
	if step == "" {
		// Fresh authorization follows all external ownership/state reads.
		if err := w.freshPayment(ctx, s); err != nil {
			return err
		}
		if err := w.futureRecurring(prior.recurring); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		today := w.today()
		if err := periodAt(s, today); err != nil {
			return err
		}
		return w.store.FinishActivation(a.finalization(), a.op.Lease, s, today, completionMessages(s, a.op.ID, w.admins))
	}
	payload, post, err := a.candidate(ctx, s, step, prior)
	if err != nil {
		return err
	}
	permit := &writePermission{attempt: a, snapshot: s, step: step, payload: payload, prior: prior}
	result, err := post(ctx, permit.authorize)
	if permit.granted {
		// Includes cancellations that are known to have happened before send:
		// permission is irrevocable once durable, and never automatically rearmed.
		if err != nil || result.id == "" {
			return review(ReviewUnknownWrite)
		}
		return a.confirm(step, result)
	}
	if permit.err != nil {
		return permit.err
	}
	if err == nil {
		return review(ReviewUnknownWrite)
	}
	return preflightError(err)
}

type postResult struct {
	id       string
	amount   json.Number
	datetime string
}

type postStep func(context.Context, func(context.Context) error) (postResult, error)

func (a *activationAttempt) candidate(ctx context.Context, s Snapshot, step Step, prior priorRecords) (any, postStep, error) {
	w := a.w
	switch step {
	case SUICreate:
		draft, err := sui.NewDisabledDraft(w.cfg.InboundIDs)
		if err != nil {
			return nil, nil, err
		}
		return draft, func(ctx context.Context, authorize func(context.Context) error) (postResult, error) {
			record, err := w.providers.SUI.CreateDisabledAuthorized(ctx, draft, authorize)
			if err != nil {
				return postResult{}, err
			}
			if sui.ValidateRecordForDraft(record, draft, false) != nil {
				return postResult{}, review(ReviewUnknownWrite)
			}
			return postResult{id: strconv.FormatInt(record.ID(), 10)}, nil
		}, nil
	case RecurringCreate:
		matches, err := w.providers.Recurring.Find(ctx, s.Account.CustomerID)
		if err != nil {
			return nil, nil, err
		}
		// Even archived/deleted/foreign history excludes first activation.
		if len(matches) != 0 {
			return nil, nil, review(ReviewRecurring)
		}
		product, err := w.providers.Products.GetProduct(ctx, w.cfg.ProductID)
		if err != nil {
			return nil, nil, err
		}
		if product.ID != w.cfg.ProductID || !product.Active() {
			return nil, nil, review(ReviewContract)
		}
		draft, err := invoiceninja.BuildRecurringDraft(s.Account.CustomerID, s.Proof.PaymentDate, w.today(), product, w.cfg.InclusiveTaxes, Marker(s.Token))
		if err != nil {
			if errors.Is(err, invoiceninja.ErrRecurringExpired) {
				return nil, nil, review(ReviewPeriod)
			}
			return nil, nil, review(ReviewContract)
		}
		return draft, func(ctx context.Context, authorize func(context.Context) error) (postResult, error) {
			record, err := w.providers.Recurring.CreateDraftAuthorized(ctx, draft, authorize)
			if err != nil {
				return postResult{}, err
			}
			if w.validateRecurring(record, draft, "", "1") != nil {
				return postResult{}, review(ReviewUnknownWrite)
			}
			return postResult{id: record.ID, amount: record.Amount, datetime: record.NextSendDatetime}, nil
		}, nil
	case SUIEnable:
		binding, err := SUIBinding(s)
		if err != nil {
			return nil, nil, review(ReviewSUI)
		}
		draft, err := sui.PrepareEnable(prior.panel, binding)
		if err != nil {
			return nil, nil, review(ReviewSUI)
		}
		return draft, func(ctx context.Context, authorize func(context.Context) error) (postResult, error) {
			record, err := w.providers.SUI.EnableAuthorized(ctx, draft, authorize)
			if err != nil {
				return postResult{}, err
			}
			if record.ID() != binding.ID || sui.ValidateRecordForDraft(record, prior.panelDraft, true) != nil {
				return postResult{}, review(ReviewUnknownWrite)
			}
			return postResult{id: strconv.FormatInt(record.ID(), 10)}, nil
		}, nil
	case RecurringStart:
		original := s.Steps[RecurringCreate]
		id := original.ExternalID
		receipt := invoiceninja.RecurringReceipt{Amount: json.Number(original.RecurringAmount), NextSendDatetime: original.RecurringDatetime}
		request := StartRequest{IDs: []string{id}, Action: "start"}
		return request, func(ctx context.Context, authorize func(context.Context) error) (postResult, error) {
			record, err := w.providers.Recurring.StartFirstBoundAuthorized(ctx, id, prior.recurringDraft, receipt, authorize)
			if err != nil {
				return postResult{}, err
			}
			if w.validateRecurring(record, prior.recurringDraft, id, "2") != nil || !sameRecurringFacts(s.Steps[RecurringCreate], record.Amount, record.NextSendDatetime) {
				return postResult{}, review(ReviewUnknownWrite)
			}
			return postResult{id: record.ID, amount: record.Amount, datetime: record.NextSendDatetime}, nil
		}, nil
	}
	return nil, nil, review(ReviewContract)
}

// This capability is given ONLY to the guarded adapters. They invoke it after
// their read-only preflight/version checks, immediately before the sole POST.
type writePermission struct {
	attempt         *activationAttempt
	snapshot        Snapshot
	step            Step
	payload         any
	prior           priorRecords
	called, granted bool
	err             error
}

func (p *writePermission) authorize(ctx context.Context) (err error) {
	if p.called {
		return ErrIntentExists
	}
	p.called = true
	defer func() { p.err = err }()
	w, s := p.attempt.w, p.snapshot
	if err := w.freshPayment(ctx, s); err != nil {
		return err
	}
	if s.Steps[RecurringCreate].ExternalID != "" {
		if err := w.futureRecurring(p.prior.recurring); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(p.payload)
	if err != nil {
		return review(ReviewContract)
	}
	if err := w.validateIntent(s, p.step, raw); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	today := w.today()
	if err := periodAt(s, today); err != nil {
		return err
	}
	dbCtx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	// Never reload/freshen s here: its preflight epoch fences another process
	// that dispatched and confirmed while this attempt's reads were in flight.
	granted, err := w.store.PrepareActivationWrite(dbCtx, p.attempt.op.Lease, s, p.step, raw, today)
	if err != nil {
		return err
	}
	if !granted {
		return ErrIntentExists
	}
	p.granted = true
	// The database wait may have crossed either deadline. Keep the durable
	// permission sticky even when we can still prevent the adapter's POST.
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := w.period(s); err != nil {
		return err
	}
	if s.Steps[RecurringCreate].ExternalID != "" {
		return w.futureRecurring(p.prior.recurring)
	}
	return nil
}

func preflightError(err error) error {
	switch {
	case errors.Is(err, invoiceninja.ErrRecurringExpired):
		return review(ReviewPeriod)
	case errors.Is(err, invoiceninja.ErrRecurringReceipt):
		return review(ReviewRecurring)
	case errors.Is(err, sui.ErrManagedConflict), errors.Is(err, sui.ErrManagedState):
		return review(ReviewSUI)
	case errors.Is(err, sui.ErrManagedInvalid), errors.Is(err, sui.ErrManagedVersion), errors.Is(err, invoiceninja.ErrWriteVersion):
		return review(ReviewContract)
	default:
		return err
	}
}
func sameRecurringFacts(intent Intent, amount json.Number, datetime string) bool {
	savedAmount, savedDatetime, err := NormalizeRecurringFacts(json.Number(intent.RecurringAmount), intent.RecurringDatetime)
	if err != nil {
		return false
	}
	actualAmount, actualDatetime, err := NormalizeRecurringFacts(amount, datetime)
	return err == nil && savedAmount == actualAmount && savedDatetime == actualDatetime
}
func errorCode(err error) int {
	var remote *integrationhttp.HTTPError
	if errors.As(err, &remote) {
		return remote.StatusCode
	}
	return 0
}
func backoff(attempt int) time.Duration {
	delay := time.Second
	for attempt > 1 && delay < 5*time.Minute {
		delay = min(delay*2, 5*time.Minute)
		attempt--
	}
	return delay
}
