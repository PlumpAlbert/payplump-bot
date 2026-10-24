package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/billing"
	"github.com/PlumpAlbert/payplump-bot/internal/importorigin"
	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/managedscope"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
)

type Worker struct {
	store  Store
	reader Reader
	botID  int64
	admins []int64
	cfg    Config
	logger *slog.Logger
	now    func() time.Time
}

const (
	attemptTimeout  = 20 * time.Minute
	databaseTimeout = 8 * time.Second
)

func New(store Store, reader Reader, botID int64, admins []int64, cfg Config, logger *slog.Logger) (*Worker, error) {
	if !cfg.Enabled {
		return nil, errors.New("invoice lifecycle is disabled")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if store == nil || reader == nil || botID <= 0 {
		return nil, errors.New("invoice lifecycle requires a store, GET reader and verified bot")
	}
	ids := sortedIDs(admins)
	ids = slices.Compact(ids)
	if len(ids) < 1 || len(ids) > 99 || ids[0] <= 0 {
		return nil, errors.New("invoice lifecycle requires 1-99 positive administrator IDs")
	}
	cfg.AllowedTelegramIDs = slices.Clone(cfg.AllowedTelegramIDs)
	cfg.ExcludedTelegramIDs = slices.Clone(cfg.ExcludedTelegramIDs)
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{store: store, reader: reader, botID: botID, admins: ids, cfg: cfg, logger: logger, now: time.Now}, nil
}

func (w *Worker) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		db, cancel := context.WithTimeout(ctx, databaseTimeout)
		allowed, err := managedscope.Load(db, w.store, w.botID, w.cfg.ManagedClients, w.cfg.AllowedTelegramIDs, w.cfg.ExcludedTelegramIDs)
		if err == nil {
			_, err = w.store.SeedInvoiceLifecycleChecks(db, w.botID, allowed, w.cfg.MaxAccounts)
		}
		cancel()
		if err != nil && ctx.Err() == nil {
			w.logger.Warn("invoice lifecycle seed deferred")
		}
		if ctx.Err() != nil {
			return nil
		}
		db, cancel = context.WithTimeout(ctx, databaseTimeout)
		claims, err := w.store.ClaimInvoiceLifecycleChecks(db, w.botID, allowed, w.cfg.Workers)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				w.logger.Warn("invoice lifecycle claim deferred")
			}
		} else {
			var tasks sync.WaitGroup
			for _, claim := range claims {
				if ctx.Err() != nil {
					break
				}
				tasks.Add(1)
				go func(claim Claim) { defer tasks.Done(); w.handle(ctx, claim) }(claim)
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

func (w *Worker) handle(service context.Context, claim Claim) {
	ctx, cancel := context.WithTimeout(service, attemptTimeout)
	defer cancel()
	db, stop := context.WithTimeout(ctx, databaseTimeout)
	snapshot, err := w.store.LoadInvoiceLifecycleSnapshot(db, claim)
	stop()
	if err != nil {
		if !errors.Is(err, queue.ErrLeaseLost) && service.Err() == nil {
			w.deferCheck(claim, err)
		}
		return
	}
	if code := w.validateSnapshot(snapshot); code != 0 {
		w.finishReview(claim, snapshot, nil, code)
		return
	}
	inventory, err := w.reader.ListInvoices(ctx, snapshot.CustomerID)
	if err != nil {
		if service.Err() == nil {
			w.deferCheck(claim, err)
		}
		return
	}
	known := make(map[string]struct{}, len(snapshot.SeenInvoiceIDs))
	for _, id := range snapshot.SeenInvoiceIDs {
		known[id] = struct{}{}
	}
	var candidateIDs []string
	for _, invoice := range inventory {
		if invoice.ClientID != snapshot.CustomerID || invoice.RecurringID != snapshot.RecurringID || invoice.ID == snapshot.FirstInvoiceID || invoice.ID == snapshot.CurrentInvoiceID {
			continue
		}
		if _, ok := known[invoice.ID]; ok {
			continue
		}
		if invoice.StatusID == "1" && !invoice.IsDeleted && invoice.ArchivedAt == 0 {
			continue
		}
		candidateIDs = append(candidateIDs, invoice.ID)
	}
	if len(candidateIDs) == 0 {
		w.finish(claim, snapshot, nil, 0, Notices{})
		return
	}
	if len(candidateIDs) > 1 {
		marker := Invoice{ID: fmt.Sprintf("collision:%d", snapshot.ClientID)}
		w.finishReview(claim, snapshot, &marker, ReviewCollision)
		return
	}
	fresh, err := w.reader.GetInvoice(ctx, candidateIDs[0])
	if err != nil {
		if service.Err() == nil {
			w.deferCheck(claim, err)
		}
		return
	}
	if fresh.ID != candidateIDs[0] {
		w.finishReview(claim, snapshot, &Invoice{ID: candidateIDs[0]}, ReviewAssociation)
		return
	}
	if fresh.ClientID != snapshot.CustomerID || fresh.RecurringID != snapshot.RecurringID || fresh.PrivateNotes != expectedPrivateNotes(snapshot) {
		w.finishReview(claim, snapshot, &Invoice{ID: fresh.ID, Number: fresh.Number, Status: fresh.StatusID}, ReviewAssociation)
		return
	}
	if fresh.StatusID == "1" && !fresh.IsDeleted && fresh.ArchivedAt == 0 {
		w.finish(claim, snapshot, nil, 0, Notices{})
		return
	}
	candidate, code := makeCandidate(fresh, w.now().Location())
	if code != 0 {
		w.finishReview(claim, snapshot, &candidate, code)
		return
	}
	var notices Notices
	if candidate.Status == "sent" || candidate.Status == "partial" {
		notices = makeNotices(w.botID, w.admins, snapshot, candidate, 0)
	}
	notices.Review = makeNotices(w.botID, w.admins, snapshot, candidate, ReviewAssociation).Review
	notices.PredecessorReview = makeNotices(w.botID, w.admins, snapshot, candidate, ReviewPreviousPayment).Review
	w.finish(claim, snapshot, &candidate, 0, notices)
}

func expectedPrivateNotes(s Snapshot) string {
	if s.ImportedOrigin != (importorigin.Ref{}) {
		return s.ImportedPrivateNotes
	}
	return activation.Marker(s.ActivationToken)
}

func (w *Worker) validateSnapshot(s Snapshot) int {
	imported := s.ImportedOrigin != (importorigin.Ref{})
	commonInvalid := s.BotID != w.botID || s.ClientID <= 0 || s.TelegramID <= 0 || s.ChatID != s.TelegramID || !validInvoiceID(s.CustomerID) || !validInvoiceID(s.CurrentInvoiceID) || !validInvoiceID(s.RecurringID) || s.SUIID == "" || !s.ManagedProvenance || s.State != "active" || !s.DesiredAccess
	if commonInvalid {
		return ReviewAssociation
	}
	if imported {
		if !s.ImportedOrigin.Valid() || s.ImportedOrigin.ClientID != s.ClientID || s.ImportedOrigin.BotID != s.BotID || s.FirstInvoiceID != "" || s.RegistrationToken != "" || s.ActivationToken != "" || !slices.Contains(s.SeenInvoiceIDs, s.CurrentInvoiceID) {
			return ReviewAssociation
		}
	} else if !validInvoiceID(s.FirstInvoiceID) || s.RegistrationToken == "" || s.ActivationToken == "" {
		return ReviewAssociation
	}
	if s.SeenInvoiceOverflow {
		return ReviewContract
	}
	if s.CurrentInvoiceID != s.FirstInvoiceID && !slices.Contains(s.SeenInvoiceIDs, s.CurrentInvoiceID) {
		return ReviewAssociation
	}
	if !managedscope.Allows(s.TelegramID, w.cfg.ManagedClients, w.cfg.AllowedTelegramIDs, w.cfg.ExcludedTelegramIDs) {
		return ReviewExcluded
	}
	return 0
}

func (w *Worker) finishReview(claim Claim, snapshot Snapshot, invoice *Invoice, code int) {
	if invoice == nil {
		invoice = &Invoice{ID: fmt.Sprintf("client:%d", snapshot.ClientID)}
	}
	notices := makeNotices(w.botID, w.admins, snapshot, *invoice, code)
	w.finish(claim, snapshot, invoice, code, notices)
}

func (w *Worker) finish(claim Claim, snapshot Snapshot, invoice *Invoice, code int, notices Notices) {
	ctx, cancel := context.WithTimeout(context.Background(), databaseTimeout)
	defer cancel()
	if err := w.store.FinishInvoiceLifecycleCheck(ctx, claim, snapshot, invoice, code, notices, w.cfg.RecheckInterval); err != nil && !errors.Is(err, queue.ErrLeaseLost) {
		w.logger.Warn("invoice lifecycle finalization deferred")
	}
}

func (w *Worker) deferCheck(claim Claim, cause error) {
	code := 9000
	if errors.Is(cause, invoiceninja.ErrCycleLookupIncomplete) {
		code = 9002
	} else if errors.Is(cause, invoiceninja.ErrSettlementVersion) {
		code = 9001
	} else if errors.Is(cause, invoiceninja.ErrSettlementContract) || errors.Is(cause, integrationhttp.ErrInvalidResponse) {
		code = 9003
	}
	var remote *integrationhttp.HTTPError
	if errors.As(cause, &remote) {
		code = remote.StatusCode
	}
	delay := 5 * time.Second
	for n := claim.Attempt; n > 1 && delay < 5*time.Minute; n-- {
		delay = min(delay*2, 5*time.Minute)
	}
	if code == 429 {
		delay = max(delay, time.Minute)
	}
	ctx, cancel := context.WithTimeout(context.Background(), databaseTimeout)
	defer cancel()
	if err := w.store.DeferInvoiceLifecycleCheck(ctx, claim, delay, code); err != nil && !errors.Is(err, queue.ErrLeaseLost) {
		w.logger.Warn("invoice lifecycle retry deferred")
	}
	w.logger.Warn("invoice lifecycle observation deferred", "error_code", code)
}

func makeCandidate(invoice invoiceninja.CycleInvoice, location *time.Location) (Invoice, int) {
	candidate := Invoice{ID: invoice.ID, Number: invoice.Number, Status: invoice.StatusID}
	if !printable(invoice.Number, 128) || invoice.Number == "" {
		return candidate, ReviewInvoice
	}
	if invoice.IsDeleted || invoice.ArchivedAt != 0 {
		return candidate, ReviewInvoice
	}
	status := ""
	switch invoice.StatusID {
	case "2":
		status = "sent"
	case "3":
		status = "partial"
	case "4":
		status = "paid"
	default:
		return candidate, ReviewInvoice
	}
	amount, amountRat, err := exactAmount(invoice.Amount)
	if err != nil {
		return candidate, ReviewBalance
	}
	balance, balanceRat, err := exactAmount(invoice.Balance)
	if err != nil {
		return candidate, ReviewBalance
	}
	paid, paidRat, err := exactAmount(invoice.PaidToDate)
	if err != nil {
		return candidate, ReviewBalance
	}
	if amountRat.Sign() <= 0 || balanceRat.Sign() < 0 || paidRat.Sign() < 0 || new(big.Rat).Add(new(big.Rat).Set(balanceRat), paidRat).Cmp(amountRat) != 0 {
		return candidate, ReviewBalance
	}
	switch status {
	case "sent":
		if balanceRat.Sign() <= 0 || paidRat.Sign() != 0 {
			return candidate, ReviewBalance
		}
	case "partial":
		if balanceRat.Sign() <= 0 || paidRat.Sign() <= 0 {
			return candidate, ReviewBalance
		}
	case "paid":
		if balanceRat.Sign() != 0 || paidRat.Cmp(amountRat) != 0 {
			return candidate, ReviewBalance
		}
	}
	sentDate, err := sentDate(invoice.Invitations, location)
	if err != nil {
		return candidate, ReviewSentDate
	}
	candidate.Status, candidate.SentDate, candidate.Amount, candidate.Balance, candidate.PaidToDate = status, sentDate, amount, balance, paid
	return candidate, 0
}

func exactAmount(value json.Number) (string, *big.Rat, error) {
	formatted, err := billing.FormatAmount(value)
	if err != nil {
		return "", nil, err
	}
	amount, ok := new(big.Rat).SetString(formatted)
	if !ok {
		return "", nil, errors.New("invalid invoice amount")
	}
	return formatted, amount, nil
}

func sentDate(invitations []invoiceninja.CycleInvitation, location *time.Location) (string, error) {
	if location == nil {
		location = time.UTC
	}
	var earliest time.Time
	for _, invitation := range invitations {
		if invitation.SentDate == "" {
			continue
		}
		parsed, err := parseSentTime(invitation.SentDate, location)
		if err != nil {
			return "", err
		}
		if earliest.IsZero() || parsed.Before(earliest) {
			earliest = parsed
		}
	}
	if earliest.IsZero() {
		return "", errors.New("missing invitation sent date")
	}
	return earliest.In(location).Format(time.DateOnly), nil
}

func parseSentTime(value string, location *time.Location) (time.Time, error) {
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed, nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05", "2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05"} {
		if parsed, err := time.ParseInLocation(layout, value, location); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, errors.New("invalid invitation sent date")
}

func (i Invoice) Validate() error {
	if !validInvoiceID(i.ID) || i.Number == "" || !printable(i.Number, 128) ||
		(i.Status != "sent" && i.Status != "partial" && i.Status != "paid") || i.SentDate == "" {
		return errors.New("invalid lifecycle invoice")
	}
	if parsed, err := time.Parse(time.DateOnly, i.SentDate); err != nil || parsed.Format(time.DateOnly) != i.SentDate {
		return errors.New("invalid lifecycle sent date")
	}
	_, amount, amountErr := exactAmount(json.Number(i.Amount))
	_, balance, balanceErr := exactAmount(json.Number(i.Balance))
	_, paid, paidErr := exactAmount(json.Number(i.PaidToDate))
	if amountErr != nil || balanceErr != nil || paidErr != nil || amount.Sign() <= 0 || balance.Sign() < 0 || paid.Sign() < 0 || balance.Cmp(amount) > 0 || new(big.Rat).Add(new(big.Rat).Set(balance), paid).Cmp(amount) != 0 {
		return errors.New("invalid lifecycle amounts")
	}
	switch i.Status {
	case "sent":
		if balance.Sign() <= 0 || paid.Sign() != 0 {
			return errors.New("lifecycle balance does not match status")
		}
	case "partial":
		if balance.Sign() <= 0 || paid.Sign() <= 0 {
			return errors.New("lifecycle balance does not match status")
		}
	case "paid":
		if balance.Sign() != 0 || paid.Cmp(amount) != 0 {
			return errors.New("lifecycle balance does not match status")
		}
	}
	return nil
}

func validInvoiceID(value string) bool {
	if value == "" || len(value) > 256 || !utf8.ValidString(value) || strings.TrimSpace(value) != value || value == "." || value == ".." || strings.ContainsAny(value, "/\\") {
		return false
	}
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return false
	}
	return true
}
