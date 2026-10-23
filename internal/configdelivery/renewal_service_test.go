package configdelivery

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
)

func prepareRenewalDelivery(t *testing.T, f *deliveryFixture) {
	t.Helper()
	paid, next := "2040-02-01", "2040-03-01"
	f.now = time.Date(2040, 2, 1, 12, 0, 0, 0, time.Local)
	f.p.invoiceID = "invoice-B"
	f.p.invoice = invoiceninja.SettlementInvoice{ID: "invoice-B", ClientID: "customer-A", Number: "INV-201", RecurringID: "recurring-A", StatusID: "4", Date: paid, DueDate: paid, Amount: "200", Balance: "0", PaidToDate: "200", ExchangeRate: "1"}
	f.p.cash = []invoiceninja.SettlementPayment{{ID: "payment-B", ClientID: "customer-A", Date: paid, StatusID: "4", CurrencyID: "1", Amount: "200", Refunded: "0", Applied: "200", ExchangeRate: "1", IsManual: true, Allocations: []invoiceninja.SettlementAllocation{{ID: "allocation-B", InvoiceID: "invoice-B", PaymentID: "payment-B", Amount: "200", Refunded: "0"}}}}
	current := f.store.view.Entitlement.Activation.Account
	current.InvoiceID, current.PaymentDate, current.NextInvoiceDate = "invoice-B", paid, next
	verified := payments.Evaluate(current, f.p.customer, f.p.invoice, f.p.cash, "1", paid)
	if verified.Kind != payments.Verified {
		t.Fatalf("synthetic current-period proof not verified kind=%s code=%d", verified.Kind, verified.Code)
	}
	e := f.store.view.Entitlement
	e.Period = RenewalPeriod
	e.Current = current
	e.CurrentProof = *verified.Proof
	rec := f.rec.record
	rec.NextSendDate, rec.NextSendDatetime, rec.LastSentDate = next, next+"T12:00:00Z", "2040-01-31 12:00:00"
	f.rec.record = rec
	instant, err := invoiceninja.ParseRecurringUTC(rec.NextSendDatetime)
	if err != nil {
		t.Fatal("synthetic final recurring time invalid")
	}
	e.Renewal = RenewalAudit{OperationID: 27, TargetDate: next, PlanHash: [32]byte{1}, Amount: "200", NextSendUTC: instant}
	hash, err := e.Fingerprint()
	if err != nil {
		t.Fatal("synthetic renewal entitlement invalid")
	}
	r := f.store.view.Request
	r.Period, r.RenewalID, r.InvoiceID, r.ProofHash = RenewalPeriod, 27, "invoice-B", verified.Proof.Hash
	r.EntitlementHash, r.CreatedAt, r.ExpiresAt = hash, f.now, f.now.Add(RequestTTL)
	f.store.view = View{Request: r, Entitlement: e, Eligible: true}
}

func TestRenewalDeliveryUsesCurrentProofAndPreservesSubscriptionURL(t *testing.T) {
	f := newDeliveryFixture(t)
	prepareRenewalDelivery(t, f)
	id, err := f.deliver()
	if err != nil || id != 7001 || f.store.decision != (Decision{Kind: Secret}) || f.tg.posts != 1 || f.tg.gets != 1 {
		t.Fatal("valid paid renewal did not reach one protected send")
	}
	if f.p.invoiceID != "invoice-B" || f.p.inventories != 2 || f.rec.record.NextSendDate != "2040-03-01" {
		t.Fatal("renewal verification did not use current invoice and completed schedule")
	}
	if f.store.original.Entitlement.Activation.Proof.InvoiceID != "invoice-A" || f.store.original.Entitlement.CurrentProof.InvoiceID != "invoice-B" {
		t.Fatal("renewal replaced the immutable first-activation audit")
	}
	if f.tg.content.Text != "Ваша персональная ссылка на подписку:\nhttps://subscription.invalid/sub/"+url.PathEscape(f.store.view.Entitlement.Binding.Name)+"?format=json\n\nПомощь: /help" {
		t.Fatal("renewal changed the existing personal subscription URL")
	}
}

func TestRenewalPaymentGETFailureIsUnavailableButChangedProofIsStickyCandidate(t *testing.T) {
	t.Run("GET failure", func(t *testing.T) {
		f := newDeliveryFixture(t)
		prepareRenewalDelivery(t, f)
		f.p.hook = func(string, int) error { return context.DeadlineExceeded }
		_, err := f.deliver()
		if err != nil || f.store.decision != (Decision{Kind: Denial, Reason: DeniedUnavailable}) || f.store.decision.PaymentChanged {
			t.Fatal("transient current-payment GET failure became sticky review")
		}
	})
	t.Run("changed authoritative proof", func(t *testing.T) {
		f := newDeliveryFixture(t)
		prepareRenewalDelivery(t, f)
		f.p.invoice.Amount, f.p.invoice.PaidToDate = "250", "250"
		f.p.cash[0].Amount, f.p.cash[0].Applied, f.p.cash[0].Allocations[0].Amount = "250", "250", "250"
		_, err := f.deliver()
		if err != nil || f.store.decision != (Decision{Kind: Denial, Reason: DeniedPayment, PaymentChanged: true}) {
			t.Fatal("changed current-period proof did not produce period-scoped sticky candidate")
		}
	})
}
