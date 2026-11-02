//go:build integration

package postgres

import "testing"

func TestAuthorityInvoiceChangesAreAtomicDeduplicatedAndNotPaymentProof(t *testing.T) {
	s, ctx := authorityFixture(t)
	o := authorityObservation(67000)
	o.InvoiceNumber = "SYNTHETIC-1"
	o.InvoiceStatus = "2"
	if err := s.SaveAuthorityObservation(ctx, 9123, o); err != nil {
		t.Fatal(err)
	}
	paymentSQL(t, s, ctx, `SELECT NOT EXISTS(SELECT 1 FROM outbox)`)
	o.InvoiceStatus = "4"
	for i := 0; i < 2; i++ {
		if err := s.SaveAuthorityObservation(ctx, 9123, o); err != nil {
			t.Fatal(err)
		}
	}
	paymentSQL(t, s, ctx, `SELECT (SELECT count(*)=1 FROM outbox WHERE message_key LIKE 'authority-change:%:invoice') AND NOT EXISTS(SELECT 1 FROM payment_confirmations)`)
	o.InvoiceStatus = "2"
	if err := s.SaveAuthorityObservation(ctx, 9123, o); err != nil {
		t.Fatal(err)
	}
	o.InvoiceStatus = "4"
	if err := s.SaveAuthorityObservation(ctx, 9123, o); err != nil {
		t.Fatal(err)
	}
	paymentSQL(t, s, ctx, `SELECT (SELECT count(*)=3 FROM outbox WHERE message_key LIKE 'authority-change:%:invoice') AND NOT EXISTS(SELECT 1 FROM payment_confirmations) AND NOT EXISTS(SELECT 1 FROM subscriptions WHERE payment_date IS NOT NULL)`)
}
