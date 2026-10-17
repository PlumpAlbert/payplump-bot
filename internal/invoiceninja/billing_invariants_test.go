package invoiceninja_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
)

func billingInvoiceObject(t *testing.T) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(fixture(t, "billing_invoice_draft")))
	decoder.UseNumber()
	var envelope map[string]any
	if decoder.Decode(&envelope) != nil {
		t.Fatal("bad synthetic fixture")
	}
	return envelope["data"].(map[string]any)
}

func TestBillingCreateRejectsUnrequestedFinancialAdjustments(t *testing.T) {
	changes := map[string]func(map[string]any){
		"line discount": func(i map[string]any) { i["line_items"].([]any)[0].(map[string]any)["discount"] = json.Number("50") },
		"fee line":      func(i map[string]any) { i["line_items"].([]any)[0].(map[string]any)["type_id"] = "4" },
	}
	for _, key := range []string{"discount", "tax_rate1", "tax_rate2", "tax_rate3", "custom_surcharge1", "custom_surcharge2", "custom_surcharge3", "custom_surcharge4", "partial", "paid_to_date"} {
		changes[key] = func(i map[string]any) { i[key] = json.Number("10") }
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			invoice := billingInvoiceObject(t)
			change(invoice)
			body := billingJSON(t, map[string]any{"data": invoice})
			posts := 0
			s := billingServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					posts++
					_, _ = io.WriteString(w, body)
				} else {
					_, _ = io.WriteString(w, `{"data":[]}`)
				}
			})
			if _, err := billingClientAt(t, s.URL, 1, 1).CreateInvoice(context.Background(), billingInvoiceDraft(t)); err == nil || posts != 1 {
				t.Fatal("unexpected financial adjustment accepted or POST retried")
			}
		})
	}
}

func TestBillingRequiresExplicitResponseAdjustmentFields(t *testing.T) {
	for _, key := range []string{"discount", "is_amount_discount", "tax_rate1", "tax_rate2", "tax_rate3", "custom_surcharge1", "custom_surcharge2", "custom_surcharge3", "custom_surcharge4", "partial", "paid_to_date", "line:discount", "line:is_amount_discount", "line:type_id", "recurring_id", "is_deleted", "archived_at"} {
		t.Run(key, func(t *testing.T) {
			invoice := billingInvoiceObject(t)
			if strings.HasPrefix(key, "line:") {
				delete(invoice["line_items"].([]any)[0].(map[string]any), strings.TrimPrefix(key, "line:"))
			} else {
				delete(invoice, key)
			}
			body := billingJSON(t, map[string]any{"data": invoice})
			s := billingServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Error("read used a write")
				}
				_, _ = io.WriteString(w, body)
			})
			if _, err := billingClientAt(t, s.URL, 1, 1).GetInvoice(context.Background(), "billing-invoice"); err == nil {
				t.Fatal("missing financial field was silently treated as zero")
			}
		})
	}
}

func TestBillingCustomerLifecycleFieldsCannotDefaultToActive(t *testing.T) {
	for _, key := range []string{"is_deleted", "archived_at"} {
		for _, null := range []bool{false, true} {
			var envelope map[string]any
			if json.Unmarshal([]byte(fixture(t, "billing_customer")), &envelope) != nil {
				t.Fatal("bad customer fixture")
			}
			customer := envelope["data"].(map[string]any)
			if null {
				customer[key] = nil
			} else {
				delete(customer, key)
			}
			body := billingJSON(t, envelope)
			s := billingServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					_, _ = io.WriteString(w, body)
				} else {
					_, _ = io.WriteString(w, `{"data":[]}`)
				}
			})
			if _, err := billingClientAt(t, s.URL, 1, 1).CreateCustomer(context.Background(), billingDraft()); err == nil {
				t.Fatal("missing lifecycle evidence defaulted to active")
			}
		}
	}
}

func TestBillingLookupRetainsMissingFieldEvidenceForReconciliation(t *testing.T) {
	invoice := billingInvoiceObject(t)
	delete(invoice, "discount")
	body := listBody("["+billingJSON(t, invoice)+"]", 1, 1, 1, 1, 1)
	s := billingServer(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) })
	items, err := billingClientAt(t, s.URL, 1, 1).FindInvoices(context.Background(), "billing-client")
	if err != nil || len(items) != 1 {
		t.Fatal("inventory could not expose the conflicting record")
	}
	if invoiceninja.ValidateInvoiceForDraft(items[0], billingInvoiceDraft(t)) == nil {
		t.Fatal("reconciliation lost missing-field evidence")
	}
}

func TestBillingPresentationAndZeroDiscountModeAreNotIdentity(t *testing.T) {
	invoice := billingInvoice(t)
	draft := billingInvoiceDraft(t)
	invoice.LineItems[0].Notes = "Expanded source keyword: Synthetic customer"
	invoice.LineItems[0].IsAmountDiscount = !invoice.LineItems[0].IsAmountDiscount
	invoice.IsAmountDiscount = !invoice.IsAmountDiscount
	if err := invoiceninja.ValidateInvoiceForDraft(invoice, draft); err != nil {
		t.Fatal("presentation or a zero-discount mode changed financial ownership")
	}
	draft.LineItems[0].Discount = "1"
	if invoiceninja.ValidateInvoiceDraft(draft) == nil {
		t.Fatal("caller could inject an unsupported discount")
	}
}
