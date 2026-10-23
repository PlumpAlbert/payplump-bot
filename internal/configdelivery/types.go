// Package configdelivery resolves protected, actor-scoped period intents.
// Rendered subscription URLs exist only in memory at the final send boundary.
package configdelivery

import (
	"context"
	"errors"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

const (
	DeniedPolicy          = 4001
	DeniedEntitlement     = 4002
	DeniedPayment         = 4003
	DeniedSUI             = 4004
	DeniedRecurring       = 4005
	DeniedExpired         = 4006
	DeniedUnavailable     = 4007
	DeniedUnknownDelivery = 4008
	RequestTTL            = 5 * time.Minute
)

var (
	ErrDenied            = errors.New("protected configuration delivery denied")
	ErrAlreadyDispatched = errors.New("protected configuration transmission already reserved")
	ErrStalePolicy       = errors.New("protected configuration policy is stale or conflicting")
)

type Policy struct {
	Enabled  bool
	Revision int64
	Hash     [32]byte
}

type PeriodKind string

const (
	FirstPeriod   PeriodKind = "first"
	RenewalPeriod PeriodKind = "renewal"
)

type Request struct {
	OutboxID, BotID, ClientID, ActorID, ChatID, ActivationID int64
	RenewalID                                                int64
	Period                                                   PeriodKind
	ClientRevision, PolicyRevision                           int64
	PolicyHash, ProofHash, EntitlementHash                   [32]byte
	InvoiceID                                                string
	CreatedAt, ExpiresAt                                     time.Time
}

type RenewalAudit struct {
	OperationID                                                     int64
	TargetDate                                                      string
	RequiresPause, RequiresSchedule, RequiresEnable, RequiresResume bool
	StepIDs                                                         [4]string
	StepHashes                                                      [4][32]byte
	PlanHash                                                        [32]byte
	Amount                                                          string
	NextSendUTC                                                     time.Time
}

func (Request) String() string   { return "[protected configuration request]" }
func (Request) GoString() string { return "[protected configuration request]" }

type Entitlement struct {
	Activation   activation.Snapshot // immutable first-activation audit
	ActivationID int64
	Revision     int64
	Binding      sui.Binding
	Period       PeriodKind
	Current      payments.Snapshot
	CurrentProof payments.Proof
	Renewal      RenewalAudit
}

func (Entitlement) String() string   { return "[private configuration entitlement]" }
func (Entitlement) GoString() string { return "[private configuration entitlement]" }

type View struct {
	Request                        Request
	Entitlement                    Entitlement
	Eligible                       bool
	DenialCode                     int
	AlreadyDispatched, ChatBlocked bool
}

func (View) String() string   { return "[private configuration view]" }
func (View) GoString() string { return "[private configuration view]" }

type Kind string

const (
	Secret Kind = "secret"
	Denial Kind = "denial"
)

// PaymentChanged is set only for authoritative changed/nonverified evidence,
// never for an outage, incomplete GET or provider-version failure.
type Decision struct {
	Kind           Kind
	Reason         int
	PaymentChanged bool
}
type Permit struct {
	Kind     Kind
	Reason   int
	NotAfter time.Time
}

// Every authorization is one-shot. A reserved/unknown secret transmission pins
// a non-expiring per-chat guard; lease expiry or a new request cannot rearm it.
// Known Telegram rejection may release it, but requires a NEW explicit request.
type Store interface {
	InstallConfigurationPolicy(context.Context, int64, Policy) error
	LoadConfiguration(context.Context, queue.Message) (View, error)
	AuthorizeConfiguration(context.Context, queue.Message, View, Decision) (Permit, error)
}
