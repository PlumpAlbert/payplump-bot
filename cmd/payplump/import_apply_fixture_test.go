package main

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/importplan"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

// importApplyFixture is a bounded, synthetic pair of source APIs plus a private
// manifest. It never opens a provider write route, Telegram connection, or DB.
type importApplyFixture struct {
	env                    map[string]string
	manifestPath, planHash string
	now                    time.Time
	GETs                   atomic.Int64
	fault                  atomic.Int32 // 1: payment GET unavailable, 2: current invoice unpaid
	server                 *httptest.Server
}

func newImportApplyFixture(t *testing.T) *importApplyFixture {
	t.Helper()
	const (
		ninjaToken  = "synthetic-ninja-token"
		suiToken    = "synthetic-panel-token"
		customerID  = "customer-42"
		recurringID = "recurring-42"
		invoiceID   = "invoice-42"
	)
	draft, err := sui.NewDisabledDraft([]int64{1})
	if err != nil {
		t.Fatal(err)
	}
	base, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(base, &fields); err != nil {
		t.Fatal(err)
	}
	fields["id"] = json.RawMessage(`42`)
	fields["name"] = json.RawMessage(`"42"`)
	fields["enable"] = json.RawMessage(`true`)
	fields["inbounds"] = json.RawMessage(`[1]`)
	var configs map[string]map[string]any
	if err = json.Unmarshal(fields["config"], &configs); err != nil {
		t.Fatal(err)
	}
	for _, c := range configs {
		for _, key := range []string{"name", "username"} {
			if _, ok := c[key]; ok {
				c[key] = "42"
			}
		}
	}
	fields["config"], err = json.Marshal(configs)
	if err != nil {
		t.Fatal(err)
	}
	recordRaw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := sui.ParseClientRecord(recordRaw)
	if err != nil {
		t.Fatal(err)
	}
	ch := parsed.CredentialsHash()
	configHash := hex.EncodeToString(ch[:])
	line := map[string]any{"product_key": "legacy", "notes": "legacy monthly", "cost": 100, "quantity": 1, "discount": 0, "is_amount_discount": false, "type_id": "1", "tax_name1": "", "tax_name2": "", "tax_name3": "", "tax_rate1": 0, "tax_rate2": 0, "tax_rate3": 0, "tax_id": ""}
	recurring := map[string]any{"id": recurringID, "client_id": customerID, "status_id": "2", "private_notes": "historical legacy note", "frequency_id": "5", "next_send_date": "2026-04-01", "next_send_datetime": "2026-04-01T00:00:00.000000Z", "last_sent_date": "2026-03-02", "due_date": "2026-04-02", "due_date_days": "on_receipt", "auto_bill": "off", "auto_bill_enabled": false, "remaining_cycles": -1, "amount": 100, "balance": 100, "uses_inclusive_taxes": false, "line_items": []any{line}, "is_deleted": false, "archived_at": 0, "discount": 0, "is_amount_discount": true, "tax_rate1": 0, "tax_rate2": 0, "tax_rate3": 0, "custom_surcharge1": 0, "custom_surcharge2": 0, "custom_surcharge3": 0, "custom_surcharge4": 0, "partial": 0, "paid_to_date": 0, "exchange_rate": 1}
	invoice := map[string]any{"id": invoiceID, "client_id": customerID, "recurring_id": recurringID, "number": "N-42", "private_notes": "paid current cycle", "status_id": "4", "date": "2026-03-01", "due_date": "2026-03-02", "amount": 100, "balance": 0, "paid_to_date": 100, "exchange_rate": 1, "is_deleted": false, "archived_at": 0, "updated_at": 0, "invitations": []any{}}
	customer := map[string]any{"id": customerID, "id_number": "42", "private_notes": "legacy customer", "is_deleted": false, "archived_at": 0}
	settlementCustomer := map[string]any{"id": customerID, "id_number": "42", "group_settings_id": "", "settings": map[string]any{"currency_id": "1"}, "currency_id": "ignored", "is_deleted": false, "archived_at": 0}
	settlementInvoice := map[string]any{"id": invoiceID, "client_id": customerID, "recurring_id": recurringID, "number": "N-42", "status_id": "4", "date": "2026-03-01", "due_date": "2026-03-02", "amount": 100, "balance": 0, "paid_to_date": 100, "exchange_rate": 1, "is_deleted": false, "archived_at": 0}
	allocation := map[string]any{"id": "allocation-42", "invoice_id": invoiceID, "payment_id": "payment-42", "amount": 100, "refunded": 0, "archived_at": 0}
	payment := map[string]any{"id": "payment-42", "client_id": customerID, "date": "2026-03-01", "status_id": "4", "currency_id": "1", "exchange_currency_id": "", "amount": 100, "refunded": 0, "applied": 100, "exchange_rate": 1, "is_manual": true, "is_deleted": false, "archived_at": 0, "paymentables": []any{allocation}, "invoices": []any{}}
	f := &importApplyFixture{now: time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)}
	var invoiceGets atomic.Int64
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || (r.Body != http.NoBody && r.ContentLength != 0) {
			t.Errorf("unexpected source method/body: %s %s", r.Method, r.URL)
		}
		f.GETs.Add(1)
		if r.Header.Get("X-API-TOKEN") != ninjaToken && r.Header.Get("Token") != suiToken {
			t.Errorf("missing synthetic source auth on %s", r.URL.Path)
		}
		if strings.HasPrefix(r.URL.Path, "/ninja/api/v1/") {
			w.Header().Set("X-APP-VERSION", "5.13.47")
			write := func(v any) { w.Header().Set("Content-Type", "application/json"); _ = json.NewEncoder(w).Encode(v) }
			switch r.URL.Path {
			case "/ninja/api/v1/clients":
				if r.URL.Query().Get("id_number") != "42" {
					t.Errorf("unbounded customer query: %s", r.URL.RawQuery)
				}
				write(ninjaPage([]any{customer}))
			case "/ninja/api/v1/recurring_invoices":
				write(ninjaPage([]any{recurring}))
			case "/ninja/api/v1/recurring_invoices/" + recurringID:
				write(map[string]any{"data": recurring, "links": map[string]string{"next": "http://must-not-follow.invalid"}})
			case "/ninja/api/v1/invoices":
				write(ninjaPage([]any{invoice}))
			case "/ninja/api/v1/invoices/" + invoiceID:
				if f.fault.Load() == 2 {
					unpaid := make(map[string]any, len(invoice))
					for k, v := range invoice {
						unpaid[k] = v
					}
					unpaid["status_id"], unpaid["balance"], unpaid["paid_to_date"] = "2", 100, 0
					write(map[string]any{"data": unpaid})
				} else if invoiceGets.Add(1)%3 == 1 {
					write(map[string]any{"data": invoice})
				} else {
					write(map[string]any{"data": settlementInvoice})
				}
			case "/ninja/api/v1/clients/" + customerID:
				write(map[string]any{"data": settlementCustomer})
			case "/ninja/api/v1/payments":
				if f.fault.Load() == 1 {
					http.Error(w, "synthetic outage", http.StatusServiceUnavailable)
					return
				}
				write(ninjaPage([]any{payment}))
			default:
				http.NotFound(w, r)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		writeSUI := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"success": true, "obj": v}) }
		switch r.URL.Path {
		case "/panel/apiv2/status":
			writeSUI(map[string]any{"sys": map[string]any{"appVersion": "1.6.4"}})
		case "/panel/apiv2/clients":
			if r.URL.Query().Get("id") == "42" {
				writeSUI(map[string]any{"clients": []json.RawMessage{recordRaw}})
			} else {
				writeSUI(map[string]any{"clients": []any{map[string]any{"id": 42, "name": "42", "enable": true, "desc": "", "remark": "", "inbounds": []int{1}, "expiry": 0, "volume": 0}}})
			}
		case "/panel/apiv2/settings":
			writeSUI(map[string]any{"subURI": "https://subs.invalid/", "subPath": "private-subpath", "subPort": "443", "subDomain": ""})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	f.env = map[string]string{"INVOICE_NINJA_BASE_URL": f.server.URL + "/ninja", "INVOICE_NINJA_API_TOKEN": ninjaToken, "INVOICE_NINJA_VERSION": "5.13.47", "INVOICE_NINJA_TELEGRAM_ID_FIELD": "id_number", "SUI_BASE_URL": f.server.URL + "/panel", "SUI_API_TOKEN": suiToken, "SUI_VERSION": "1.6.4", "SUI_SUBSCRIPTION_BASE_URL": "https://subs.invalid/", "SUI_INBOUND_IDS": "1", "PAYMENTS_CURRENCY_ID": "1", "IMPORT_VERIFY_BOT_ID": "9", "IMPORT_VERIFY_ALLOWED_TELEGRAM_IDS": "42", "IMPORT_VERIFY_CURRENCY_CODE": "USD", "IMPORT_VERIFY_PRODUCT_KEY": "legacy", "TZ": "UTC"}
	claim := importplan.Record{TelegramID: "42", InvoiceCustomerID: customerID, InvoiceNumber: "42", CurrentInvoiceID: invoiceID, InvoiceStatus: "paid", RecurringID: recurringID, SUIID: "42", SUIName: "42", ConfigHash: configHash, PeriodStart: "2026-03-01", PeriodEnd: "2026-04-01", Currency: "USD", Lifecycle: "active", SourceVersion: "5.13.47", PinnedProvenance: true, CompleteProof: true, UnpaidInvoices: 0, Amount: "100"}
	data, err := claim.PrivateJSON()
	if err != nil {
		t.Fatal(err)
	}
	f.manifestPath = filepath.Join(t.TempDir(), "manifest.json")
	if err = os.WriteFile(f.manifestPath, []byte(`{"records":[`+string(data)+`]}`), 0600); err != nil {
		t.Fatal(err)
	}
	f.planHash = importplan.Analyze(importplan.Manifest{Records: []importplan.Record{claim}}).Hash
	return f
}
