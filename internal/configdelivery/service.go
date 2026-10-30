package configdelivery

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/billing"
	"github.com/PlumpAlbert/payplump-bot/internal/importorigin"
	"github.com/PlumpAlbert/payplump-bot/internal/importverify"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
)

// Providers deliberately exposes only GET capabilities. Neither reconciliation,
// adoption nor any provider write is part of configuration delivery.
type Providers struct {
	Payments payments.Reader
	SUI      interface {
		GetRecord(context.Context, int64) (sui.ClientRecord, error)
	}
	Recurring interface {
		Get(context.Context, string) (invoiceninja.RecurringRecord, error)
	}
}

type Transport interface {
	GetPrivateChat(context.Context, int64) error
	SendProtectedMessage(context.Context, int64, func(context.Context) (telegram.ProtectedMessage, error)) (int64, error)
}

type Service struct {
	store     Store
	providers Providers
	transport Transport
	botID     int64
	cfg       Config
	logger    *slog.Logger
	now       func() time.Time
}

func (*Service) String() string   { return "[protected configuration service]" }
func (*Service) GoString() string { return "[protected configuration service]" }

const databaseTimeout = 5 * time.Second

const denialText = "Не удалось подтвердить безопасную выдачу конфигурации по этому запросу. Проверьте /status или обратитесь к администратору. Ранее выданную подписку не удаляйте."

func New(store Store, providers Providers, transport Transport, botID int64, cfg Config, logger *slog.Logger) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, deliveryError(err)
	}
	if store == nil || transport == nil || botID <= 0 {
		return nil, errors.New("configuration delivery requires store, protected transport and verified bot")
	}
	if cfg.Enabled && (providers.Payments == nil || providers.SUI == nil || providers.Recurring == nil) {
		return nil, errors.New("configuration delivery requires GET providers")
	}
	cfg.AllowedTelegramIDs = slices.Clone(cfg.AllowedTelegramIDs)
	cfg.ExcludedTelegramIDs = slices.Clone(cfg.ExcludedTelegramIDs)
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{store: store, providers: providers, transport: transport, botID: botID, cfg: cfg, logger: logger, now: time.Now}, nil
}

