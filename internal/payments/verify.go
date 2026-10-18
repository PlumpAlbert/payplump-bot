package payments

import (
	"crypto/sha256"
	"encoding/json"
	"math/big"
	"sort"
	"strconv"

	"github.com/PlumpAlbert/payplump-bot/internal/billing"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
)

func review(code int) Decision { return Decision{Kind: NeedsReview, Code: code} }
func money(value json.Number) (string, *big.Rat, bool) {
	s, err := billing.FormatAmount(value)
	if err != nil {
		return "", nil, false
	}
	r, ok := new(big.Rat).SetString(s)
	return s, r, ok
}
func zero(value json.Number) bool { _, r, ok := money(value); return ok && r.Sign() == 0 }
func one(value json.Number) bool  { _, r, ok := money(value); return ok && r.Cmp(big.NewRat(1, 1)) == 0 }

// Evaluate accepts only fully allocated, completed manual cash evidence in the
// configured native currency. Credits, FX and partially refunded sources are
// not supported. Provider status and zero balance alone are never proof.
func Evaluate(local Snapshot, customer invoiceninja.SettlementCustomer, invoice invoiceninja.SettlementInvoice, payments []invoiceninja.SettlementPayment, nativeCurrency, today string) Decision {
	if local.ClientID <= 0 || local.TelegramID <= 0 || local.ChatID != local.TelegramID || local.BotID <= 0 || !identifier(local.CustomerID) || !identifier(local.InvoiceID) || customer.ID != local.CustomerID || customer.IDNumber != strconv.FormatInt(local.TelegramID, 10) || customer.IsDeleted || customer.ArchivedAt != 0 || invoice.ID != local.InvoiceID || invoice.ClientID != local.CustomerID {
		return review(ReviewAssociation)
	}
	if local.InvoiceID == local.FirstInvoiceID {
		if invoice.RecurringID != "" {
			return review(ReviewAssociation)
		}
	} else if local.RecurringID == "" || invoice.RecurringID != local.RecurringID {
		return review(ReviewAssociation)
	}
	// Newly created customers inherit the explicitly confirmed company currency.
	// A group override with no explicit client currency is unknown, not guessed.
	if !currency(nativeCurrency) || customer.CurrencyID != "" && customer.CurrencyID != nativeCurrency || customer.CurrencyID == "" && customer.GroupID != "" || !one(invoice.ExchangeRate) {
		return review(ReviewCurrency)
	}
	amount, a, aok := money(invoice.Amount)
	_, balance, bok := money(invoice.Balance)
	_, paid, pok := money(invoice.PaidToDate)
	if !aok || !bok || !pok || a.Sign() <= 0 || balance.Cmp(a) > 0 || paid.Cmp(a) > 0 || invoice.IsDeleted || invoice.ArchivedAt != 0 || !validDate(invoice.Date) || !validDate(invoice.DueDate) || invoice.DueDate < invoice.Date {
		return review(ReviewInvoice)
	}
	if invoice.StatusID == "2" || invoice.StatusID == "3" {
		if balance.Sign() > 0 && paid.Cmp(a) < 0 {
			return Decision{Kind: Pending}
		}
		return review(ReviewInvoice)
	}
	if invoice.StatusID != "4" || balance.Sign() != 0 || paid.Cmp(a) != 0 {
		return review(ReviewInvoice)
	}
	if !validDate(today) || invoice.Number == "" || !printable(invoice.Number, 128) {
		return review(ReviewDate)
	}

	rootIDs := map[string]bool{}
	allocationIDs := map[string]bool{}
	var allocations []Allocation
	total := new(big.Rat)
	for _, payment := range payments {
		if !identifier(payment.ID) || rootIDs[payment.ID] || payment.ClientID != local.CustomerID {
			return review(ReviewAllocations)
		}
		rootIDs[payment.ID] = true
		target := false
		credit := false
		for _, allocation := range payment.Allocations {
			if allocation.InvoiceID == invoice.ID {
				target = true
			}
			if allocation.CreditID != "" {
				credit = true
			}
		}
		if !target {
			continue
		}
		// An old/refunded/cancelled payment is not cash evidence. Other clean
		// payments may still prove the full current amount independently.
		if payment.StatusID != "4" || payment.IsDeleted || payment.ArchivedAt != 0 || !payment.IsManual || credit || !zero(payment.Refunded) {
			continue
		}
		if payment.CurrencyID != nativeCurrency || payment.ExchangeCurrencyID != "" && payment.ExchangeCurrencyID != nativeCurrency || !one(payment.ExchangeRate) {
			return review(ReviewCurrency)
		}
		if !validDate(payment.Date) || payment.Date > today {
			return review(ReviewDate)
		}
		_, paymentAmount, maok := money(payment.Amount)
		_, applied, apok := money(payment.Applied)
		if !maok || !apok || paymentAmount.Sign() <= 0 || applied.Cmp(paymentAmount) > 0 {
			return review(ReviewAllocations)
		}
		allApplied := new(big.Rat)
		var candidate []Allocation
		candidateIDs := map[string]bool{}
		unsupported := false
		for _, allocation := range payment.Allocations {
			if !identifier(allocation.ID) || allocation.PaymentID != payment.ID || candidateIDs[allocation.ID] || allocationIDs[allocation.ID] || allocation.InvoiceID == "" || allocation.CreditID != "" {
				return review(ReviewAllocations)
			}
			candidateIDs[allocation.ID] = true
			value, n, ok := money(allocation.Amount)
			if !ok {
				return review(ReviewAllocations)
			}
			if allocation.ArchivedAt != 0 || !zero(allocation.Refunded) {
				unsupported = true
				continue
			}
			allApplied.Add(allApplied, n)
			if allocation.InvoiceID == invoice.ID && n.Sign() > 0 {
				candidate = append(candidate, Allocation{PaymentID: payment.ID, AllocationID: allocation.ID, Amount: value, Date: payment.Date})
			}
		}
		if unsupported {
			continue
		}
		if allApplied.Cmp(applied) != 0 {
			return review(ReviewAllocations)
		}
		for _, allocation := range candidate {
			n, _ := new(big.Rat).SetString(allocation.Amount)
			total.Add(total, n)
			allocations = append(allocations, allocation)
		}
		for id := range candidateIDs {
			allocationIDs[id] = true
		}
	}
	if len(allocations) == 0 || len(allocations) > 1000 || total.Cmp(a) != 0 {
		return review(ReviewAllocations)
	}
	sort.Slice(allocations, func(i, j int) bool {
		a, b := allocations[i], allocations[j]
		if a.Date != b.Date {
			return a.Date < b.Date
		}
		if a.PaymentID != b.PaymentID {
			return a.PaymentID < b.PaymentID
		}
		return a.AllocationID < b.AllocationID
	})
	date := allocations[len(allocations)-1].Date
	next, err := billing.NextBillingDate(date)
	if err != nil {
		return review(ReviewDate)
	}
	evidence := Evidence{InvoiceID: invoice.ID, CustomerID: customer.ID, Amount: amount, CurrencyID: nativeCurrency, PaymentDate: date, NextInvoiceDate: next, Allocations: allocations}
	body, err := json.Marshal(evidence)
	if err != nil || len(body) > 64*1024 {
		return review(ReviewAllocations)
	}
	proof := Proof{Evidence: evidence, InvoiceNumber: invoice.Number, Hash: sha256.Sum256(body), JSON: body}
	if proof.Validate() != nil {
		return review(ReviewAllocations)
	}
	return Decision{Kind: Verified, Proof: &proof}
}
