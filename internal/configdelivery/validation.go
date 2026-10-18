package configdelivery

import (
	"crypto/sha256"
	"encoding/json"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
)

func (d Decision) Validate() error {
	if d.Kind == Secret && d.Reason == 0 && !d.PaymentChanged {
		return nil
	}
	if d.Kind == Denial && d.Reason >= DeniedPolicy && d.Reason <= DeniedUnknownDelivery && (!d.PaymentChanged || d.Reason == DeniedPayment) {
		return nil
	}
	return ErrDenied
}
func (r Request) Validate() error {
	if r.OutboxID <= 0 || r.BotID <= 0 || r.ClientID <= 0 || r.ActorID <= 0 || r.ChatID != r.ActorID || r.ActivationID <= 0 || r.ClientRevision < 0 || r.PolicyRevision < 1 || r.PolicyHash == ([32]byte{}) || r.ProofHash == ([32]byte{}) || r.EntitlementHash == ([32]byte{}) || !activation.ValidID(r.InvoiceID) || r.CreatedAt.IsZero() || !r.ExpiresAt.After(r.CreatedAt) || r.ExpiresAt.Sub(r.CreatedAt) > RequestTTL {
		return ErrDenied
	}
	return nil
}
func (e Entitlement) Validate() error {
	if e.ActivationID <= 0 || e.Revision < 0 || activation.ValidateCompletedSnapshot(e.Activation, e.Binding) != nil {
		return ErrDenied
	}
	return nil
}

// Fingerprint contains no live counters. The local revision additionally fences
// authorization changes that return to the same apparent values (ABA).
func (e Entitlement) Fingerprint() ([32]byte, error) {
	if e.Validate() != nil {
		return [32]byte{}, ErrDenied
	}
	s := e.Activation
	s.WriteEpoch, s.WriterBusy = 0, false // unrelated clients' dispatches are irrelevant.
	raw, err := json.Marshal(struct {
		ActivationID, Revision int64
		Snapshot               activation.Snapshot
		BindingID              int64
		BindingName            string
		BindingHash            [32]byte
	}{e.ActivationID, e.Revision, s, e.Binding.ID, e.Binding.Name, e.Binding.CredentialsHash})
	if err != nil {
		return [32]byte{}, ErrDenied
	}
	return sha256.Sum256(raw), nil
}
func (e Entitlement) Deadline() (time.Time, error) {
	if e.Validate() != nil {
		return time.Time{}, ErrDenied
	}
	deadline, err := time.ParseInLocation(time.DateOnly, e.Activation.Proof.NextInvoiceDate, time.Local)
	if err != nil {
		return time.Time{}, ErrDenied
	}
	intent := e.Activation.Steps[activation.RecurringCreate]
	_, canonical, err := activation.NormalizeRecurringFacts(json.Number(intent.RecurringAmount), intent.RecurringDatetime)
	if err != nil {
		return time.Time{}, ErrDenied
	}
	instant, err := time.Parse(time.RFC3339Nano, canonical)
	if err != nil {
		return time.Time{}, ErrDenied
	}
	if instant.Before(deadline) {
		deadline = instant
	}
	return deadline, nil
}
func (v View) Matches() bool {
	if !v.Eligible || v.Request.Validate() != nil || v.Entitlement.Validate() != nil {
		return false
	}
	r, e := v.Request, v.Entitlement
	a := e.Activation.Account
	hash, err := e.Fingerprint()
	return err == nil && r.ClientID == a.ClientID && r.BotID == a.BotID && r.ActorID == a.TelegramID && r.ChatID == a.ChatID && r.ActivationID == e.ActivationID && r.ClientRevision == e.Revision && r.ProofHash == e.Activation.Proof.Hash && r.InvoiceID == a.InvoiceID && r.EntitlementHash == hash
}
