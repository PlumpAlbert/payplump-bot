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

func TestVerifiedImportBatchRollsBackAllClientsAndStagingOnLaterConflict(t *testing.T) {
	s, ctx := activationTestStore(t)
	const botID = int64(9)
	if _, err := s.pool.Exec(ctx, `INSERT INTO telegram_bots(bot_id) VALUES($1) ON CONFLICT DO NOTHING`, botID); err != nil {
		t.Fatal(err)
	}
	prepared, err := admissionBatchPreparedFixture(t, ctx, botID)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.RowCount() != 2 || prepared.Plan().Imported != 2 {
		t.Fatalf("fixture did not prepare both records: rows=%d plan=%+v", prepared.RowCount(), prepared.Plan())
	}
	if err := s.SyncImportAdmissionPolicy(ctx, botID, 1, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
		t.Fatal(err)
	}

	// The 43 conflict is deliberately present before this one-batch Apply.
	local, err := s.RegisterPrivateClient(ctx, TelegramIdentity{TelegramID: 43, ChatID: 43, DisplayName: "Pre-existing local 43"})
	if err != nil || !local.Created || !local.ClientCreated {
		t.Fatalf("local conflict fixture: %+v %v", local, err)
	}
	var beforeGuard int64
	var beforeFree bool
	if err := s.pool.QueryRow(ctx, `SELECT epoch,operation_id IS NULL FROM activation_write_guard WHERE id=1`).Scan(&beforeGuard, &beforeFree); err != nil {
		t.Fatal(err)
	}
	if !beforeFree {
		t.Fatal("test store unexpectedly has a held activation guard")
	}
	if _, err := s.ApplyVerifiedImport(ctx, prepared); err == nil {
		t.Fatal("batch accepted a later unmanaged-client conflict")
	}

	var clients42, subs42, origins, batches, staged int
	for query, dst := range map[string]*int{
		`SELECT count(*) FROM clients WHERE telegram_id=42`:                                                   &clients42,
		`SELECT count(*) FROM subscriptions WHERE client_id IN (SELECT id FROM clients WHERE telegram_id=42)`: &subs42,
		`SELECT count(*) FROM verified_import_origins`:                                                        &origins,
		`SELECT count(*) FROM import_batches`:                                                                 &batches,
		`SELECT count(*) FROM import_candidates_staging`:                                                      &staged,
	} {
		if err := s.pool.QueryRow(ctx, query).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	if clients42 != 0 || subs42 != 0 || origins != 0 || batches != 0 || staged != 0 {
		t.Fatalf("failed batch leaked state: clients42=%d subscriptions=%d origins=%d batches=%d candidates=%d", clients42, subs42, origins, batches, staged)
	}
	var localRows, localOps int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM clients WHERE id=$1 AND telegram_id=43 AND display_name='Pre-existing local 43'`, local.Client.ID).Scan(&localRows); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM operations WHERE client_id=$1`, local.Client.ID).Scan(&localOps); err != nil {
		t.Fatal(err)
	}
	if localRows != 1 || localOps != 1 {
		t.Fatalf("existing local client/journal changed: client=%d operations=%d", localRows, localOps)
	}
	var localSubscriptionState string
	var localDesiredAccess bool
	var localInvoice, localRecurring *string
	if err := s.pool.QueryRow(ctx, `SELECT state,desired_access,current_invoice_id,recurring_id FROM subscriptions WHERE client_id=$1`, local.Client.ID).Scan(&localSubscriptionState, &localDesiredAccess, &localInvoice, &localRecurring); err != nil {
		t.Fatal(err)
	}
	if localSubscriptionState != "awaiting_payment" || localDesiredAccess || localInvoice != nil || localRecurring != nil {
		t.Fatalf("existing local subscription changed: state=%q desired=%v invoice=%v recurring=%v", localSubscriptionState, localDesiredAccess, localInvoice, localRecurring)
	}
	var afterGuard int64
	var afterFree bool
	if err := s.pool.QueryRow(ctx, `SELECT epoch,operation_id IS NULL FROM activation_write_guard WHERE id=1`).Scan(&afterGuard, &afterFree); err != nil {
		t.Fatal(err)
	}
	if afterGuard != beforeGuard || afterFree != beforeFree {
		t.Fatalf("failed batch changed activation guard: before=(%d,%v) after=(%d,%v)", beforeGuard, beforeFree, afterGuard, afterFree)
	}
}

