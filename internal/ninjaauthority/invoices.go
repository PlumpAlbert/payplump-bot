package ninjaauthority

import "github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"

// observeInvoices validates the whole inventory before returning any ledger
// batch. Menu ordering is only a deterministic representative, not proof of
// creation order or a financial period. Empty dates remain eligible.
func observeInvoices(customer string, records []invoiceninja.CycleInvoice) ([]Invoice, *invoiceninja.CycleInvoice, bool) {
	invoices := make([]Invoice, 0, len(records))
	seen := make(map[string]bool, len(records))
	var representative *invoiceninja.CycleInvoice
	for idx := range records {
		i := &records[idx]
		if i.ID == "" || seen[i.ID] || i.ClientID != customer {
			return nil, nil, false
		}
		seen[i.ID] = true
		invoices = append(invoices, Invoice{ID: i.ID, CustomerID: i.ClientID, Number: i.Number, Status: i.StatusID, Date: i.Date, Amount: i.Amount.String(), ArchivedAt: i.ArchivedAt, IsDeleted: i.IsDeleted})
		if i.IsDeleted || i.ArchivedAt != 0 || i.StatusID == "5" {
			continue
		}
		if representative == nil || i.Date > representative.Date ||
			(i.Date == representative.Date && (i.UpdatedAt > representative.UpdatedAt ||
				(i.UpdatedAt == representative.UpdatedAt && i.ID > representative.ID))) {
			representative = i
		}
	}
	return invoices, representative, true
}
