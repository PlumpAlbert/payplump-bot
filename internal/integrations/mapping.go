package integrations

import (
	"strconv"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

// Exact matches are read-only import candidates, not permission to manage an
// account. Display labels are never identifiers, and unmatched S-UI accounts
// remain outside the bot's scope. Existing names and subscription URLs stay put.
func mapExactIDs(report *Report, customers []invoiceninja.Customer, accounts []sui.Account, cfg Config) {
	byName := make(map[string][]sui.Account)
	accountIDs := make(map[int64]int)
	for _, account := range accounts {
		byName[account.Name] = append(byName[account.Name], account)
		accountIDs[account.ID]++
	}
	for _, matches := range byName {
		if len(matches) > 1 {
			report.Mapping.DuplicateSUINames++
		}
	}
	for _, count := range accountIDs {
		if count > 1 {
			report.Mapping.DuplicateSUIRecordIDs++
		}
	}
	var eligible []string
	identities := make(map[string]int)
	for _, customer := range customers {
		if !customer.Active() {
			continue
		}
		report.InvoiceNinja.ActiveClients++
		if fieldValue(customer, cfg.InvoiceNinja.TelegramIDField) == "" {
			report.Mapping.InvoiceClientsWithoutTelegramID++
			continue
		}
		id, err := customer.TelegramID(cfg.InvoiceNinja.TelegramIDField)
		if err != nil {
			report.Mapping.InvalidTelegramIDs++
			continue
		}
		key := strconv.FormatInt(id, 10)
		identities[key]++
		eligible = append(eligible, key)
	}
	for _, count := range identities {
		if count > 1 {
			report.Mapping.DuplicateTelegramIDs++
		}
	}
	for _, key := range eligible {
		matches := byName[key]
		if identities[key] != 1 || len(matches) != 1 {
			report.Mapping.InvoiceClientsWithoutSUI++
			continue
		}
		report.Mapping.ExactMatches++
		account := matches[0]
		if !account.Enabled {
			report.Mapping.MatchedAccountsDisabled++
		}
		assigned := make(map[int64]bool)
		for _, id := range account.Inbounds {
			assigned[id] = true
		}
		for _, id := range cfg.SUI.InboundIDs {
			if !assigned[id] {
				report.Mapping.MatchedWithoutConfiguredInbounds++
				break
			}
		}
	}
	for _, account := range accounts {
		if identities[account.Name] != 1 || len(byName[account.Name]) != 1 {
			report.Mapping.SUIOutsideExactMapping++
		}
	}
}

func fieldValue(c invoiceninja.Customer, field string) string {
	switch field {
	case "id_number":
		return c.IDNumber
	case "custom_value1":
		return c.CustomValue1
	case "custom_value2":
		return c.CustomValue2
	case "custom_value3":
		return c.CustomValue3
	case "custom_value4":
		return c.CustomValue4
	default:
		return ""
	}
}
