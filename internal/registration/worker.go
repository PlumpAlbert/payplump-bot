package registration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
)

type API interface {
	FindCustomers(context.Context, int64) ([]invoiceninja.Customer, error)
	FindInvoices(context.Context, string) ([]invoiceninja.Invoice, error)
	GetCustomer(context.Context, string) (invoiceninja.Customer, error)
	GetInvoice(context.Context, string) (invoiceninja.Invoice, error)
	GetProduct(context.Context, string) (invoiceninja.Product, error)
	CreateCustomer(context.Context, invoiceninja.CustomerDraft) (invoiceninja.Customer, error)
	CreateInvoice(context.Context, invoiceninja.InvoiceDraft) (invoiceninja.Invoice, error)
	IssueInvoice(context.Context, invoiceninja.IssueDraft) (invoiceninja.Invoice, error)
}

type Worker struct {
	store  Store
	api    API
	botID  int64
	admins []int64
	cfg    Config
	logger *slog.Logger
	now    func() time.Time
}

const (
	leaseDuration   = 2 * time.Minute
	attemptTimeout  = time.Minute
	databaseTimeout = 8 * time.Second
)

type reviewError struct{ code int }

func (e reviewError) Error() string { return "registration requires manual review" }
func (e reviewError) Unwrap() error { return ErrReviewRequired }
func review(code int) error         { return reviewError{code: code} }

func New(store Store, api API, botID int64, admins []int64, cfg Config, logger *slog.Logger) (*Worker, error) {
	if !cfg.Enabled {
		return nil, errors.New("registration worker is disabled")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if store == nil || api == nil || botID <= 0 {
		return nil, errors.New("registration requires a store, API and verified bot identity")
	}
	ids := slices.Clone(admins)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) < 1 || len(ids) > 99 || ids[0] <= 0 {
		return nil, errors.New("registration requires 1-99 positive administrator IDs")
	}
	if logger == nil {
		logger = slog.Default()
	}
	cfg.ExcludedTelegramIDs = slices.Clone(cfg.ExcludedTelegramIDs)
	return &Worker{store: store, api: api, botID: botID, admins: ids, cfg: cfg, logger: logger, now: time.Now}, nil
}

func (w *Worker) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		claimCtx, cancel := context.WithTimeout(ctx, databaseTimeout)
		work, err := w.store.ClaimOperations(claimCtx, []string{"register_client"}, w.cfg.Workers, leaseDuration)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				w.logger.Error("registration claim failed")
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

func (w *Worker) handle(serviceCtx context.Context, op queue.Operation) {
	ctx, cancel := context.WithTimeout(serviceCtx, attemptTimeout)
	defer cancel()
	dbCtx, dbCancel := context.WithTimeout(ctx, databaseTimeout)
	snapshot, err := w.store.LoadRegistration(dbCtx, op.Lease, w.botID)
	dbCancel()
	if err == nil {
		err = w.advance(ctx, op, snapshot)
	}
	if err == nil || errors.Is(err, queue.ErrLeaseLost) {
		return
	}
	var held reviewError
	if errors.As(err, &held) {
		w.hold(op, snapshot, held.code)
		return
	}
	if serviceCtx.Err() != nil {
		return
	}
	if snapshot.Token != "" && (errors.Is(err, queue.ErrConflict) || errors.Is(err, invoiceninja.ErrWriteVersion) || errors.Is(err, invoiceninja.ErrLookupIncomplete)) {
		reason := ReviewContract
		if errors.Is(err, queue.ErrConflict) {
			reason = ReviewLocalAssociation
		}
		w.hold(op, snapshot, reason)
		return
	}
	w.logger.Warn("registration attempt deferred", "work_id", op.ID, "error_code", errorCode(err))
	_ = w.retry(op.Lease, backoff(op.Attempt), errorCode(err))
}

