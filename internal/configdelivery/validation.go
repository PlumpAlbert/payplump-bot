package configdelivery

import (
	"crypto/sha256"
	"encoding/json"
	"strconv"
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
	origin := r.Origin
	if origin == "" {
		origin = OwnedOrigin
	}
	periodValid := (origin == OwnedOrigin && ((r.Period == FirstPeriod && r.RenewalID == 0) || (r.Period == RenewalPeriod && r.RenewalID > 0)) && r.ActivationID > 0 && r.ImportedOriginClientID == 0 && r.ImportedAdmissionPermissionRevision == 0 && r.ImportedRuntimeRevision == 0) ||
		(origin == ImportedOrigin && ((r.Period == ImportedBaselinePeriod && r.RenewalID == 0) || (r.Period == RenewalPeriod && r.RenewalID > 0)) && r.ActivationID == 0 && r.ImportedOriginClientID == r.ClientID && r.ImportedAdmissionPermissionRevision > 0 && r.ImportedRuntimeRevision > 0)
	if r.OutboxID <= 0 || r.BotID <= 0 || r.ClientID <= 0 || r.ActorID <= 0 || r.ChatID != r.ActorID || !periodValid || r.ClientRevision < 0 || r.PolicyRevision < 1 || r.PolicyHash == ([32]byte{}) || r.ProofHash == ([32]byte{}) || r.EntitlementHash == ([32]byte{}) || !activation.ValidID(r.InvoiceID) || r.CreatedAt.IsZero() || !r.ExpiresAt.After(r.CreatedAt) || r.ExpiresAt.Sub(r.CreatedAt) > RequestTTL {
		return ErrDenied
	}
	return nil
}
func (e Entitlement) Validate() error {
	if e.Imported != nil {
		return e.validateImported()
	}
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
		e.Current.CustomerID != e.Activation.Account.CustomerID || e.Current.FirstInvoiceID != e.Activation.Account.FirstInvoiceID ||
		e.Current.RecurringID != e.Activation.Account.RecurringID || e.Current.SUIID != e.Activation.Account.SUIID ||
		e.Current.PaymentDate != e.CurrentProof.PaymentDate || e.Current.NextInvoiceDate != e.CurrentProof.NextInvoiceDate || e.Current.InvoiceID != e.CurrentProof.InvoiceID ||
		e.CurrentProof.CustomerID != e.Current.CustomerID || e.CurrentProof.Validate() != nil || e.Renewal.TargetDate != e.CurrentProof.NextInvoiceDate ||
		e.Renewal.PlanHash == ([32]byte{}) || e.Renewal.Amount == "" || e.Renewal.NextSendUTC.IsZero() || e.Renewal.OriginMode != "" {
		return ErrDenied
	}
	if e.CurrentProof.Hash == e.Activation.Proof.Hash || !activation.ValidID(e.Current.InvoiceID) {
		return ErrDenied
	}
	if !validRenewalReceipts(e.Renewal) {
		return ErrDenied
	}
	amount, instant, err := activation.NormalizeRecurringFacts(json.Number(e.Renewal.Amount), e.Renewal.NextSendUTC.Format(time.RFC3339Nano))
	if err != nil || amount != e.Renewal.Amount || !instantTimeEqual(instant, e.Renewal.NextSendUTC) {
		return ErrDenied
	}
	return nil
}

func validRenewalReceipts(a RenewalAudit) bool {
	for i, required := range []bool{a.RequiresPause, a.RequiresSchedule, a.RequiresEnable, a.RequiresResume} {
		idPresent := a.StepIDs[i] != ""
		hashPresent := a.StepHashes[i] != ([32]byte{})
		if required {
			if !idPresent || !hashPresent {
				return false
			}
		} else if idPresent || hashPresent {
			return false
		}
	}
	return true
}

