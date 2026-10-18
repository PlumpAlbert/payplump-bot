package payments

import (
	"crypto/sha256"
	"encoding/json"
	"slices"
	"sort"
	"strings"

	"github.com/PlumpAlbert/payplump-bot/internal/billing"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
)

// Compare complete read inventories, including ignored/non-target records, so a
// concurrently changing source cannot be laundered through the smaller proof.
// Sorting and exact decimal normalization tolerate representation/order only.
func inventoryHash(input []invoiceninja.SettlementPayment) ([32]byte, error) {
	items := slices.Clone(input)
	for i := range items {
		p := &items[i]
		p.Allocations = slices.Clone(p.Allocations)
		for _, value := range []*json.Number{&p.Amount, &p.Refunded, &p.Applied, &p.ExchangeRate} {
			if err := normalizeNumber(value); err != nil {
				return [32]byte{}, err
			}
		}
		for j := range p.Allocations {
			a := &p.Allocations[j]
			if err := normalizeNumber(&a.Amount); err != nil {
				return [32]byte{}, err
			}
			if err := normalizeNumber(&a.Refunded); err != nil {
				return [32]byte{}, err
			}
		}
		sort.Slice(p.Allocations, func(i, j int) bool { return p.Allocations[i].ID < p.Allocations[j].ID })
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	body, err := json.Marshal(items)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(body), nil
}
func sameInvoiceFacts(a, b invoiceninja.SettlementInvoice) bool {
	for _, invoice := range []*invoiceninja.SettlementInvoice{&a, &b} {
		for _, value := range []*json.Number{&invoice.Amount, &invoice.Balance, &invoice.PaidToDate, &invoice.ExchangeRate} {
			if normalizeNumber(value) != nil {
				return false
			}
		}
	}
	return a == b
}

func normalizeNumber(value *json.Number) error {
	raw := value.String()
	negative := strings.HasPrefix(raw, "-")
	if negative {
		raw = raw[1:]
	}
	normalized, err := billing.FormatAmount(json.Number(raw))
	if err != nil {
		return err
	}
	if negative && normalized != "0" {
		normalized = "-" + normalized
	}
	*value = json.Number(normalized)
	return nil
}