func (w *Worker) advance(ctx context.Context, op queue.Operation, s Snapshot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.BotID != w.botID || s.TelegramID <= 0 || s.ChatID != s.TelegramID || s.SubscriptionState != "awaiting_payment" || s.DesiredAccess || s.ExistingSUIID != "" || s.ExistingRecurringID != "" {
		return review(ReviewLocalAssociation)
	}
	if slices.Contains(w.cfg.ExcludedTelegramIDs, s.TelegramID) {
		return review(ReviewExcludedUser)
	}
	customerStep, customerStarted := s.Steps[CustomerCreate]
	invoiceStep, invoiceStarted := s.Steps[InvoiceCreate]
	if s.ExistingCustomerID != "" && s.ExistingCustomerID != customerStep.ExternalID {
		return review(ReviewLocalAssociation)
	}
	if (s.ExistingCurrentInvoiceID != "" && s.ExistingCurrentInvoiceID != invoiceStep.ExternalID) || (s.ExistingFirstInvoiceID != "" && s.ExistingFirstInvoiceID != invoiceStep.ExternalID) {
		return review(ReviewLocalAssociation)
	}
	var customerDraft invoiceninja.CustomerDraft
	if customerStarted {
		if decodeIntent(customerStep.Payload, &customerDraft) != nil || invoiceninja.ValidateCustomerDraft(customerDraft) != nil || customerDraft.IDNumber != strconv.FormatInt(s.TelegramID, 10) || customerDraft.PrivateNotes != marker(s.Token, CustomerCreate) {
			return review(ReviewContract)
		}
	} else {
		name := strings.TrimSpace(s.DisplayName)
		if name == "" {
			name = "Клиент Telegram"
		}
		customerDraft = invoiceninja.CustomerDraft{Name: name, IDNumber: strconv.FormatInt(s.TelegramID, 10), PrivateNotes: marker(s.Token, CustomerCreate)}
		if invoiceninja.ValidateCustomerDraft(customerDraft) != nil {
			return review(ReviewContract)
		}
	}
	if customerStep.ExternalID == "" {
		matches, err := w.api.FindCustomers(ctx, s.TelegramID)
		if err != nil {
			return err
		}
		if len(matches) > 1 {
			return review(ReviewCustomerConflict)
		}
		if len(matches) == 1 {
			if !customerStarted || !matches[0].Active() || invoiceninja.ValidateCustomerForDraft(matches[0], customerDraft) != nil {
				return review(ReviewExistingCustomer)
			}
			return w.confirmAndRetry(op.Lease, CustomerCreate, matches[0].ID)
		}
		if customerStarted {
			return review(ReviewUnknownWrite)
		}
		granted, err := w.prepare(ctx, op.Lease, CustomerCreate, customerDraft)
		if err != nil {
			return err
		}
		if !granted {
			return w.retry(op.Lease, time.Second, 0)
		}
		created, err := w.api.CreateCustomer(ctx, customerDraft)
		if err != nil {
			return w.unknownCreate(op, CustomerCreate, err)
		}
		if !created.Active() || invoiceninja.ValidateCustomerForDraft(created, customerDraft) != nil {
			return w.unknownCreate(op, CustomerCreate, ErrReviewRequired)
		}
		return w.confirmAndRetry(op.Lease, CustomerCreate, created.ID)
	}
	customer, err := w.api.GetCustomer(ctx, customerStep.ExternalID)
	if err != nil {
		return err
	}
	if !customer.Active() || invoiceninja.ValidateCustomerForDraft(customer, customerDraft) != nil {
		return review(ReviewCustomerConflict)
	}
	var draft invoiceninja.InvoiceDraft
	if invoiceStarted {
		if decodeIntent(invoiceStep.Payload, &draft) != nil || invoiceninja.ValidateInvoiceDraft(draft) != nil || draft.ClientID != customer.ID || draft.PrivateNotes != marker(s.Token, InvoiceCreate) {
			return review(ReviewContract)
		}
	}
	if invoiceStep.ExternalID == "" {
		invoices, err := w.api.FindInvoices(ctx, customer.ID)
		if err != nil {
			return err
		}
		var owned []invoiceninja.Invoice
		for _, invoice := range invoices {
			if invoice.PrivateNotes == marker(s.Token, InvoiceCreate) {
				owned = append(owned, invoice)
			}
		}
		if len(owned) > 1 {
			return review(ReviewInvoiceConflict)
		}
		if len(owned) == 1 {
			if len(invoices) != 1 || !invoiceStarted || owned[0].IsDeleted || owned[0].ArchivedAt != 0 || invoiceninja.ValidateInvoiceForDraft(owned[0], draft) != nil {
				return review(ReviewInvoiceConflict)
			}
			return w.confirmAndRetry(op.Lease, InvoiceCreate, owned[0].ID)
		}
		if len(invoices) > 0 {
			return review(ReviewInvoiceConflict)
		}
		if invoiceStarted {
			return review(ReviewUnknownWrite)
		}
		product, err := w.api.GetProduct(ctx, w.cfg.ProductID)
		if err != nil {
			return err
		}
		if !product.Active() {
			return review(ReviewContract)
		}
		draft = invoiceDraft(customer.ID, product, w.now().In(time.Local).Format(time.DateOnly), marker(s.Token, InvoiceCreate), w.cfg.InclusiveTaxes)
		if invoiceninja.ValidateInvoiceDraft(draft) != nil {
			return review(ReviewContract)
		}
		granted, err := w.prepare(ctx, op.Lease, InvoiceCreate, draft)
		if err != nil {
			return err
		}
		if !granted {
			return w.retry(op.Lease, time.Second, 0)
		}
		created, err := w.api.CreateInvoice(ctx, draft)
		if err != nil {
			return w.unknownCreate(op, InvoiceCreate, err)
		}
		if created.IsDeleted || created.ArchivedAt != 0 || invoiceninja.ValidateInvoiceForDraft(created, draft) != nil {
			return w.unknownCreate(op, InvoiceCreate, ErrReviewRequired)
		}
		return w.confirmAndRetry(op.Lease, InvoiceCreate, created.ID)
	}
	invoice, err := w.api.GetInvoice(ctx, invoiceStep.ExternalID)
	if err != nil {
		return err
	}
	if invoice.IsDeleted || invoice.ArchivedAt != 0 || invoiceninja.ValidateInvoiceForDraft(invoice, draft) != nil {
		return review(ReviewInvoiceConflict)
	}
	issueStep, issueStarted := s.Steps[InvoiceIssue]
	if issueStarted {
		var request invoiceninja.IssueDraft
		if decodeIntent(issueStep.Payload, &request) != nil || request.Action != "mark_sent" || len(request.IDs) != 1 || request.IDs[0] != invoice.ID {
			return review(ReviewContract)
		}
	}
	if issueStarted && issueStep.ExternalID == "" {
		// Upstream marks status before completing its ledger effects. A later
		// status=sent does not establish that an ambiguous mark_sent completed.
		return review(ReviewUnknownWrite)
	}
	if !issueStarted {
		// Do not issue an old draft or alter a frozen request to today's dates.
		// Already confirmed issuance may finish locally after a later restart.
		if draft.Date != w.now().In(time.Local).Format(time.DateOnly) {
			return review(ReviewInvoiceState)
		}
		if invoice.StatusID != "1" || !unissued(invoice) {
			return review(ReviewInvoiceState)
		}
		request := invoiceninja.IssueDraft{Action: "mark_sent", IDs: []string{invoice.ID}}
		granted, err := w.prepare(ctx, op.Lease, InvoiceIssue, request)
		if err != nil {
			return err
		}
		if !granted {
			return w.retry(op.Lease, time.Second, 0)
		}
		issued, err := w.api.IssueInvoice(ctx, request)
		if err != nil {
			w.logger.Warn("registration issue result unknown", "work_id", op.ID, "error_code", errorCode(err))
			return review(ReviewUnknownWrite)
		}
		if issued.ID != invoice.ID || issued.StatusID != "2" || invoiceninja.ValidateInvoiceForDraft(issued, draft) != nil || !unpaid(issued) {
			return review(ReviewInvoiceState)
		}
		return w.confirmAndRetry(op.Lease, InvoiceIssue, issued.ID)
	}
	if issueStep.ExternalID != invoice.ID || invoice.StatusID != "2" || !unpaid(invoice) {
		return review(ReviewInvoiceState)
	}
	messages, err := invoiceMessages(s, invoice, w.cfg, w.admins)
	if err != nil {
		return review(ReviewContract)
	}
	finishCtx, finishCancel := context.WithTimeout(context.Background(), databaseTimeout)
	defer finishCancel()
	return w.store.FinishRegistration(finishCtx, op.Lease, customer.ID, invoice.ID, messages)
}

