// Package registration executes explicitly enabled first-invoice onboarding.
// It never grants S-UI access or creates recurring invoices.
package registration

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/queue"
)

type Step string

const (
	CustomerCreate Step = "customer_create"
	InvoiceCreate  Step = "invoice_create"
	InvoiceIssue   Step = "invoice_issue"
)

func (s Step) Valid() bool {
	return s == CustomerCreate || s == InvoiceCreate || s == InvoiceIssue
}

// Codes are safe classifications, never remote error text or personal data.
const (
	ReviewExistingCustomer = 1001
	ReviewCustomerConflict = 1002
	ReviewInvoiceConflict  = 1003
	ReviewUnknownWrite     = 1004
	ReviewInvoiceState     = 1005
	ReviewContract         = 1006
	ReviewLocalAssociation = 1007
	ReviewExcludedUser     = 1008
)

var ErrReviewRequired = errors.New("registration requires manual review")

// Intent is immutable once stored. ExternalID is set only after validating an
// external response or a positive reconciliation. An absent result NEVER grants
// another write permission for the same step.
type Intent struct {
	Payload    json.RawMessage
	ExternalID string
}

// Snapshot contains private account data and must never be logged or exposed
// through a generic JSON report. It is local state, not proof of payment.
type Snapshot struct {
	ClientID, TelegramID, ChatID, BotID int64
	DisplayName, Token                  string
	ExistingCustomerID                  string
	ExistingCurrentInvoiceID            string
	ExistingFirstInvoiceID              string
	ExistingRecurringID                 string
	ExistingSUIID                       string
	SubscriptionState                   string
	DesiredAccess                       bool
	Steps                               map[Step]Intent
}

type Store interface {
	ClaimOperations(context.Context, []string, int, time.Duration) ([]queue.Operation, error)
	RetryOperation(context.Context, queue.Lease, time.Duration, int) error
	LoadRegistration(context.Context, queue.Lease, int64) (Snapshot, error)
	PrepareRegistrationWrite(context.Context, queue.Lease, Step, json.RawMessage) (bool, error)
	ConfirmRegistrationStep(context.Context, queue.Lease, Step, string) error
	HoldRegistration(context.Context, queue.Lease, int, []queue.Draft) error
	FinishRegistration(context.Context, queue.Lease, string, string, []queue.Draft) error
}
