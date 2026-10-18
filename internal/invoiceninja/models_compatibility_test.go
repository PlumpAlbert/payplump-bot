package invoiceninja_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
)

func TestLegacyIntegerDiscountModeKeepsReadOnlyCompatibility(t *testing.T) {
	for _, mode := range []json.Number{"0", "1"} {
		t.Run(mode.String(), func(t *testing.T) {
			invoice := billingInvoiceObject(t)
			invoice["line_items"].([]any)[0].(map[string]any)["is_amount_discount"] = mode
			body := listBody("["+billingJSON(t, invoice)+"]", 1, 1, 1, 1, 1)
			s := billingServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Error("read-only call mutated data")
				}
				_, _ = io.WriteString(w, body)
			})
			readOnly, err := invoiceninja.New(s.URL, syntheticToken, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			page, err := readOnly.ListInvoices(context.Background(), 1, 1)
			if err != nil || len(page.Data) != 1 {
				t.Fatal("legacy numeric mode broke invoice inventory")
			}
			got := page.Data[0]
			if len(got.LineItems) != 1 || got.LineItems[0].IsAmountDiscount != (mode == "1") {
				t.Fatal("legacy numeric mode broke existing invoice reads")
			}
			if err := invoiceninja.ValidateInvoiceForDraft(got, billingInvoiceDraft(t)); err != nil {
				t.Fatal("zero discount mode normalization changed financial ownership")
			}
		})
	}
}
