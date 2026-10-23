package configdelivery

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
)

func prepareImportedRenewal(t *testing.T, f *deliveryFixture, mode string) {
	t.Helper()
	prepareImportedBaseline(t, f)
	v := f.store.view
	e, b := v.Entitlement, v.Entitlement.Imported
	var proof *payments.Proof
	current := e.Current
	if mode == "ledger" || mode == "ledger_paused" {
		paid, next := "2040-02-01", "2040-03-01"
		f.p.invoiceID = "invoice-B"
		f.p.invoice = invoiceninja.SettlementInvoice{ID: "invoice-B", ClientID: current.CustomerID, Number: "INV-201", RecurringID: current.RecurringID, StatusID: "4", Date: paid, DueDate: paid, Amount: "200", Balance: "0", PaidToDate: "200", ExchangeRate: "1"}
		f.p.cash = []invoiceninja.SettlementPayment{{ID: "payment-B", ClientID: current.CustomerID, Date: paid, StatusID: "4", CurrencyID: "1", Amount: "200", Refunded: "0", Applied: "200", ExchangeRate: "1", IsManual: true, Allocations: []invoiceninja.SettlementAllocation{{ID: "allocation-B", InvoiceID: "invoice-B", PaymentID: "payment-B", Amount: "200", Refunded: "0"}}}}
		current.InvoiceID, current.PaymentDate, current.NextInvoiceDate = "invoice-B", paid, next
		verified := payments.Evaluate(current, f.p.customer, f.p.invoice, f.p.cash, f.cfg.CurrencyID, paid)
		if verified.Kind != payments.Verified {
			t.Fatal("current imported renewal proof fixture invalid")
		}
		proof = verified.Proof
		b.Lifecycle = "active"
		if mode == "ledger_paused" {
			b.Lifecycle = "paused"
			mode = "ledger"
		}
		if b.Lifecycle == "active" {
			b.PaidAnchorInvoiceID = b.InvoiceID
		} else {
			b.PaidAnchorInvoiceID = "invoice-anchor"
		}
		b.NextSendUTC = time.Date(2039, 12, 1, 12, 0, 0, 0, time.UTC)
		f.now = time.Date(2040, 2, 1, 12, 0, 0, 0, time.Local)
	} else {
		// Restoration proves a fresh payment for the imported invoice, distinct
		// from the immutable historical anchor proof.
		paid := "2040-01-30"
		f.p.invoice.Date, f.p.invoice.DueDate = paid, paid
		f.p.cash[0].Date = paid
		current.PaymentDate = paid
		verified := payments.Evaluate(current, f.p.customer, f.p.invoice, f.p.cash, f.cfg.CurrencyID, paid)
		if verified.Kind != payments.Verified {
			t.Fatal("restoration payment proof fixture invalid")
		}
		proof = verified.Proof
		current.PaymentDate, current.NextInvoiceDate = proof.PaymentDate, proof.NextInvoiceDate
		b.Lifecycle = "paused"
		b.PaidAnchorInvoiceID = "invoice-anchor"
		b.NextSendUTC = time.Time{}
		mode = "imported_initial_paused"
	}
	current.ImportedBaselineProofHash = [32]byte{}
	e.Period, e.Current, e.CurrentProof = RenewalPeriod, current, *proof
	f.rec.record.NextSendDate = proof.NextInvoiceDate
	f.rec.record.NextSendDatetime = proof.NextInvoiceDate + "T12:00:00Z"
	instant, err := invoiceninja.ParseRecurringUTC(f.rec.record.NextSendDatetime)
	if err != nil {
		t.Fatal("invalid renewal schedule")
	}
	e.Renewal = RenewalAudit{OriginMode: mode, OperationID: 51, TargetDate: proof.NextInvoiceDate, PlanHash: [32]byte{4}, Amount: "200", NextSendUTC: instant}
	if mode == "imported_initial_paused" {
		e.Renewal.RequiresSchedule, e.Renewal.RequiresEnable, e.Renewal.RequiresResume = true, true, true
		e.Renewal.StepIDs = [4]string{"", current.RecurringID, current.SUIID, current.RecurringID}
		e.Renewal.StepHashes = [4][32]byte{{}, {5}, {6}, {7}}
	}
	hash, err := e.Fingerprint()
	if err != nil {
		t.Fatalf("imported renewal rejected: %v", err)
	}
	r := v.Request
	r.Period, r.RenewalID, r.InvoiceID, r.ProofHash = RenewalPeriod, e.Renewal.OperationID, current.InvoiceID, proof.Hash
	r.EntitlementHash, r.CreatedAt, r.ExpiresAt = hash, f.now, f.now.Add(RequestTTL)
	f.store.view = View{Request: r, Entitlement: e, Eligible: true}
	if !f.store.view.Matches() {
		t.Fatal("valid imported renewal view did not match")
	}
}