// Deliver runs within the outbox's aggregate budget; it never retries. An
// authorization, including an uncertain result, is not released here. Only the
// durable finalizer may classify a known rejection, and a new request is needed
// for another attempt. The immutable loaded view is retained through prepare.
func (s *Service) Deliver(ctx context.Context, m queue.Message) (int64, error) {
	if ctx == nil || !m.ProtectedConfiguration || m.ClientMenu || m.Text != queue.ConfigurationIntentBody || m.BotID != s.botID || m.ChatID <= 0 || m.ID <= 0 || m.Attempt <= 0 {
		return 0, ErrDenied
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	loadCtx, cancel := context.WithTimeout(ctx, databaseTimeout)
	view, err := s.store.LoadConfiguration(loadCtx, m)
	loadErr := loadCtx.Err()
	cancel()
	if err != nil {
		return 0, deliveryError(err)
	}
	if loadErr != nil {
		return 0, deliveryError(loadErr)
	}
	if view.AlreadyDispatched {
		return 0, ErrAlreadyDispatched
	}
	decision := s.evaluate(ctx, m, view)
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if decision.Kind == Secret {
		// This GET is deliberately absent from ALL local-scope denial paths.
		if err := s.transport.GetPrivateChat(ctx, m.ChatID); err != nil {
			return 0, deliveryError(err)
		}
	}
	var prepared atomic.Bool
	id, err := s.transport.SendProtectedMessage(ctx, m.ChatID, func(prepareCtx context.Context) (telegram.ProtectedMessage, error) {
		if !prepared.CompareAndSwap(false, true) {
			return telegram.ProtectedMessage{}, ErrAlreadyDispatched
		}
		if prepareCtx == nil {
			return telegram.ProtectedMessage{}, ErrDenied
		}
		if err := ctx.Err(); err != nil {
			return telegram.ProtectedMessage{}, err
		}
		if err := prepareCtx.Err(); err != nil {
			return telegram.ProtectedMessage{}, err
		}
		authorizeCtx, cancel := context.WithTimeout(prepareCtx, databaseTimeout)
		// Also retain the caller's deadline if a transport supplies a new context.
		if deadline, ok := ctx.Deadline(); ok {
			var deadlineCancel context.CancelFunc
			authorizeCtx, deadlineCancel = context.WithDeadline(authorizeCtx, deadline)
			defer deadlineCancel()
		}
		permit, err := s.store.AuthorizeConfiguration(authorizeCtx, m, view, decision)
		authorizeErr := authorizeCtx.Err()
		cancel()
		if err != nil {
			return telegram.ProtectedMessage{}, deliveryError(err)
		}
		if authorizeErr != nil {
			return telegram.ProtectedMessage{}, deliveryError(authorizeErr)
		}
		// A DB downgrade must govern the body. An upgrade is never permitted.
		if permit.Kind != Denial && (permit.Kind != Secret || decision.Kind != Secret) {
			return telegram.ProtectedMessage{}, ErrDenied
		}
		if err := ctx.Err(); err != nil {
			return telegram.ProtectedMessage{}, err
		}
		if err := prepareCtx.Err(); err != nil {
			return telegram.ProtectedMessage{}, err
		}
		notAfter := permit.NotAfter
		if notAfter.IsZero() {
			return telegram.ProtectedMessage{}, ErrDenied
		}
		// The transport also retains its caller context: an aggregate deadline
		// or cancellation must prevent transmission even before NotAfter.
		if permit.Kind == Secret {
			deadline, err := view.Entitlement.Deadline()
			if err != nil {
				return telegram.ProtectedMessage{}, ErrDenied
			}
			for _, bound := range []time.Time{view.Request.ExpiresAt, deadline} {
				if bound.Before(notAfter) {
					notAfter = bound
				}
			}
		}
		if !s.now().Before(notAfter) {
			return telegram.ProtectedMessage{}, context.DeadlineExceeded
		}
		text := denialText
		if permit.Kind == Denial {
			s.logger.Info("configuration denial authorized", "work_id", m.ID, "reason_code", permit.Reason)
		}
		if permit.Kind == Secret {
			// This is the FIRST point at which a personal URL may exist. Never
			// use panel links, fetch this URL, log it or put it in durable work.
			uri, err := sui.SubscriptionURL(s.cfg.SubscriptionBaseURL, view.Entitlement.Binding.Name)
			if err != nil {
				return telegram.ProtectedMessage{}, ErrDenied
			}
			text = "Ваша персональная ссылка на подписку:\n" + uri + "\n\nПомощь: /help"
		}
		if err := ctx.Err(); err != nil {
			return telegram.ProtectedMessage{}, err
		}
		if err := prepareCtx.Err(); err != nil {
			return telegram.ProtectedMessage{}, err
		}
		if !s.now().Before(notAfter) {
			return telegram.ProtectedMessage{}, context.DeadlineExceeded
		}
		return telegram.ProtectedMessage{Text: text, NotAfter: notAfter}, nil
	})
	return id, deliveryError(err)
}

func deny(reason int) Decision { return Decision{Kind: Denial, Reason: reason} }

func (s *Service) evaluate(ctx context.Context, m queue.Message, v View) Decision {
	policy, r := s.cfg.Policy(), v.Request
	if !s.cfg.Enabled || !s.cfg.Allows(m.ChatID) || r.PolicyRevision != policy.Revision || r.PolicyHash != policy.Hash {
		return deny(DeniedPolicy)
	}
	if v.ChatBlocked {
		return deny(DeniedUnknownDelivery)
	}
	if !v.Matches() || r.OutboxID != m.ID || r.BotID != m.BotID || r.ChatID != m.ChatID {
		reason := v.DenialCode
		if reason < DeniedPolicy || reason > DeniedUnknownDelivery {
			reason = DeniedEntitlement
		}
		return deny(reason)
	}
	e := v.Entitlement
	if e.Imported != nil {
		return s.evaluateImported(ctx, v)
	}
	deadline, err := e.Deadline()
	if err != nil {
		return deny(DeniedEntitlement)
	}
	if !deliveryFuture(e, r, deadline, s.now()) {
		return deny(DeniedExpired)
	}
	var suiDraft sui.CreateDraft
	var recurringDraft invoiceninja.RecurringDraft
	if activation.DecodeIntent(e.Activation.Steps[activation.SUICreate].Payload, &suiDraft) != nil || activation.DecodeIntent(e.Activation.Steps[activation.RecurringCreate].Payload, &recurringDraft) != nil {
		return deny(DeniedEntitlement)
	}
	if result := s.readBound(ctx, e, suiDraft, recurringDraft); result.Kind != Secret {
		return result
	}
	account := e.Activation.Account
	expectedProof := e.Activation.Proof
	if e.Period == RenewalPeriod {
		account, expectedProof = e.Current, e.CurrentProof
	}
	observed, err := payments.Observe(ctx, s.providers.Payments, account, s.cfg.CurrencyID, s.now().In(time.Local).Format(time.DateOnly))
	if err != nil || observed.Validate() != nil {
		return deny(DeniedUnavailable)
	}
	if observed.Kind != payments.Verified || observed.Proof.Hash != expectedProof.Hash {
		return Decision{Kind: Denial, Reason: DeniedPayment, PaymentChanged: true}
	}
	// Financial verification cannot bless records which changed while it ran.
	if result := s.readBound(ctx, e, suiDraft, recurringDraft); result.Kind != Secret {
		return result
	}
	if !deliveryFuture(e, r, deadline, s.now()) {
		return deny(DeniedExpired)
	}
	return Decision{Kind: Secret}
}

func (s *Service) evaluateImported(ctx context.Context, v View) Decision {
	e, r, b := v.Entitlement, v.Request, v.Entitlement.Imported
	scope, scopeErr := importverify.SourceScope(s.cfg.BaseURL, s.cfg.Token, invoiceninja.SupportedSettlementVersion, s.cfg.SUIBaseURL, s.cfg.SUIToken, s.cfg.SUIVersion)
	scopeHash, scopeOK := importorigin.ParseHashHex(scope)
	if scopeErr != nil || !scopeOK || scopeHash != b.Origin.SourceScopeHash || sha256.Sum256([]byte(s.cfg.SubscriptionBaseURL)) != b.PublicBaseHash || s.cfg.CurrencyID != e.CurrentProof.CurrencyID {
		return deny(DeniedPolicy)
	}
	deadline, err := e.Deadline()
	if err != nil {
		return deny(DeniedEntitlement)
	}
	if !deliveryFutureImported(e, r, deadline, s.now()) {
		return deny(DeniedExpired)
	}
	if result := s.readImportedBound(ctx, e); result.Kind != Secret {
		return result
	}
	observed, err := payments.Observe(ctx, s.providers.Payments, e.Current, s.cfg.CurrencyID, s.now().In(time.Local).Format(time.DateOnly))
	if err != nil || observed.Validate() != nil {
		return deny(DeniedUnavailable)
	}
	expectedHash := b.ProofHash
	if e.Period == RenewalPeriod {
		expectedHash = e.CurrentProof.Hash
	}
	if observed.Kind != payments.Verified || observed.Proof.Hash != expectedHash || observed.Proof.InvoiceNumber != e.CurrentProof.InvoiceNumber {
		return Decision{Kind: Denial, Reason: DeniedPayment, PaymentChanged: true}
	}
	if result := s.readImportedBound(ctx, e); result.Kind != Secret {
		return result
	}
	if !deliveryFutureImported(e, r, deadline, s.now()) {
		return deny(DeniedExpired)
	}
	return Decision{Kind: Secret}
}

func deliveryFutureImported(e Entitlement, r Request, deadline, now time.Time) bool {
	instant := e.Imported.NextSendUTC
	if e.Period == RenewalPeriod {
		instant = e.Renewal.NextSendUTC
	}
	scheduleFuture := instant.After(now) || e.Period == ImportedBaselinePeriod && e.Imported.Profile.BaselinePaused()
	return !now.Before(r.CreatedAt) && now.Before(r.ExpiresAt) && now.Before(deadline) && e.Current.NextInvoiceDate > now.In(time.Local).Format(time.DateOnly) && scheduleFuture
}

func (s *Service) readImportedBound(ctx context.Context, e Entitlement) Decision {
	b := e.Imported
	record, err := s.providers.SUI.GetRecord(ctx, e.Binding.ID)
	if err != nil {
		return deny(DeniedUnavailable)
	}
	if record.ID() != e.Binding.ID || record.Name() != e.Binding.Name || record.CredentialsHash() != e.Binding.CredentialsHash || !slices.Equal(record.InboundIDs(), b.InboundIDs) || !record.Enabled() || sui.ValidateImportedRecordBinding(record, e.Binding, b.InboundIDs) != nil {
		return deny(DeniedSUI)
	}
	recurring, err := s.providers.Recurring.Get(ctx, e.Current.RecurringID)
	if err != nil {
		return deny(DeniedUnavailable)
	}
	if recurring.ID != e.Current.RecurringID || recurring.ClientID != e.Current.CustomerID || recurring.IsDeleted || recurring.ArchivedAt != 0 || invoiceninja.ValidateImportedRecurringForProfile(recurring, b.Profile) != nil {
		return deny(DeniedRecurring)
	}
	if e.Period == ImportedBaselinePeriod && b.Profile.BaselinePaused() && recurring.StatusID == "3" {
		// No billing action is inferred from the access entitlement. Bind the
		// unchanged paused source, but expire access by the independently paid period.
		amount, amountErr := billing.FormatAmount(recurring.Amount)
		if invoiceninja.ValidateImportedRecurringBaseline(recurring, b.Profile) != nil || amountErr != nil || amount != e.CurrentProof.Amount {
			return deny(DeniedRecurring)
		}
		return Decision{Kind: Secret}
	}
	if recurring.StatusID != "2" || recurring.NextSendDate != e.CurrentProof.NextInvoiceDate {
		return deny(DeniedRecurring)
	}
	amount, instant, normalizeErr := activation.NormalizeRecurringFacts(recurring.Amount, recurring.NextSendDatetime)
	expectedAmount := e.CurrentProof.Amount
	expectedInstant := b.NextSendUTC
	if e.Period == RenewalPeriod {
		expectedAmount = e.Renewal.Amount
		expectedInstant = e.Renewal.NextSendUTC
	}
	frozenAmount, frozenInstant, frozenErr := activation.NormalizeRecurringFacts(json.Number(expectedAmount), expectedInstant.Format(time.RFC3339Nano))
	if normalizeErr != nil || frozenErr != nil || amount != frozenAmount || instant != frozenInstant || instant != expectedInstant.Format(time.RFC3339Nano) || e.Period == RenewalPeriod && recurring.NextSendDate != e.Renewal.TargetDate {
		return deny(DeniedRecurring)
	}
	return Decision{Kind: Secret}
}

func deliveryFuture(e Entitlement, r Request, deadline, now time.Time) bool {
	today := now.In(time.Local).Format(time.DateOnly)
	proof, nextDate := e.Activation.Proof, e.Activation.Account.NextInvoiceDate
	if e.Period == RenewalPeriod {
		proof, nextDate = e.CurrentProof, e.Current.NextInvoiceDate
	}
	return !now.Before(r.CreatedAt) && now.Before(r.ExpiresAt) && now.Before(deadline) && proof.NextInvoiceDate > today && nextDate > today &&
		(e.Period != RenewalPeriod || e.Renewal.NextSendUTC.After(now))
}

func (s *Service) readBound(ctx context.Context, e Entitlement, draft sui.CreateDraft, recurringDraft invoiceninja.RecurringDraft) Decision {
	record, err := s.providers.SUI.GetRecord(ctx, e.Binding.ID)
	if err != nil {
		return deny(DeniedUnavailable)
	}
	if record.ID() != e.Binding.ID || record.Name() != e.Binding.Name || record.CredentialsHash() != e.Binding.CredentialsHash || !slices.Equal(record.InboundIDs(), draft.InboundIDs()) || sui.ValidateRecordForDraft(record, draft, true) != nil {
		return deny(DeniedSUI)
	}
	recurringID := e.Activation.Account.RecurringID
	if e.Period == RenewalPeriod {
		recurringID = e.Current.RecurringID
	}
	recurring, err := s.providers.Recurring.Get(ctx, recurringID)
	if err != nil {
		return deny(DeniedUnavailable)
	}
	if recurring.ID != recurringID || recurring.StatusID != "2" || recurring.IsDeleted || recurring.ArchivedAt != 0 {
		return deny(DeniedRecurring)
	}
	if e.Period == RenewalPeriod {
		today := s.now().In(time.Local).Format(time.DateOnly)
		if invoiceninja.ValidateLifecycleRecurringForDraft(recurring, recurringDraft, e.CurrentProof.NextInvoiceDate, today) != nil {
			return deny(DeniedRecurring)
		}
		amount, instant, normalizeErr := activation.NormalizeRecurringFacts(recurring.Amount, recurring.NextSendDatetime)
		finalAmount, finalUTC, finalErr := activation.NormalizeRecurringFacts(json.Number(e.Renewal.Amount), e.Renewal.NextSendUTC.Format(time.RFC3339Nano))
		if normalizeErr != nil || finalErr != nil || amount != finalAmount || instant != finalUTC {
			return deny(DeniedRecurring)
		}
	} else {
		if invoiceninja.ValidateRecurringForDraft(recurring, recurringDraft) != nil {
			return deny(DeniedRecurring)
		}
		original := e.Activation.Steps[activation.RecurringCreate]
		amount, instant, normalizeErr := activation.NormalizeRecurringFacts(recurring.Amount, recurring.NextSendDatetime)
		originalAmount, originalInstant, originalErr := activation.NormalizeRecurringFacts(json.Number(original.RecurringAmount), original.RecurringDatetime)
		if normalizeErr != nil || originalErr != nil || amount != originalAmount || instant != originalInstant {
			return deny(DeniedRecurring)
		}
	}
	return Decision{Kind: Secret}
}

// Preserve errors.Is/errors.As (notably Telegram cooldown/rejection evidence)
// without exposing adapter, database or context error strings in logs.
type deliveryFailure struct{ cause error }

func (deliveryFailure) Error() string    { return "protected configuration delivery failed" }
func (deliveryFailure) String() string   { return "protected configuration delivery failed" }
func (deliveryFailure) GoString() string { return "protected configuration delivery failed" }
func (e deliveryFailure) Unwrap() error  { return e.cause }

func deliveryError(err error) error {
	if err == nil {
		return nil
	}
	return deliveryFailure{cause: err}
}
