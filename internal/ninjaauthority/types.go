// Package ninjaauthority observes Invoice Ninja as authority. Observations are
// not payment proofs or authorizations to modify an existing S-UI account.
package ninjaauthority

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

var ErrConflict = errors.New("authority identity or intent conflict")
var ErrUnknown = errors.New("authority creation has an unresolved dispatch")

type Binding struct {
	CustomerID      string
	SUIID           int64
	Name            string
	CredentialsHash [32]byte
}

func (Binding) String() string   { return "[private authority binding]" }
func (Binding) GoString() string { return "[private authority binding]" }

type Observation struct {
	Actor                                                    int64
	CustomerID, RecurringID, InvoiceID                       string
	InvoiceNumber, InvoiceAmount, InvoiceStatus, InvoiceDate string
	ScopeHash                                                string
	StateKnown, Enabled                                      bool
	NextInvoiceDate                                          string
	SourceHash                                               [32]byte
	Binding                                                  Binding
	InboundIDs                                               []int64
	SUIEnabled                                               bool
	ObservedAt                                               time.Time
}

func (Observation) String() string   { return "[private Ninja authority observation]" }
func (Observation) GoString() string { return "[private Ninja authority observation]" }
func (o Observation) Hash() [32]byte {
	o.ObservedAt = time.Time{}
	raw, _ := json.Marshal(o)
	return sha256.Sum256(raw)
}

type CreateIntent struct {
	Draft      sui.AuthorityCreateDraft
	SourceHash [32]byte
	Dispatched bool
	Completed  bool
}

func (CreateIntent) String() string   { return "[sensitive authority create intent]" }
func (CreateIntent) GoString() string { return "[sensitive authority create intent]" }

type Store interface {
	WithAuthorityScan(context.Context, int64, func(context.Context) error) error
	LoadAuthorityBindings(context.Context, int64) (map[int64]Binding, error)
	SaveAuthorityObservation(context.Context, int64, Observation) error
	PrepareAuthorityCreate(context.Context, int64, Observation, sui.AuthorityCreateDraft) (CreateIntent, error)
	AuthorizeAuthorityCreate(context.Context, int64, int64, [32]byte) error
	CompleteAuthorityCreate(context.Context, int64, int64, sui.ClientRecord) error
	AuthorityNotice(context.Context, int64, string, int64, string, string, []int64) error
	ReconcileAuthorityNotices(context.Context, int64, []string, time.Time) error
}

type Ninja interface {
	ListCustomers(context.Context) ([]invoiceninja.Customer, error)
	ListRecurring(context.Context, string) ([]invoiceninja.RecurringRecord, error)
}
type Invoices interface {
	ListInvoices(context.Context, string) ([]invoiceninja.CycleInvoice, error)
}
type Panel interface {
	Inventory(context.Context) ([]sui.ClientRecord, error)
	FindCreated(context.Context, sui.AuthorityCreateDraft) (sui.ClientRecord, bool, error)
	CreateAuthorized(context.Context, sui.AuthorityCreateDraft, func(context.Context) error) (sui.ClientRecord, error)
}

type Config struct {
	BotID           int64
	SourceScopeHash string
	InboundIDs      []int64
	ExcludedIDs     []int64
	AdminIDs        []int64
}
