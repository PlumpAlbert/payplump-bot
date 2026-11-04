package ninjaauthority

import (
	"context"
	"errors"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
)

type inventoryInvoices struct {
	records []invoiceninja.CycleInvoice
	err     error
}

func (i inventoryInvoices) ListInvoices(context.Context, string) ([]invoiceninja.CycleInvoice, error) {
	return i.records, i.err
}

func TestCompleteInvoiceInventoryIndependentOfRecurring(t *testing.T) {
	for _, state := range []string{"2", "1", "none", "unavailable"} {
		t.Run(state, func(t *testing.T) {
			s, st, n, p := fixture(t, state, false)
			if state == "none" {
				n.records = nil
			}
			if state == "unavailable" {
				s.ninja = diagnosticRecurring{n, errors.New("synthetic")}
			}
			records := []invoiceninja.CycleInvoice{
				{ID: "recurring", ClientID: "customer-42", RecurringID: "recurring-42", StatusID: "2", Date: "2040-01-01", Amount: "100"},
				{ID: "manual", ClientID: "customer-42", StatusID: "2", Date: "2040-01-02", Amount: "200"},
				{ID: "draft", ClientID: "customer-42", StatusID: "1", Date: "2040-01-03", Amount: "300"},
				{ID: "archived", ClientID: "customer-42", StatusID: "2", Date: "2041-01-01", ArchivedAt: 1},
				{ID: "deleted", ClientID: "customer-42", StatusID: "2", Date: "2041-01-01", IsDeleted: true},
				{ID: "canceled", ClientID: "customer-42", StatusID: "5", Date: "2041-01-01"},
			}
			s.invoices = inventoryInvoices{records: records}
			if err := s.Scan(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(st.invoiceBatches) != 1 || len(st.invoiceBatches[0]) != len(records) {
				t.Fatal("incomplete ledger inventory")
			}
			for idx, i := range st.invoiceBatches[0] {
				r := records[idx]
				if i.ID != r.ID || i.CustomerID != r.ClientID || i.Status != r.StatusID || i.Amount != r.Amount.String() || i.ArchivedAt != r.ArchivedAt || i.IsDeleted != r.IsDeleted {
					t.Fatal("lost invoice fields")
				}
			}
			o := st.observations[42]
			if o.InvoiceID != "draft" {
				t.Fatal("draft/manual invoice hidden")
			}
			if state != "2" && (o.StateKnown || p.posts != 0) {
				t.Fatal("invoices manufactured access state")
			}
		})
	}
}

func TestInvoiceRepresentativeTiesAndEmptyDates(t *testing.T) {
	for _, date := range []string{"", "2040-01-01"} {
		records := []invoiceninja.CycleInvoice{{ID: "a", ClientID: "c", Date: date}, {ID: "b", ClientID: "c", Date: date}}
		for round := 0; round < 2; round++ {
			invoices, representative, valid := observeInvoices("c", records)
			if !valid || len(invoices) != 2 || representative == nil || representative.ID != "b" {
				t.Fatal("tie erased invoices or unstable representative")
			}
			records[0], records[1] = records[1], records[0]
		}
		records[0].UpdatedAt = 1
		_, representative, _ := observeInvoices("c", records)
		if representative.ID != "a" {
			t.Fatal("updated timestamp not used for menu tie")
		}
	}
}

func TestInvalidInvoiceInventoryNeverSavesBatch(t *testing.T) {
	for _, kind := range []string{"empty_id", "duplicate", "foreign", "partial"} {
		t.Run(kind, func(t *testing.T) {
			s, st, _, p := fixture(t, "2", false)
			records := []invoiceninja.CycleInvoice{{ID: "valid", ClientID: "customer-42"}, {ID: "other", ClientID: "customer-42"}}
			var err error
			switch kind {
			case "empty_id":
				records[1].ID = ""
			case "duplicate":
				records[1].ID = "valid"
			case "foreign":
				records[1].ClientID = "other"
			case "partial":
				err = errors.New("incomplete page")
			}
			s.invoices = inventoryInvoices{records, err}
			if err := s.Scan(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(st.invoiceBatches) != 0 || len(st.observations) != 0 || p.posts != 0 {
				t.Fatal("invalid inventory crossed save boundary")
			}
		})
	}
}
