package lifecycle

import (
	"math/big"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/billing"
)

// validImportedInitialDebt authenticates only the frozen, unpaid baseline invoice.
// The current payment is independently re-observed through payments.Observe.
func validImportedInitialDebt(s RenewalSnapshot) bool {
	debt := s.ImportedInitialDebt
	if !s.ImportedInitialPaused || debt.ID == "" || debt.ID != s.InvoiceID || debt.ClientID != s.CustomerID || debt.RecurringID != s.RecurringID ||
		debt.Number == "" || debt.Number != s.Proof.InvoiceNumber || debt.StatusID != "2" || debt.IsDeleted || debt.ArchivedAt != 0 ||
		s.ImportedPaidAnchorInvoiceID == "" || s.ImportedPaidAnchorInvoiceID == debt.ID || debt.Date == "" {
		return false
	}
	if date, err := time.Parse(time.DateOnly, debt.Date); err != nil || date.Format(time.DateOnly) != debt.Date {
		return false
	}
	amount, err := billing.FormatAmount(debt.Amount)
	if err != nil || amount != s.Proof.Amount {
		return false
	}
	balance, err := billing.FormatAmount(debt.Balance)
	if err != nil || balance != amount {
		return false
	}
	paid, err := billing.FormatAmount(debt.PaidToDate)
	if err != nil || paid != "0" {
		return false
	}
	a, aok := new(big.Rat).SetString(amount)
	b, bok := new(big.Rat).SetString(balance)
	p, pok := new(big.Rat).SetString(paid)
	return aok && bok && pok && a.Sign() > 0 && new(big.Rat).Add(b, p).Cmp(a) == 0
}

func zeroRenewalLedger(s RenewalSnapshot) bool {
	return s.LedgerCustomerID == "" && s.LedgerRecurringID == "" && s.LedgerInvoiceNumber == "" && s.LedgerStatus == "" &&
		s.LedgerAmount == "" && s.LedgerBalance == "" && s.LedgerPaidToDate == "" && s.LedgerSentDate == ""
}

// ValidImportedInitialAuthority accepts only the immutable imported current-debt baseline,
// the matching new payment proof, and an action snapshot with no synthetic ledger facts.
func ValidImportedInitialAuthority(s RenewalSnapshot) bool {
	return s.ImportedInitialPaused && s.State == "overdue" && !s.DesiredAccess && s.Payment.State == "overdue" && !s.Payment.DesiredAccess && validImportedInitialDebt(s) && zeroRenewalLedger(s) &&
		s.ImportedOrigin.Valid() && s.ImportedOrigin.ClientID == s.ClientID && s.Payment.InvoiceID == s.InvoiceID && s.Proof.InvoiceID == s.InvoiceID && s.Proof.CustomerID == s.CustomerID && s.Proof.Validate() == nil &&
		s.ImportedPaidAnchorInvoiceID != s.ImportedInitialDebt.ID && s.FirstInvoiceID == "" &&
		s.RegistrationToken == "" && s.ActivationToken == "" && s.Payment.FirstInvoiceID == "" &&
		s.Payment.ImportedBaselineProofHash == ([32]byte{})
}
