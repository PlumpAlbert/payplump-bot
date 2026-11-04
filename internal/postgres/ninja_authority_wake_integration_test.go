//go:build integration

package postgres

import (
	"crypto/sha256"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/ninjaauthority"
)

func TestAuthorityWakeReadsExistingGenerationsOnly(t *testing.T) {
	s, ctx := authorityFixture(t)
	check := func(bot int64, invoices, payments int64) {
		t.Helper()
		got, err := s.LoadAuthorityWake(ctx, bot)
		if err != nil || got != (ninjaauthority.WakeGeneration{Invoices: invoices, Payments: payments}) {
			t.Fatalf("wake=%+v error=%v", got, err)
		}
	}
	check(9123, 0, 0)
	check(9124, 0, 0)
	paymentSQL(t, s, ctx, `SELECT NOT EXISTS(SELECT 1 FROM invoice_lifecycle_wake_state) AND NOT EXISTS(SELECT 1 FROM payment_wake_state) AND NOT EXISTS(SELECT 1 FROM telegram_bots WHERE bot_id=9124)`)
	if err := s.RecordInvoiceLifecycleWake(ctx, 9123); err != nil {
		t.Fatal(err)
	}
	check(9123, 1, 0)
	hash := sha256.Sum256([]byte("synthetic-payment-wake"))
	fresh, err := s.RecordPaymentWake(ctx, 9123, hash)
	if err != nil || !fresh {
		t.Fatalf("fresh=%v error=%v", fresh, err)
	}
	check(9123, 1, 1)
	fresh, err = s.RecordPaymentWake(ctx, 9123, hash)
	if err != nil || fresh {
		t.Fatalf("duplicate fresh=%v error=%v", fresh, err)
	}
	check(9123, 1, 1)
	if err := s.RecordInvoiceLifecycleWake(ctx, 9123); err != nil {
		t.Fatal(err)
	}
	check(9123, 2, 1)
	if err := s.RecordInvoiceLifecycleWake(ctx, 9124); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordPaymentWake(ctx, 9124, hash); err != nil {
		t.Fatal(err)
	}
	check(9124, 1, 1)
	check(9123, 2, 1)
	check(9125, 0, 0)
	if _, err := s.LoadAuthorityWake(ctx, 0); err == nil {
		t.Fatal("invalid bot accepted")
	}
	paymentSQL(t, s, ctx, `SELECT NOT EXISTS(SELECT 1 FROM telegram_bots WHERE bot_id=9125)`)
}
