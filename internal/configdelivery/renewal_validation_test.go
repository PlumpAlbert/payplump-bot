package configdelivery

import (
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/payments"
)

func renewalProof(invoice, paid string) payments.Proof {
	e := payments.Evidence{InvoiceID: invoice, CustomerID: "customer-A", Amount: "200", CurrencyID: "1", PaymentDate: paid, NextInvoiceDate: "2040-02-29",
		Allocations: []payments.Allocation{{PaymentID: "payment-" + invoice, AllocationID: "allocation-" + invoice, Amount: "200", Date: paid}}}
	raw, _ := json.Marshal(e)
	return payments.Proof{Evidence: e, InvoiceNumber: "synthetic-" + invoice, JSON: raw, Hash: sha256.Sum256(raw)}
}

func renewalEntitlement(t *testing.T) (Entitlement, Request, time.Time) {
	t.Helper()
	f := newDeliveryFixture(t)
	e := f.store.view.Entitlement
	proof := renewalProof("invoice-B", "2040-01-31")
	if proof.Validate() != nil {
		t.Fatalf("synthetic current proof invalid: %+v", proof.Evidence)
	}
	e.Period = RenewalPeriod
	e.Current = payments.Snapshot{ClientID: 7, TelegramID: 101, ChatID: 101, BotID: 42, CustomerID: "customer-A", InvoiceID: "invoice-B", FirstInvoiceID: "invoice-A",
		RecurringID: "recurring-A", SUIID: "41", State: "active", DesiredAccess: true, PaymentDate: proof.PaymentDate, NextInvoiceDate: proof.NextInvoiceDate}
	e.CurrentProof = proof
	e.Renewal = RenewalAudit{OperationID: 27, TargetDate: proof.NextInvoiceDate, PlanHash: [32]byte{1}, Amount: "200", NextSendUTC: time.Date(2040, 2, 29, 12, 0, 0, 0, time.UTC)}
	hash, err := e.Fingerprint()
	if err != nil {
		t.Fatalf("valid renewal entitlement rejected: validate=%v fingerprint=%v", e.Validate(), err)
	}
	p := f.cfg.Policy()
	r := Request{OutboxID: 80, BotID: 42, ClientID: 7, ActorID: 101, ChatID: 101, ActivationID: 9, RenewalID: 27, Period: RenewalPeriod,
		ClientRevision: 2, PolicyRevision: p.Revision, PolicyHash: p.Hash, ProofHash: proof.Hash, EntitlementHash: hash, InvoiceID: "invoice-B",
		CreatedAt: f.now, ExpiresAt: f.now.Add(RequestTTL)}
	return e, r, f.now
}

func TestRenewalEntitlementUsesSeparateCurrentProofAndHistoricalAudit(t *testing.T) {
	e, r, now := renewalEntitlement(t)
	v := View{Request: r, Entitlement: e, Eligible: true}
	if !v.Matches() {
		t.Fatal("valid renewal view did not match")
	}
	if e.Activation.Proof.InvoiceID != "invoice-A" || e.CurrentProof.InvoiceID != "invoice-B" {
		t.Fatal("renewal overwrote historical proof")
	}
	deadline, err := e.Deadline()
	if err != nil || !deadline.After(now) || deadline.After(e.Renewal.NextSendUTC) {
		t.Fatal("renewal deadline did not use current period bounds")
	}
	oldRequest := r
	oldRequest.Period, oldRequest.RenewalID, oldRequest.InvoiceID, oldRequest.ProofHash = FirstPeriod, 0, "invoice-A", e.Activation.Proof.Hash
	v.Request = oldRequest
	if v.Matches() {
		t.Fatal("first-period request matched current renewal entitlement")
	}
}

func TestRenewalRequestIdentityMismatchesNeverMatch(t *testing.T) {
	e, request, _ := renewalEntitlement(t)
	for name, mutate := range map[string]func(*Request){
		"period":            func(r *Request) { r.Period = FirstPeriod; r.RenewalID = 0 },
		"renewal operation": func(r *Request) { r.RenewalID++ },
		"invoice":           func(r *Request) { r.InvoiceID = "invoice-A" },
		"proof":             func(r *Request) { r.ProofHash[0] ^= 0xff },
		"revision":          func(r *Request) { r.ClientRevision++ },
	} {
		t.Run(name, func(t *testing.T) {
			changed := request
			mutate(&changed)
			if (View{Request: changed, Entitlement: e, Eligible: true}).Matches() {
				t.Fatal("mismatched frozen renewal request was accepted")
			}
		})
	}
}

func TestOwnedRenewalRejectsUnexpectedPartialReceipt(t *testing.T) {
	for _, mutate := range []func(*RenewalAudit){
		func(a *RenewalAudit) { a.StepIDs[0] = "unexpected-step" },
		func(a *RenewalAudit) { a.StepHashes[0] = [32]byte{1} },
	} {
		e, _, _ := renewalEntitlement(t)
		mutate(&e.Renewal)
		if e.Validate() == nil {
			t.Fatal("partial optional renewal receipt was accepted")
		}
	}
}

func TestRenewalEntitlementFingerprintFencesCurrentAuthority(t *testing.T) {
	e, _, _ := renewalEntitlement(t)
	base, err := e.Fingerprint()
	if err != nil {
		t.Fatalf("valid renewal entitlement rejected: validate=%v fingerprint=%v", e.Validate(), err)
	}
	changed := e
	changed.CurrentProof.InvoiceNumber += "-changed"
	if hash, err := changed.Fingerprint(); err != nil || hash == base {
		t.Fatal("current proof facts were omitted from fingerprint")
	}
	changed = e
	changed.Renewal.StepIDs[0] = "synthetic-step"
	changed.Renewal.StepHashes[0] = [32]byte{2}
	changed.Renewal.RequiresPause = true
	if hash, err := changed.Fingerprint(); err != nil || hash == base {
		t.Fatal("renewal receipts were omitted from fingerprint")
	}
}
