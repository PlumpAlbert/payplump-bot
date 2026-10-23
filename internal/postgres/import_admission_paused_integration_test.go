//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/importadmission"
	"github.com/PlumpAlbert/payplump-bot/internal/importplan"
	"github.com/PlumpAlbert/payplump-bot/internal/importverify"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

func TestVerifiedImportPausedKeepsCurrentDebtAndDistinctHistoricalAnchor(t *testing.T) {
	s, ctx := activationTestStore(t)
	if _, err := s.pool.Exec(ctx, `INSERT INTO telegram_bots(bot_id) VALUES($1) ON CONFLICT DO NOTHING`, admissionFixtureBotID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	periodStart, periodEnd := monthStart.AddDate(0, -1, 0).Format(time.DateOnly), monthStart.Format(time.DateOnly)
	prepared, err := admissionPausedPrepared(t, ctx, periodStart, periodEnd, now.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SyncImportAdmissionPolicy(ctx, admissionFixtureBotID, 1, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
		t.Fatal(err)
	}
	result, err := s.ApplyVerifiedImport(ctx, prepared)
	if err != nil || result.Admitted != 1 || result.AlreadyAdmitted != 0 {
		t.Fatalf("paused admission=%+v err=%v", result, err)
	}
	var clientID int64
	if err = s.pool.QueryRow(ctx, `SELECT id FROM clients WHERE telegram_id=42 AND chat_id=42 AND invoice_ninja_id='customer-42' AND sui_id='42'`).Scan(&clientID); err != nil {
		t.Fatal(err)
	}
	var state string
	var desired bool
	var current, recurring *string
	var paymentDate, nextDate *time.Time
	var first *string
	if err = s.pool.QueryRow(ctx, `SELECT state,desired_access,current_invoice_id,recurring_id,payment_date,next_invoice_date,first_invoice_id FROM subscriptions WHERE client_id=$1`, clientID).Scan(&state, &desired, &current, &recurring, &paymentDate, &nextDate, &first); err != nil {
		t.Fatal(err)
	}
	if state != "overdue" || desired || current == nil || *current != "debt-current" || recurring == nil || *recurring != admissionFixtureRec || paymentDate == nil || paymentDate.Format(time.DateOnly) != periodStart || nextDate == nil || nextDate.Format(time.DateOnly) != periodEnd || first != nil {
		t.Fatalf("paused subscription state=%q access=%v current=%v recurring=%v payment=%v next=%v first=%v", state, desired, current, recurring, paymentDate, nextDate, first)
	}
	var originCurrent, anchor string
	var template, proof, history, evidence []byte
	if err = s.pool.QueryRow(ctx, `SELECT current_invoice_id,paid_anchor_invoice_id,template,proof,historical_invoice_ids,evidence FROM verified_import_origins WHERE client_id=$1`, clientID).Scan(&originCurrent, &anchor, &template, &proof, &history, &evidence); err != nil {
		t.Fatal(err)
	}
	if originCurrent != "debt-current" || anchor != "paid-historical" || originCurrent == anchor || !strings.Contains(string(template), "historical legacy note") || !strings.Contains(string(template), `"next_send_date":"`+periodEnd+`"`) || !strings.Contains(string(proof), "paid-historical") || !strings.Contains(string(history), "debt-current") || !strings.Contains(string(history), "paid-historical") || !strings.Contains(string(evidence), `"current_invoice_status":"2"`) || !strings.Contains(string(evidence), `"balance":100`) || !strings.Contains(string(evidence), "paid-historical") || !strings.Contains(string(evidence), `"sui_name":"42"`) {
		t.Fatalf("paused provenance current=%q anchor=%q template=%s proof=%s history=%s evidence=%s", originCurrent, anchor, template, proof, history, evidence)
	}
	for _, table := range []string{"operations", "payment_confirmations", "activation_sui_bindings", "invoice_lifecycle_checks", "invoice_lifecycle_invoices"} {
		var count int
		if err = s.pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s WHERE client_id=$1`, table), clientID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("unexpected %s rows=%d err=%v", table, count, err)
		}
	}
	var count int
	for _, q := range []struct {
		sql  string
		args []any
	}{{`SELECT count(*) FROM outbox WHERE bot_id=$1 AND chat_id=42`, []any{admissionFixtureBotID}}, {`SELECT count(*) FROM payment_checks WHERE client_id=$1`, []any{clientID}}, {`SELECT count(*) FROM activation_sui_bindings WHERE sui_id='42'`, nil}} {
		if err = s.pool.QueryRow(ctx, q.sql, q.args...).Scan(&count); err != nil || count != 0 {
			t.Fatalf("unexpected effects query=%s count=%d err=%v", q.sql, count, err)
		}
	}
}

func admissionPausedPrepared(t *testing.T, ctx context.Context, start, end string, verifiedAt time.Time) (importadmission.PreparedPlan, error) {
	t.Helper()
	const customerID, currentID, historicalID, recurringID = "customer-42", "debt-current", "paid-historical", admissionFixtureRec
	suiRaw, configHash := admissionSUIFixture(t)
	var suiFields map[string]json.RawMessage
	if err := json.Unmarshal(suiRaw, &suiFields); err != nil {
		t.Fatal(err)
	}
	suiFields["enable"] = json.RawMessage(`false`)
	suiRaw, err := json.Marshal(suiFields)
	if err != nil {
		t.Fatal(err)
	}
	suiRecord, err := sui.ParseClientRecord(suiRaw)
	if err != nil {
		t.Fatalf("parse disabled SUI fixture: %v", err)
	}
	credentials := suiRecord.CredentialsHash()
	configHash = fmt.Sprintf("%x", credentials[:])
	line := map[string]any{"product_key": "legacy", "notes": "historical legacy note", "cost": 100, "quantity": 1, "discount": 0, "is_amount_discount": false, "type_id": "1", "tax_name1": "", "tax_name2": "", "tax_name3": "", "tax_rate1": 0, "tax_rate2": 0, "tax_rate3": 0, "tax_id": ""}
	recurring := map[string]any{"id": recurringID, "client_id": customerID, "status_id": "3", "private_notes": "historical legacy note", "frequency_id": "5", "next_send_date": end, "next_send_datetime": start + "T00:00:00.000000Z", "last_sent_date": end, "due_date": end, "due_date_days": "on_receipt", "auto_bill": "off", "auto_bill_enabled": false, "remaining_cycles": -1, "amount": 100, "balance": 100, "uses_inclusive_taxes": false, "line_items": []any{line}, "is_deleted": false, "archived_at": 0, "discount": 0, "is_amount_discount": true, "tax_rate1": 0, "tax_rate2": 0, "tax_rate3": 0, "custom_surcharge1": 0, "custom_surcharge2": 0, "custom_surcharge3": 0, "custom_surcharge4": 0, "partial": 0, "paid_to_date": 0, "exchange_rate": 1}
	debt := map[string]any{"id": currentID, "client_id": customerID, "recurring_id": recurringID, "number": "D-42", "private_notes": "current unpaid debt", "status_id": "2", "date": end, "due_date": end, "amount": 100, "balance": 100, "paid_to_date": 0, "exchange_rate": 1, "is_deleted": false, "archived_at": 0, "updated_at": 0, "invitations": []any{}}
	paid := map[string]any{"id": historicalID, "client_id": customerID, "recurring_id": recurringID, "number": "P-42", "private_notes": "paid historical invoice", "status_id": "4", "date": start, "due_date": start, "amount": 100, "balance": 0, "paid_to_date": 100, "exchange_rate": 1, "is_deleted": false, "archived_at": 0, "updated_at": 0, "invitations": []any{}}
	customer := map[string]any{"id": customerID, "id_number": "42", "private_notes": "legacy customer", "is_deleted": false, "archived_at": 0}
	settlementCustomer := map[string]any{"id": customerID, "id_number": "42", "group_settings_id": "", "settings": map[string]any{"currency_id": "1"}, "currency_id": "ignored", "is_deleted": false, "archived_at": 0}
	allocation := map[string]any{"id": "allocation-paid-42", "invoice_id": historicalID, "payment_id": "payment-paid-42", "amount": 100, "refunded": 0, "archived_at": 0}
	payment := map[string]any{"id": "payment-paid-42", "client_id": customerID, "date": start, "status_id": "4", "currency_id": "1", "exchange_currency_id": "", "amount": 100, "refunded": 0, "applied": 100, "exchange_rate": 1, "is_manual": true, "is_deleted": false, "archived_at": 0, "paymentables": []any{allocation}, "invoices": []any{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("non-GET source request: %s", r.Method)
			http.Error(w, "method not allowed", 405)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/ninja/api/v1/") {
			w.Header().Set("X-APP-VERSION", "5.13.47")
		}
		switch r.URL.Path {
		case "/ninja/api/v1/clients":
			admissionPage(w, []any{customer})
		case "/ninja/api/v1/recurring_invoices":
			admissionPage(w, []any{recurring})
		case "/ninja/api/v1/recurring_invoices/" + recurringID:
			admissionJSON(w, map[string]any{"data": recurring})
		case "/ninja/api/v1/invoices":
			admissionPage(w, []any{debt, paid})
		case "/ninja/api/v1/invoices/" + currentID:
			admissionJSON(w, map[string]any{"data": debt})
		case "/ninja/api/v1/invoices/" + historicalID:
			admissionJSON(w, map[string]any{"data": paid})
		case "/ninja/api/v1/clients/" + customerID:
			admissionJSON(w, map[string]any{"data": settlementCustomer})
		case "/ninja/api/v1/payments":
			admissionPage(w, []any{payment})
		case "/panel/apiv2/status":
			admissionJSON(w, map[string]any{"success": true, "obj": map[string]any{"sys": map[string]any{"appVersion": "1.6.4"}}})
		case "/panel/apiv2/clients":
			if r.URL.Query().Get("id") == "42" {
				admissionJSON(w, map[string]any{"success": true, "obj": map[string]any{"clients": []json.RawMessage{suiRaw}}})
			} else {
				admissionJSON(w, map[string]any{"success": true, "obj": map[string]any{"clients": []any{map[string]any{"id": 42, "name": "42", "enable": false, "desc": "", "remark": "", "inbounds": []int{1}, "expiry": 0, "volume": 0}}}})
			}
		case "/panel/apiv2/settings":
			admissionJSON(w, map[string]any{"success": true, "obj": map[string]any{"subURI": "https://subs.invalid/", "subPath": "private-path", "subPort": "443", "subDomain": ""}})
		default:
			t.Errorf("unexpected source GET %s?%s", r.URL.Path, r.URL.RawQuery)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	const ninjaToken, suiToken = "synthetic-ninja-token", "synthetic-sui-token"
	imp, err := invoiceninja.NewImportReader(server.URL+"/ninja", ninjaToken, time.Second, 10, 2)
	if err != nil {
		return importadmission.PreparedPlan{}, err
	}
	rec, err := invoiceninja.NewRecurringReader(server.URL+"/ninja", ninjaToken, time.Second)
	if err != nil {
		return importadmission.PreparedPlan{}, err
	}
	cyc, err := invoiceninja.NewCycleReader(server.URL+"/ninja", ninjaToken, time.Second, 10, 2)
	if err != nil {
		return importadmission.PreparedPlan{}, err
	}
	settle, err := invoiceninja.NewSettlementClient(server.URL+"/ninja", ninjaToken, time.Second, 10, 2)
	if err != nil {
		return importadmission.PreparedPlan{}, err
	}
	ids, err := sui.NewIdentityReader(server.URL+"/panel", suiToken, "1.6.4", time.Second)
	if err != nil {
		return importadmission.PreparedPlan{}, err
	}
	records, err := sui.NewRecordReader(server.URL+"/panel", suiToken, "1.6.4", time.Second)
	if err != nil {
		return importadmission.PreparedPlan{}, err
	}
	suiClient, err := sui.New(server.URL+"/panel", suiToken, time.Second)
	if err != nil {
		return importadmission.PreparedPlan{}, err
	}
	scope, err := importverify.SourceScope(server.URL+"/ninja", ninjaToken, "5.13.47", server.URL+"/panel", suiToken, "1.6.4")
	if err != nil {
		return importadmission.PreparedPlan{}, err
	}
	policy := importverify.Policy{BotID: admissionFixtureBotID, SourceScopeHash: scope, PilotIDs: []string{"42"}, NinjaVersion: "5.13.47", SUIVersion: "1.6.4", NativeCurrencyID: "1", NativeCurrencyCode: "USD", ProductKey: "legacy", PublicSubscriptionBase: "https://subs.invalid/", ExpectedInboundIDs: []int64{1}, MaxRecords: 10, MaxInventoryRecords: 20, MaxPages: 2, MaxHistoricalAnchors: 5, EvidenceTTL: 5 * time.Minute}
	service, err := importverify.New(policy, importverify.Providers{Customers: imp, Recurring: rec, Invoices: cyc, Payments: settle, Identities: ids, Records: records, Settings: suiClient})
	if err != nil {
		return importadmission.PreparedPlan{}, err
	}
	claim := importplan.Record{TelegramID: "42", InvoiceCustomerID: customerID, InvoiceNumber: "42", CurrentInvoiceID: currentID, InvoiceStatus: "sent", RecurringID: recurringID, SUIID: "42", SUIName: "42", ConfigHash: configHash, PeriodStart: start, PeriodEnd: end, Currency: "USD", Lifecycle: "paused", SourceVersion: "5.13.47", PinnedProvenance: true, CompleteProof: true, UnpaidInvoices: 1, Amount: "100"}
	plan := importplan.Analyze(importplan.Manifest{Records: []importplan.Record{claim}})
	if plan.Imported != 1 {
		return importadmission.PreparedPlan{}, fmt.Errorf("fixture claim not admissible: %v", plan.Reasons)
	}
	return importadmission.Prepare(ctx, service, plan, plan.Hash, 1, verifiedAt)
}
