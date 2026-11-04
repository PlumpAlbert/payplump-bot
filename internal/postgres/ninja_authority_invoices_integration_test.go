//go:build integration

package postgres

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/ninjaauthority"
)

func inventoryObservation(actor int64) ninjaauthority.Observation {
	o := authorityObservation(actor)
	o.ObservedAt = time.Date(2040, 2, 1, 12, 0, 0, 0, time.UTC)
	o.InvoiceNumber, o.InvoiceStatus, o.InvoiceAmount, o.InvoiceDate = "SYNTHETIC-BASE", "2", "0", "2040-02-01"
	return o
}

func TestAuthorityInventoryAllDocumentsAndDurableDedup(t *testing.T) {
	s, ctx := authorityFixture(t)
	o := inventoryObservation(67100)
	// Announcement policy is independent of recurring/access/payment state.
	o.RecurringID, o.NextInvoiceDate = "", ""
	o.StateKnown, o.Enabled, o.SUIEnabled = false, false, false
	base := selectedAuthorityInvoice(o)
	save := func(invoices ...ninjaauthority.Invoice) {
		t.Helper()
		if err := s.SaveAuthorityObservationWithInvoices(ctx, 9123, o, invoices); err != nil {
			t.Fatal(err)
		}
	}
	save(base)
	paymentSQL(t, s, ctx, `SELECT NOT EXISTS(SELECT 1 FROM outbox)`)
	draft := ninjaauthority.Invoice{ID: "synthetic-manual-draft", CustomerID: o.CustomerID, Number: "SYNTHETIC-DRAFT", Status: "1", Amount: "0", Date: "2040-02-01"}
	backdated := draft
	backdated.ID, backdated.Number, backdated.Date = "synthetic-backdated", "SYNTHETIC-BACKDATED", "2030-01-01"
	archived := draft
	archived.ID, archived.ArchivedAt = "synthetic-archived", 123
	deleted := draft
	deleted.ID, deleted.IsDeleted = "synthetic-deleted", true
	save(base, draft, backdated, archived, deleted)
	paymentSQL(t, s, ctx, `SELECT count(*)=4 AND bool_and(NOT client_menu AND NOT protected_configuration) FROM outbox`)
	paymentSQL(t, s, ctx, `SELECT EXISTS(SELECT 1 FROM outbox WHERE body LIKE '%архивный%') AND EXISTS(SELECT 1 FROM outbox WHERE body LIKE '%удалённый%')`)
	// A new Store value has no in-memory dedup state; canonical ledger survives.
	s = &Store{pool: s.pool}
	save(base, draft, backdated, archived, deleted)
	paymentSQL(t, s, ctx, `SELECT count(*)=4 FROM outbox`)
	// Non-current invoices change independently, multiple events per inventory.
	draft.Status = "4"
	backdated.Number = "SYNTHETIC-EDITED"
	save(base, draft, backdated, archived, deleted)
	paymentSQL(t, s, ctx, `SELECT count(*)=6 FROM outbox`)
	// Binding-only writes can change menu selection without creating notices.
	o.InvoiceID, o.InvoiceNumber, o.InvoiceStatus, o.InvoiceAmount, o.InvoiceDate = draft.ID, draft.Number, draft.Status, draft.Amount, draft.Date
	if err := s.SaveAuthorityObservation(ctx, 9123, o); err != nil {
		t.Fatal(err)
	}
	save(draft, base, backdated, archived, deleted)
	paymentSQL(t, s, ctx, `SELECT count(*)=6 FROM outbox`)
	// Missing rows are retained, not deleted or rearmed when they return.
	save(draft)
	save(draft, base, backdated, archived, deleted)
	paymentSQL(t, s, ctx, `SELECT (SELECT count(*)=6 FROM outbox) AND (SELECT count(*)=5 FROM authority_invoice_ledger)`)
	draft.IsDeleted = true
	save(draft, base, backdated, archived, deleted)
	paymentSQL(t, s, ctx, `SELECT (SELECT count(*)=7 FROM outbox) AND NOT EXISTS(SELECT 1 FROM payment_confirmations) AND NOT EXISTS(SELECT 1 FROM subscriptions WHERE payment_date IS NOT NULL) AND NOT EXISTS(SELECT 1 FROM external_write_intents)`)
}

