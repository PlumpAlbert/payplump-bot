package invoiceninja_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
)

func settlementJSONAliases(key string) []string {
	aliases := []string{key, strings.ToUpper(key)}
	if strings.Contains(key, "s") {
		aliases = append(aliases, strings.Replace(key, "s", "ſ", 1))
	}
	if strings.Contains(key, "k") {
		aliases = append(aliases, strings.Replace(key, "k", "K", 1))
	}
	return aliases
}

// Splice without decoding: marshaling a map would erase the vulnerability.
func settlementJSONAppend(body, key, value string) string {
	return body[:len(body)-1] + fmt.Sprintf(",%q:%s}", key, value)
}

func TestSettlementUniqueJSONReaderAndDirectDTO(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		newDTO     func() json.Unmarshaler
		wrap       func(string) string
		read       func(*invoiceninja.SettlementClient) error
	}{
		{"customer", settlementCustomerJSON, func() json.Unmarshaler { return new(invoiceninja.SettlementCustomer) },
			func(body string) string { return `{"data":` + body + `}` },
			func(c *invoiceninja.SettlementClient) error {
				_, err := c.GetSettlementCustomer(context.Background(), "synthetic-customer")
				return err
			}},
		{"invoice", settlementInvoiceJSON, func() json.Unmarshaler { return new(invoiceninja.SettlementInvoice) },
			func(body string) string { return `{"data":` + body + `}` },
			func(c *invoiceninja.SettlementClient) error {
				_, err := c.GetSettlementInvoice(context.Background(), "synthetic-invoice")
				return err
			}},
		{"payment", settlementPaymentJSON, func() json.Unmarshaler { return new(invoiceninja.SettlementPayment) },
			func(body string) string { return settlementPage([]string{body}, 1, 1, 1, 1) },
			func(c *invoiceninja.SettlementClient) error {
				_, err := c.ListSettlementPayments(context.Background(), "synthetic-customer")
				return err
			}},
		{"allocation", settlementAllocationJSON, func() json.Unmarshaler { return new(invoiceninja.SettlementAllocation) },
			func(body string) string {
				return settlementPage([]string{strings.Replace(settlementPaymentJSON, settlementAllocationJSON, body, 1)}, 1, 1, 1, 1)
			},
			func(c *invoiceninja.SettlementClient) error {
				_, err := c.ListSettlementPayments(context.Background(), "synthetic-customer")
				return err
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertRejected := func(t *testing.T, body string) {
				t.Helper()
				if err := json.Unmarshal([]byte(body), tc.newDTO()); err == nil {
					t.Fatal("direct DTO erased ambiguous financial facts")
				}
				err := tc.read(settlementStatic(t, tc.wrap(body), 1, 1))
				settlementRedacted(t, err)
				if (tc.name == "payment" || tc.name == "allocation") && !errors.Is(err, invoiceninja.ErrSettlementIncomplete) {
					t.Fatal("ambiguous inventory was not incomplete", err)
				}
			}
			valid := settlementJSONAppend(tc.body, "未知", `{"ſ":1,"K":2,"é":3,"e":4,"large":1e10000}`)
			if err := json.Unmarshal([]byte(valid), tc.newDTO()); err != nil {
				t.Fatal("unique Unicode metadata rejected", err)
			}
			if err := tc.read(settlementStatic(t, tc.wrap(valid), 1, 1)); err != nil {
				t.Fatal("reader rejected unique Unicode metadata", err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tc.body), &fields); err != nil {
				t.Fatal(err)
			}
			for key, value := range fields {
				for _, alias := range settlementJSONAliases(key) {
					t.Run(alias, func(t *testing.T) {
						assertRejected(t, settlementJSONAppend(tc.body, alias, string(value)))
					})
				}
			}
			for name, value := range map[string]string{
				"nested":  `[{"amount":1,"AMOUNT":2}]`,
				"unicode": `{"items":[{"s":1,"ſ":2},{"k":1,"K":2}]}`,
				"depth":   strings.Repeat("[", 129) + "0" + strings.Repeat("]", 129),
			} {
				t.Run(name, func(t *testing.T) { assertRejected(t, settlementJSONAppend(tc.body, "unknown", value)) })
			}
			t.Run("trailing", func(t *testing.T) { assertRejected(t, tc.body+` {}`) })
			if tc.name == "customer" {
				for _, alias := range settlementJSONAliases("currency_id") {
					t.Run("settings/"+alias, func(t *testing.T) {
						body := strings.Replace(tc.body, `"settings":{"currency_id":"1"}`, `"settings":{"currency_id":"1",`+fmt.Sprintf(`%q:"2"}`, alias), 1)
						assertRejected(t, body)
					})
				}
			}
			if tc.name == "payment" {
				t.Run("nested-allocation", func(t *testing.T) {
					allocation := settlementJSONAppend(settlementAllocationJSON, "AMOUNT", "0")
					assertRejected(t, strings.Replace(tc.body, settlementAllocationJSON, allocation, 1))
				})
			}
		})
	}
}

