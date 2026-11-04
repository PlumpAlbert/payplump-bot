//go:build integration

package postgres

import (
	"errors"
	"sync"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/ninjaauthority"
)

func TestAuthorityInvoiceAdminNoticesAtomicRolesAndRestart(t *testing.T) {
	s, ctx := authorityFixture(t)
	o := inventoryObservation(68100)
	base := selectedAuthorityInvoice(o)
	admins := []int64{99, 100, 99, o.Actor}
	save := func(invoices []ninjaauthority.Invoice) {
		t.Helper()
		if err := s.SaveAuthorityObservationWithInvoices(ctx, 9123, o, invoices, admins...); err != nil {
			t.Fatal(err)
		}
	}
	save([]ninjaauthority.Invoice{base})
	paymentSQL(t, s, ctx, `SELECT NOT EXISTS(SELECT 1 FROM outbox)`)
	first := ninjaauthority.Invoice{ID: "synthetic-notice-first", CustomerID: o.CustomerID, Number: "NOTICE-FIRST", Status: "1", Date: base.Date, Amount: "100"}
	second := first
	second.ID, second.Number, second.Status = "synthetic-notice-second", "NOTICE-SECOND", "2"
	batch := []ninjaauthority.Invoice{base, first, second}
	// Competing saves serialize on the existing bot/client locks. Both may update
	// observations, but every logical invoice event has exactly one row per role.
	var wg sync.WaitGroup
	failures := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			failures <- s.SaveAuthorityObservationWithInvoices(ctx, 9123, o, batch, admins...)
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	paymentSQL(t, s, ctx, `SELECT count(*)=8 AND count(*) FILTER(WHERE chat_id=68100)=4
 AND count(*) FILTER(WHERE chat_id=99)=2 AND count(*) FILTER(WHERE chat_id=100)=2
 AND bool_and(NOT protected_configuration AND NOT client_menu) FROM outbox`)
	paymentSQL(t, s, ctx, `SELECT count(*)=6 AND bool_and(body LIKE '%68100%' AND body NOT LIKE '%https://%')
 FROM outbox WHERE message_key LIKE 'authority-invoice:%:admin:%'`)
	first.Status = "4" // An observed Ninja status is still not verified payment.
	batch = []ninjaauthority.Invoice{second, first, base}
	save(batch)
	s = &Store{pool: s.pool}
	admins = append(admins, 101)
	save(batch)
	save(batch)
	paymentSQL(t, s, ctx, `SELECT count(*)=12 AND count(*) FILTER(WHERE chat_id=101)=0 FROM outbox`)
	second.Status = "4"
	batch = []ninjaauthority.Invoice{base, first, second}
	save(batch)
	paymentSQL(t, s, ctx, `SELECT count(*)=17 AND count(*) FILTER(WHERE chat_id=101)=1 FROM outbox`)
	// Removing an admin stops future summaries without changing historical rows.
	admins = []int64{100, 101}
	first.Amount = "200"
	save([]ninjaauthority.Invoice{base, first, second})
	paymentSQL(t, s, ctx, `SELECT count(*)=20 AND count(*) FILTER(WHERE chat_id=99)=4 FROM outbox`)
	paymentSQL(t, s, ctx, `SELECT NOT EXISTS(SELECT 1 FROM payment_confirmations)
 AND NOT EXISTS(SELECT 1 FROM subscriptions WHERE payment_date IS NOT NULL)
 AND NOT EXISTS(SELECT 1 FROM external_write_intents)`)
}

