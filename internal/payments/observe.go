package payments

import (
	"context"
	"errors"
)

var errUnstableEvidence = errors.New("payment evidence changed during verification")

// Observe verifies payment evidence using GETs only and a caller-supplied current
// canonical local date. Snapshot ownership and financial policy remain Evaluate's
// responsibility. Failed/incomplete reads return errors, never review decisions.
func Observe(ctx context.Context, reader Reader, s Snapshot, currencyID, today string) (Decision, error) {
	if ctx == nil || reader == nil || !validDate(today) || !currency(currencyID) {
		return Decision{}, errors.New("invalid payment observation inputs")
	}
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	customer, err := reader.GetSettlementCustomer(ctx, s.CustomerID)
	if err != nil {
		return Decision{}, err
	}
	invoice, err := reader.GetSettlementInvoice(ctx, s.InvoiceID)
	if err != nil {
		return Decision{}, err
	}
	if invoice.StatusID != "4" {
		return Evaluate(s, customer, invoice, nil, currencyID, today), nil
	}
	first, err := reader.ListSettlementPayments(ctx, s.CustomerID)
	if err != nil {
		return Decision{}, err
	}
	decision := Evaluate(s, customer, invoice, first, currencyID, today)
	if decision.Kind != Verified {
		return decision, nil
	}
	// The provider has no transactional read snapshot. Require repeatable
	// allocation evidence and unchanged surrounding customer/invoice facts.
	second, err := reader.ListSettlementPayments(ctx, s.CustomerID)
	if err != nil {
		return Decision{}, err
	}
	firstHash, firstErr := inventoryHash(first)
	secondHash, secondErr := inventoryHash(second)
	if firstErr != nil || secondErr != nil || firstHash != secondHash {
		return Decision{}, errUnstableEvidence
	}
	after, err := reader.GetSettlementInvoice(ctx, s.InvoiceID)
	if err != nil {
		return Decision{}, err
	}
	owner, err := reader.GetSettlementCustomer(ctx, s.CustomerID)
	if err != nil {
		return Decision{}, err
	}
	if !sameInvoiceFacts(after, invoice) || owner != customer {
		return Decision{}, errUnstableEvidence
	}
	repeated := Evaluate(s, owner, after, second, currencyID, today)
	if repeated.Kind != Verified || repeated.Proof.Hash != decision.Proof.Hash {
		return Decision{}, errUnstableEvidence
	}
	return repeated, nil
}
