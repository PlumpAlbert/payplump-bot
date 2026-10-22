package lifecycle

import (
	"context"
	"encoding/json"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

const OverdueOperationKind = "invoice_overdue_enforcement"

const (
	OverduePauseRecurring EnforcementStep = "pause_recurring"
	OverdueDisableSUI     EnforcementStep = "disable_sui"
	OverdueCloseSessions  EnforcementStep = "close_sessions"
)

type EnforcementStep string

func (s EnforcementStep) Valid() bool {
	return s == OverduePauseRecurring || s == OverdueDisableSUI || s == OverdueCloseSessions
}

const (
	ReviewOverdueAssociation = 5101 + iota
	ReviewOverdueInvoice
	ReviewOverduePayment
	ReviewOverdueContract
	ReviewOverdueUnknownWrite
	ReviewOverdueSequence
)

type OverdueStep struct {
	Request         json.RawMessage
	ExternalID      string
	DispatchAttempt int
	Confirmed       bool
}

// EnforcementSnapshot is a local, bot-scoped view of one already-observed
// invoice. It is not itself permission to write; the store revalidates it under
// the shared external-write guard after each adapter's read-only preflight.
type EnforcementSnapshot struct {
	ClientID, TelegramID, ChatID, BotID                             int64
	CustomerID, FirstInvoiceID, CurrentInvoiceID, InvoiceID         string
	RecurringID, SUIID, RegistrationToken, ActivationToken          string
	State                                                           string
	DesiredAccess, ManagedProvenance                                bool
	Invoice                                                         Invoice
	RecurringDraft                                                  invoiceninja.RecurringDraft
	SUIBinding                                                      sui.Binding
	PaymentConfirmed, PaymentReview, PaymentLocked, LifecycleReview bool
	WriteEpoch                                                      int64
	WriterBusy                                                      bool
	WriterOperationID                                               int64
	WriterStep                                                      string
	Steps                                                           map[EnforcementStep]OverdueStep
}

func (EnforcementSnapshot) String() string   { return "[sensitive overdue enforcement snapshot]" }
func (EnforcementSnapshot) GoString() string { return "[sensitive overdue enforcement snapshot]" }

type EnforcementStore interface {
	SeedOverdueOperations(context.Context, int64, []int64, string, int) (int, error)
	ClaimOperations(context.Context, []string, int, time.Duration) ([]queue.Operation, error)
	LoadOverdueSnapshot(context.Context, queue.Operation, int64) (EnforcementSnapshot, error)
	AuthorizeOverdueWrite(context.Context, queue.Operation, int64, EnforcementSnapshot, EnforcementStep, json.RawMessage, string, []int64) (bool, error)
	ConfirmOverdueStep(context.Context, queue.Operation, EnforcementStep, string) error
	RetryOperation(context.Context, queue.Lease, time.Duration, int) error
	HoldOverdue(context.Context, queue.Operation, int64, EnforcementSnapshot, int, []queue.Draft) error
	FinishPaidOverdue(context.Context, queue.Operation, int64, EnforcementSnapshot) error
	FinishOverdue(context.Context, queue.Operation, int64, EnforcementSnapshot, []queue.Draft) error
}

type EnforcementInvoiceReader interface {
	GetInvoice(context.Context, string) (invoiceninja.CycleInvoice, error)
}

type EnforcementRecurringWriter interface {
	Get(context.Context, string) (invoiceninja.RecurringRecord, error)
	PauseAuthorized(context.Context, invoiceninja.PauseDraft, func(context.Context) error) (invoiceninja.RecurringRecord, error)
}

type EnforcementSUIWriter interface {
	GetRecord(context.Context, int64) (sui.ClientRecord, error)
	DisableAuthorized(context.Context, sui.DisableDraft, func(context.Context) error) (sui.ClientRecord, error)
	CloseSessionsAuthorized(context.Context, sui.CloseSessionsDraft, func(context.Context) error) error
}
