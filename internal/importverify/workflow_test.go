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
	var secondExtraInvoice any
	var anchorFailure bool
	var failurePath string
	var failureRecord bool
	var subscriptionBase = "https://subs.invalid/"
	var failSecondCustomerRead bool
	var customerReads atomic.Int32
	var extraPayments []any
	var pausedEnabled = true
	var pausedMode bool
	var driftRecurring bool
	var driftRecurringGets atomic.Int32
	var currentInvoiceDrift bool
	var currentInvoiceGets atomic.Int32
	var settingsDrift bool
	var settingsGets atomic.Int32
	var suiSubPath = "private-subpath"
	var paymentInvoiceID = workflowInvoice
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
		if path == failurePath || (failureRecord && path == "/panel/apiv2/clients" && r.URL.Query().Get("id") == "42") || (path == "/ninja/api/v1/clients" && failSecondCustomerRead && customerReads.Add(1) == 2) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("private-token secret-error-body"))
			return
		}
		if strings.HasPrefix(path, "/ninja/api/v1/") {
			w.Header().Set("X-APP-VERSION", "5.13.47")
		}
		if path == "/ninja/api/v1/clients" && (r.URL.Query().Get("id_number") == "42" || r.URL.Query().Get("id_number") == "") {
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
			if driftRecurring && driftRecurringGets.Add(1)%2 == 1 {
				recurring["private_notes"] = "source changed between verification passes"
			}
			return
		}
		if path == "/ninja/api/v1/invoices" {
			w.Header().Set("X-APP-VERSION", "5.13.47")
			rows := []any{invoice}
			if extraInvoice != nil {
				rows = append(rows, extraInvoice)
			}
			if secondExtraInvoice != nil {
				rows = append(rows, secondExtraInvoice)
			}
			writeWorkflowPage(w, rows, len(rows), 1)
			return
		}
		if strings.HasPrefix(path, "/ninja/api/v1/invoices/") {
			id := strings.TrimPrefix(path, "/ninja/api/v1/invoices/")
			if anchorFailure && id != workflowInvoice {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			if id == workflowInvoice && (pausedMode || invoiceGets.Add(1)%3 == 1) {
				w.Header().Set("X-APP-VERSION", "5.13.47")
				writeWorkflowJSON(w, map[string]any{"data": invoice})
				if currentInvoiceDrift && currentInvoiceGets.Add(1) == 1 {
					invoice["private_notes"] = "current invoice drift between passes"
				}
				return
			}
			settlement := settlementInvoice
			if id != workflowInvoice {
				source := invoice
				for _, candidate := range []any{extraInvoice, secondExtraInvoice} {
					if row, ok := candidate.(map[string]any); ok && row["id"] == id {
						source = row
					}
				}
				settlement = map[string]any{}
				for k, v := range source {
					settlement[k] = v
				}
				settlement["id"] = id
				settlement["status_id"] = "4"
				settlement["balance"] = 0
				settlement["paid_to_date"] = 100
			}
			writeWorkflowJSON(w, map[string]any{"data": settlement})
			return
		}
		if path == "/ninja/api/v1/clients/"+workflowCustomer {
			writeWorkflowJSON(w, map[string]any{"data": settlementCustomer})
			return
		}
		if path == "/ninja/api/v1/payments" {
			allocation["invoice_id"] = paymentInvoiceID
			rows := []any{payment}
			rows = append(rows, extraPayments...)
			writeWorkflowPage(w, rows, len(rows), 1)
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
			writeWorkflowJSON(w, map[string]any{"success": true, "obj": map[string]any{"clients": []any{map[string]any{"id": 42, "name": "42", "enable": pausedEnabled, "desc": "", "remark": "", "inbounds": []int{1}, "expiry": 0, "volume": 0}}}})
			return
		}
		if path == "/panel/apiv2/settings" {
			writeWorkflowJSON(w, map[string]any{"success": true, "obj": map[string]any{"subURI": subscriptionBase, "subPath": suiSubPath, "subPort": "443", "subDomain": ""}})
			if settingsDrift && settingsGets.Add(1) == 1 {
				suiSubPath = "changed-private-path"
			}
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
	if result.Diagnostics != nil || strings.Contains(string(mustJSON(t, result)), "diagnostics") {
		t.Fatal("successful verification must omit diagnostics")
	}
	initialEvidenceHash := result.EvidenceHash
	// Numeric Ninja flags and zero-rate classification labels have no monetary
	// effect, but must remain frozen in the imported profile and its evidence.
	line["is_amount_discount"] = 1
	line["tax_id"], line["tax_name1"], line["tax_name2"], line["tax_name3"] = "legacy-classification", "legacy-tax-label", "legacy-tax-label-two", "legacy-tax-label-three"
	metadataResult, metadataEvidence, metadataErr := svc.Verify(context.Background(), plan, plan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	if metadataErr != nil || metadataResult.Verified != 1 || len(metadataEvidence) != 1 {
		t.Fatalf("zero-effect line metadata rejected: reasons=%v err=%v", metadataResult.Reasons, metadataErr)
	}
	var frozen invoiceninja.RecurringRecord
	if err := json.Unmarshal(metadataEvidence[0].LegacyRecurring, &frozen); err != nil {
		t.Fatal(err)
	}
	if !frozen.LineItems[0].IsAmountDiscount || frozen.LineItems[0].TaxID != "legacy-classification" || frozen.LineItems[0].TaxName1 != "legacy-tax-label" || frozen.LineItems[0].TaxName2 != "legacy-tax-label-two" || frozen.LineItems[0].TaxName3 != "legacy-tax-label-three" {
		t.Fatal("representation metadata was erased from evidence")
	}
	if _, err := invoiceninja.NewImportedRecurringProfile(frozen, policy.ProductKey); err != nil {
		t.Fatal("verified profile incompatible with executor", err)
	}
	if metadataResult.EvidenceHash == initialEvidenceHash || metadataEvidence[0].LegacyRecurringHash == evidence[0].LegacyRecurringHash || metadataEvidence[0].SourceFactsHash == evidence[0].SourceFactsHash || metadataEvidence[0].ProofHash != evidence[0].ProofHash {
		t.Fatal("metadata must change source/evidence hashes without changing payment proof")
	}
	line["is_amount_discount"] = false
	line["tax_id"], line["tax_name1"], line["tax_name2"], line["tax_name3"] = "", "", "", ""
	// Read failures expose only the fixed GET stage and safe HTTP classification.
	for _, tc := range []struct{ path, stage string }{
		{"/ninja/api/v1/clients", "customers"},
		{"/ninja/api/v1/recurring_invoices", "recurring"},
		{"/ninja/api/v1/recurring_invoices/" + workflowRecurring, "recurring_record"},
		{"/panel/apiv2/clients", "sui_inventory"},
		{"/panel/apiv2/settings", "sui_settings"},
		{"/ninja/api/v1/invoices", "invoices"},
		{"/ninja/api/v1/invoices/" + workflowInvoice, "invoice"},
		{"/ninja/api/v1/clients/" + workflowCustomer, "payments"},
	} {
		failurePath = tc.path
		invoiceGets.Store(0)
		failed, failedEvidence, verifyErr := svc.Verify(context.Background(), plan, plan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
		if verifyErr != nil || failed.Verified != 0 || len(failedEvidence) != 0 || failed.Reasons[ReasonSourceFailure] != 1 {
			t.Fatalf("read failure accepted for %s: reasons=%v err=%v", tc.stage, failed.Reasons, verifyErr)
		}
		check := "read"
		if tc.stage == "payments" {
			check = "observe"
		}
		assertVerificationDiagnostic(t, failed, Diagnostic{Stage: tc.stage, Check: check, Category: "http", HTTPStatus: 403})
		for _, public := range []string{fmt.Sprint(failed), fmt.Sprintf("%#v", failed), string(mustJSON(t, failed)), fmt.Sprint(failed.Diagnostics), fmt.Sprintf("%#v", failed.Diagnostics)} {
			for _, secret := range []string{workflowToken, workflowCustomer, workflowRecurring, workflowInvoice, server.URL, "private-token", "secret-error-body"} {
				if strings.Contains(public, secret) {
					t.Fatalf("diagnostic leaked private data: %s", public)
				}
			}
		}
	}
	failurePath = ""
	invoiceGets.Store(0)
	failSecondCustomerRead = true
	secondFailure, _, secondErr := svc.Verify(context.Background(), plan, plan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	if secondErr != nil || secondFailure.Reasons[ReasonChanged] != 1 || secondFailure.Reasons[ReasonSourceFailure] != 0 {
		t.Fatal("second-pass failure reason changed", secondFailure.Reasons, secondErr)
	}
	assertVerificationDiagnostic(t, secondFailure, Diagnostic{Stage: "customers", Check: "read", Category: "http", HTTPStatus: 403})
	failSecondCustomerRead = false
	invoiceGets.Store(0)
	failureRecord = true
	recordFailure, _, recordErr := svc.Verify(context.Background(), plan, plan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	if recordErr != nil || recordFailure.Reasons[ReasonSourceFailure] != 1 {
		t.Fatal("SUI record failure lost", recordErr)
	}
	assertVerificationDiagnostic(t, recordFailure, Diagnostic{Stage: "sui_record", Check: "read", Category: "http", HTTPStatus: 403})
	failureRecord = false
	subscriptionBase = "https://other.invalid/"
	baseFailure, _, baseErr := svc.Verify(context.Background(), plan, plan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	if baseErr != nil || baseFailure.Reasons[ReasonUnsupported] != 1 {
		t.Fatal("subscription base mismatch accepted", baseErr)
	}
	assertVerificationDiagnostic(t, baseFailure, Diagnostic{Stage: "sui_settings", Check: "subscription_base"})
	subscriptionBase = "https://subs.invalid/"
	for _, tc := range []struct{ field, value, check string }{
		{"inbounds", `[2]`, "inbounds"},
		{"volume", `100`, "quotas"},
		{"enable", `false`, "identity_or_lifecycle"},
	} {
		original := suiFields[tc.field]
		suiFields[tc.field] = json.RawMessage(tc.value)
		suiRaw = mustJSON(t, suiFields)
		failed, _, verifyErr := svc.Verify(context.Background(), plan, plan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
		if verifyErr != nil || failed.Reasons[ReasonUnsupported] != 1 {
			t.Fatalf("SUI %s mismatch accepted: reasons=%v err=%v", tc.field, failed.Reasons, verifyErr)
		}
		assertVerificationDiagnostic(t, failed, Diagnostic{Stage: "sui_record", Check: tc.check})
		if original == nil {
			delete(suiFields, tc.field)
		} else {
			suiFields[tc.field] = original
		}
		suiRaw = mustJSON(t, suiFields)
	}
	// Ninja may serialize the same client-calendar date as SQL midnight.
	// Both the inventory and the exact GET must normalize it identically,
	// without changing the verified instant, business proof or evidence hash.
	recurring["next_send_date"] = "2026-04-01 00:00:00"
	calendarResult, calendarEvidence, calendarErr := svc.Verify(context.Background(), plan, plan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	if calendarErr != nil || calendarResult.Verified != 1 || len(calendarEvidence) != 1 || calendarResult.EvidenceHash != initialEvidenceHash {
		t.Fatalf("SQL calendar representation changed verification: result=%+v err=%v", calendarResult, calendarErr)
	}
	recurring["next_send_date"] = "2026-04-01"
	// Keep the established active expired-period reason stable.
	recurring["next_send_date"] = "2026-03-01"
	expiredClaim := claim
	expiredClaim.PeriodStart, expiredClaim.PeriodEnd = "2026-02-01", "2026-03-01"
	expiredPlan := importplan.Analyze(importplan.Manifest{Records: []importplan.Record{expiredClaim}})
	expiredResult, _, expiredErr := svc.Verify(context.Background(), expiredPlan, expiredPlan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	if expiredErr != nil || expiredResult.Verified != 0 || expiredResult.Reasons[ReasonExpired] != 1 {
		t.Fatal("active expired-period reason changed", expiredResult.Reasons, expiredErr)
	}
	assertVerificationDiagnostic(t, expiredResult, Diagnostic{Stage: "verification", Check: "active_period_expired"})
	recurring["next_send_date"] = "2026-04-01"
	for _, public := range []string{result.String(), result.GoString(), fmt.Sprint(result), string(mustJSON(t, result)), fmt.Sprint(evidence[0]), evidence[0].GoString(), string(mustJSON(t, evidence[0]))} {
		for _, secret := range []string{workflowCustomer, workflowInvoice, workflowRecurring, workflowToken, server.URL, "historical legacy note"} {
			if strings.Contains(public, secret) {
				t.Fatalf("public result disclosed private source fact: %s", public)
			}
		}
	}
	// Older paid history is allowed, but the claimed current invoice must be the unique date-latest issued cycle.
	setExtraInvoice := func(id, date, status string) {
		extraInvoice = map[string]any{}
		for k, v := range invoice {
			extraInvoice.(map[string]any)[k] = v
		}
		extraInvoice.(map[string]any)["id"] = id
		extraInvoice.(map[string]any)["date"] = date
		extraInvoice.(map[string]any)["status_id"] = status
	}
	setExtraInvoice("invoice-old", "2026-02-01", "4")
	out, _, err := svc.Verify(context.Background(), plan, plan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	if err != nil || out.Verified != 1 {
		t.Fatal("older paid history hid the current invoice", out.Reasons, err)
	}
	extraInvoice = nil
	setExtraInvoice("invoice-newer", "2026-03-02", "4")
	out, _, err = svc.Verify(context.Background(), plan, plan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	if err != nil || out.Verified != 0 || out.Reasons[ReasonAmbiguous] != 1 {
		t.Fatal("newer paid cycle did not make claimed current ambiguous", out.Reasons, err)
	}
	extraInvoice = nil
	setExtraInvoice("invoice-same-date", "2026-03-01", "4")
	out, _, err = svc.Verify(context.Background(), plan, plan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	if err != nil || out.Verified != 0 || out.Reasons[ReasonAmbiguous] != 1 {
		t.Fatal("same-date issued cycles were not manual", out.Reasons, err)
	}
	extraInvoice = nil

	// These source-only mutations are readable by the strict adapter but outside the supported legacy profile.
	for _, mutation := range []struct {
		field  string
		value  any
		target map[string]any
	}{
		{field: "line_items.type_id", value: "2", target: line},
		{field: "due_date_days", value: "7", target: recurring},
		{field: "product_key", value: "other-product", target: line},
		{field: "quantity", value: 2, target: line},
		{field: "frequency_id", value: "4", target: recurring},
		{field: "auto_bill", value: "always", target: recurring},
		{field: "remaining_cycles", value: 1, target: recurring},
		{field: "uses_inclusive_taxes", value: true, target: recurring},
		{field: "paid_to_date", value: 1, target: recurring},
		{field: "balance", value: 99, target: recurring},
		{field: "discount", value: 1, target: recurring},
		{field: "tax_rate1", value: 1, target: recurring},
		{field: "custom_surcharge1", value: 1, target: recurring},
		{field: "partial", value: 1, target: recurring},
		{field: "discount", value: 1, target: line},
		{field: "tax_rate1", value: 1, target: line},
		{field: "exchange_rate", value: 2, target: recurring},
		{field: "next_send_datetime", value: "2026-03-10T12:00:00.000000Z", target: recurring},
	} {
		key := mutation.field
		if key == "line_items.type_id" {
			key = "type_id"
		}
		original := mutation.target[key]
		mutation.target[key] = mutation.value
		out, _, err = svc.Verify(context.Background(), plan, plan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
		if err != nil || out.Verified != 0 || out.Manual != 1 || out.Reasons[ReasonUnsupported] != 1 {
			t.Fatalf("unsupported recurring %s was accepted: reasons=%v err=%v", mutation.field, out.Reasons, err)
		}
		checks := map[string]string{
			"line_items.type_id": "legacy_product", "product_key": "legacy_product", "quantity": "legacy_quantity",
			"due_date_days": "legacy_due_date_days", "frequency_id": "legacy_frequency", "auto_bill": "legacy_auto_bill",
			"remaining_cycles": "legacy_remaining_cycles", "uses_inclusive_taxes": "legacy_tax", "paid_to_date": "legacy_paid_to_date",
			"balance": "legacy_balance", "discount": "legacy_discount", "tax_rate1": "legacy_tax",
			"custom_surcharge1": "legacy_surcharge", "partial": "legacy_partial", "exchange_rate": "legacy_exchange_rate",
			"next_send_datetime": "schedule_datetime",
		}
		check := checks[mutation.field]
		assertVerificationDiagnostic(t, out, Diagnostic{Stage: "recurring_record", Check: check})
		mutation.target[key] = original
	}
	// Source ownership, live recurring ambiguity, and additional debt cannot be authorized by manifest claims.
	customer["id_number"] = "43"
	out, _, err = svc.Verify(context.Background(), plan, plan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
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
	// A paused account is reportable only with one current debt and a separately paid historical anchor.
	pausedMode = true
	pausedEnabled = false
	paymentInvoiceID = "invoice-anchor"
	payment["date"] = "2026-02-01"
	recurring["status_id"] = "3"
	recurring["next_send_date"] = "2026-02-01"
	recurring["next_send_datetime"] = "2026-02-01T00:00:00.000000Z"
	invoice["status_id"], invoice["date"], invoice["balance"], invoice["paid_to_date"] = "2", "2026-03-01", 100, 0
	settlementInvoice["status_id"], settlementInvoice["date"], settlementInvoice["balance"], settlementInvoice["paid_to_date"] = "2", "2026-03-01", 100, 0
	setExtraInvoice("invoice-anchor", "2026-02-01", "4")
	secondExtraInvoice = map[string]any{}
	for k, v := range invoice {
		secondExtraInvoice.(map[string]any)[k] = v
	}
	secondExtraInvoice.(map[string]any)["id"] = "invoice-000-old"
	secondExtraInvoice.(map[string]any)["date"] = "2026-01-01"
	secondExtraInvoice.(map[string]any)["status_id"] = "4"
	secondExtraInvoice.(map[string]any)["balance"] = 0
	secondExtraInvoice.(map[string]any)["paid_to_date"] = 100
	extraPayments = []any{map[string]any{"id": "payment-old", "client_id": workflowCustomer, "date": "2026-01-01", "status_id": "4", "currency_id": "1", "exchange_currency_id": "", "amount": 100, "refunded": 0, "applied": 100, "exchange_rate": 1, "is_manual": true, "is_deleted": false, "archived_at": 0, "paymentables": []any{map[string]any{"id": "allocation-old", "invoice_id": "invoice-000-old", "payment_id": "payment-old", "amount": 100, "refunded": 0, "archived_at": 0}}, "invoices": []any{}}}
	for k, v := range invoice {
		if k != "id" && k != "date" && k != "status_id" {
			extraInvoice.(map[string]any)[k] = v
		}
	}
	extraInvoice.(map[string]any)["id"] = "invoice-anchor"
	extraInvoice.(map[string]any)["date"] = "2026-02-01"
	extraInvoice.(map[string]any)["status_id"] = "4"
	extraInvoice.(map[string]any)["balance"] = 0
	extraInvoice.(map[string]any)["paid_to_date"] = 100
	for k := range suiFields {
		if k == "enable" {
			suiFields[k] = json.RawMessage(`false`)
		}
	}
	suiRaw, err = json.Marshal(suiFields)
	if err != nil {
		t.Fatal(err)
	}
	pausedClaim := claim
	pausedClaim.CurrentInvoiceID, pausedClaim.InvoiceStatus = workflowInvoice, "sent"
	pausedClaim.PeriodStart, pausedClaim.PeriodEnd = "2026-02-01", "2026-03-01"
	pausedClaim.Lifecycle, pausedClaim.UnpaidInvoices = "paused", 1
	pausedPlan := importplan.Analyze(importplan.Manifest{Records: []importplan.Record{pausedClaim}})
	pausedResult, pausedEvidence, err := svc.Verify(context.Background(), pausedPlan, pausedPlan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	if err != nil || pausedResult.Verified != 1 || len(pausedEvidence) != 1 || pausedEvidence[0].CurrentInvoiceID == pausedEvidence[0].PaidAnchorInvoiceID || pausedEvidence[0].PaidAnchorInvoiceID != "invoice-anchor" {
		t.Fatalf("paused historical-anchor verification failed: result=%+v evidence=%+v err=%v", pausedResult, pausedEvidence, err)
	}
	// Automatic discovery must derive the paused period from the distinct paid
	// anchor, not treat the current unpaid invoice as financial authority.
	autoPolicy := policy
	autoPolicy.Automatic, autoPolicy.PilotIDs = true, nil
	autoProviders := providers
	inventory, err := invoiceninja.NewCustomerInventoryReader(server.URL+"/ninja", workflowToken, time.Second, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	autoProviders.Customers = inventory
	autoService, err := New(autoPolicy, autoProviders)
	if err != nil {
		t.Fatal(err)
	}
	autoNow := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	discovered, err := autoService.Discover(context.Background(), autoNow, nil)
	if err != nil || len(discovered) != 1 || discovered[0].Reason != "" {
		t.Fatal("paused automatic discovery failed")
	}
	derived := discovered[0].Facts
	if derived.Lifecycle != "paused" || derived.InvoiceStatus != "sent" || derived.CurrentInvoiceID != workflowInvoice || derived.PeriodStart != "2026-02-01" || derived.PeriodEnd != "2026-03-01" || derived.UnpaidInvoices != 1 {
		t.Fatal("automatic discovery lost the paused debt/paid-anchor distinction")
	}
	derivedPlan := importplan.Analyze(importplan.Manifest{Records: []importplan.Record{derived}})
	derivedResult, derivedEvidence, err := autoService.Verify(context.Background(), derivedPlan, derivedPlan.Hash, autoNow)
	if err != nil || derivedResult.Verified != 1 || len(derivedEvidence) != 1 || derivedEvidence[0].PaidAnchorInvoiceID != "invoice-anchor" || derivedEvidence[0].CurrentInvoiceID != workflowInvoice {
		t.Fatal("automatically derived paused candidate did not pass fresh strict verification")
	}
	var privateSnapshot struct {
		CurrentInvoice invoiceninja.CycleInvoice `json:"current_invoice"`
		SUISettings    sui.Settings              `json:"sui_settings"`
	}
	pausedPrivateJSON, privateErr := pausedEvidence[0].PrivateJSON()
	if privateErr != nil {
		t.Fatal(privateErr)
	}
	if err := json.Unmarshal(pausedPrivateJSON, &privateSnapshot); err != nil || privateSnapshot.CurrentInvoice.ID != workflowInvoice || privateSnapshot.CurrentInvoice.RecurringID != workflowRecurring || privateSnapshot.CurrentInvoice.StatusID != "2" || privateSnapshot.CurrentInvoice.Date != "2026-03-01" || privateSnapshot.CurrentInvoice.Balance.String() != "100" || privateSnapshot.CurrentInvoice.PaidToDate.String() != "0" || privateSnapshot.CurrentInvoice.ExchangeRate.String() != "1" || privateSnapshot.SUISettings.SubURI != "https://subs.invalid/" || privateSnapshot.SUISettings.SubPath != "private-subpath" || privateSnapshot.SUISettings.SubPort != "443" {
		t.Fatalf("private evidence omitted actual current debt / S-UI settings: %+v err=%v", privateSnapshot, err)
	}
	for _, public := range []string{fmt.Sprint(pausedEvidence[0]), pausedEvidence[0].GoString(), string(mustJSON(t, pausedEvidence[0]))} {
		for _, secret := range []string{workflowInvoice, workflowRecurring, "private-subpath", "2026-03-01"} {
			if strings.Contains(public, secret) {
				t.Fatalf("private snapshot leaked through generic formatting: %s", public)
			}
		}
	}
	// Actual private fields are part of canonical aggregate evidence.
	settingsGets.Store(0)
	settingsDrift = true
	driftedSettings, _, err := svc.Verify(context.Background(), pausedPlan, pausedPlan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	if err != nil || driftedSettings.Verified != 0 || driftedSettings.Reasons[ReasonChanged] != 1 {
		t.Fatal("between-pass S-UI settings drift was accepted", driftedSettings.Reasons, err)
	}
	assertVerificationDiagnostic(t, driftedSettings, Diagnostic{Stage: "verification", Check: "second_read_changed"})
	settingsDrift = false
	suiSubPath = "private-subpath"
	currentInvoiceGets.Store(0)
	currentInvoiceDrift = true
	driftedInvoice, _, err := svc.Verify(context.Background(), pausedPlan, pausedPlan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	if err != nil || driftedInvoice.Verified != 0 || driftedInvoice.Reasons[ReasonChanged] != 1 {
		t.Fatal("between-pass current-invoice drift was accepted", driftedInvoice.Reasons, err)
	}
	currentInvoiceDrift = false
	invoice["private_notes"] = "paid current cycle"
	// The complete source authority is reread; an intervening source drift invalidates the report.
	driftRecurringGets.Store(0)
	driftRecurring = true
	drifted, _, err := svc.Verify(context.Background(), pausedPlan, pausedPlan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	if err != nil || drifted.Verified != 0 || drifted.Reasons[ReasonChanged] != 1 {
		t.Fatal("between-pass source drift was accepted", drifted.Reasons, err)
	}
	driftRecurring = false
	// Debt and anchor failures remain manual, never forgiven or silently selected.
	invoice["balance"] = 99
	badDebt, _, err := svc.Verify(context.Background(), pausedPlan, pausedPlan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	if err != nil || badDebt.Verified != 0 {
		t.Fatal("partial current debt accepted", badDebt.Reasons, err)
	}
	invoice["balance"] = 100
	// A second unpaid debt and an enabled S-UI record both fail closed.
	secondExtraInvoice = map[string]any{}
	for k, v := range invoice {
		secondExtraInvoice.(map[string]any)[k] = v
	}
	secondExtraInvoice.(map[string]any)["id"] = "invoice-second-debt"
	secondExtraInvoice.(map[string]any)["recurring_id"] = ""
	secondDebt, _, err := svc.Verify(context.Background(), pausedPlan, pausedPlan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	if err != nil || secondDebt.Verified != 0 || secondDebt.Reasons[ReasonUnpaid] != 1 {
		t.Fatal("multiple debts accepted", secondDebt.Reasons, err)
	}
	secondExtraInvoice = nil
	pausedEnabled = true
	enabledMismatch, _, err := svc.Verify(context.Background(), pausedPlan, pausedPlan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	if err != nil || enabledMismatch.Verified != 0 {
		t.Fatal("enabled paused S-UI record accepted", enabledMismatch.Reasons, err)
	}
	pausedEnabled = false
	// Source errors while checking a possible paid anchor invalidate the whole report.
	anchorFailure = true
	sourceFailure, _, err := svc.Verify(context.Background(), pausedPlan, pausedPlan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	if err != nil || sourceFailure.Verified != 0 || sourceFailure.Reasons[ReasonSourceFailure] != 1 {
		t.Fatal("historical source failure was skipped", sourceFailure.Reasons, err)
	}
	assertVerificationDiagnostic(t, sourceFailure, Diagnostic{Stage: "historical_invoice", Check: "read", Category: "http", HTTPStatus: 503})
	anchorFailure = false
	// Current and historical invoice ownership is checked against source inventory, not claims.
	for _, ownedInvoice := range []map[string]any{invoice, extraInvoice.(map[string]any)} {
		owner := ownedInvoice["client_id"]
		ownedInvoice["client_id"] = "foreign-customer"
		wrongOwner, _, verifyErr := svc.Verify(context.Background(), pausedPlan, pausedPlan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
		if verifyErr != nil || wrongOwner.Verified != 0 {
			t.Fatal("foreign invoice owner accepted", wrongOwner.Reasons, verifyErr)
		}
		ownedInvoice["client_id"] = owner
	}
	// Refunds, FX and credit-like non-manual payments cannot serve as anchors.
	for _, badPayment := range []struct {
		field string
		value any
	}{{"refunded", 100}, {"exchange_currency_id", "2"}, {"is_manual", false}} {
		original := payment[badPayment.field]
		payment[badPayment.field] = badPayment.value
		badProof, _, verifyErr := svc.Verify(context.Background(), pausedPlan, pausedPlan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
		if verifyErr != nil || badProof.Verified != 0 {
			t.Fatalf("unsupported anchor payment %s accepted: %v %v", badPayment.field, badProof.Reasons, verifyErr)
		}
		payment[badPayment.field] = original
	}
	// Two fully matching historical proofs are ambiguous, never first/top-N selected.
	secondExtraInvoice = map[string]any{}
	for k, v := range extraInvoice.(map[string]any) {
		secondExtraInvoice.(map[string]any)[k] = v
	}
	secondExtraInvoice.(map[string]any)["id"] = "invoice-anchor-two"
	secondExtraInvoice.(map[string]any)["date"] = "2026-02-15"
	extraAllocation := map[string]any{"id": "allocation-extra", "invoice_id": "invoice-anchor-two", "payment_id": "payment-extra", "amount": 100, "refunded": 0, "archived_at": 0}
	extraPayments = append(extraPayments, map[string]any{"id": "payment-extra", "client_id": workflowCustomer, "date": "2026-02-01", "status_id": "4", "currency_id": "1", "exchange_currency_id": "", "amount": 100, "refunded": 0, "applied": 100, "exchange_rate": 1, "is_manual": true, "is_deleted": false, "archived_at": 0, "paymentables": []any{extraAllocation}, "invoices": []any{}})
	ambiguousAnchors, _, err := svc.Verify(context.Background(), pausedPlan, pausedPlan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	if err != nil || ambiguousAnchors.Verified != 0 || ambiguousAnchors.Reasons[ReasonAmbiguous] != 1 {
		t.Fatal("multiple paid anchors were selected", ambiguousAnchors.Reasons, err)
	}
	extraPayments = extraPayments[:1]
	// No top-N selection: the configured cap rejects the full historical inventory.
	secondExtraInvoice = map[string]any{}
	for k, v := range extraInvoice.(map[string]any) {
		secondExtraInvoice.(map[string]any)[k] = v
	}
	secondExtraInvoice.(map[string]any)["id"] = "invoice-anchor-two"
	secondExtraInvoice.(map[string]any)["date"] = "2026-02-15"
	limitedPolicy := policy
	limitedPolicy.MaxHistoricalAnchors = 1
	limitedSvc, err := New(limitedPolicy, providers)
	if err != nil {
		t.Fatal(err)
	}
	limited, _, err := limitedSvc.Verify(context.Background(), pausedPlan, pausedPlan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	if err != nil || limited.Verified != 0 || limited.Reasons[ReasonHistoryLimit] != 1 {
		t.Fatal("historical inventory was truncated or selected", limited.Reasons, err)
	}
	secondExtraInvoice = nil
	extraPayments = nil
	extraInvoice = nil
	noAnchor, _, err := svc.Verify(context.Background(), pausedPlan, pausedPlan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	if err != nil || noAnchor.Verified != 0 {
		t.Fatal("missing paid anchor accepted", noAnchor.Reasons, err)
	}
	pausedMode = false
	invoiceGets.Store(0)
	pausedEnabled = true
	suiFields["enable"] = json.RawMessage(`true`)
	suiRaw, err = json.Marshal(suiFields)
	if err != nil {
		t.Fatal(err)
	}
	paymentInvoiceID = workflowInvoice
	payment["date"] = "2026-03-01"
	recurring["status_id"] = "2"
	recurring["next_send_date"] = "2026-04-01"
	recurring["next_send_datetime"] = "2026-04-01T00:00:00.000000Z"
	invoice["status_id"], invoice["date"], invoice["balance"], invoice["paid_to_date"] = "4", "2026-03-01", 0, 100
	settlementInvoice["status_id"], settlementInvoice["date"], settlementInvoice["balance"], settlementInvoice["paid_to_date"] = "4", "2026-03-01", 0, 100
	extraInvoice = nil
	// Evidence hashing includes private source material, while public rendering stays redacted.
	recurring["private_notes"] = "changed legacy source note"
	changed, changedEvidence, err := svc.Verify(context.Background(), plan, plan.Hash, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	if err != nil || changed.Verified != 1 || len(changedEvidence) != 1 || changed.EvidenceHash == initialEvidenceHash {
		t.Fatalf("private source change did not alter evidence digest: result=%+v reasons=%v evidence=%d initial=%s err=%v", changed, changed.Reasons, len(changedEvidence), initialEvidenceHash, err)
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

func assertVerificationDiagnostic(t *testing.T, result Result, want Diagnostic) {
	t.Helper()
	if len(result.Diagnostics) != 1 || result.Diagnostics[0] != want {
		t.Fatalf("diagnostics=%+v want=%+v", result.Diagnostics, want)
	}
}