func TestAuthorityInvoiceAdminFailureRollsBackWholeEvent(t *testing.T) {
	s, ctx := authorityFixture(t)
	o := inventoryObservation(68200)
	base := selectedAuthorityInvoice(o)
	admins := []int64{99, 100}
	save := func(invoices []ninjaauthority.Invoice) error {
		return s.SaveAuthorityObservationWithInvoices(ctx, 9123, o, invoices, admins...)
	}
	if err := save([]ninjaauthority.Invoice{base}); err != nil {
		t.Fatal(err)
	}
	paymentExec(t, s, ctx, `ALTER TABLE outbox ADD CONSTRAINT synthetic_admin_failure CHECK(chat_id<>100)`)
	next := base
	next.ID, next.Number = "synthetic-atomic-admin", "ATOMIC-ADMIN"
	batch := []ninjaauthority.Invoice{base, next}
	if err := save(batch); err == nil {
		t.Fatal("expected admin insert rejection")
	}
	paymentSQL(t, s, ctx, `SELECT NOT EXISTS(SELECT 1 FROM outbox)
 AND (SELECT count(*)=1 FROM authority_invoice_ledger)
 AND (SELECT revision=1 FROM authority_observations)`)
	paymentExec(t, s, ctx, `ALTER TABLE outbox DROP CONSTRAINT synthetic_admin_failure`)
	if err := save(batch); err != nil {
		t.Fatal(err)
	}
	if err := save(batch); err != nil {
		t.Fatal(err)
	}
	paymentSQL(t, s, ctx, `SELECT count(*)=3 AND count(*) FILTER(WHERE chat_id=68200)=1
 AND count(*) FILTER(WHERE chat_id=99)=1 AND count(*) FILTER(WHERE chat_id=100)=1 FROM outbox`)
	// Baseline compatibility also has to pair the client's selected-change event.
	other := inventoryObservation(68201)
	if err := s.SaveAuthorityObservation(ctx, 9123, other, admins...); err != nil {
		t.Fatal(err)
	}
	other.InvoiceStatus = "4"
	if err := s.SaveAuthorityObservationWithInvoices(ctx, 9123, other, []ninjaauthority.Invoice{selectedAuthorityInvoice(other)}, admins...); err != nil {
		t.Fatal(err)
	}
	paymentSQL(t, s, ctx, `SELECT count(*)=6 FROM outbox`)
}

func TestAuthorityInvoiceOldClientNoticeDoesNotBackfillAdmins(t *testing.T) {
	s, ctx := authorityFixture(t)
	o := inventoryObservation(68300)
	base := selectedAuthorityInvoice(o)
	// Empty admin recipients represent the prior version's client-only event.
	if err := s.SaveAuthorityObservationWithInvoices(ctx, 9123, o, []ninjaauthority.Invoice{base}); err != nil {
		t.Fatal(err)
	}
	base.Status = "4"
	o.InvoiceStatus = base.Status
	if err := s.SaveAuthorityObservationWithInvoices(ctx, 9123, o, []ninjaauthority.Invoice{base}); err != nil {
		t.Fatal(err)
	}
	s = &Store{pool: s.pool}
	if err := s.SaveAuthorityObservationWithInvoices(ctx, 9123, o, []ninjaauthority.Invoice{base}, 99, 100); err != nil {
		t.Fatal(err)
	}
	paymentSQL(t, s, ctx, `SELECT count(*)=1 AND bool_and(chat_id=68300) FROM outbox`)
	base.Status = "2"
	o.InvoiceStatus = base.Status
	if err := s.SaveAuthorityObservationWithInvoices(ctx, 9123, o, []ninjaauthority.Invoice{base}, 99, 100); err != nil {
		t.Fatal(err)
	}
	paymentSQL(t, s, ctx, `SELECT count(*)=4 AND count(*) FILTER(WHERE chat_id IN (99,100))=2 FROM outbox`)
}

func TestAuthorityInvalidAdminRecipientsCannotPartiallyAdopt(t *testing.T) {
	for _, invalid := range []int64{0, -1} {
		s, ctx := authorityFixture(t)
		o := inventoryObservation(68400)
		if err := s.SaveAuthorityObservationWithInvoices(ctx, 9123, o, []ninjaauthority.Invoice{selectedAuthorityInvoice(o)}, 99, invalid); !errors.Is(err, ninjaauthority.ErrConflict) {
			t.Fatalf("unexpected error: %v", err)
		}
		if err := s.SaveAuthorityObservation(ctx, 9123, o, 99, invalid); !errors.Is(err, ninjaauthority.ErrConflict) {
			t.Fatalf("unexpected error: %v", err)
		}
		paymentSQL(t, s, ctx, `SELECT NOT EXISTS(SELECT 1 FROM authority_observations)
 AND NOT EXISTS(SELECT 1 FROM authority_invoice_baselines) AND NOT EXISTS(SELECT 1 FROM outbox)`)
	}
}