func validImportedRenewalStepIDs(a RenewalAudit, current payments.Snapshot) bool {
	expected := [4]string{current.RecurringID, current.RecurringID, current.SUIID, current.RecurringID}
	for i, required := range []bool{a.RequiresPause, a.RequiresSchedule, a.RequiresEnable, a.RequiresResume} {
		if required && a.StepIDs[i] != expected[i] {
			return false
		}
	}
	return true
}

func validRenewalFacts(a RenewalAudit) error {
	amount, instant, err := activation.NormalizeRecurringFacts(json.Number(a.Amount), a.NextSendUTC.Format(time.RFC3339Nano))
	if err != nil || amount != a.Amount || !instantTimeEqual(instant, a.NextSendUTC) {
		return ErrDenied
	}
	return nil
}

func (e Entitlement) validateImported() error {
	b, c, p := e.Imported, e.Current, e.CurrentProof
	if b == nil || e.ActivationID != 0 || !emptyActivationSnapshot(e.Activation) || e.Revision <= 0 ||
		!b.Origin.Valid() || b.Origin.ConfigurationRevision != e.Revision || b.Origin.RuntimeRevision <= 0 ||
		b.Origin.ClientID != c.ClientID || b.Origin.BotID != c.BotID || b.AdmissionPermissionRevision <= 0 || b.Origin.AdmissionRevision <= 0 ||
		b.InvoiceID == "" || (b.Lifecycle != "active" && b.Lifecycle != "paused") || b.ProofHash == ([32]byte{}) ||
		b.PublicBaseHash == ([32]byte{}) || b.Profile.Hash() == "" || len(b.InboundIDs) == 0 || !validPositiveIDs(b.InboundIDs) ||
		c.ClientID <= 0 || c.TelegramID <= 0 || c.ChatID != c.TelegramID || c.BotID <= 0 || c.CustomerID == "" ||
		c.FirstInvoiceID != "" || c.State != "active" || !c.DesiredAccess || c.SUIID == "" || c.RecurringID == "" ||
		c.ImportedOrigin != b.Origin || e.Binding.ID <= 0 || e.Binding.Name != strconv.FormatInt(c.TelegramID, 10) ||
		e.Binding.CredentialsHash == ([32]byte{}) || c.SUIID != strconv.FormatInt(e.Binding.ID, 10) {
		return ErrDenied
	}

	if b.Lifecycle == "active" {
		if b.PaidAnchorInvoiceID != b.InvoiceID {
			return ErrDenied
		}
	} else if b.PaidAnchorInvoiceID == "" || b.PaidAnchorInvoiceID == b.InvoiceID {
		return ErrDenied
	}

	if e.Period == ImportedBaselinePeriod {
		if e.Renewal != (RenewalAudit{}) || c.ImportedBaselineProofHash != b.ProofHash || b.Lifecycle != "active" ||
			b.NextSendUTC.IsZero() && !b.Profile.BaselinePaused() || c.InvoiceID != b.InvoiceID || p.Validate() != nil || p.Hash != b.ProofHash ||
			p.InvoiceID != b.InvoiceID || p.CustomerID != c.CustomerID || p.PaymentDate != c.PaymentDate || p.NextInvoiceDate != c.NextInvoiceDate {
			return ErrDenied
		}
		return nil
	}

	if e.Period != RenewalPeriod || c.ImportedBaselineProofHash != ([32]byte{}) || e.Renewal.OperationID <= 0 ||
		e.Renewal.TargetDate == "" || e.Renewal.PlanHash == ([32]byte{}) || e.Renewal.Amount == "" ||
		e.Renewal.NextSendUTC.IsZero() || (e.Renewal.OriginMode != "ledger" && e.Renewal.OriginMode != "imported_initial_paused") ||
		!validRenewalReceipts(e.Renewal) || validRenewalFacts(e.Renewal) != nil {
		return ErrDenied
	}
	if e.Renewal.OriginMode == "ledger" {
		if c.InvoiceID == b.InvoiceID || c.InvoiceID == b.PaidAnchorInvoiceID {
			return ErrDenied
		}
	} else {
		if b.Lifecycle != "paused" || c.InvoiceID != b.InvoiceID ||
			e.Renewal.RequiresPause || !e.Renewal.RequiresEnable || !e.Renewal.RequiresResume {
			return ErrDenied
		}
	}
	if b.PaidAnchorInvoiceID == c.InvoiceID || p.Validate() != nil || p.Hash == b.ProofHash || p.InvoiceID != c.InvoiceID ||
		p.CustomerID != c.CustomerID || p.PaymentDate != c.PaymentDate || p.NextInvoiceDate != c.NextInvoiceDate ||
		p.Amount != e.Renewal.Amount || e.Renewal.TargetDate != c.NextInvoiceDate || e.Renewal.TargetDate != p.NextInvoiceDate ||
		p.NextInvoiceDate == "" || p.InvoiceNumber == "" {
		return ErrDenied
	}
	if !validImportedRenewalStepIDs(e.Renewal, c) {
		return ErrDenied
	}
	return nil
}

