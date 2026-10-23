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
	if s.BotID != botID || s.ClientID <= 0 || s.TelegramID <= 0 || s.ChatID != s.TelegramID || s.ManagedProvenance == false || allowed != nil && !containsID(allowed, s.TelegramID) || s.CustomerID == "" || s.InvoiceID == "" || s.CurrentInvoiceID != s.InvoiceID || s.FirstInvoiceID == "" || s.FirstInvoiceID == s.InvoiceID || s.RecurringID == "" || s.SUIID == "" || s.RegistrationToken == "" || s.ActivationToken == "" {
		return false
	}
	if s.Payment.ClientID != s.ClientID || s.Payment.TelegramID != s.TelegramID || s.Payment.ChatID != s.ChatID || s.Payment.BotID != botID || s.Payment.CustomerID != s.CustomerID || s.Payment.InvoiceID != s.InvoiceID || s.Payment.FirstInvoiceID != s.FirstInvoiceID || s.Payment.RecurringID != s.RecurringID || s.Payment.State != s.State || s.Payment.DesiredAccess != s.DesiredAccess || s.Proof.InvoiceID != s.InvoiceID || s.Proof.CustomerID != s.CustomerID || s.Proof.Validate() != nil {
		return false
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
	if s.RecurringDraft.ClientID != s.CustomerID || s.RecurringDraft.PrivateNotes != activation.Marker(s.ActivationToken) || invoiceninja.ValidateRecurringDraft(s.RecurringDraft) != nil || s.SUIBinding.ID <= 0 || strconv.FormatInt(s.SUIBinding.ID, 10) != s.SUIID || s.SUIBinding.Name == "" {
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