func TestSettlementUniqueJSONEnvelopeAndPagination(t *testing.T) {
	base := `{"data":` + settlementInvoiceJSON + `}`
	for name, body := range map[string]string{
		"duplicate-data": settlementJSONAppend(base, "data", settlementInvoiceJSON),
		"case-data":      settlementJSONAppend(base, "DATA", settlementInvoiceJSON),
		"meta":           settlementJSONAppend(settlementJSONAppend(base, "meta", `{}`), "META", `{}`),
		"pagination":     settlementJSONAppend(base, "meta", `{"pagination":{"total":0,"TOTAL":1}}`),
		"unicode":        settlementJSONAppend(base, "meta", `{"links":[{"K":0,"K":1}]}`),
		"trailing":       base + ` null`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := settlementStatic(t, body, 1, 1).GetSettlementInvoice(context.Background(), "synthetic-invoice")
			settlementRedacted(t, err)
		})
	}
	page := settlementPage([]string{settlementPaymentJSON}, 1, 1, 1, 1)
	for _, key := range []string{"meta", "pagination", "total", "count", "per_page", "current_page", "total_pages"} {
		for _, alias := range settlementJSONAliases(key) {
			t.Run("inventory/"+alias, func(t *testing.T) {
				value := "1"
				if key == "meta" || key == "pagination" {
					value = "{}"
				}
				body := strings.Replace(page, fmt.Sprintf(`%q:`, key), fmt.Sprintf(`%q:%s,%q:`, alias, value, key), 1)
				_, err := settlementStatic(t, body, 1, 1).ListSettlementPayments(context.Background(), "synthetic-customer")
				if !errors.Is(err, invoiceninja.ErrSettlementIncomplete) || !errors.Is(err, invoiceninja.ErrSettlementContract) {
					t.Fatal("ambiguous pagination was not a contract failure", err)
				}
			})
		}
	}
}

func TestSettlementUniqueJSONPreservesPendingDeletedPayment(t *testing.T) {
	body := settlementField(t, settlementPaymentJSON, "status_id", `"1"`)
	body = settlementField(t, body, "currency_id", `""`)
	body = settlementField(t, body, "is_deleted", `true`)
	body = settlementField(t, body, "archived_at", `99`)
	body = settlementJSONAppend(body, "未知", `{"ſ":1,"K":2}`)
	var direct invoiceninja.SettlementPayment
	if err := json.Unmarshal([]byte(body), &direct); err != nil {
		t.Fatal("valid history rejected", err)
	}
	payments, err := settlementStatic(t, settlementPage([]string{body}, 1, 1, 1, 1), 1, 1).ListSettlementPayments(context.Background(), "synthetic-customer")
	if err != nil || len(payments) != 1 || payments[0].StatusID != "1" || payments[0].CurrencyID != "" || !payments[0].IsDeleted || payments[0].ArchivedAt != 99 || payments[0].Amount.String() != settlementExact || direct.Amount != payments[0].Amount {
		t.Fatal("pending/deleted history or exact money changed", err)
	}
}