func TestAuthorityInventoryLegacyUpgradeOnlySelectedChange(t *testing.T) {
	for _, change := range []string{"none", "id", "status"} {
		t.Run(change, func(t *testing.T) {
			s, ctx := authorityFixture(t)
			o := inventoryObservation(67200)
			if err := s.SaveAuthorityObservation(ctx, 9123, o); err != nil {
				t.Fatal(err)
			}
			history := selectedAuthorityInvoice(o)
			history.ID, history.Number = "synthetic-old-history", "SYNTHETIC-OLD"
			if change == "id" {
				o.InvoiceID = "synthetic-new-selected"
			}
			if change == "status" {
				o.InvoiceStatus = "4"
			}
			invoices := []ninjaauthority.Invoice{history, selectedAuthorityInvoice(o)}
			for i := 0; i < 2; i++ {
				if err := s.SaveAuthorityObservationWithInvoices(ctx, 9123, o, invoices); err != nil {
					t.Fatal(err)
				}
			}
			want := 0
			if change != "none" {
				want = 1
			}
			paymentSQL(t, s, ctx, `SELECT count(*)=$1 FROM outbox`, want)
			paymentSQL(t, s, ctx, `SELECT count(*)=2 AND bool_and(generation=1) FROM authority_invoice_ledger`)
			// Once baselined, later historical-record changes use the ledger too.
			invoices[0].Status = "3"
			if err := s.SaveAuthorityObservationWithInvoices(ctx, 9123, o, invoices); err != nil {
				t.Fatal(err)
			}
			paymentSQL(t, s, ctx, `SELECT count(*)=$1 FROM outbox`, want+1)
		})
	}
}

func TestAuthorityInventoryMalformedHasNoPartialAdoption(t *testing.T) {
	for _, tc := range []string{"duplicate", "customer", "missing-selected", "partial", "oversize-id", "oversize-number", "oversize-status", "oversize-amount", "bad-date", "negative-archive", "too-many"} {
		t.Run(tc, func(t *testing.T) {
			s, ctx := authorityFixture(t)
			o := inventoryObservation(67300)
			i := selectedAuthorityInvoice(o)
			invoices := []ninjaauthority.Invoice{i}
			switch tc {
			case "duplicate":
				invoices = append(invoices, i)
			case "customer":
				invoices[0].CustomerID = "synthetic-foreign"
			case "missing-selected":
				invoices = nil
			case "partial":
				invoices[0].Amount = ""
			case "oversize-id":
				invoices[0].ID = strings.Repeat("a", 257)
			case "oversize-number":
				invoices[0].Number = strings.Repeat("a", 257)
			case "oversize-status":
				invoices[0].Status = strings.Repeat("a", 65)
			case "oversize-amount":
				invoices[0].Amount = strings.Repeat("1", 65)
			case "bad-date":
				invoices[0].Date = "2040-02-31"
			case "negative-archive":
				invoices[0].ArchivedAt = -1
			case "too-many":
				invoices = make([]ninjaauthority.Invoice, 10001)
			}
			if err := s.SaveAuthorityObservationWithInvoices(ctx, 9123, o, invoices); !errors.Is(err, ninjaauthority.ErrConflict) {
				t.Fatalf("error=%v", err)
			}
			paymentSQL(t, s, ctx, `SELECT NOT EXISTS(SELECT 1 FROM authority_invoice_baselines) AND NOT EXISTS(SELECT 1 FROM authority_invoice_ledger) AND NOT EXISTS(SELECT 1 FROM authority_observations) AND NOT EXISTS(SELECT 1 FROM outbox)`)
			// Failure must not consume the first complete inventory baseline.
			if err := s.SaveAuthorityObservationWithInvoices(ctx, 9123, o, []ninjaauthority.Invoice{i}); err != nil {
				t.Fatal(err)
			}
			paymentSQL(t, s, ctx, `SELECT NOT EXISTS(SELECT 1 FROM outbox)`)
		})
	}
}

func TestAuthorityInventoryOwnershipConflictsAreAtomic(t *testing.T) {
	s, ctx := authorityFixture(t)
	o := inventoryObservation(67400)
	first := selectedAuthorityInvoice(o)
	if err := s.SaveAuthorityObservationWithInvoices(ctx, 9123, o, []ninjaauthority.Invoice{first}); err != nil {
		t.Fatal(err)
	}
	foreign := inventoryObservation(67401)
	own := selectedAuthorityInvoice(foreign)
	stolen := first
	stolen.CustomerID = foreign.CustomerID
	// The first record has been written before the conflicting second record;
	// failure must roll back both ledger and the new client's baseline/adoption.
	if err := s.SaveAuthorityObservationWithInvoices(ctx, 9123, foreign, []ninjaauthority.Invoice{own, stolen}); !errors.Is(err, ninjaauthority.ErrConflict) {
		t.Fatalf("error=%v", err)
	}
	paymentSQL(t, s, ctx, `SELECT (SELECT count(*)=1 FROM authority_invoice_baselines) AND (SELECT count(*)=1 FROM authority_invoice_ledger) AND (SELECT count(*)=1 FROM authority_observations) AND NOT EXISTS(SELECT 1 FROM clients WHERE telegram_id=67401) AND NOT EXISTS(SELECT 1 FROM outbox)`)
	foreign.CustomerID = o.CustomerID
	foreign.InvoiceID, foreign.InvoiceNumber, foreign.InvoiceStatus, foreign.InvoiceAmount, foreign.InvoiceDate = first.ID, first.Number, first.Status, first.Amount, first.Date
	if err := s.SaveAuthorityObservationWithInvoices(ctx, 9123, foreign, []ninjaauthority.Invoice{first}); !errors.Is(err, ninjaauthority.ErrConflict) {
		t.Fatalf("error=%v", err)
	}
	paymentSQL(t, s, ctx, `SELECT (SELECT count(*)=1 FROM authority_invoice_baselines) AND NOT EXISTS(SELECT 1 FROM outbox)`)
}