func TestImportedRenewalModesAndCompletedDelivery(t *testing.T) {
	for _, mode := range []string{"ledger", "ledger_paused", "imported_initial_paused"} {
		t.Run(mode, func(t *testing.T) {
			f := newDeliveryFixture(t)
			prepareImportedRenewal(t, f, mode)
			id, err := f.deliver()
			if err != nil || id != 7001 || f.store.decision != (Decision{Kind: Secret}) {
				t.Fatal("valid completed imported renewal was not delivered")
			}
			if f.store.view.Entitlement.Imported.ProofHash == f.store.view.Entitlement.CurrentProof.Hash {
				t.Fatal("current payment overwrote original imported proof")
			}
		})
	}
}

func TestImportedRenewalRejectsAuthorityAndReceiptMutations(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*View)
	}{
		{"unpaid-anchor", func(v *View) { v.Entitlement.Imported.PaidAnchorInvoiceID = v.Entitlement.Current.InvoiceID }},
		{"current-proof", func(v *View) { v.Entitlement.CurrentProof.Hash = v.Entitlement.Imported.ProofHash }},
		{"mode", func(v *View) { v.Entitlement.Renewal.OriginMode = "unknown" }},
		{"missing-operation", func(v *View) { v.Entitlement.Renewal.OperationID = 0 }},
		{"missing-plan", func(v *View) { v.Entitlement.Renewal.PlanHash = [32]byte{} }},
		{"missing-target", func(v *View) { v.Entitlement.Renewal.NextSendUTC = time.Time{} }},
		{"current-baseline-pin", func(v *View) { v.Entitlement.Current.ImportedBaselineProofHash = v.Entitlement.Imported.ProofHash }},
		{"amount-does-not-match-current-proof", func(v *View) { v.Entitlement.Renewal.Amount = "201" }},
		{"unexpected-step-id", func(v *View) { v.Entitlement.Renewal.StepIDs[0] = "recurring-A" }},
		{"unexpected-step-hash", func(v *View) { v.Entitlement.Renewal.StepHashes[0] = [32]byte{9} }},
		{"active-anchor-drift", func(v *View) { v.Entitlement.Imported.PaidAnchorInvoiceID = "other-anchor" }},
		{"paused-origin-anchor-equals-debt-invoice", func(v *View) {
			v.Entitlement.Imported.Lifecycle = "paused"
			v.Entitlement.Imported.PaidAnchorInvoiceID = v.Entitlement.Imported.InvoiceID
		}},

		{"wrong-initial-mode", func(v *View) {
			v.Entitlement.Renewal.OriginMode = "imported_initial_paused"
			v.Entitlement.Imported.Lifecycle = "paused"
		}},
		{"historical-anchor-fact", func(v *View) { v.Entitlement.Imported.ProofHash[0] ^= 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeliveryFixture(t)
			prepareImportedRenewal(t, f, "ledger")
			v := f.store.view
			tc.mutate(&v)
			if v.Entitlement.Validate() == nil && v.Matches() {
				t.Fatal("mutated imported renewal view accepted")
			}
		})
	}
}

func TestImportedInitialRenewalAllowsCompletedTwoWriterRestoration(t *testing.T) {
	f := newDeliveryFixture(t)
	prepareImportedRenewal(t, f, "imported_initial_paused")
	a := &f.store.view.Entitlement.Renewal
	a.RequiresSchedule, a.StepIDs[1], a.StepHashes[1] = false, "", [32]byte{}
	if f.store.view.Entitlement.Validate() != nil {
		t.Fatal("valid two-writer initial restoration was rejected")
	}
}

func TestImportedInitialRenewalRequiresKnownCompletedReceipts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*RenewalAudit)
	}{
		{"missing-enable", func(a *RenewalAudit) { a.RequiresEnable = false; a.StepIDs[2] = ""; a.StepHashes[2] = [32]byte{} }},
		{"missing-resume", func(a *RenewalAudit) { a.RequiresResume = false; a.StepIDs[3] = ""; a.StepHashes[3] = [32]byte{} }},
		{"unexpected-pause", func(a *RenewalAudit) {
			a.RequiresPause = true
			a.StepIDs[0] = a.StepIDs[1]
			a.StepHashes[0] = [32]byte{9}
		}},
		{"wrong-recurring-id", func(a *RenewalAudit) { a.StepIDs[1] = "not-the-recurring-id" }},
		{"wrong-sui-id", func(a *RenewalAudit) { a.StepIDs[2] = "not-the-sui-id" }},
		{"wrong-resume-id", func(a *RenewalAudit) { a.StepIDs[3] = "not-the-recurring-id" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeliveryFixture(t)
			prepareImportedRenewal(t, f, "imported_initial_paused")
			tc.mutate(&f.store.view.Entitlement.Renewal)
			if f.store.view.Entitlement.Validate() == nil {
				t.Fatal("invalid initial-restoration receipt accepted")
			}
		})
	}
}

