// Package lifecycle detects invoices issued by managed recurring accounts.
// Stage one records only observations and Telegram notices; it never writes to
// Invoice Ninja or S-UI and never changes access state.
package lifecycle

import (
	"context"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/importorigin"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
)

const LeaseDuration = 25 * time.Minute

const (
	ReviewAssociation     = 4001
	ReviewInvoice         = 4002
	ReviewSentDate        = 4003
	ReviewBalance         = 4004
	ReviewCollision       = 4005
	ReviewExcluded        = 4006
	ReviewContract        = 4007
	ReviewPreviousPayment = 4008
)

type Claim struct {
	queue.Lease
	BotID, Generation int64
}

type Snapshot struct {
	ClientID, TelegramID, ChatID, BotID                              int64
	CustomerID, FirstInvoiceID, CurrentInvoiceID, RecurringID, SUIID string
	ImportedPrivateNotes                                             string
	RegistrationToken, ActivationToken, State                        string
	DesiredAccess, ManagedProvenance, SeenInvoiceOverflow            bool
	SeenInvoiceIDs                                                   []string
	ImportedOrigin                                                   importorigin.Ref
}

func (Snapshot) String() string   { return "[sensitive lifecycle snapshot]" }
func (Snapshot) GoString() string { return "[sensitive lifecycle snapshot]" }

type Invoice struct {
	ID, Number, Status, SentDate, Amount, Balance, PaidToDate string
}

func (Invoice) String() string   { return "[invoice lifecycle observation]" }
func (Invoice) GoString() string { return "[invoice lifecycle observation]" }

type Notices struct {
	Client            *queue.Draft
	Admins            []queue.Draft
	Review            []queue.Draft
	PredecessorReview []queue.Draft
}

type Store interface {
	RecordInvoiceLifecycleWake(context.Context, int64) error
	SeedInvoiceLifecycleChecks(context.Context, int64, []int64, int) (int, error)
	ClaimInvoiceLifecycleChecks(context.Context, int64, []int64, int) ([]Claim, error)
	LoadInvoiceLifecycleSnapshot(context.Context, Claim) (Snapshot, error)
	DeferInvoiceLifecycleCheck(context.Context, Claim, time.Duration, int) error
	FinishInvoiceLifecycleCheck(context.Context, Claim, Snapshot, *Invoice, int, Notices, time.Duration) error
}

type Reader interface {
	ListInvoices(context.Context, string) ([]invoiceninja.CycleInvoice, error)
	GetInvoice(context.Context, string) (invoiceninja.CycleInvoice, error)
}
