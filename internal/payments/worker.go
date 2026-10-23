package payments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
)

type Reader interface {
	GetSettlementCustomer(context.Context, string) (invoiceninja.SettlementCustomer, error)
	GetSettlementInvoice(context.Context, string) (invoiceninja.SettlementInvoice, error)
	ListSettlementPayments(context.Context, string) ([]invoiceninja.SettlementPayment, error)
}

type Worker struct {
	store  Store
	reader Reader
	botID  int64
	admins []int64
	cfg    Config
	logger *slog.Logger
	now    func() time.Time
}

const databaseTimeout = 8 * time.Second

func New(store Store, reader Reader, botID int64, admins []int64, cfg Config, logger *slog.Logger) (*Worker, error) {
	if !cfg.Enabled {
		return nil, errors.New("payment verification is disabled")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if store == nil || reader == nil || botID <= 0 {
		return nil, errors.New("payment verification requires store, GET reader and verified bot")
	}
	ids := slices.Clone(admins)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) < 1 || len(ids) > 99 || ids[0] <= 0 {
		return nil, errors.New("payment verification requires 1-99 positive administrators")
	}
	cfg.ExcludedTelegramIDs = slices.Clone(cfg.ExcludedTelegramIDs)
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{store: store, reader: reader, botID: botID, admins: ids, cfg: cfg, logger: logger, now: time.Now}, nil
}
func (w *Worker) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		seed, cancel := context.WithTimeout(ctx, databaseTimeout)
		_, err := w.store.SeedPaymentChecks(seed, w.botID)
		cancel()
		if err != nil && ctx.Err() == nil {
			w.logger.Error("payment check discovery failed")
		}
		claim, cancel := context.WithTimeout(ctx, databaseTimeout)
		work, err := w.store.ClaimPaymentChecks(claim, w.botID, w.cfg.Workers)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				w.logger.Error("payment check claim failed")
			}
		} else {
			var tasks sync.WaitGroup
			for _, item := range work {
				if ctx.Err() != nil {
					break
				}
				tasks.Add(1)
				go func(item Claim) { defer tasks.Done(); w.handle(ctx, item) }(item)
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
	ctx, cancel := context.WithTimeout(service, time.Minute)
	defer cancel()
	db, stop := context.WithTimeout(ctx, databaseTimeout)
	snapshot, err := w.store.LoadPaymentSnapshot(db, claim)
	stop()
	if err != nil {
		if !errors.Is(err, queue.ErrLeaseLost) && service.Err() == nil {
			w.deferCheck(claim, err)
		}
		return
	}
	var decision Decision
	if snapshot.BotID != w.botID || snapshot.ClientID != claim.ID {
		decision = review(ReviewAssociation)
	} else if slices.Contains(w.cfg.ExcludedTelegramIDs, snapshot.TelegramID) {
		decision = review(ReviewExcluded)
	} else {
		decision, err = w.observe(ctx, snapshot)
	}
	if err != nil {
		// An incomplete/failed read is not an authoritative negative finding.
		// In particular ErrSettlementIncomplete can wrap HTTP/cancellation:
		// never turn an outage into sticky invalidation of a previous payment.
		if service.Err() == nil {
			w.deferCheck(claim, err)
		}
		return
	}
	if decision.Validate() != nil {
		decision = review(ReviewContract)
	}
	finalize, stop := context.WithTimeout(context.Background(), databaseTimeout)
	defer stop()
	if err := w.store.FinishPaymentCheck(finalize, claim, snapshot, decision, w.notices(snapshot, decision), w.cfg.RecheckInterval); err != nil && !errors.Is(err, queue.ErrLeaseLost) {
		w.logger.Warn("payment observation finalization failed", "work_id", claim.ID)
		// No second finalization budget during shutdown. The lease is recoverable.
	}
}
func (w *Worker) observe(ctx context.Context, s Snapshot) (Decision, error) {
	return Observe(ctx, w.reader, s, w.cfg.CurrencyID, w.now().In(time.Local).Format(time.DateOnly))
}
func (w *Worker) deferCheck(claim Claim, cause error) {
	code := 9000
	if errors.Is(cause, invoiceninja.ErrSettlementVersion) {
		code = 9001
	} else if errors.Is(cause, invoiceninja.ErrSettlementIncomplete) {
		code = 9002
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
	if err := w.store.DeferPaymentCheck(ctx, claim, delay, code); err != nil && !errors.Is(err, queue.ErrLeaseLost) {
		w.logger.Error("payment check retry failed", "work_id", claim.ID)
	}
	w.logger.Warn("payment check deferred", "work_id", claim.ID, "error_code", code)
}
func (w *Worker) notices(s Snapshot, d Decision) Notices {
	digest := sha256.Sum256([]byte(s.InvoiceID))
	prefix := "payment:" + hex.EncodeToString(digest[:])
	reviewText := fmt.Sprintf("Оплата требует ручной проверки. Telegram ID %d, локальный клиент #%d.\nПодтверждение изменилось или данные проверки неоднозначны. По этой проверке доступ и recurring не менялись.", s.TelegramID, s.ClientID)
	var notices Notices
	for _, id := range w.admins {
		notices.Review = append(notices.Review, queue.Draft{BotID: w.botID, ChatID: id, Key: prefix + ":review:admin:" + strconv.FormatInt(id, 10), Text: reviewText})
	}
	if d.Kind == Verified && s.ImportedBaselineProofHash == ([32]byte{}) {
		p := d.Proof
		body := fmt.Sprintf("Платёж по счёту №%s проверен.\nСумма: %s. Дата платежа: %s.\nРасчётная дата следующего счёта: %s.\nЭто подтверждение платежа, не состояния доступа. Настройка доступа выполняется отдельно; проверьте статус в меню или обратитесь к администратору.", p.InvoiceNumber, p.Amount, p.PaymentDate, p.NextInvoiceDate)
		notices.Confirmed = append(notices.Confirmed, queue.Draft{BotID: w.botID, ChatID: s.ChatID, Key: prefix + ":confirmed:client", Text: body})
		for _, id := range w.admins {
			notices.Confirmed = append(notices.Confirmed, queue.Draft{BotID: w.botID, ChatID: id, Key: prefix + ":confirmed:admin:" + strconv.FormatInt(id, 10), Text: fmt.Sprintf("Платёж проверен: Telegram ID %d, счёт №%s, сумма %s, Payment.date %s, следующая расчётная дата %s. Это наблюдение платежа; настройка доступа и recurring учитывается отдельно.", s.TelegramID, p.InvoiceNumber, p.Amount, p.PaymentDate, p.NextInvoiceDate)})
		}
	}
	return notices
}