func TestImportedRenewalPaymentUsesCurrentPaidProof(t *testing.T) {
	t.Run("source GET outage is non-sticky", func(t *testing.T) {
		f := newDeliveryFixture(t)
		prepareImportedRenewal(t, f, "ledger")
		f.rec.hook = func(int) error { return context.DeadlineExceeded }
		f.assertDenial(DeniedUnavailable, false)
	})
	t.Run("current invoice number drift is sticky", func(t *testing.T) {
		f := newDeliveryFixture(t)
		prepareImportedRenewal(t, f, "ledger")
		f.p.invoice.Number = "INV-CHANGED"
		f.assertDenial(DeniedPayment, true)
	})
}

func TestImportedRenewalFingerprintIncludesAuditAndKeepsLegacyHashes(t *testing.T) {
	f := newDeliveryFixture(t)
	prepareImportedBaseline(t, f)
	baseline := f.store.view.Entitlement
	got, err := baseline.Fingerprint()
	if err != nil {
		t.Fatal("baseline fingerprint failed")
	}
	want, err := legacyImportedBaselineFingerprint(baseline)
	if err != nil || got != want {
		t.Fatal("imported baseline fingerprint changed from legacy encoding")
	}

	own, _, _ := renewalEntitlement(t)
	got, err = own.Fingerprint()
	if err != nil {
		t.Fatal("owned renewal fingerprint failed")
	}
	want, err = legacyOwnedRenewalFingerprint(own)
	if err != nil || got != want {
		t.Fatal("owned renewal fingerprint changed from legacy encoding")
	}

	fresh := newDeliveryFixture(t)
	prepareImportedRenewal(t, fresh, "ledger")
	e := fresh.store.view.Entitlement
	before, err := e.Fingerprint()
	if err != nil {
		t.Fatal("imported renewal fingerprint failed")
	}
	e.Renewal.StepIDs[0], e.Renewal.StepHashes[0], e.Renewal.RequiresPause = e.Current.RecurringID, [32]byte{8}, true
	after, err := e.Fingerprint()
	if err != nil || before == after {
		t.Fatal("renewal receipt omitted from fingerprint")
	}
}

type legacyRenewalAudit struct {
	OperationID                                                     int64
	TargetDate                                                      string
	RequiresPause, RequiresSchedule, RequiresEnable, RequiresResume bool
	StepIDs                                                         [4]string
	StepHashes                                                      [4][32]byte
	PlanHash                                                        [32]byte
	Amount                                                          string
	NextSendUTC                                                     time.Time
}

// These test-only encoders preserve the pre-OriginMode fingerprint projections.
func legacyImportedBaselineFingerprint(e Entitlement) ([32]byte, error) {
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
		b.AdmissionPermissionRevision, b.InvoiceID, b.PaidAnchorInvoiceID, b.Lifecycle, b.ProofHash, b.PublicBaseHash, b.Profile.Hash(), b.InboundIDs, b.NextSendUTC,
		e.Binding.ID, e.Binding.Name, e.Binding.CredentialsHash, e.Period, e.Current, e.CurrentProof})
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(raw), nil
}

func legacyOwnedRenewalFingerprint(e Entitlement) ([32]byte, error) {
	s := e.Activation
	s.WriteEpoch, s.WriterBusy = 0, false
	raw, err := json.Marshal(struct {
		ActivationID, Revision int64
		Snapshot               activation.Snapshot
		BindingID              int64
		BindingName            string
		BindingHash            [32]byte
		Period                 PeriodKind
		Current                payments.Snapshot
		CurrentProof           payments.Proof
		Renewal                legacyRenewalAudit
	}{e.ActivationID, e.Revision, s, e.Binding.ID, e.Binding.Name, e.Binding.CredentialsHash, e.Period, e.Current, e.CurrentProof, legacyRenewalAudit{
		e.Renewal.OperationID, e.Renewal.TargetDate, e.Renewal.RequiresPause, e.Renewal.RequiresSchedule, e.Renewal.RequiresEnable, e.Renewal.RequiresResume,
		e.Renewal.StepIDs, e.Renewal.StepHashes, e.Renewal.PlanHash, e.Renewal.Amount, e.Renewal.NextSendUTC}})
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(raw), nil
}
