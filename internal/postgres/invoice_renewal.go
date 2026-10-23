package postgres

import (
	"encoding/json"
	"math/big"
	"strconv"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/billing"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/lifecycle"
)

func validRenewalStoreFacts(s lifecycle.RenewalSnapshot, botID int64, allowed []int64) bool {
	if s.BotID != botID || s.ClientID <= 0 || s.TelegramID <= 0 || s.ChatID != s.TelegramID || !s.ManagedProvenance || allowed != nil && !containsID(allowed, s.TelegramID) || s.CustomerID == "" || s.InvoiceID == "" || s.CurrentInvoiceID != s.InvoiceID || s.RecurringID == "" || s.SUIID == "" {
		return false
	}
	if s.ImportedOrigin.Valid() {
		if s.ImportedInitialPaused && !lifecycle.ValidImportedInitialAuthority(s) || !s.ImportedInitialPaused && s.ImportedInitialDebt.ID != "" {
			return false
		}
		if s.ImportedOrigin.ClientID != s.ClientID || s.ImportedOrigin.BotID != botID || s.FirstInvoiceID != "" || s.RegistrationToken != "" || s.ActivationToken != "" || s.ImportedAdmissionPermissionRevision <= 0 || s.ImportedEffectsRevision <= 0 || s.ImportedRecurringProfile.Hash() == "" || s.RecurringDraft.ClientID != "" || s.RecurringDraft.PrivateNotes != "" || len(s.ImportedInboundIDs) == 0 || s.Payment.FirstInvoiceID != "" || s.Payment.ImportedOrigin != s.ImportedOrigin || s.Payment.ImportedBaselineProofHash != ([32]byte{}) {
			return false
		}
		seen := map[int64]bool{}
		for _, id := range s.ImportedInboundIDs {
			if id <= 0 || seen[id] {
				return false
			}
			seen[id] = true
		}
	} else if s.FirstInvoiceID == "" || s.FirstInvoiceID == s.InvoiceID || s.RegistrationToken == "" || s.ActivationToken == "" || s.Payment.ImportedOrigin.Valid() || s.Payment.ImportedBaselineProofHash != ([32]byte{}) {
		return false
	}
	if s.Payment.ClientID != s.ClientID || s.Payment.TelegramID != s.TelegramID || s.Payment.ChatID != s.ChatID || s.Payment.BotID != botID || s.Payment.CustomerID != s.CustomerID || s.Payment.InvoiceID != s.InvoiceID || s.Payment.FirstInvoiceID != s.FirstInvoiceID || s.Payment.RecurringID != s.RecurringID || s.Payment.State != s.State || s.Payment.DesiredAccess != s.DesiredAccess || s.Proof.InvoiceID != s.InvoiceID || s.Proof.CustomerID != s.CustomerID || s.Proof.Validate() != nil {
		return false
	}
	if s.ImportedInitialPaused {
		return lifecycle.ValidImportedInitialAuthority(s) && s.LedgerCustomerID == "" && s.LedgerRecurringID == "" && s.LedgerInvoiceNumber == "" && s.LedgerStatus == "" && s.LedgerAmount == "" && s.LedgerBalance == "" && s.LedgerPaidToDate == "" && s.LedgerSentDate == "" && s.SUIBinding.ID > 0 && strconv.FormatInt(s.SUIBinding.ID, 10) == s.SUIID && s.SUIBinding.Name != "" && s.ImportedRecurringProfile.Hash() != ""
	}
	if s.LedgerCustomerID != s.CustomerID || s.LedgerRecurringID != s.RecurringID || s.LedgerInvoiceNumber != s.Proof.InvoiceNumber || s.LedgerAmount != s.Proof.Amount || s.LedgerStatus != "sent" && s.LedgerStatus != "partial" && s.LedgerStatus != "paid" {
		return false
	}
	if date, err := time.Parse(time.DateOnly, s.LedgerSentDate); err != nil || date.Format(time.DateOnly) != s.LedgerSentDate {
		return false
	}
	amount, err := billing.FormatAmount(json.Number(s.LedgerAmount))
	if err != nil || amount != s.LedgerAmount || amount != s.Proof.Amount {
		return false
	}
	balance, err := billing.FormatAmount(json.Number(s.LedgerBalance))
	if err != nil || balance != s.LedgerBalance {
		return false
	}
	paid, err := billing.FormatAmount(json.Number(s.LedgerPaidToDate))
	if err != nil || paid != s.LedgerPaidToDate {
		return false
	}
	a, aok := new(big.Rat).SetString(amount)
	b, bok := new(big.Rat).SetString(balance)
	p, pok := new(big.Rat).SetString(paid)
	if !aok || !bok || !pok || new(big.Rat).Add(b, p).Cmp(a) != 0 {
		return false
	}
	switch s.LedgerStatus {
	case "sent":
		if balance != amount || paid != "0" {
			return false
		}
	case "partial":
		if balance == "0" || paid == "0" {
			return false
		}
	case "paid":
		if balance != "0" || paid != amount {
			return false
		}
	default:
		return false
	}
	if s.SUIBinding.ID <= 0 || strconv.FormatInt(s.SUIBinding.ID, 10) != s.SUIID || s.SUIBinding.Name == "" {
		return false
	}
	if s.ImportedOrigin.Valid() {
		return s.ImportedRecurringProfile.Hash() != ""
	}
	if s.RecurringDraft.ClientID != s.CustomerID || s.RecurringDraft.PrivateNotes != activation.Marker(s.ActivationToken) || invoiceninja.ValidateRecurringDraft(s.RecurringDraft) != nil {
		return false
	}
	return true
}

func hasRenewalOverduePrefix(s lifecycle.RenewalSnapshot) bool {
	if s.OverduePending {
		return false
	}
	return s.OverdueSteps["pause_recurring"] && s.OverdueSteps["disable_sui"]
}

func hasRenewalOverduePausePrefix(s lifecycle.RenewalSnapshot) bool {
	return s.OverdueReviewPayment && !s.OverduePending && !s.OverdueOtherSteps && s.OverdueSteps["pause_recurring"] && len(s.OverdueSteps) == 1
}