func marker(token string, step Step) string { return "payplump:" + token + ":" + string(step) }

func decodeIntent(raw json.RawMessage, destination any) error {
	if len(raw) > 64*1024 {
		return ErrReviewRequired
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return ErrReviewRequired
	}
	return nil
}

func invoiceDraft(customerID string, p invoiceninja.Product, date, privateMarker string, inclusive bool) invoiceninja.InvoiceDraft {
	return invoiceninja.InvoiceDraft{ClientID: customerID, Date: date, DueDate: date, PrivateNotes: privateMarker, UsesInclusiveTaxes: inclusive, LineItems: []invoiceninja.InvoiceLine{{
		Discount: "0", TypeID: "1", ProductKey: p.ProductKey, Notes: p.Notes, Cost: p.Price, Quantity: json.Number("1"),
		TaxName1: p.TaxName1, TaxName2: p.TaxName2, TaxName3: p.TaxName3, TaxRate1: p.TaxRate1, TaxRate2: p.TaxRate2, TaxRate3: p.TaxRate3, TaxID: p.TaxID,
	}}}
}

func (w *Worker) prepare(ctx context.Context, lease queue.Lease, step Step, payload any) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return false, review(ReviewContract)
	}
	dbCtx, cancel := context.WithTimeout(ctx, databaseTimeout)
	defer cancel()
	return w.store.PrepareRegistrationWrite(dbCtx, lease, step, raw)
}

