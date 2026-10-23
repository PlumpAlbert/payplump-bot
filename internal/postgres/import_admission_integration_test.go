//go:build integration

package postgres

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/billing"
	"github.com/PlumpAlbert/payplump-bot/internal/importadmission"
	"github.com/PlumpAlbert/payplump-bot/internal/importplan"
	"github.com/PlumpAlbert/payplump-bot/internal/importverify"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	admissionFixtureBotID = int64(918273)
	admissionFixtureTG    = "42"
	admissionFixtureCust  = "customer-42"
	admissionFixtureRec   = "recurring-42"
	admissionFixtureInv   = "invoice-42"
)

func TestVerifiedImportAdmissionGETBridgeAndIdempotence(t *testing.T) {
	s, ctx := activationTestStore(t)
	if _, err := s.pool.Exec(ctx, `INSERT INTO telegram_bots(bot_id) VALUES($1) ON CONFLICT DO NOTHING`, admissionFixtureBotID); err != nil {
		t.Fatal(err)
	}
	prepare := admissionPreparedFixtureFactory(t, ctx)
	prepared, err := prepare(time.Now().UTC().Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SyncImportAdmissionPolicy(ctx, admissionFixtureBotID, 1, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncImportAdmissionPolicy(ctx, admissionFixtureBotID, 1, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
		t.Fatalf("idempotent policy sync: %v", err)
	}
	if err := s.SyncImportAdmissionPolicy(ctx, admissionFixtureBotID, 1, strings.Repeat("f", 64), prepared.SourceScopeHash(), true); err == nil {
		t.Fatal("same revision accepted changed policy hash")
	}
	if err := s.SyncImportAdmissionPolicy(ctx, admissionFixtureBotID, 1, prepared.PolicyHash(), strings.Repeat("f", 64), true); err == nil {
		t.Fatal("same revision accepted changed source scope")
	}
	stale, err := prepare(time.Now().UTC().Add(-6 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyVerifiedImport(ctx, stale); err == nil {
		t.Fatal("accepted expired prepared evidence")
	}
	if _, err := s.ApplyVerifiedImport(ctx, importadmission.PreparedPlan{}); err == nil {
		t.Fatal("accepted zero prepared value")
	}

	// A real OWN activation write intent occupies the shared guard. The failed
	// import must roll back candidate staging and leave guard owner/epoch intact.
	seedActivationAccounts(t, s, ctx, 123, 19999, 1)
	seedActivationBatch(t, s, ctx, 123, activationAllow(19999, 1), 1)
	owner, snapshot := claimOneActivation(t, s, ctx, 123)
	payload := activationPayload(t, snapshot, activation.SUICreate)
	if granted, err := s.PrepareActivationWrite(ctx, owner.Lease, snapshot, activation.SUICreate, payload, activationToday); err != nil || !granted {
		t.Fatalf("own guard fixture: %v %v", granted, err)
	}
	var epoch int64
	var heldStep string
	if err := s.pool.QueryRow(ctx, `SELECT epoch,step FROM activation_write_guard WHERE id=1 AND operation_id=$1`, owner.ID).Scan(&epoch, &heldStep); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyVerifiedImport(ctx, prepared); err == nil {
		t.Fatal("import crossed occupied shared writer guard")
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM clients WHERE telegram_id=42`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed import leaked client: count=%d err=%v", count, err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM import_batches`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed import leaked staging: count=%d err=%v", count, err)
	}
	var held bool
	if err := s.pool.QueryRow(ctx, `SELECT epoch=$2 AND operation_id=$1 AND step=$3 FROM activation_write_guard WHERE id=1`, owner.ID, epoch, heldStep).Scan(&held); err != nil || !held {
		t.Fatalf("import modified unknown guard: held=%v err=%v", held, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE activation_write_guard SET operation_id=NULL,step=NULL WHERE id=1`); err != nil {
		t.Fatal(err)
	}

	result, err := s.ApplyVerifiedImport(ctx, prepared)
	if err != nil || result.Admitted != 1 || result.AlreadyAdmitted != 0 {
		t.Fatalf("admission result=%+v err=%v", result, err)
	}
	var clientID int64
	if err := s.pool.QueryRow(ctx, `SELECT id FROM clients WHERE telegram_id=42`).Scan(&clientID); err != nil {
		t.Fatal(err)
	}
	var firstInvoice *string
	if err := s.pool.QueryRow(ctx, `SELECT first_invoice_id FROM subscriptions WHERE client_id=$1`, clientID).Scan(&firstInvoice); err != nil || firstInvoice != nil {
		t.Fatalf("fabricated first invoice: %v %v", firstInvoice, err)
	}
	for _, table := range []string{"operations", "payment_confirmations", "payment_checks", "activation_sui_bindings"} {
		if err := s.pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s WHERE client_id=$1`, table), clientID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("import created %s rows: %d %v", table, count, err)
		}
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE bot_id=$1 AND chat_id=42`, admissionFixtureBotID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("import created outbox rows: %d %v", count, err)
	}
	var sourceHash []byte
	var clientConfigRevision, originConfigRevision int64
	var template, proof, history, evidence []byte
	if err := s.pool.QueryRow(ctx, `SELECT c.configuration_revision,o.configuration_revision_at_admission,o.source_facts_hash,o.template,o.proof,o.historical_invoice_ids,o.evidence FROM clients c JOIN verified_import_origins o ON o.client_id=c.id WHERE c.id=$1`, clientID).Scan(&clientConfigRevision, &originConfigRevision, &sourceHash, &template, &proof, &history, &evidence); err != nil {
		t.Fatal(err)
	}
	if len(sourceHash) != 32 || clientConfigRevision != originConfigRevision || !strings.Contains(string(template), "historical legacy note") || !strings.Contains(string(proof), admissionFixtureInv) || !strings.Contains(string(history), admissionFixtureInv) || !strings.Contains(string(evidence), "historical legacy note") {
		t.Fatal("persisted origin omitted verified source facts")
	}

	// /start can refresh display-only data without revoking immutable origin.
	registered, err := s.RegisterPrivateClient(ctx, TelegramIdentity{TelegramID: 42, ChatID: 42, DisplayName: "Synthetic renamed"})
	if err != nil || registered.Created || registered.ClientCreated {
		t.Fatalf("repeat registration: %+v %v", registered, err)
	}
	preparedAgain, err := prepare(time.Now().UTC().Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := s.ApplyVerifiedImport(ctx, preparedAgain)
	if err != nil || replayed.Admitted != 0 || replayed.AlreadyAdmitted != 1 {
		t.Fatalf("idempotent admission: %+v %v", replayed, err)
	}
	var displayName string
	if err := s.pool.QueryRow(ctx, `SELECT display_name FROM clients WHERE id=$1`, clientID).Scan(&displayName); err != nil || displayName != "Synthetic renamed" {
		t.Fatalf("display name lost: %q %v", displayName, err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM operations WHERE client_id=$1`, clientID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("repeat registration enqueued operation: %d %v", count, err)
	}
	for _, q := range []string{`UPDATE verified_import_origins SET lifecycle='paused' WHERE client_id=$1`, `DELETE FROM verified_import_origins WHERE client_id=$1`} {
		_, err = s.pool.Exec(ctx, q, clientID)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("mutable origin accepted (%s): %v", q, err)
		}
	}
	if err := s.SyncImportAdmissionPolicy(ctx, admissionFixtureBotID, 2, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncImportAdmissionPolicy(ctx, admissionFixtureBotID, 1, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err == nil {
		t.Fatal("accepted lower policy revision")
	}
	if err := s.SyncImportAdmissionPolicy(ctx, admissionFixtureBotID, 3, prepared.PolicyHash(), prepared.SourceScopeHash(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyVerifiedImport(ctx, preparedAgain); err == nil {
		t.Fatal("disabled policy admitted prepared import")
	}
	if err := s.SyncImportAdmissionPolicy(ctx, admissionFixtureBotID, 3, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err == nil {
		t.Fatal("same-revision re-enable bypassed revocation")
	}
	_, err = s.pool.Exec(ctx, `DELETE FROM import_admission_policy WHERE bot_id=$1`, admissionFixtureBotID)
	var deleteErr *pgconn.PgError
	if !errors.As(err, &deleteErr) || deleteErr.Code != "23514" {
		t.Fatalf("policy deletion accepted: %v", err)
	}
	if err := s.SyncImportAdmissionPolicy(ctx, admissionFixtureBotID, 4, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyVerifiedImport(ctx, preparedAgain); err == nil {
		t.Fatal("old prepared revision admitted after policy revision advanced")
	}

	// Reverse identity exclusion: a valid OWN activation can't finalize an ID
	// already recorded by import provenance.
	if err := finishOwnedActivationWithSUIID(t, s, ctx, owner, "42"); err == nil {
		t.Fatal("own activation adopted imported SUI ID")
	} else {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			t.Fatalf("wrong OWN/import collision error: %v", err)
		}
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM activation_sui_bindings WHERE operation_id=$1`, owner.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed OWN finalization persisted binding: %d %v", count, err)
	}

	// A valid second OWN binding may not be edited onto the imported ID or
	// name either; exercise the UPDATE trigger, not just the insert path.
	seedActivationAccounts(t, s, ctx, 123, 20000, 1)
	seedActivationBatch(t, s, ctx, 123, activationAllow(20000, 1), 1)
	updater, _ := claimOneActivation(t, s, ctx, 123)
	if err := finishOwnedActivationWithSUIID(t, s, ctx, updater, "43"); err != nil {
		t.Fatal(err)
	}
	for _, update := range []string{`UPDATE activation_sui_bindings SET sui_id=42 WHERE operation_id=$1`, `UPDATE activation_sui_bindings SET name='42' WHERE operation_id=$1`} {
		_, err := s.pool.Exec(ctx, update, updater.ID)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			t.Fatalf("OWN binding update bypassed imported identity exclusion: %q err=%v", update, err)
		}
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM activation_sui_bindings WHERE operation_id=$1 AND sui_id=43`, updater.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rejected OWN binding update changed existing binding: count=%d err=%v", count, err)
	}
}

func TestVerifiedImportRejectsExistingOwnedSUIBindingAtomically(t *testing.T) {
	s, ctx := activationTestStore(t)
	if _, err := s.pool.Exec(ctx, `INSERT INTO telegram_bots(bot_id) VALUES($1) ON CONFLICT DO NOTHING`, admissionFixtureBotID); err != nil {
		t.Fatal(err)
	}
	prepared := admissionPreparedFixture(t, ctx)
	if err := s.SyncImportAdmissionPolicy(ctx, admissionFixtureBotID, 1, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
		t.Fatal(err)
	}

	// Finish a real OWN activation first. FinishActivation commits both this
	// binding and clients.sui_id in one transaction; import must reject it.
	seedActivationAccounts(t, s, ctx, 123, 20000, 1)
	seedActivationBatch(t, s, ctx, 123, activationAllow(20000, 1), 1)
	owner, _ := claimOneActivation(t, s, ctx, 123)
	if err := finishOwnedActivationWithSUIID(t, s, ctx, owner, admissionFixtureTG); err != nil {
		t.Fatalf("finish OWN activation: %v", err)
	}
	var clientID int64
	if err := s.pool.QueryRow(ctx, `SELECT id FROM clients WHERE telegram_id=20000 AND sui_id='42'`).Scan(&clientID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyVerifiedImport(ctx, prepared); err == nil {
		t.Fatal("import accepted an existing OWN SUI binding")
	}
	var clients, subscriptions, origins, batches, bindings int
	for query, dst := range map[string]*int{
		`SELECT count(*) FROM clients WHERE telegram_id=42`:                                                &clients,
		`SELECT count(*) FROM subscriptions WHERE client_id=(SELECT id FROM clients WHERE telegram_id=42)`: &subscriptions,
		`SELECT count(*) FROM verified_import_origins`:                                                     &origins,
		`SELECT count(*) FROM import_batches`:                                                              &batches,
		`SELECT count(*) FROM activation_sui_bindings WHERE operation_id=$1 AND sui_id=42`:                 &bindings,
	} {
		var err error
		if dst == &bindings {
			err = s.pool.QueryRow(ctx, query, owner.ID).Scan(dst)
		} else {
			err = s.pool.QueryRow(ctx, query).Scan(dst)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if clients != 0 || subscriptions != 0 || origins != 0 || batches != 0 || bindings != 1 {
		t.Fatalf("OWN collision changed state: clients=%d subscriptions=%d origins=%d batches=%d bindings=%d", clients, subscriptions, origins, batches, bindings)
	}
	var ownClientID int64
	if err := s.pool.QueryRow(ctx, `SELECT id FROM clients WHERE id=$1 AND telegram_id=20000 AND sui_id=42`, clientID).Scan(&ownClientID); err != nil || ownClientID != clientID {
		t.Fatalf("existing OWN client changed: id=%d err=%v", ownClientID, err)
	}
	var bindingName string
	var journalCount int
	if err := s.pool.QueryRow(ctx, `SELECT name FROM activation_sui_bindings WHERE operation_id=$1 AND sui_id=42`, owner.ID).Scan(&bindingName); err != nil || bindingName != "42" {
		t.Fatalf("existing OWN binding changed: name=%q err=%v", bindingName, err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM operations WHERE id=$1`, owner.ID).Scan(&journalCount); err != nil || journalCount != 1 {
		t.Fatalf("existing OWN activation journal changed: count=%d err=%v", journalCount, err)
	}
}

func TestVerifiedImportRejectsUnmanagedLocalIdentity(t *testing.T) {
	s, ctx := activationTestStore(t)
	if _, err := s.pool.Exec(ctx, `INSERT INTO telegram_bots(bot_id) VALUES($1) ON CONFLICT DO NOTHING`, admissionFixtureBotID); err != nil {
		t.Fatal(err)
	}
	prepared := admissionPreparedFixture(t, ctx)
	if err := s.SyncImportAdmissionPolicy(ctx, admissionFixtureBotID, 1, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
		t.Fatal(err)
	}
	local, err := s.RegisterPrivateClient(ctx, TelegramIdentity{TelegramID: 42, ChatID: 42, DisplayName: "Existing local user"})
	if err != nil || !local.Created || !local.ClientCreated {
		t.Fatalf("local identity fixture: %+v %v", local, err)
	}
	if _, err = s.ApplyVerifiedImport(ctx, prepared); err == nil {
		t.Fatal("import adopted an unmanaged local client")
	}
	var clients, origins, batches, operations int
	for query, dst := range map[string]*int{
		`SELECT count(*) FROM clients WHERE telegram_id=42 AND display_name='Existing local user'`: &clients,
		`SELECT count(*) FROM verified_import_origins`:                                             &origins,
		`SELECT count(*) FROM import_batches`:                                                      &batches,
		`SELECT count(*) FROM operations WHERE client_id=$1`:                                       &operations,
	} {
		var err error
		if dst == &operations {
			err = s.pool.QueryRow(ctx, query, local.Client.ID).Scan(dst)
		} else {
			err = s.pool.QueryRow(ctx, query).Scan(dst)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if clients != 1 || origins != 0 || batches != 0 || operations != 1 {
		t.Fatalf("local conflict changed state: clients=%d origins=%d batches=%d operations=%d", clients, origins, batches, operations)
	}
}

func admissionPreparedFixture(t *testing.T, ctx context.Context) importadmission.PreparedPlan {
	p, err := admissionPreparedFixtureAt(t, ctx, time.Now().UTC().Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func admissionPreparedFixtureAt(t *testing.T, ctx context.Context, at time.Time) (importadmission.PreparedPlan, error) {
	t.Helper()
	prepare := admissionPreparedFixtureFactory(t, ctx)
	return prepare(at)
}

func admissionPreparedFixtureFactory(t *testing.T, ctx context.Context) func(time.Time) (importadmission.PreparedPlan, error) {
	t.Helper()
	now := time.Now().UTC()
	startDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -10)
	start := startDate.Format(time.DateOnly)
	end, err := billing.NextBillingDate(start)
	if err != nil {
		t.Fatal(err)
	}
	suiRaw, configHash := admissionSUIFixture(t)
	line := map[string]any{"product_key": "legacy", "notes": "legacy monthly", "cost": 100, "quantity": 1, "discount": 0, "is_amount_discount": false, "type_id": "1", "tax_name1": "", "tax_name2": "", "tax_name3": "", "tax_rate1": 0, "tax_rate2": 0, "tax_rate3": 0, "tax_id": ""}
	recurring := map[string]any{"id": admissionFixtureRec, "client_id": admissionFixtureCust, "status_id": "2", "private_notes": "historical legacy note", "frequency_id": "5", "next_send_date": end, "next_send_datetime": end + "T00:00:00.000000Z", "last_sent_date": start, "due_date": end, "due_date_days": "on_receipt", "auto_bill": "off", "auto_bill_enabled": false, "remaining_cycles": -1, "amount": 100, "balance": 100, "uses_inclusive_taxes": false, "line_items": []any{line}, "is_deleted": false, "archived_at": 0, "discount": 0, "is_amount_discount": true, "tax_rate1": 0, "tax_rate2": 0, "tax_rate3": 0, "custom_surcharge1": 0, "custom_surcharge2": 0, "custom_surcharge3": 0, "custom_surcharge4": 0, "partial": 0, "paid_to_date": 0, "exchange_rate": 1}
	invoice := map[string]any{"id": admissionFixtureInv, "client_id": admissionFixtureCust, "recurring_id": admissionFixtureRec, "number": "N-42", "private_notes": "paid current cycle", "status_id": "4", "date": start, "due_date": start, "amount": 100, "balance": 0, "paid_to_date": 100, "exchange_rate": 1, "is_deleted": false, "archived_at": 0, "updated_at": 0, "invitations": []any{}}
	customer := map[string]any{"id": admissionFixtureCust, "id_number": admissionFixtureTG, "private_notes": "legacy customer", "is_deleted": false, "archived_at": 0}
	settlementCustomer := map[string]any{"id": admissionFixtureCust, "id_number": admissionFixtureTG, "group_settings_id": "", "settings": map[string]any{"currency_id": "1"}, "currency_id": "ignored", "is_deleted": false, "archived_at": 0}
	allocation := map[string]any{"id": "allocation-42", "invoice_id": admissionFixtureInv, "payment_id": "payment-42", "amount": 100, "refunded": 0, "archived_at": 0}
	payment := map[string]any{"id": "payment-42", "client_id": admissionFixtureCust, "date": start, "status_id": "4", "currency_id": "1", "exchange_currency_id": "", "amount": 100, "refunded": 0, "applied": 100, "exchange_rate": 1, "is_manual": true, "is_deleted": false, "archived_at": 0, "paymentables": []any{allocation}, "invoices": []any{}}
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
		case "/ninja/api/v1/recurring_invoices/" + admissionFixtureRec:
			admissionJSON(w, map[string]any{"data": recurring})
		case "/ninja/api/v1/invoices":
			admissionPage(w, []any{invoice})
		case "/ninja/api/v1/invoices/" + admissionFixtureInv:
			admissionJSON(w, map[string]any{"data": invoice})
		case "/ninja/api/v1/clients/" + admissionFixtureCust:
			admissionJSON(w, map[string]any{"data": settlementCustomer})
		case "/ninja/api/v1/payments":
			admissionPage(w, []any{payment})
		case "/panel/apiv2/status":
			admissionJSON(w, map[string]any{"success": true, "obj": map[string]any{"sys": map[string]any{"appVersion": "1.6.4"}}})
		case "/panel/apiv2/clients":
			if r.URL.Query().Get("id") == admissionFixtureTG {
				admissionJSON(w, map[string]any{"success": true, "obj": map[string]any{"clients": []json.RawMessage{suiRaw}}})
			} else {
				admissionJSON(w, map[string]any{"success": true, "obj": map[string]any{"clients": []any{map[string]any{"id": 42, "name": "42", "enable": true, "desc": "", "remark": "", "inbounds": []int{1}, "expiry": 0, "volume": 0}}}})
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
		t.Fatal(err)
	}
	rec, err := invoiceninja.NewRecurringReader(server.URL+"/ninja", ninjaToken, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cyc, err := invoiceninja.NewCycleReader(server.URL+"/ninja", ninjaToken, time.Second, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	settle, err := invoiceninja.NewSettlementClient(server.URL+"/ninja", ninjaToken, time.Second, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := sui.NewIdentityReader(server.URL+"/panel", suiToken, "1.6.4", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	records, err := sui.NewRecordReader(server.URL+"/panel", suiToken, "1.6.4", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	suiClient, err := sui.New(server.URL+"/panel", suiToken, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := importverify.SourceScope(server.URL+"/ninja", ninjaToken, "5.13.47", server.URL+"/panel", suiToken, "1.6.4")
	if err != nil {
		t.Fatal(err)
	}
	policy := importverify.Policy{BotID: admissionFixtureBotID, SourceScopeHash: scope, PilotIDs: []string{admissionFixtureTG}, NinjaVersion: "5.13.47", SUIVersion: "1.6.4", NativeCurrencyID: "1", NativeCurrencyCode: "USD", ProductKey: "legacy", PublicSubscriptionBase: "https://subs.invalid/", ExpectedInboundIDs: []int64{1}, MaxRecords: 10, MaxInventoryRecords: 20, MaxPages: 2, MaxHistoricalAnchors: 5, EvidenceTTL: 5 * time.Minute}
	service, err := importverify.New(policy, importverify.Providers{Customers: imp, Recurring: rec, Invoices: cyc, Payments: settle, Identities: ids, Records: records, Settings: suiClient})
	if err != nil {
		t.Fatal(err)
	}
	claim := importplan.Record{TelegramID: admissionFixtureTG, InvoiceCustomerID: admissionFixtureCust, InvoiceNumber: admissionFixtureTG, CurrentInvoiceID: admissionFixtureInv, InvoiceStatus: "paid", RecurringID: admissionFixtureRec, SUIID: admissionFixtureTG, SUIName: admissionFixtureTG, ConfigHash: configHash, PeriodStart: start, PeriodEnd: end, Currency: "USD", Lifecycle: "active", SourceVersion: "5.13.47", PinnedProvenance: true, CompleteProof: true, Amount: "100"}
	plan := importplan.Analyze(importplan.Manifest{Records: []importplan.Record{claim}})
	if plan.Imported != 1 {
		t.Fatalf("fixture claim not admissible: %v", plan.Reasons)
	}
	return func(at time.Time) (importadmission.PreparedPlan, error) {
		return importadmission.Prepare(ctx, service, plan, plan.Hash, 1, at)
	}
}

func admissionSUIFixture(t *testing.T) (json.RawMessage, string) {
	t.Helper()
	draft, err := sui.NewDisabledDraft([]int64{1})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	fields["id"], fields["name"], fields["enable"], fields["inbounds"] = json.RawMessage(`42`), json.RawMessage(`"42"`), json.RawMessage(`true`), json.RawMessage(`[1]`)
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
	raw, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	record, err := sui.ParseClientRecord(raw)
	if err != nil {
		t.Fatal(err)
	}
	sum := record.CredentialsHash()
	return raw, hex.EncodeToString(sum[:])
}

func finishOwnedActivationWithSUIID(t *testing.T, s *Store, ctx context.Context, operation queue.Operation, suiID string) error {
	t.Helper()
	initial := loadActivation(t, s, ctx, operation.Lease, 123)
	if step, ok := initial.Steps[activation.SUICreate]; !ok || step.DispatchAttempt == 0 {
		payload := activationPayload(t, initial, activation.SUICreate)
		if granted, err := s.PrepareActivationWrite(ctx, operation.Lease, initial, activation.SUICreate, payload, activationToday); err != nil || !granted {
			return fmt.Errorf("prepare SUI create: %v %v", granted, err)
		}
	}
	if err := s.ConfirmActivationStep(ctx, operation.Lease, activation.SUICreate, suiID); err != nil {
		return err
	}
	for _, step := range activation.OrderedSteps()[1:] {
		snapshot := loadActivation(t, s, ctx, operation.Lease, 123)
		payload := activationPayload(t, snapshot, step)
		if granted, err := s.PrepareActivationWrite(ctx, operation.Lease, snapshot, step, payload, activationToday); err != nil || !granted {
			return fmt.Errorf("prepare %s: %v %v", step, granted, err)
		}
		var err error
		if step == activation.RecurringCreate || step == activation.RecurringStart {
			err = s.ConfirmRecurringActivationStep(ctx, operation.Lease, step, "recurring-synthetic", "125.25", snapshot.Proof.NextInvoiceDate+"T12:00:00Z")
		} else {
			err = s.ConfirmActivationStep(ctx, operation.Lease, step, suiID)
		}
		if err != nil {
			return err
		}
	}
	snapshot := loadActivation(t, s, ctx, operation.Lease, 123)
	return s.FinishActivation(ctx, operation.Lease, snapshot, activationToday, nil)
}
func admissionJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		panic(err)
	}
}
func admissionPage(w http.ResponseWriter, rows []any) {
	admissionJSON(w, map[string]any{"data": rows, "meta": map[string]any{"pagination": map[string]any{"total": len(rows), "count": len(rows), "per_page": 10, "current_page": 1, "total_pages": 1, "links": map[string]string{"next": "https://must-not-follow.invalid"}}}})
}
