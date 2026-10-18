// Package activation journals first-payment provisioning. It never treats a
// stored payment observation as authorization for a new external effect.
package activation

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
)

const Kind = "activate_first_subscription"

type Step string

const (
	SUICreate       Step = "sui_create"
	RecurringCreate Step = "recurring_create"
	SUIEnable       Step = "sui_enable"
	RecurringStart  Step = "recurring_start"
)

func OrderedSteps() []Step { return []Step{SUICreate, RecurringCreate, SUIEnable, RecurringStart} }
func (s Step) Valid() bool {
	switch s {
	case SUICreate, RecurringCreate, SUIEnable, RecurringStart:
		return true
	}
	return false
}

const (
	ReviewAssociation  = 3001
	ReviewPayment      = 3002
	ReviewPeriod       = 3003
	ReviewSUI          = 3004
	ReviewRecurring    = 3005
	ReviewUnknownWrite = 3006
	ReviewContract     = 3007
	ReviewExcluded     = 3008
	ReviewPolicy       = 3009
)

var (
	ErrReviewRequired = errors.New("activation requires manual review")
	ErrGuardBusy      = errors.New("activation external writer is reserved")
	ErrIntentExists   = errors.New("activation write intent already exists; do not transmit")
)

// Intent is sensitive. ExternalID is known only after a validated response or a
// positive create reconciliation. Absence NEVER permits another POST.
type Intent struct {
	Payload         json.RawMessage
	ExternalID      string
	DispatchAttempt int
	// Recurring totals and UTC schedule are immutable known-response facts,
	// not reconstructed from a later GET or a locally calculated tax total.
	RecurringAmount   string
	RecurringDatetime string
}

func (Intent) String() string   { return "[sensitive activation intent]" }
func (Intent) GoString() string { return "[sensitive activation intent]" }

// Snapshot binds an immutable flow to its original confirmation. PaymentReview
// includes BOTH sticky confirmation review and the payment check review code.
// Account is revalidated under locks before reserving a write and finalization.
type Snapshot struct {
	Account       payments.Snapshot
	Token         string
	PolicyHash    [32]byte
	Proof         payments.Proof
	PaymentReview bool
	// WriteEpoch is captured before adapter preflight. Any intervening dispatch
	// invalidates it even after that dispatch's guard is released.
	WriteEpoch int64
	WriterBusy bool
	ReviewCode int
	Steps      map[Step]Intent
}

func (Snapshot) String() string   { return "[sensitive activation snapshot]" }
func (Snapshot) GoString() string { return "[sensitive activation snapshot]" }

// StartRequest is the exact first-start wire action; its schedule/financial
// policy is already frozen in the preceding recurring_create intent.
type StartRequest struct {
	IDs    []string `json:"ids"`
	Action string   `json:"action"`
}

// All writes are durably reserved through the adapter's final authorization
// callback, AFTER its read-only preflight. One global durable guard serializes
// these effects across application processes; unknown results do not expire it.
// Confirm only records a known result; it is NOT a new entitlement decision.
type Store interface {
	SeedActivations(context.Context, int64, []int64, [32]byte) (int, error)
	ClaimActivations(context.Context, int64, int) ([]queue.Operation, error)
	RetryOperation(context.Context, queue.Lease, time.Duration, int) error
	LoadActivation(context.Context, queue.Lease, int64) (Snapshot, error)
	PrepareActivationWrite(context.Context, queue.Lease, Snapshot, Step, json.RawMessage, string) (bool, error)
	ConfirmActivationStep(context.Context, queue.Lease, Step, string) error
	ConfirmRecurringActivationStep(context.Context, queue.Lease, Step, string, json.Number, string) error
	HoldActivation(context.Context, queue.Lease, int, []queue.Draft) error
	FinishActivation(context.Context, queue.Lease, Snapshot, string, []queue.Draft) error
}
