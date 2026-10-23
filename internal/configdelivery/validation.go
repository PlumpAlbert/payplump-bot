package configdelivery

import (
	"crypto/sha256"
	"encoding/json"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/billing"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
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
	periodValid := (r.Period == FirstPeriod && r.RenewalID == 0) || (r.Period == RenewalPeriod && r.RenewalID > 0)
	if r.OutboxID <= 0 || r.BotID <= 0 || r.ClientID <= 0 || r.ActorID <= 0 || r.ChatID != r.ActorID || r.ActivationID <= 0 || !periodValid || r.ClientRevision < 0 || r.PolicyRevision < 1 || r.PolicyHash == ([32]byte{}) || r.ProofHash == ([32]byte{}) || r.EntitlementHash == ([32]byte{}) || !activation.ValidID(r.InvoiceID) || r.CreatedAt.IsZero() || !r.ExpiresAt.After(r.CreatedAt) || r.ExpiresAt.Sub(r.CreatedAt) > RequestTTL {
		return ErrDenied
	}
	return nil
}
func (e Entitlement) Validate() error {
	if e.ActivationID <= 0 || e.Revision < 0 || activation.ValidateCompletedSnapshot(e.Activation, e.Binding) != nil {
		return ErrDenied
	}
	if e.Period == FirstPeriod {
		if e.Current != (payments.Snapshot{}) || !emptyProof(e.CurrentProof) || e.Renewal != (RenewalAudit{}) {
			return ErrDenied
		}
		return nil
	}
	if e.Period != RenewalPeriod || e.Current.InvoiceID == "" || e.Current.InvoiceID == e.Activation.Account.FirstInvoiceID ||
		e.Renewal.OperationID <= 0 || e.Renewal.TargetDate == "" || e.Activation.Account.State != "active" || !e.Activation.Account.DesiredAccess ||
		e.Current.State != "active" || !e.Current.DesiredAccess || e.Current.ClientID != e.Activation.Account.ClientID ||
		e.Current.TelegramID != e.Activation.Account.TelegramID || e.Current.ChatID != e.Activation.Account.ChatID || e.Current.BotID != e.Activation.Account.BotID ||
		e.Current.CustomerID != e.Activation.Account.CustomerID || e.Current.FirstInvoiceID != e.Activation.Account.FirstInvoiceID || e.Current.RecurringID != e.Activation.Account.RecurringID || e.Current.SUIID != e.Activation.Account.SUIID ||
		e.Current.PaymentDate != e.CurrentProof.PaymentDate || e.Current.NextInvoiceDate != e.CurrentProof.NextInvoiceDate || e.Current.InvoiceID != e.CurrentProof.InvoiceID ||
		e.CurrentProof.CustomerID != e.Current.CustomerID || e.CurrentProof.Validate() != nil || e.Renewal.TargetDate != e.CurrentProof.NextInvoiceDate || e.Renewal.PlanHash == ([32]byte{}) || e.Renewal.Amount == "" || e.Renewal.NextSendUTC.IsZero() {
		return ErrDenied
	}
	if e.CurrentProof.Hash == e.Activation.Proof.Hash || !activation.ValidID(e.Current.InvoiceID) {
		return ErrDenied
	}
	for i, required := range []bool{e.Renewal.RequiresPause, e.Renewal.RequiresSchedule, e.Renewal.RequiresEnable, e.Renewal.RequiresResume} {
		if required != (e.Renewal.StepIDs[i] != "" && e.Renewal.StepHashes[i] != ([32]byte{})) {
			return ErrDenied
		}
	}
	amount, instant, err := activation.NormalizeRecurringFacts(json.Number(e.Renewal.Amount), e.Renewal.NextSendUTC.Format(time.RFC3339Nano))
	if err != nil || amount != e.Renewal.Amount || !instantTimeEqual(instant, e.Renewal.NextSendUTC) {
		return ErrDenied
	}
	return nil
}

func emptyProof(p payments.Proof) bool {
	e := p.Evidence
	return e.InvoiceID == "" && e.CustomerID == "" && e.Amount == "" && e.CurrencyID == "" && e.PaymentDate == "" && e.NextInvoiceDate == "" && len(e.Allocations) == 0 && p.InvoiceNumber == "" && p.Hash == ([32]byte{}) && len(p.JSON) == 0
}

func instantTimeEqual(canonical string, instant time.Time) bool {
	parsed, err := time.Parse(time.RFC3339Nano, canonical)
	return err == nil && parsed.Equal(instant)
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
		Period                 PeriodKind
		Current                payments.Snapshot
		CurrentProof           payments.Proof
		Renewal                RenewalAudit
	}{e.ActivationID, e.Revision, s, e.Binding.ID, e.Binding.Name, e.Binding.CredentialsHash,
		e.Period, e.Current, e.CurrentProof, e.Renewal})
	if err != nil {
		return [32]byte{}, ErrDenied
	}
	return sha256.Sum256(raw), nil
}
func (e Entitlement) Deadline() (time.Time, error) {
	if e.Validate() != nil {
		return time.Time{}, ErrDenied
	}
	proof := e.Activation.Proof
	var instant time.Time
	if e.Period == RenewalPeriod {
		proof = e.CurrentProof
		instant = e.Renewal.NextSendUTC
	} else {
		intent := e.Activation.Steps[activation.RecurringCreate]
		_, canonical, err := activation.NormalizeRecurringFacts(json.Number(intent.RecurringAmount), intent.RecurringDatetime)
		if err != nil {
			return time.Time{}, ErrDenied
		}
		instant, err = time.Parse(time.RFC3339Nano, canonical)
		if err != nil {
			return time.Time{}, ErrDenied
		}
	}
	var deadline time.Time
	var err error
	if e.Period == RenewalPeriod {
		// Find the start of the next local calendar date via the end of the
		// previous UTC calendar date; this handles DST gaps and folds correctly.
		previous, err := time.Parse(time.DateOnly, proof.NextInvoiceDate)
		if err != nil {
			return time.Time{}, ErrDenied
		}
		previous = previous.AddDate(0, 0, -1)
		deadline, err = billing.PaymentDeadline(previous.Format(time.DateOnly))
		if err != nil {
			return time.Time{}, ErrDenied
		}
	} else {
		// Preserve the established first-cycle date-boundary behavior.
		deadline, err = time.ParseInLocation(time.DateOnly, proof.NextInvoiceDate, time.Local)
		if err != nil {
			return time.Time{}, ErrDenied
		}
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
	period := e.Period
	invoice, proof := a.InvoiceID, e.Activation.Proof
	if period == RenewalPeriod {
		invoice, proof = e.Current.InvoiceID, e.CurrentProof
	}
	return err == nil && r.ClientID == a.ClientID && r.BotID == a.BotID && r.ActorID == a.TelegramID && r.ChatID == a.ChatID && r.ActivationID == e.ActivationID && r.ClientRevision == e.Revision && r.Period == period && r.RenewalID == e.Renewal.OperationID && r.ProofHash == proof.Hash && r.InvoiceID == invoice && r.EntitlementHash == hash
}