func emptyActivationSnapshot(s activation.Snapshot) bool {
	return s.Account == (payments.Snapshot{}) && s.Token == "" && s.PolicyHash == ([32]byte{}) && emptyProof(s.Proof) && !s.PaymentReview && s.WriteEpoch == 0 && !s.WriterBusy && s.ReviewCode == 0 && len(s.Steps) == 0
}
func validPositiveIDs(ids []int64) bool {
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id <= 0 || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}
func emptyProof(p payments.Proof) bool {
	e := p.Evidence
	return e.InvoiceID == "" && e.CustomerID == "" && e.Amount == "" && e.CurrencyID == "" && e.PaymentDate == "" && e.NextInvoiceDate == "" && len(e.Allocations) == 0 && p.InvoiceNumber == "" && p.Hash == ([32]byte{}) && len(p.JSON) == 0
}
func instantTimeEqual(canonical string, instant time.Time) bool {
	parsed, err := time.Parse(time.RFC3339Nano, canonical)
	return err == nil && parsed.Equal(instant)
}

// Fingerprint contains no live counters. The local revision additionally fences authorization changes that return to the same apparent values (ABA).
func (e Entitlement) Fingerprint() ([32]byte, error) {
	if e.Validate() != nil {
		return [32]byte{}, ErrDenied
	}
	if e.Imported != nil {
		return e.importedFingerprint()
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
func (e Entitlement) importedFingerprint() ([32]byte, error) {
	b := e.Imported
	raw, err := json.Marshal(struct {
		ActivationID, Revision int64
		Origin                 struct {
			ClientID, BotID, AdmissionRevision, RuntimeRevision, ConfigurationRevision int64
			StableHash, PolicyHash, SourceScopeHash                                    [32]byte
		}
		AdmissionPermissionRevision               int64
		InvoiceID, PaidAnchorInvoiceID, Lifecycle string
		ProofHash                                 [32]byte
		PublicBaseHash                            [32]byte
		ProfileHash                               string
		InboundIDs                                []int64
		NextSendUTC                               time.Time
		BindingID                                 int64
		BindingName                               string
		BindingHash                               [32]byte
		Period                                    PeriodKind
		Current                                   payments.Snapshot
		CurrentProof                              payments.Proof
	}{e.ActivationID, e.Revision,
		struct {
			ClientID, BotID, AdmissionRevision, RuntimeRevision, ConfigurationRevision int64
			StableHash, PolicyHash, SourceScopeHash                                    [32]byte
		}{b.Origin.ClientID, b.Origin.BotID, b.Origin.AdmissionRevision, b.Origin.RuntimeRevision, b.Origin.ConfigurationRevision, b.Origin.StableHash, b.Origin.PolicyHash, b.Origin.SourceScopeHash},
		b.AdmissionPermissionRevision, b.InvoiceID, b.PaidAnchorInvoiceID, b.Lifecycle, b.ProofHash, b.PublicBaseHash, b.Profile.Hash(), b.InboundIDs, b.NextSendUTC, e.Binding.ID, e.Binding.Name, e.Binding.CredentialsHash, e.Period, e.Current, e.CurrentProof})
	if err != nil {
		return [32]byte{}, ErrDenied
	}
	base := sha256.Sum256(raw)
	if e.Period == ImportedBaselinePeriod {
		return base, nil
	}
	wrapped, err := json.Marshal(struct {
		Version  int
		BaseHash [32]byte
		Renewal  RenewalAudit
	}{1, base, e.Renewal})
	if err != nil {
		return [32]byte{}, ErrDenied
	}
	return sha256.Sum256(wrapped), nil
}

func (e Entitlement) Deadline() (time.Time, error) {
	if e.Validate() != nil {
		return time.Time{}, ErrDenied
	}
	if e.Imported != nil {
		instant := e.Imported.NextSendUTC
		if e.Period == RenewalPeriod {
			instant = e.Renewal.NextSendUTC
		}
		// The imported baseline send is a frozen source fact; renewal cycles use
		// the completed operation's new target instead.
		previous, err := time.Parse(time.DateOnly, e.Current.NextInvoiceDate)
		if err != nil {
			return time.Time{}, ErrDenied
		}
		deadline, err := billing.PaymentDeadline(previous.AddDate(0, 0, -1).Format(time.DateOnly))
		if err != nil {
			return time.Time{}, ErrDenied
		}
		// A paused billing schedule is not the end of a verified paid period.
		if !(e.Period == ImportedBaselinePeriod && e.Imported.Profile.BaselinePaused()) && instant.Before(deadline) {
			deadline = instant
		}
		return deadline, nil
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
		deadline, err = billing.PaymentDeadline(previous.AddDate(0, 0, -1).Format(time.DateOnly))
	} else {
		// Preserve the established first-cycle date-boundary behavior.
		deadline, err = time.ParseInLocation(time.DateOnly, proof.NextInvoiceDate, time.Local)
	}
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
	hash, err := e.Fingerprint()
	if e.Imported != nil {
		b, c := e.Imported, e.Current
		origin := r.Origin
		if origin == "" {
			origin = OwnedOrigin
		}
		invoice, proof := b.InvoiceID, b.ProofHash
		period, renewal := ImportedBaselinePeriod, int64(0)
		if e.Period == RenewalPeriod {
			invoice, proof = c.InvoiceID, e.CurrentProof.Hash
			period, renewal = RenewalPeriod, e.Renewal.OperationID
		}
		return err == nil && origin == ImportedOrigin && r.ClientID == c.ClientID && r.BotID == c.BotID && r.ActorID == c.TelegramID && r.ChatID == c.ChatID && r.ActivationID == 0 && r.ClientRevision == e.Revision && r.Period == period && r.RenewalID == renewal && r.ImportedOriginClientID == b.Origin.ClientID && r.ImportedAdmissionPermissionRevision == b.AdmissionPermissionRevision && r.ImportedRuntimeRevision == b.Origin.RuntimeRevision && r.ProofHash == proof && r.InvoiceID == invoice && r.EntitlementHash == hash
	}
	a := e.Activation.Account
	period := e.Period
	invoice, proof := a.InvoiceID, e.Activation.Proof
	if period == RenewalPeriod {
		invoice, proof = e.Current.InvoiceID, e.CurrentProof
	}
	return err == nil && r.ClientID == a.ClientID && r.BotID == a.BotID && r.ActorID == a.TelegramID && r.ChatID == a.ChatID && r.ActivationID == e.ActivationID && r.ClientRevision == e.Revision && r.Period == period && r.RenewalID == e.Renewal.OperationID && r.ProofHash == proof.Hash && r.InvoiceID == invoice && r.EntitlementHash == hash
}