func TestAuthorityInventoryOutboxFailureRollsBackEverything(t *testing.T) {
	s, ctx := authorityFixture(t)
	o := inventoryObservation(67500)
	if err := s.SaveAuthorityObservation(ctx, 9123, o); err != nil {
		t.Fatal(err)
	}
	// Force a notice insertion failure on legacy bootstrap, after baseline and
	// ledger insertion, to exercise transaction boundaries rather than mocks.
	paymentExec(t, s, ctx, `ALTER TABLE outbox ADD CONSTRAINT synthetic_no_invoice CHECK(message_key NOT LIKE 'authority-invoice:%')`)
	o.InvoiceStatus = "4"
	if err := s.SaveAuthorityObservationWithInvoices(ctx, 9123, o, []ninjaauthority.Invoice{selectedAuthorityInvoice(o)}); err == nil {
		t.Fatal("expected outbox constraint failure")
	}
	paymentSQL(t, s, ctx, `SELECT NOT EXISTS(SELECT 1 FROM authority_invoice_baselines) AND NOT EXISTS(SELECT 1 FROM authority_invoice_ledger) AND NOT EXISTS(SELECT 1 FROM outbox) AND (SELECT revision=1 AND observation->>'InvoiceStatus'='2' FROM authority_observations)`)
	paymentExec(t, s, ctx, `ALTER TABLE outbox DROP CONSTRAINT synthetic_no_invoice`)
	if err := s.SaveAuthorityObservationWithInvoices(ctx, 9123, o, []ninjaauthority.Invoice{selectedAuthorityInvoice(o)}); err != nil {
		t.Fatal(err)
	}
	paymentSQL(t, s, ctx, `SELECT count(*)=1 FROM outbox`)
	// Failure after baseline must also leave no partial events/generations.
	paymentExec(t, s, ctx, `ALTER TABLE outbox ADD CONSTRAINT synthetic_no_created CHECK(message_key NOT LIKE 'authority-invoice:%:2')`)
	changed := selectedAuthorityInvoice(o)
	changed.Amount = "1"
	o.InvoiceAmount = "1"
	if err := s.SaveAuthorityObservationWithInvoices(ctx, 9123, o, []ninjaauthority.Invoice{changed}); err == nil {
		t.Fatal("expected second-generation failure")
	}
	paymentSQL(t, s, ctx, `SELECT (SELECT count(*)=1 FROM outbox) AND (SELECT generation=1 AND snapshot->>'Amount'='0' FROM authority_invoice_ledger) AND (SELECT revision=2 AND observation->>'InvoiceAmount'='0' FROM authority_observations)`)
}

func TestAuthorityInventoryEmptyBaselineAndRemovedHistory(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "removed-history", true: "empty"}[empty], func(t *testing.T) {
			s, ctx := authorityFixture(t)
			o := inventoryObservation(67600)
			old := selectedAuthorityInvoice(o)
			old.ArchivedAt, old.IsDeleted = 123, true
			o.InvoiceID, o.InvoiceNumber, o.InvoiceStatus, o.InvoiceAmount, o.InvoiceDate = "", "", "", "", ""
			invoices := []ninjaauthority.Invoice{}
			if !empty {
				invoices = append(invoices, old)
			}
			if err := s.SaveAuthorityObservationWithInvoices(ctx, 9123, o, invoices); err != nil {
				t.Fatal(err)
			}
			paymentSQL(t, s, ctx, `SELECT NOT EXISTS(SELECT 1 FROM outbox) AND (SELECT count(*)=1 FROM authority_invoice_baselines)`)
			fresh := old
			fresh.ID = "synthetic-later-removed"
			fresh.Number = "SYNTHETIC-LATER-REMOVED"
			invoices = append(invoices, fresh)
			if err := s.SaveAuthorityObservationWithInvoices(ctx, 9123, o, invoices); err != nil {
				t.Fatal(err)
			}
			paymentSQL(t, s, ctx, `SELECT count(*)=1 AND bool_and(body LIKE '%архивный%' AND body LIKE '%удалённый%') FROM outbox`)
			// Neither the inventory scope nor observation can drift independently.
			o.ScopeHash = strings.Repeat("b", 64)
			if err := s.SaveAuthorityObservationWithInvoices(ctx, 9123, o, invoices); !errors.Is(err, ninjaauthority.ErrConflict) {
				t.Fatalf("error=%v", err)
			}
			paymentSQL(t, s, ctx, `SELECT (SELECT count(*)=1 FROM outbox) AND (SELECT scope_hash=repeat('a',64) FROM authority_invoice_baselines)`)
		})
	}
}
