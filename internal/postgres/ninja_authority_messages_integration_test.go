//go:build integration

package postgres

import (
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/ninjaauthority"
)

func selectedAuthorityInvoice(o ninjaauthority.Observation) ninjaauthority.Invoice {
	return ninjaauthority.Invoice{ID: o.InvoiceID, CustomerID: o.CustomerID, Number: o.InvoiceNumber, Status: o.InvoiceStatus, Amount: o.InvoiceAmount, Date: o.InvoiceDate}
}

func TestAuthorityInvoiceChangesAreAtomicDeduplicatedAndNotPaymentProof(t *testing.T) {
	s, ctx := authorityFixture(t)
	o := authorityObservation(67000)
	o.InvoiceNumber, o.InvoiceAmount, o.InvoiceStatus, o.InvoiceDate = "SYNTHETIC-1", "0", "2", "2040-02-01"
	save := func() {
		t.Helper()
		if err := s.SaveAuthorityObservationWithInvoices(ctx, 9123, o, []ninjaauthority.Invoice{selectedAuthorityInvoice(o)}); err != nil {
			t.Fatal(err)
		}
	}
	save()
	paymentSQL(t, s, ctx, `SELECT NOT EXISTS(SELECT 1 FROM outbox)`)
	o.InvoiceStatus = "4"
	save()
	save()
	paymentSQL(t, s, ctx, `SELECT (SELECT count(*)=1 FROM outbox WHERE message_key LIKE 'authority-invoice:%') AND NOT EXISTS(SELECT 1 FROM payment_confirmations)`)
	o.InvoiceStatus = "2"
	save()
	o.InvoiceStatus = "4"
	save()
	paymentSQL(t, s, ctx, `SELECT (SELECT count(*)=3 FROM outbox WHERE message_key LIKE 'authority-invoice:%') AND NOT EXISTS(SELECT 1 FROM payment_confirmations) AND NOT EXISTS(SELECT 1 FROM subscriptions WHERE payment_date IS NOT NULL)`)
}
