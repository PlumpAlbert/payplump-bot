package importverify

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/importplan"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

const (
	workflowNow       = "2026-03-10"
	workflowToken     = "synthetic-workflow-token"
	workflowCustomer  = "customer-42"
	workflowRecurring = "recurring-42"
	workflowInvoice   = "invoice-42"
)

// The fixtures model the pinned legacy sources, entirely behind httptest.
// Every request is asserted GET-only and response-provided links are decoys.
func TestWorkflowRealGETReadersVerifyLegacyPaidBridge(t *testing.T) {
	policy := validPolicy()
	policy.MaxRecords = 10
	policy.MaxInventoryRecords = 20
	policy.SourceScopeHash = strings.Repeat("a", 64)

	suiDraft, err := sui.NewDisabledDraft([]int64{1})
	if err != nil {
		t.Fatal(err)
	}
	suiRaw, err := json.Marshal(suiDraft)
	if err != nil {
		t.Fatal(err)
	}
	var suiFields map[string]json.RawMessage
	if err = json.Unmarshal(suiRaw, &suiFields); err != nil {
		t.Fatal(err)
	}
	suiFields["id"] = json.RawMessage(`42`)
	suiFields["name"] = json.RawMessage(`"42"`)
	suiFields["enable"] = json.RawMessage(`true`)
	suiFields["inbounds"] = json.RawMessage(`[1]`)
	var configs map[string]map[string]any
	if err = json.Unmarshal(suiFields["config"], &configs); err != nil {
		t.Fatal(err)
	}
	for _, config := range configs {
		for _, key := range []string{"name", "username"} {
			if _, ok := config[key]; ok {
				config[key] = "42"
			}
		}
	}
	suiFields["config"], err = json.Marshal(configs)
	if err != nil {
		t.Fatal(err)
	}
	suiRaw, err = json.Marshal(suiFields)
	if err != nil {
		t.Fatal(err)
	}
	parsedRecord, err := sui.ParseClientRecord(suiRaw)
	if err != nil {
		t.Fatal(err)
	}
	configSum := parsedRecord.CredentialsHash()
	configHash := hex.EncodeToString(configSum[:])

	line := map[string]any{"product_key": "legacy", "notes": "legacy monthly", "cost": 100, "quantity": 1, "discount": 0, "is_amount_discount": false, "type_id": "1", "tax_name1": "", "tax_name2": "", "tax_name3": "", "tax_rate1": 0, "tax_rate2": 0, "tax_rate3": 0, "tax_id": ""}
	recurring := map[string]any{"id": workflowRecurring, "client_id": workflowCustomer, "status_id": "2", "private_notes": "historical legacy note", "frequency_id": "5", "next_send_date": "2026-04-01", "next_send_datetime": "2026-04-01T00:00:00.000000Z", "last_sent_date": "2026-03-02", "due_date": "2026-04-02", "due_date_days": "on_receipt", "auto_bill": "off", "auto_bill_enabled": false, "remaining_cycles": -1, "amount": 100, "balance": 100, "uses_inclusive_taxes": false, "line_items": []any{line}, "is_deleted": false, "archived_at": 0, "discount": 0, "is_amount_discount": true, "tax_rate1": 0, "tax_rate2": 0, "tax_rate3": 0, "custom_surcharge1": 0, "custom_surcharge2": 0, "custom_surcharge3": 0, "custom_surcharge4": 0, "partial": 0, "paid_to_date": 0, "exchange_rate": 1}
	invoice := map[string]any{"id": workflowInvoice, "client_id": workflowCustomer, "recurring_id": workflowRecurring, "number": "N-42", "private_notes": "paid current cycle", "status_id": "4", "date": "2026-03-01", "due_date": "2026-03-02", "amount": 100, "balance": 0, "paid_to_date": 100, "exchange_rate": 1, "is_deleted": false, "archived_at": 0, "updated_at": 0, "invitations": []any{}}
	customer := map[string]any{"id": workflowCustomer, "id_number": "42", "private_notes": "legacy customer", "is_deleted": false, "archived_at": 0}
	settlementCustomer := map[string]any{"id": workflowCustomer, "id_number": "42", "group_settings_id": "", "settings": map[string]any{"currency_id": "1"}, "currency_id": "ignored", "is_deleted": false, "archived_at": 0}
	settlementInvoice := map[string]any{"id": workflowInvoice, "client_id": workflowCustomer, "recurring_id": workflowRecurring, "number": "N-42", "status_id": "4", "date": "2026-03-01", "due_date": "2026-03-02", "amount": 100, "balance": 0, "paid_to_date": 100, "exchange_rate": 1, "is_deleted": false, "archived_at": 0}
	allocation := map[string]any{"id": "allocation-42", "invoice_id": workflowInvoice, "payment_id": "payment-42", "amount": 100, "refunded": 0, "archived_at": 0}
	payment := map[string]any{"id": "payment-42", "client_id": workflowCustomer, "date": "2026-03-01", "status_id": "4", "currency_id": "1", "exchange_currency_id": "", "amount": 100, "refunded": 0, "applied": 100, "exchange_rate": 1, "is_manual": true, "is_deleted": false, "archived_at": 0, "paymentables": []any{allocation}, "invoices": []any{}}

	var requests atomic.Int32
	var invoiceGets atomic.Int32
	var extraRecurring any
	var extraInvoice any
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.Body != http.NoBody && r.ContentLength != 0 {
			t.Errorf("unexpected non-GET request")
		}
		if r.Header.Get("X-API-TOKEN") != workflowToken && r.Header.Get("Token") != workflowToken {
			t.Errorf("missing synthetic source authentication")
		}
		if r.Header.Get("X-API-TOKEN") != "" && (r.Header.Get("X-Requested-With") != "XMLHttpRequest" || r.Header.Get("Accept") != "application/json") {
			t.Errorf("Invoice Ninja GET headers missing")
		}
		path := r.URL.Path
		if strings.HasPrefix(path, "/ninja/api/v1/") {
			w.Header().Set("X-APP-VERSION", "5.13.47")
		}
		if path == "/ninja/api/v1/clients" && r.URL.Query().Get("id_number") == "42" {
			w.Header().Set("X-APP-VERSION", "5.13.47")
			writeWorkflowPage(w, []any{customer}, 1, 1)
			return
		}
		if path == "/ninja/api/v1/recurring_invoices" {
			w.Header().Set("X-APP-VERSION", "5.13.47")
			rows := []any{recurring}
			if extraRecurring != nil {
				rows = append(rows, extraRecurring)
			}
			writeWorkflowPage(w, rows, len(rows), 1)
			return
		}
		if path == "/ninja/api/v1/recurring_invoices/"+workflowRecurring {
			w.Header().Set("X-APP-VERSION", "5.13.47")
			writeWorkflowJSON(w, map[string]any{"data": recurring, "links": map[string]string{"next": "http://must-not-follow.invalid"}})
			return
		}
		if path == "/ninja/api/v1/invoices" {
			w.Header().Set("X-APP-VERSION", "5.13.47")
			rows := []any{invoice}
			if extraInvoice != nil {
				rows = append(rows, extraInvoice)
			}
			writeWorkflowPage(w, rows, len(rows), 1)
			return
		}
		if path == "/ninja/api/v1/invoices/"+workflowInvoice {
			if invoiceGets.Add(1)%3 != 1 {
				writeWorkflowJSON(w, map[string]any{"data": settlementInvoice})
				return
			}
			w.Header().Set("X-APP-VERSION", "5.13.47")
			writeWorkflowJSON(w, map[string]any{"data": invoice})
			return
		}
		if path == "/ninja/api/v1/clients/"+workflowCustomer {
			writeWorkflowJSON(w, map[string]any{"data": settlementCustomer})
			return
		}
		if path == "/ninja/api/v1/payments" {
			writeWorkflowPage(w, []any{payment}, 1, 1)
			return
		}
		if path == "/panel/apiv2/status" {
			writeWorkflowJSON(w, map[string]any{"success": true, "obj": map[string]any{"sys": map[string]any{"appVersion": "1.6.4"}}})
			return
		}
		if path == "/panel/apiv2/clients" && r.URL.Query().Get("id") == "42" {
			writeWorkflowJSON(w, map[string]any{"success": true, "obj": map[string]any{"clients": []json.RawMessage{suiRaw}}})
			return
		}
		if path == "/panel/apiv2/clients" {
			writeWorkflowJSON(w, map[string]any{"success": true, "obj": map[string]any{"clients": []any{map[string]any{"id": 42, "name": "42", "enable": true, "desc": "", "remark": "", "inbounds": []int{1}, "expiry": 0, "volume": 0}}}})
			return
		}
		if path == "/panel/apiv2/settings" {
			writeWorkflowJSON(w, map[string]any{"success": true, "obj": map[string]any{"subURI": "https://subs.invalid/", "subPath": "sub", "subPort": "443", "subDomain": ""}})
			return
		}
		t.Errorf("unexpected request path %s?%s", path, r.URL.RawQuery)
		http.NotFound(w, r)
	}))
	defer server.Close()

	imp, err := invoiceninja.NewImportReader(server.URL+"/ninja", workflowToken, time.Second, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := invoiceninja.NewRecurringReader(server.URL+"/ninja", workflowToken, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cyc, err := invoiceninja.NewCycleReader(server.URL+"/ninja", workflowToken, time.Second, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	settle, err := invoiceninja.NewSettlementClient(server.URL+"/ninja", workflowToken, time.Second, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := sui.NewIdentityReader(server.URL+"/panel", workflowToken, "1.6.4", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	records, err := sui.NewRecordReader(server.URL+"/panel", workflowToken, "1.6.4", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	suiClient, err := sui.New(server.URL+"/panel", workflowToken, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	// Claims are deliberately explicit; source assertions come from real readers.
	claim := importplan.Record{TelegramID: "42", InvoiceCustomerID: workflowCustomer, InvoiceNumber: "42", CurrentInvoiceID: workflowInvoice, InvoiceStatus: "paid", RecurringID: workflowRecurring, SUIID: "42", SUIName: "42", ConfigHash: configHash, PeriodStart: "2026-03-01", PeriodEnd: "2026-04-01", Currency: "USD", Lifecycle: "active", SourceVersion: "5.13.47", PinnedProvenance: true, CompleteProof: true, UnpaidInvoices: 0, Amount: "100"}
	plan := importplan.Analyze(importplan.Manifest{Records: []importplan.Record{claim}})
	if plan.Imported != 1 {
		t.Fatalf("fixture claim rejected by plan: %#v", plan.Reasons)
	}
	providers := Providers{Customers: imp, Recurring: rec, Invoices: cyc, Payments: settle, Identities: ids, Records: records, Settings: suiClient}
	svc, err := New(policy, providers)
	if err != nil {
		t.Fatal(err)
	}
	result, evidence, err := svc.Verify(context.Background(), plan, plan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	if err != nil || result.Verified != 1 || result.Manual != 0 || len(evidence) != 1 {
		t.Fatalf("real source verification failed: result=%+v reasons=%v err=%v", result, result.Reasons, err)
	}
	private, err := evidence[0].PrivateJSON()
	if err != nil || !strings.Contains(string(private), workflowCustomer) || !strings.Contains(string(private), workflowInvoice) || !strings.Contains(string(private), "historical legacy note") {
		t.Fatal("private journal omitted actual source evidence")
	}
	initialEvidenceHash := result.EvidenceHash
	for _, public := range []string{result.String(), result.GoString(), fmt.Sprint(result), string(mustJSON(t, result)), fmt.Sprint(evidence[0]), evidence[0].GoString(), string(mustJSON(t, evidence[0]))} {
		for _, secret := range []string{workflowCustomer, workflowInvoice, workflowRecurring, workflowToken, server.URL, "historical legacy note"} {
			if strings.Contains(public, secret) {
				t.Fatalf("public result disclosed private source fact: %s", public)
			}
		}
	}
	// These source-only mutations are readable by the strict adapter but outside the supported legacy profile.
	for _, mutation := range []struct {
		field  string
		value  any
		target map[string]any
	}{
		{field: "line_items.type_id", value: "2", target: line},
		{field: "due_date_days", value: "7", target: recurring},
		{field: "next_send_datetime", value: "2026-03-10T12:00:00.000000Z", target: recurring},
	} {
		key := mutation.field
		if key == "line_items.type_id" {
			key = "type_id"
		}
		original := mutation.target[key]
		mutation.target[key] = mutation.value
		out, _, err := svc.Verify(context.Background(), plan, plan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
		if err != nil || out.Verified != 0 || out.Manual != 1 || out.Reasons[ReasonUnsupported] != 1 {
			t.Fatalf("unsupported recurring %s was accepted: reasons=%v err=%v", mutation.field, out.Reasons, err)
		}
		mutation.target[key] = original
	}
	// Source ownership, live recurring ambiguity, and additional debt cannot be authorized by manifest claims.
	customer["id_number"] = "43"
	out, _, err := svc.Verify(context.Background(), plan, plan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	if err != nil || out.Verified != 0 || out.Manual != 1 {
		t.Fatal("foreign source owner was accepted", out.Reasons, err)
	}
	customer["id_number"] = "42"
	extraRecurring = map[string]any{}
	for k, v := range recurring {
		extraRecurring.(map[string]any)[k] = v
	}
	extraRecurring.(map[string]any)["id"] = "recurring-extra"
	out, _, err = svc.Verify(context.Background(), plan, plan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	if err != nil || out.Verified != 0 || out.Reasons[ReasonAmbiguous] != 1 {
		t.Fatal("second live recurring source was accepted", out.Reasons, err)
	}
	extraRecurring = nil
	extraInvoice = map[string]any{}
	for k, v := range invoice {
		extraInvoice.(map[string]any)[k] = v
	}
	extraInvoice.(map[string]any)["id"] = "invoice-debt"
	extraInvoice.(map[string]any)["status_id"] = "2"
	extraInvoice.(map[string]any)["recurring_id"] = ""
	out, _, err = svc.Verify(context.Background(), plan, plan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	if err != nil || out.Verified != 0 || out.Reasons[ReasonUnpaid] != 1 {
		t.Fatal("additional unpaid debt was accepted", out.Reasons, err)
	}
	extraInvoice = nil
	// Evidence hashing includes private source material, while public rendering stays redacted.
	recurring["private_notes"] = "changed legacy source note"
	changed, changedEvidence, err := svc.Verify(context.Background(), plan, plan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	if err != nil || changed.Verified != 1 || len(changedEvidence) != 1 || changed.EvidenceHash == initialEvidenceHash {
		t.Fatal("private source change did not alter evidence digest", err)
	}
	before := requests.Load()
	excludedPolicy := policy
	excludedPolicy.ExcludedIDs = []string{"42"}
	excluded, err := New(excludedPolicy, providers)
	if err != nil {
		t.Fatal(err)
	}
	out, _, err = excluded.Verify(context.Background(), plan, plan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	if err != nil || out.Verified != 0 || out.Reasons[ReasonExcluded] != 1 || requests.Load() != before {
		t.Fatal("trusted exclusion did not win before provider IO", err)
	}
	if _, _, err := svc.Verify(context.Background(), plan, strings.Repeat("0", 64), time.Now()); err == nil || requests.Load() != before {
		t.Fatal("bad plan digest performed source reads")
	}
}

func writeWorkflowJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
func writeWorkflowPage(w http.ResponseWriter, rows []any, total, page int) {
	writeWorkflowJSON(w, map[string]any{"data": rows, "meta": map[string]any{"pagination": map[string]any{"total": total, "count": len(rows), "per_page": 10, "current_page": page, "total_pages": 1, "links": map[string]string{"next": "https://never-follow.invalid"}}}})
}