func (w *Worker) confirmAndRetry(lease queue.Lease, step Step, id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), databaseTimeout)
	defer cancel()
	if err := w.store.ConfirmRegistrationStep(ctx, lease, step, id); err != nil {
		return err
	}
	return w.store.RetryOperation(ctx, lease, 0, 0)
}

func (w *Worker) retry(lease queue.Lease, delay time.Duration, code int) error {
	ctx, cancel := context.WithTimeout(context.Background(), databaseTimeout)
	defer cancel()
	return w.store.RetryOperation(ctx, lease, delay, code)
}

func (w *Worker) unknownCreate(op queue.Operation, step Step, err error) error {
	w.logger.Warn("registration create result unknown", "work_id", op.ID, "step", string(step), "error_code", errorCode(err))
	return w.retry(op.Lease, 30*time.Second, errorCode(err))
}

func (w *Worker) hold(op queue.Operation, s Snapshot, code int) {
	drafts := reviewMessages(s, op.ID, code, w.admins)
	ctx, cancel := context.WithTimeout(context.Background(), databaseTimeout)
	defer cancel()
	if err := w.store.HoldRegistration(ctx, op.Lease, code, drafts); err != nil {
		w.logger.Error("registration review persistence failed", "work_id", op.ID, "review_code", code)
	} else {
		w.logger.Warn("registration held for review", "work_id", op.ID, "review_code", code)
	}
}

func errorCode(err error) int {
	var remote *integrationhttp.HTTPError
	if errors.As(err, &remote) {
		return remote.StatusCode
	}
	return 0
}
func backoff(attempt int) time.Duration {
	d := time.Second
	for attempt > 1 && d < 5*time.Minute {
		d = min(d*2, 5*time.Minute)
		attempt--
	}
	return min(d+time.Duration(rand.Int64N(int64(time.Second))), 5*time.Minute)
}
