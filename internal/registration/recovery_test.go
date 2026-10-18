package registration

import (
	"errors"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
)

func TestConfirmedIssuanceCanFinishAfterMidnight(t *testing.T) {
	f := newWorkerFixture(t)
	f.passes(3)
	f.now = f.now.AddDate(0, 0, 1)
	f.pass()
	if !f.store.states[1].finished || f.store.states[1].held {
		t.Fatal("known successful issuance could not finish after restart on a later day")
	}
	f.wantPosts(1, 1, 1)
}

func TestPositiveInvoiceReconciliationRejectsOtherHistory(t *testing.T) {
	f := newWorkerFixture(t)
	f.passes(1)
	f.api.writeErr[InvoiceCreate] = errors.New("synthetic lost acknowledgement")
	f.api.commitOnError[InvoiceCreate] = true
	f.pass()
	other := cloneInvoice(f.api.invoices[0])
	other.ID = "foreign-invoice"
	other.PrivateNotes = "not owned by this operation"
	f.api.invoices = append(f.api.invoices, other)
	f.restart()
	f.pass()
	f.wantHeld(ReviewInvoiceConflict)
	f.wantPosts(1, 1, 0)
}

func TestChangedFinancialModifiersNeverReachIssuance(t *testing.T) {
	for name, change := range map[string]func(*invoiceninja.Invoice){
		"invoice discount": func(i *invoiceninja.Invoice) { i.Discount = "50" },
		"line discount":    func(i *invoiceninja.Invoice) { i.LineItems[0].Discount = "50" },
		"global tax":       func(i *invoiceninja.Invoice) { i.TaxRate1 = "10" },
		"surcharge":        func(i *invoiceninja.Invoice) { i.CustomSurcharge1 = "10" },
		"deposit":          func(i *invoiceninja.Invoice) { i.Partial = "10" },
		"prior payment":    func(i *invoiceninja.Invoice) { i.PaidToDate = "10" },
		"fee line":         func(i *invoiceninja.Invoice) { i.LineItems[0].TypeID = "4" },
		"recurring":        func(i *invoiceninja.Invoice) { i.RecurringID = "foreign-schedule" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newWorkerFixture(t)
			f.passes(2)
			change(&f.api.invoices[0])
			f.pass()
			f.wantHeld(ReviewInvoiceConflict)
			f.wantPosts(1, 1, 0)
		})
	}
}
