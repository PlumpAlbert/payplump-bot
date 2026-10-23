package configdelivery

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestImportedBaselineRejectsAuthorityAndRequestMutations(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*View)
	}{
		{"originally-paused", func(v *View) { v.Entitlement.Imported.Lifecycle = "paused" }},
		{"historical-anchor", func(v *View) { v.Entitlement.Imported.PaidAnchorInvoiceID = "old-anchor" }},
		{"invoice-drift", func(v *View) { v.Entitlement.Current.InvoiceID = "invoice-next" }},
		{"subsequent-period", func(v *View) { v.Entitlement.Current.FirstInvoiceID = "invoice-first" }},
		{"hybrid-activation", func(v *View) { v.Entitlement.Activation.Token = "owned-token" }},
		{"current-first-invoice", func(v *View) { v.Entitlement.Current.FirstInvoiceID = v.Entitlement.Current.InvoiceID }},
		{"overdue-state", func(v *View) { v.Entitlement.Current.State = "overdue" }},
		{"no-desired-access", func(v *View) { v.Entitlement.Current.DesiredAccess = false }},
		{"actor-chat-mismatch", func(v *View) { v.Entitlement.Current.ChatID++ }},
		{"noncanonical-binding-name", func(v *View) { v.Entitlement.Binding.Name = "pp-owned-style-name" }},
		{"origin-identity", func(v *View) { v.Entitlement.Imported.Origin.ClientID++ }},
		{"origin-runtime-revision", func(v *View) { v.Entitlement.Imported.Origin.RuntimeRevision++ }},
		{"configuration-revision", func(v *View) { v.Entitlement.Revision++ }},
		{"request-origin-client", func(v *View) { v.Request.ImportedOriginClientID++ }},
		{"request-origin-client-not-client", func(v *View) { v.Request.ImportedOriginClientID = v.Request.ClientID + 1 }},
		{"request-activation-id", func(v *View) { v.Request.ActivationID = 9 }},
		{"mixed-request-origin-fields", func(v *View) { v.Request.ImportedAdmissionPermissionRevision++ }},
		{"current-proof-hash", func(v *View) { v.Entitlement.CurrentProof.Hash[0] ^= 1 }},
		{"current-proof-amount", func(v *View) { v.Entitlement.CurrentProof.Amount = "250" }},
		{"duplicate-inbound", func(v *View) { v.Entitlement.Imported.InboundIDs = []int64{7, 7} }},
		{"invalid-inbound", func(v *View) { v.Entitlement.Imported.InboundIDs = []int64{0} }},
		{"binding-credential-hash", func(v *View) { v.Entitlement.Binding.CredentialsHash[0] ^= 1 }},
		{"hybrid-entitlement-period", func(v *View) { v.Entitlement.Period = FirstPeriod }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeliveryFixture(t)
			prepareImportedBaseline(t, f)
			view := f.store.view
			tc.mutate(&view)
			if view.Matches() {
				t.Fatal("mutated imported entitlement unexpectedly matched")
			}
		})
	}
}

func TestImportedBaselineRequestAndPublicOriginShape(t *testing.T) {
	f := newDeliveryFixture(t)
	prepareImportedBaseline(t, f)
	r := f.store.view.Request
	if err := r.Validate(); err != nil {
		t.Fatal("valid imported request rejected")
	}
	r.ImportedOriginClientID = r.ClientID + 1
	if err := r.Validate(); err == nil {
		t.Fatal("imported origin ID not equal to local client was accepted")
	}
}

