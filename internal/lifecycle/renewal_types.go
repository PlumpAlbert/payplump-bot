package lifecycle

import (
	"context"
	"encoding/json"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/importorigin"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

const RenewalOperationKind = "invoice_paid_renewal"

type RenewalStep string

const (
	RenewalPause    RenewalStep = "pause_recurring"
	RenewalSchedule RenewalStep = "schedule_recurring"
	RenewalEnable   RenewalStep = "enable_sui"
	RenewalResume   RenewalStep = "resume_recurring"
)

func (s RenewalStep) Valid() bool {
	return s == RenewalPause || s == RenewalSchedule || s == RenewalEnable || s == RenewalResume
}

const (
	ReviewRenewalAssociation = 5301 + iota
	ReviewRenewalProof
	ReviewRenewalTarget
	ReviewRenewalExternalState
	ReviewRenewalUnknownWrite
	ReviewRenewalSequence
	ReviewRenewalContract
)

type RenewalStepRecord struct {
	Request         json.RawMessage
	ExternalID      string
	Receipt         json.RawMessage
	DispatchAttempt int
	Confirmed       bool
}

// RenewalPlan is a fixed, immutable requirement set. It is deliberately not a generic workflow format.
type RenewalPlan struct {
	TargetDate      string                        `json:"target_date"`
	Pause           bool                          `json:"pause"`
	Schedule        bool                          `json:"schedule"`
	Enable          bool                          `json:"enable"`
	Resume          bool                          `json:"resume"`
	BaselineReceipt invoiceninja.RecurringReceipt `json:"baseline_receipt"`
}

func (p RenewalPlan) Required(step RenewalStep) bool {
	switch step {
	case RenewalPause:
		return p.Pause
	case RenewalSchedule:
		return p.Schedule
	case RenewalEnable:
		return p.Enable
	case RenewalResume:
		return p.Resume
	default:
		return false
	}
}

type RenewalSnapshot struct {
	ClientID, TelegramID, ChatID, BotID                                    int64
	CustomerID, FirstInvoiceID, CurrentInvoiceID, InvoiceID                string
	RecurringID, SUIID, RegistrationToken, ActivationToken                 string
	LedgerCustomerID, LedgerRecurringID, LedgerInvoiceNumber, LedgerStatus string
	LedgerAmount, LedgerBalance, LedgerPaidToDate, LedgerSentDate          string
	State                                                                  string
	DesiredAccess, ManagedProvenance                                       bool
	Payment                                                                payments.Snapshot
	Proof                                                                  payments.Proof
	RecurringDraft                                                         invoiceninja.RecurringDraft
	SUIBinding                                                             sui.Binding
	PaymentReview, PaymentLocked, LifecycleReview                          bool
	WriteEpoch                                                             int64
	WriterBusy                                                             bool
	WriterOperationID                                                      int64
	WriterStep                                                             string
	Outcome                                                                string
	Plan                                                                   *RenewalPlan
	PlanBaseline                                                           json.RawMessage
	Steps                                                                  map[RenewalStep]RenewalStepRecord
	OverdueSteps                                                           map[string]bool
	OverduePending                                                         bool
	OverdueReviewPayment                                                   bool
	OverdueOtherSteps                                                      bool
	ImportedOrigin                                                         importorigin.Ref
	ImportedAdmissionPermissionRevision                                    int64
	ImportedEffectsRevision                                                int64
	ImportedEffectsAllowed                                                 bool
	ImportedRecurringProfile                                               invoiceninja.ImportedRecurringProfile
	ImportedPrivateNotes                                                   string
	ImportedInboundIDs                                                     []int64
}

func (RenewalSnapshot) String() string   { return "[sensitive paid renewal snapshot]" }
func (RenewalSnapshot) GoString() string { return "[sensitive paid renewal snapshot]" }

type RenewalStore interface {
	SeedRenewalOperations(context.Context, int64, []int64, int) (int, error)
	ClaimOperations(context.Context, []string, int, time.Duration) ([]queue.Operation, error)
	LoadRenewalSnapshot(context.Context, queue.Operation, int64) (RenewalSnapshot, error)
	FreezeRenewalPlan(context.Context, queue.Operation, int64, RenewalSnapshot, RenewalPlan, json.RawMessage, []int64) error
	AuthorizeRenewalWrite(context.Context, queue.Operation, int64, RenewalSnapshot, RenewalStep, json.RawMessage, string, []int64) (bool, error)
	ConfirmRenewalStep(context.Context, queue.Operation, RenewalStep, string, json.RawMessage) error
	RetryOperation(context.Context, queue.Lease, time.Duration, int) error
	ReviewRenewal(context.Context, queue.Operation, int64, RenewalSnapshot, int, []queue.Draft) error
	FinishRenewal(context.Context, queue.Operation, int64, RenewalSnapshot, []queue.Draft) error
}

type RenewalInvoiceReader interface {
	GetInvoice(context.Context, string) (invoiceninja.CycleInvoice, error)
}
type RenewalRecurringWriter interface {
	Get(context.Context, string) (invoiceninja.RecurringRecord, error)
	PauseAuthorized(context.Context, invoiceninja.PauseDraft, func(context.Context) error) (invoiceninja.RecurringRecord, error)
	RescheduleAuthorized(context.Context, invoiceninja.ScheduleDraft, func(context.Context) error) (invoiceninja.RecurringRecord, error)
	ResumeAuthorized(context.Context, invoiceninja.ResumeDraft, func(context.Context) error) (invoiceninja.RecurringRecord, error)
}
type RenewalImportedRecurringWriter interface {
	Get(context.Context, string) (invoiceninja.RecurringRecord, error)
	PauseAuthorized(context.Context, invoiceninja.ImportedPauseDraft, func(context.Context) error) (invoiceninja.RecurringRecord, error)
	RescheduleAuthorized(context.Context, invoiceninja.ImportedScheduleDraft, func(context.Context) error) (invoiceninja.RecurringRecord, error)
	ResumeAuthorized(context.Context, invoiceninja.ImportedResumeDraft, func(context.Context) error) (invoiceninja.RecurringRecord, error)
}
type RenewalSUIWriter interface {
	GetRecord(context.Context, int64) (sui.ClientRecord, error)
	EnableAuthorized(context.Context, sui.EnableDraft, func(context.Context) error) (sui.ClientRecord, error)
}
type RenewalPaymentReader interface{ payments.Reader }
