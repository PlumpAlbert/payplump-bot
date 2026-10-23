package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/importplan"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

// Exercises the command's real, version-pinned read clients and verification
// service against synthetic source APIs. No provider or DTO is substituted.
func TestImportVerifyWorkflowRealSources(t *testing.T) {
	const (
		ninjaToken  = "synthetic-ninja-token"
		suiToken    = "synthetic-panel-token"
		customerID  = "customer-42"
		recurringID = "recurring-42"
		invoiceID   = "invoice-42"
	)
	var recordRaw json.RawMessage
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
	recordRaw, err = json.Marshal(fields)
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

	requests := map[string]int{}
	var invoiceGets int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Body != http.NoBody && r.ContentLength != 0 {
			t.Errorf("not GET-only: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("X-API-TOKEN") != ninjaToken && r.Header.Get("Token") != suiToken {
			t.Errorf("missing synthetic auth on %s", r.URL.Path)
		}
		requests[r.URL.Path+"?"+r.URL.RawQuery]++
		if strings.HasPrefix(r.URL.Path, "/ninja/api/v1/") {
			w.Header().Set("X-APP-VERSION", "5.13.47")
			write := func(v any) { w.Header().Set("Content-Type", "application/json"); _ = json.NewEncoder(w).Encode(v) }
			switch r.URL.Path {
			case "/ninja/api/v1/clients":
				if r.URL.Query().Get("id_number") != "42" {
					t.Errorf("unbounded customer query: %s", r.URL.RawQuery)
				}
				write(ninjaPage([]any{customer}))
				return
			case "/ninja/api/v1/recurring_invoices":
				write(ninjaPage([]any{recurring}))
				return
			case "/ninja/api/v1/recurring_invoices/" + recurringID:
				write(map[string]any{"data": recurring, "links": map[string]string{"next": "http://must-not-follow.invalid"}})
				return
			case "/ninja/api/v1/invoices":
				write(ninjaPage([]any{invoice}))
				return
			case "/ninja/api/v1/invoices/" + invoiceID:
				invoiceGets++
				if invoiceGets%3 == 1 {
					write(map[string]any{"data": invoice})
				} else {
					write(map[string]any{"data": settlementInvoice})
				}
				return
			case "/ninja/api/v1/clients/" + customerID:
				write(map[string]any{"data": settlementCustomer})
				return
			case "/ninja/api/v1/payments":
				write(ninjaPage([]any{payment}))
				return
			}
		}
		writeSUI := func(v any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "obj": v})
		}
		switch r.URL.Path {
		case "/panel/apiv2/status":
			writeSUI(map[string]any{"sys": map[string]any{"appVersion": "1.6.4"}})
			return
		case "/panel/apiv2/clients":
			if r.URL.Query().Get("id") == "42" {
				writeSUI(map[string]any{"clients": []json.RawMessage{recordRaw}})
				return
			}
			writeSUI(map[string]any{"clients": []any{map[string]any{"id": 42, "name": "42", "enable": true, "desc": "", "remark": "", "inbounds": []int{1}, "expiry": 0, "volume": 0}}})
			return
		case "/panel/apiv2/settings":
			writeSUI(map[string]any{"subURI": "https://subs.invalid/", "subPath": "private-subpath", "subPort": "443", "subDomain": ""})
			return
		}
		t.Errorf("unexpected request %s?%s", r.URL.Path, r.URL.RawQuery)
		http.NotFound(w, r)
	}))
	defer server.Close()

	values := map[string]string{"INVOICE_NINJA_BASE_URL": server.URL + "/ninja", "INVOICE_NINJA_API_TOKEN": ninjaToken, "INVOICE_NINJA_VERSION": "5.13.47", "INVOICE_NINJA_TELEGRAM_ID_FIELD": "id_number", "SUI_BASE_URL": server.URL + "/panel", "SUI_API_TOKEN": suiToken, "SUI_VERSION": "1.6.4", "SUI_SUBSCRIPTION_BASE_URL": "https://subs.invalid/", "SUI_INBOUND_IDS": "1", "PAYMENTS_CURRENCY_ID": "1", "IMPORT_VERIFY_BOT_ID": "9", "IMPORT_VERIFY_ALLOWED_TELEGRAM_IDS": "42", "IMPORT_VERIFY_CURRENCY_CODE": "USD", "IMPORT_VERIFY_PRODUCT_KEY": "legacy", "TZ": "UTC"}
	getenv := func(k string) string {
		if k == "DATABASE_URL" || k == "TELEGRAM_ENABLED" {
			t.Fatalf("startup env accessed: %s", k)
		}
		return values[k]
	}
	_, err = loadVerifyConfig(getenv)
	if err != nil {
		t.Fatal(err)
	}
	claim := importplan.Record{TelegramID: "42", InvoiceCustomerID: customerID, InvoiceNumber: "42", CurrentInvoiceID: invoiceID, InvoiceStatus: "paid", RecurringID: recurringID, SUIID: "42", SUIName: "42", ConfigHash: configHash, PeriodStart: "2026-03-01", PeriodEnd: "2026-04-01", Currency: "USD", Lifecycle: "active", SourceVersion: "5.13.47", PinnedProvenance: true, CompleteProof: true, UnpaidInvoices: 0, Amount: "100"}
	manifest := filepath.Join(t.TempDir(), "manifest.json")
	data, err := claim.PrivateJSON()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(manifest, []byte(`{"records":[`+string(data)+`]}`), 0600); err != nil {
		t.Fatal(err)
	}
	plan := importplan.Analyze(importplan.Manifest{Records: []importplan.Record{claim}})
	evidencePath := filepath.Join(t.TempDir(), "evidence.json")
	var out bytes.Buffer
	err = importVerifyWith([]string{"--file", manifest, "--expected-hash", plan.Hash, "--read-only", "--evidence-file", evidencePath}, &out, getenv, newVerifyProviders, func() time.Time { return time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatalf("actual import-verify failed: %v", err)
	}
	var result struct {
		Verified     int    `json:"verified"`
		Manual       int    `json:"manual"`
		PlanHash     string `json:"plan_hash"`
		EvidenceHash string `json:"evidence_hash"`
	}
	if err = json.Unmarshal(out.Bytes(), &result); err != nil || result.Verified != 1 || result.Manual != 0 || result.PlanHash != plan.Hash {
		t.Fatalf("unexpected public result %s err=%v requests=%v", out.String(), err, requests)
	}
	st, err := os.Stat(evidencePath)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("private evidence permissions: %v %v", st, err)
	}
	evidence, err := os.ReadFile(evidencePath)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		SchemaVersion int                          `json:"schema_version"`
		EvidenceHash  string                       `json:"evidence_hash"`
		Evidence      []map[string]json.RawMessage `json:"evidence"`
	}
	if err = json.Unmarshal(evidence, &doc); err != nil || doc.SchemaVersion != 1 || doc.EvidenceHash != result.EvidenceHash || len(doc.Evidence) != 1 {
		t.Fatalf("invalid private evidence: %s err=%v", evidence, err)
	}
	for _, key := range []string{"customer_id", "current_invoice", "paid_anchor_invoice_id", "legacy_recurring", "sui_settings", "source_scope", "policy_hash"} {
		if _, ok := doc.Evidence[0][key]; !ok {
			t.Errorf("evidence missing %s", key)
		}
	}
	for _, secret := range []string{customerID, invoiceID, recurringID, ninjaToken, suiToken, server.URL, "legacy customer", "private-subpath", "historical legacy note"} {
		if strings.Contains(out.String(), secret) {
			t.Errorf("public output disclosed %q", secret)
		}
	}
	for path, n := range requests {
		if strings.Contains(path, "must-not-follow") || n == 0 {
			t.Errorf("unexpected request path %s", path)
		}
	}
	if countRequest(requests, "/ninja/api/v1/clients?id_number=") == 0 || countRequest(requests, "/panel/apiv2/settings?") == 0 {
		t.Fatalf("real source readers were not exercised: %v", requests)
	}
}

func countRequest(requests map[string]int, prefix string) int {
	total := 0
	for path, n := range requests {
		if strings.HasPrefix(path, prefix) {
			total += n
		}
	}
	return total
}

func ninjaPage(rows []any) map[string]any {
	return map[string]any{"data": rows, "meta": map[string]any{"pagination": map[string]any{"total": len(rows), "count": len(rows), "per_page": 50, "current_page": 1, "total_pages": 1, "links": map[string]string{"next": "https://must-not-follow.invalid"}}}}
}