func TestImportedBaselineProviderDriftAndPaymentClassification(t *testing.T) {
	t.Run("source GET outage is non-sticky", func(t *testing.T) {
		f := newDeliveryFixture(t)
		prepareImportedBaseline(t, f)
		f.sui.hook = func(int) error { return context.DeadlineExceeded }
		f.assertDenial(DeniedUnavailable, false)
	})
	t.Run("payment GET outage is non-sticky", func(t *testing.T) {
		f := newDeliveryFixture(t)
		prepareImportedBaseline(t, f)
		f.p.hook = func(string, int) error { return context.DeadlineExceeded }
		f.assertDenial(DeniedUnavailable, false)
	})
	t.Run("invoice number changed with same proof hash is sticky", func(t *testing.T) {
		f := newDeliveryFixture(t)
		prepareImportedBaseline(t, f)
		f.p.invoice.Number = "INV-CHANGED"
		f.assertDenial(DeniedPayment, true)
	})
	t.Run("authoritative changed payment proof is sticky", func(t *testing.T) {
		f := newDeliveryFixture(t)
		prepareImportedBaseline(t, f)
		f.p.invoice.Amount, f.p.invoice.PaidToDate = "250", "250"
		f.p.cash[0].Amount, f.p.cash[0].Applied, f.p.cash[0].Allocations[0].Amount = "250", "250", "250"
		f.assertDenial(DeniedPayment, true)
	})
	t.Run("pending payment is sticky candidate", func(t *testing.T) {
		f := newDeliveryFixture(t)
		prepareImportedBaseline(t, f)
		f.p.invoice.StatusID = "2"
		f.p.invoice.Balance = "200"
		f.p.invoice.PaidToDate = "0"
		f.assertDenial(DeniedPayment, true)
	})
	t.Run("inbound drift blocks send", func(t *testing.T) {
		f := newDeliveryFixture(t)
		prepareImportedBaseline(t, f)
		f.sui.record = deliveryMutateRecord(t, f.sui.record, func(fields map[string]json.RawMessage) { fields["inbounds"] = deliveryJSON(t, []int64{7}) })
		f.assertDenial(DeniedSUI, false)
	})
	t.Run("credential drift blocks send", func(t *testing.T) {
		f := newDeliveryFixture(t)
		prepareImportedBaseline(t, f)
		f.sui.record = deliveryMutateRecord(t, f.sui.record, func(fields map[string]json.RawMessage) {
			var config map[string]map[string]any
			if json.Unmarshal(fields["config"], &config) != nil {
				t.Fatal("cannot mutate test credentials")
			}
			config["mixed"]["password"] = "changed-imported-credential"
			fields["config"] = deliveryJSON(t, config)
		})
		f.assertDenial(DeniedSUI, false)
	})
	t.Run("frozen imported price drift blocks send", func(t *testing.T) {
		f := newDeliveryFixture(t)
		prepareImportedBaseline(t, f)
		f.rec.record.Amount = "250"
		f.assertDenial(DeniedRecurring, false)
	})
	t.Run("late recurring source change blocks send", func(t *testing.T) {
		f := newDeliveryFixture(t)
		prepareImportedBaseline(t, f)
		f.rec.hook = func(n int) error {
			if n == 2 {
				f.rec.record.Amount = "250"
			}
			return nil
		}
		f.assertDenial(DeniedRecurring, false)
	})
}

func TestImportedBaselineFrozenProfileAndDeadline(t *testing.T) {
	t.Run("legacy notes and imported price remain frozen", func(t *testing.T) {
		f := newDeliveryFixture(t)
		prepareImportedBaseline(t, f)
		profileHash := f.store.view.Entitlement.Imported.Profile.Hash()
		if profileHash == "" || f.rec.record.PrivateNotes != "" || len(f.rec.record.LineItems) != 1 || f.rec.record.LineItems[0].Notes != "" || f.rec.record.Amount != "200" || f.rec.record.LineItems[0].Cost != "200" {
			t.Fatal("fixture does not retain imported legacy notes and price")
		}
		if _, err := f.deliver(); err != nil || f.store.decision.Kind != Secret {
			t.Fatal("valid frozen imported profile denied")
		}
	})
	t.Run("local payment-date boundary expired", func(t *testing.T) {
		f := newDeliveryFixture(t)
		prepareImportedBaseline(t, f)
		f.now = time.Date(2040, 2, 29, 0, 0, 0, 0, time.Local)
		f.store.view.Request.CreatedAt, f.store.view.Request.ExpiresAt = f.now.Add(-time.Minute), f.now.Add(time.Minute)
		f.assertDenial(DeniedExpired, false)
	})
	t.Run("pinned UTC send expired", func(t *testing.T) {
		f := newDeliveryFixture(t)
		prepareImportedBaseline(t, f)
		f.now = time.Date(2040, 2, 28, 12, 0, 0, 0, time.Local)
		f.store.view.Entitlement.Imported.NextSendUTC = f.now.Add(-time.Second).UTC()
		hash, err := f.store.view.Entitlement.Fingerprint()
		if err != nil {
			t.Fatal("cannot reseal UTC expiry fixture")
		}
		f.store.view.Request.EntitlementHash = hash
		f.store.view.Request.CreatedAt, f.store.view.Request.ExpiresAt = f.now.Add(-time.Minute), f.now.Add(time.Minute)
		f.assertDenial(DeniedExpired, false)
	})
}

func TestImportedBaselineAdmissionRevisionFingerprint(t *testing.T) {
	f := newDeliveryFixture(t)
	prepareImportedBaseline(t, f)
	e := f.store.view.Entitlement
	before, err := e.Fingerprint()
	if err != nil {
		t.Fatal("cannot fingerprint baseline")
	}
	e.Imported.AdmissionPermissionRevision++
	after, err := e.Fingerprint()
	if err != nil || before == after {
		t.Fatal("permission revision was not fingerprinted")
	}
	ref := e.Imported.Origin
	ref.SourceScopeHash[0] ^= 1
	e.Imported.Origin, e.Current.ImportedOrigin = ref, ref
	after, err = e.Fingerprint()
	if err != nil || before == after {
		t.Fatal("actual ref hashes were not fingerprinted")
	}
}