func admissionBatchPreparedFixture(t *testing.T, ctx context.Context, botID int64) (importadmission.PreparedPlan, error) {
	t.Helper()
	type fixture struct{ tg, customer, recurring, invoice string }
	actors := []fixture{{"42", "customer-42", "recurring-42", "invoice-42"}, {"43", "customer-43", "recurring-43", "invoice-43"}}
	now := time.Now().UTC()
	startDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -10)
	endDate := startDate.AddDate(0, 1, 0)
	start, end := startDate.Format(time.DateOnly), endDate.Format(time.DateOnly)
	customers, recurring, invoices, settlements, payments := make(map[string]any), make(map[string]any), make(map[string]any), make(map[string]any), make(map[string]any)
	claims := make([]importplan.Record, 0, len(actors))
	suiRecords := make([]json.RawMessage, 0, len(actors))
	for _, a := range actors {
		line := map[string]any{"product_key": "legacy", "notes": "legacy monthly", "cost": 100, "quantity": 1, "discount": 0, "is_amount_discount": false, "type_id": "1", "tax_name1": "", "tax_name2": "", "tax_name3": "", "tax_rate1": 0, "tax_rate2": 0, "tax_rate3": 0, "tax_id": ""}
		r := map[string]any{"id": a.recurring, "client_id": a.customer, "status_id": "2", "private_notes": "historical legacy note", "frequency_id": "5", "next_send_date": end, "next_send_datetime": end + "T00:00:00.000000Z", "last_sent_date": start, "due_date": end, "due_date_days": "on_receipt", "auto_bill": "off", "auto_bill_enabled": false, "remaining_cycles": -1, "amount": 100, "balance": 100, "uses_inclusive_taxes": false, "line_items": []any{line}, "is_deleted": false, "archived_at": 0, "discount": 0, "is_amount_discount": true, "tax_rate1": 0, "tax_rate2": 0, "tax_rate3": 0, "custom_surcharge1": 0, "custom_surcharge2": 0, "custom_surcharge3": 0, "custom_surcharge4": 0, "partial": 0, "paid_to_date": 0, "exchange_rate": 1}
		i := map[string]any{"id": a.invoice, "client_id": a.customer, "recurring_id": a.recurring, "number": "N-" + a.tg, "private_notes": "paid current cycle", "status_id": "4", "date": start, "due_date": start, "amount": 100, "balance": 0, "paid_to_date": 100, "exchange_rate": 1, "is_deleted": false, "archived_at": 0, "updated_at": 0, "invitations": []any{}}
		c := map[string]any{"id": a.customer, "id_number": a.tg, "private_notes": "legacy customer", "is_deleted": false, "archived_at": 0}
		sc := map[string]any{"id": a.customer, "id_number": a.tg, "group_settings_id": "", "settings": map[string]any{"currency_id": "1"}, "currency_id": "ignored", "is_deleted": false, "archived_at": 0}
		allocation := map[string]any{"id": "allocation-" + a.tg, "invoice_id": a.invoice, "payment_id": "payment-" + a.tg, "amount": 100, "refunded": 0, "archived_at": 0}
		p := map[string]any{"id": "payment-" + a.tg, "client_id": a.customer, "date": start, "status_id": "4", "currency_id": "1", "exchange_currency_id": "", "amount": 100, "refunded": 0, "applied": 100, "exchange_rate": 1, "is_manual": true, "is_deleted": false, "archived_at": 0, "paymentables": []any{allocation}, "invoices": []any{}}
		customers[a.tg], recurring[a.customer], invoices[a.invoice], settlements[a.customer], payments[a.customer] = c, r, i, sc, p
		raw, configHash := admissionBatchSUIFixture(t, a.tg)
		suiRecords = append(suiRecords, raw)
		claims = append(claims, importplan.Record{TelegramID: a.tg, InvoiceCustomerID: a.customer, InvoiceNumber: a.tg, CurrentInvoiceID: a.invoice, InvoiceStatus: "paid", RecurringID: a.recurring, SUIID: a.tg, SUIName: a.tg, ConfigHash: configHash, PeriodStart: start, PeriodEnd: end, Currency: "USD", Lifecycle: "active", SourceVersion: "5.13.47", PinnedProvenance: true, CompleteProof: true, Amount: "100"})
	}
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
			a := customers[r.URL.Query().Get("id_number")]
			if a == nil {
				admissionPage(w, []any{})
				return
			}
			admissionPage(w, []any{a})
		case "/ninja/api/v1/recurring_invoices":
			a := recurring[r.URL.Query().Get("client_id")]
			if a == nil {
				admissionPage(w, []any{})
				return
			}
			admissionPage(w, []any{a})
		case "/ninja/api/v1/recurring_invoices/recurring-42":
			admissionJSON(w, map[string]any{"data": recurring["customer-42"]})
		case "/ninja/api/v1/recurring_invoices/recurring-43":
			admissionJSON(w, map[string]any{"data": recurring["customer-43"]})
		case "/ninja/api/v1/invoices":
			admissionPage(w, []any{invoices["invoice-42"], invoices["invoice-43"]})
		case "/ninja/api/v1/invoices/invoice-42":
			admissionJSON(w, map[string]any{"data": invoices["invoice-42"]})
		case "/ninja/api/v1/invoices/invoice-43":
			admissionJSON(w, map[string]any{"data": invoices["invoice-43"]})
		case "/ninja/api/v1/clients/customer-42":
			admissionJSON(w, map[string]any{"data": settlements["customer-42"]})
		case "/ninja/api/v1/clients/customer-43":
			admissionJSON(w, map[string]any{"data": settlements["customer-43"]})
		case "/ninja/api/v1/payments":
			a := payments[r.URL.Query().Get("client_id")]
			if a == nil {
				admissionPage(w, []any{})
				return
			}
			admissionPage(w, []any{a})
		case "/panel/apiv2/status":
			admissionJSON(w, map[string]any{"success": true, "obj": map[string]any{"sys": map[string]any{"appVersion": "1.6.4"}}})
		case "/panel/apiv2/clients":
			if id := r.URL.Query().Get("id"); id != "" {
				idx := 0
				if id == "43" {
					idx = 1
				}
				admissionJSON(w, map[string]any{"success": true, "obj": map[string]any{"clients": []json.RawMessage{suiRecords[idx]}}})
				return
			}
			admissionJSON(w, map[string]any{"success": true, "obj": map[string]any{"clients": []json.RawMessage{suiRecords[0], suiRecords[1]}}})
		case "/panel/apiv2/settings":
			admissionJSON(w, map[string]any{"success": true, "obj": map[string]any{"subURI": "https://subs.invalid/", "subPath": "private-path", "subPort": "443", "subDomain": ""}})
		default:
			t.Errorf("unexpected source GET %s?%s", r.URL.Path, r.URL.RawQuery)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
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
	recordReader, err := sui.NewRecordReader(server.URL+"/panel", suiToken, "1.6.4", time.Second)
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
	policy := importverify.Policy{BotID: botID, SourceScopeHash: scope, PilotIDs: []string{"42", "43"}, NinjaVersion: "5.13.47", SUIVersion: "1.6.4", NativeCurrencyID: "1", NativeCurrencyCode: "USD", ProductKey: "legacy", PublicSubscriptionBase: "https://subs.invalid/", ExpectedInboundIDs: []int64{1}, MaxRecords: 10, MaxInventoryRecords: 20, MaxPages: 2, MaxHistoricalAnchors: 5, EvidenceTTL: 5 * time.Minute}
	service, err := importverify.New(policy, importverify.Providers{Customers: imp, Recurring: rec, Invoices: cyc, Payments: settle, Identities: ids, Records: recordReader, Settings: suiClient})
	if err != nil {
		return importadmission.PreparedPlan{}, err
	}
	plan := importplan.Analyze(importplan.Manifest{Records: claims})
	if plan.Imported != 2 {
		return importadmission.PreparedPlan{}, fmt.Errorf("batch fixture plan not admissible: %v", plan.Reasons)
	}
	return importadmission.Prepare(ctx, service, plan, plan.Hash, 1, time.Now().UTC())
}

func admissionBatchSUIFixture(t *testing.T, id string) (json.RawMessage, string) {
	t.Helper()
	raw, _ := admissionSUIFixture(t)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	fields["id"], _ = json.Marshal(id)
	fields["name"], _ = json.Marshal(id)
	var configs map[string]map[string]any
	if err := json.Unmarshal(fields["config"], &configs); err != nil {
		t.Fatal(err)
	}
	for _, config := range configs {
		for _, key := range []string{"name", "username"} {
			if _, ok := config[key]; ok {
				config[key] = id
			}
		}
	}
	fields["config"], _ = json.Marshal(configs)
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	record, err := sui.ParseClientRecord(raw)
	if err != nil {
		t.Fatal(err)
	}
	hash := record.CredentialsHash()
	return raw, fmt.Sprintf("%x", hash)
}
