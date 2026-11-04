package postgres

import (
	"strings"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/ninjaauthority"
)

func TestValidateAuthorityInvoicesBoundsAndDrafts(t *testing.T) {
	// Empty draft number/date, zero/negative amounts and arbitrary source status
	// remain ordinary observations, not financial or entitlement eligibility.
	o := ninjaauthority.Observation{CustomerID: "synthetic-customer", ScopeHash: strings.Repeat("a", 64)}
	invoice := ninjaauthority.Invoice{ID: "synthetic-id", CustomerID: o.CustomerID, Status: "1", Amount: "0"}
	for _, amount := range []string{"0", "-1.01", "1e3", "0.00000000000000000001"} {
		for _, status := range []string{"1", "4", "5", "unknown"} {
			i := invoice
			i.Amount, i.Status = amount, status
			if err := validateAuthorityInvoices(o, []ninjaauthority.Invoice{i}); err != nil {
				t.Fatalf("valid observation rejected: %v", err)
			}
		}
	}
	if err := validateAuthorityInvoices(o, nil); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*ninjaauthority.Invoice){
		func(i *ninjaauthority.Invoice) { i.ID = "" },
		func(i *ninjaauthority.Invoice) { i.ID = " " },
		func(i *ninjaauthority.Invoice) { i.ID = "synthetic\x00" },
		func(i *ninjaauthority.Invoice) { i.ID = strings.Repeat("a", 257) },
		func(i *ninjaauthority.Invoice) { i.CustomerID = "synthetic-foreign" },
		func(i *ninjaauthority.Invoice) { i.Number = "\xff" },
		func(i *ninjaauthority.Invoice) { i.Number = "line\nbreak" },
		func(i *ninjaauthority.Invoice) { i.Number = strings.Repeat("a", 257) },
		func(i *ninjaauthority.Invoice) { i.Status = "" },
		func(i *ninjaauthority.Invoice) { i.Status = strings.Repeat("a", 65) },
		func(i *ninjaauthority.Invoice) { i.Amount = "" },
		func(i *ninjaauthority.Invoice) { i.Amount = "null" },
		func(i *ninjaauthority.Invoice) { i.Amount = "NaN" },
		func(i *ninjaauthority.Invoice) { i.Amount = "01" },
		func(i *ninjaauthority.Invoice) { i.Amount = strings.Repeat("1", 65) },
		func(i *ninjaauthority.Invoice) { i.Date = "2040-02-31" },
		func(i *ninjaauthority.Invoice) { i.Date = strings.Repeat("1", 1000) },
		func(i *ninjaauthority.Invoice) { i.ArchivedAt = -1 },
	} {
		i := invoice
		mutate(&i)
		if err := validateAuthorityInvoices(o, []ninjaauthority.Invoice{i}); err == nil {
			t.Fatal("invalid invoice accepted")
		}
	}
	if err := validateAuthorityInvoices(o, []ninjaauthority.Invoice{invoice, invoice}); err == nil {
		t.Fatal("duplicate accepted")
	}
	if err := validateAuthorityInvoices(o, make([]ninjaauthority.Invoice, 10001)); err == nil {
		t.Fatal("unbounded inventory accepted")
	}
	o.InvoiceID = invoice.ID
	o.InvoiceStatus = invoice.Status
	o.InvoiceAmount = invoice.Amount
	if err := validateAuthorityInvoices(o, nil); err == nil {
		t.Fatal("missing selected invoice accepted")
	}
	if err := validateAuthorityInvoices(o, []ninjaauthority.Invoice{invoice}); err != nil {
		t.Fatal(err)
	}
	o.InvoiceAmount = "1"
	if err := validateAuthorityInvoices(o, []ninjaauthority.Invoice{invoice}); err == nil {
		t.Fatal("inconsistent selected invoice accepted")
	}
}
