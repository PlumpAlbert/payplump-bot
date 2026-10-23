//go:build integration

package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/billing"
	"github.com/PlumpAlbert/payplump-bot/internal/clientmenu"
	"github.com/PlumpAlbert/payplump-bot/internal/configdelivery"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/lifecycle"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
	"github.com/jackc/pgx/v5/pgconn"
)

const renewalWorkflowBotID int64 = 77123
const renewalWorkflowActor int64 = 77101
const renewalWorkflowAdmin int64 = 77901

func renewalWorkflowAssertInitialSnapshot(t *testing.T, s *Store, ctx context.Context, clientID int64, late bool, proofHash []byte, historical activation.Snapshot, draft invoiceninja.RecurringDraft, binding sui.Binding) {
	t.Helper()
	if n, err := s.SeedRenewalOperations(ctx, renewalWorkflowBotID, []int64{renewalWorkflowActor}, 10); err != nil || n != 1 {
		t.Fatalf("seed initial renewal diagnostic n=%d err=%v", n, err)
	}
	claims, err := s.ClaimOperations(ctx, []string{lifecycle.RenewalOperationKind}, 1, time.Minute)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim initial renewal diagnostic: %v", err)
	}
	snapshot, err := s.LoadRenewalSnapshot(ctx, claims[0], renewalWorkflowBotID)
	if err != nil {
		t.Fatalf("load initial renewal snapshot: %v", err)
	}
	if err := activation.ValidateCompletedSnapshot(historical, binding); err != nil {
		t.Fatalf("historical activation template/binding validation after renewal claim: %v", err)
	}
	var historicalProofHash []byte
	if err := s.pool.QueryRow(ctx, `SELECT proof_hash FROM payment_confirmations WHERE invoice_id=$1`, historical.Account.FirstInvoiceID).Scan(&historicalProofHash); err != nil || string(historicalProofHash) != string(historical.Proof.Hash[:]) {
		t.Fatalf("historical activation proof readback mismatch: err=%v", err)
	}
	wantState, wantDesired := "active", true
	if late {
		wantState, wantDesired = "overdue", false
	}
	valid := snapshot.ClientID == clientID && snapshot.TelegramID == renewalWorkflowActor && snapshot.ChatID == renewalWorkflowActor && snapshot.BotID == renewalWorkflowBotID && snapshot.ManagedProvenance && snapshot.InvoiceID == "invoice-current" && snapshot.CurrentInvoiceID == snapshot.InvoiceID && snapshot.FirstInvoiceID == "invoice-77101" && snapshot.Proof.Validate() == nil && snapshot.Proof.InvoiceID == snapshot.InvoiceID && string(snapshot.Proof.Hash[:]) == string(proofHash) && snapshot.State == wantState && snapshot.DesiredAccess == wantDesired && !snapshot.PaymentReview && !snapshot.PaymentLocked && !snapshot.LifecycleReview && !snapshot.WriterBusy && snapshot.RecurringID == "recurring-synthetic" && snapshot.ActivationToken == historical.Token && reflect.DeepEqual(snapshot.RecurringDraft, draft) && snapshot.RecurringDraft.ClientID == snapshot.CustomerID && snapshot.RecurringDraft.PrivateNotes == activation.Marker(snapshot.ActivationToken) && invoiceninja.ValidateRecurringDraft(snapshot.RecurringDraft) == nil && snapshot.SUIBinding == binding && snapshot.SUIID == strconv.FormatInt(binding.ID, 10) && snapshot.LedgerCustomerID == snapshot.CustomerID && snapshot.LedgerRecurringID == snapshot.RecurringID && snapshot.LedgerAmount == "125.25"
	if late {
		valid = valid && snapshot.LedgerStatus == "sent" && snapshot.LedgerBalance == "125.25" && snapshot.LedgerPaidToDate == "0" && snapshot.OverdueSteps["pause_recurring"] && snapshot.OverdueSteps["disable_sui"] && !snapshot.OverduePending && !snapshot.OverdueOtherSteps
	} else {
		valid = valid && snapshot.LedgerStatus == "paid" && snapshot.LedgerBalance == "0" && snapshot.LedgerPaidToDate == "125.25"
	}
	if !valid {
		t.Fatalf("invalid initial renewal snapshot: managed=%v invoice=%q current=%q first=%q state=%q desired=%v ledger=%q/%q/%q overdue_pause=%v overdue_disable=%v", snapshot.ManagedProvenance, snapshot.InvoiceID, snapshot.CurrentInvoiceID, snapshot.FirstInvoiceID, snapshot.State, snapshot.DesiredAccess, snapshot.LedgerStatus, snapshot.LedgerBalance, snapshot.LedgerPaidToDate, snapshot.OverdueSteps["pause_recurring"], snapshot.OverdueSteps["disable_sui"])
	}
	if err := s.RetryOperation(ctx, claims[0].Lease, 0, 0); err != nil {
		t.Fatalf("release initial renewal diagnostic claim: %v", err)
	}
}

func renewalWorkflowPrepared(t *testing.T, s *Store, ctx context.Context, updateID int64, policy configdelivery.Policy) (telegram.Update, clientmenu.Prepared) {
	t.Helper()
	u := menuTestUpdate(updateID, renewalWorkflowActor, "/config")
	snapshot, duplicate, err := s.LoadMenuSnapshot(ctx, renewalWorkflowBotID, updateID, renewalWorkflowActor)
	if err != nil || duplicate {
		t.Fatalf("renewal configuration menu snapshot unavailable: %v", err)
	}
	return u, clientmenu.Prepared{Request: clientmenu.Request{BotID: renewalWorkflowBotID, UpdateID: updateID, TelegramID: renewalWorkflowActor, ChatID: renewalWorkflowActor, Action: clientmenu.Configuration}, Snapshot: snapshot, Text: queue.ConfigurationIntentBody, ProtectedConfiguration: true, ConfigurationPolicyRevision: policy.Revision, ConfigurationPolicyHash: policy.Hash}
}

// The historical first activation is assembled from canonical payment evidence
// and the production intent constructors. FinishActivation's live-payment
// deadline is intentionally not weakened to replay a historical event.
func renewalWorkflowHistoricalFirst(t *testing.T, s *Store, ctx context.Context) (int64, activation.Snapshot, sui.Binding, invoiceninja.RecurringDraft, sui.ClientRecord) {
	t.Helper()
	ids := seedPaymentAccounts(t, s, ctx, renewalWorkflowBotID, int(renewalWorkflowActor), 1)
	seedPaymentChecks(t, s, ctx, renewalWorkflowBotID, 1)
	claim, account := claimSinglePayment(t, s, ctx, renewalWorkflowBotID)
	var todayCustomer string
	if err := s.pool.QueryRow(ctx, `SELECT invoice_ninja_id FROM clients WHERE id=$1`, ids[0]).Scan(&todayCustomer); err != nil {
		t.Fatal(err)
	}
	today := time.Now().In(time.Local).Format(time.DateOnly)
	paidDate := time.Now().In(time.Local).AddDate(0, -2, 0).Format(time.DateOnly)
	firstTarget, err := billing.NextBillingDate(paidDate)
	if err != nil {
		t.Fatal(err)
	}
	if account.CustomerID != todayCustomer {
		t.Fatalf("registered payment customer changed: snapshot=%q row=%q", account.CustomerID, todayCustomer)
	}
	customer := invoiceninja.SettlementCustomer{ID: todayCustomer, IDNumber: "77101", CurrencyID: "1"}
	invoice := invoiceninja.SettlementInvoice{ID: account.InvoiceID, ClientID: account.CustomerID, Number: "HISTORICAL-FIRST", Date: paidDate, DueDate: firstTarget, Amount: "125.25", Balance: "0", PaidToDate: "125.25", ExchangeRate: "1", StatusID: "4"}
	payment := invoiceninja.SettlementPayment{ID: "historical-payment", ClientID: account.CustomerID, Date: paidDate, StatusID: "4", CurrencyID: "1", Amount: "125.25", Refunded: "0", Applied: "125.25", ExchangeRate: "1", IsManual: true, Allocations: []invoiceninja.SettlementAllocation{{ID: "historical-allocation", InvoiceID: account.InvoiceID, PaymentID: "historical-payment", Amount: "125.25", Refunded: "0"}}}
	decision := payments.Evaluate(account, customer, invoice, []invoiceninja.SettlementPayment{payment}, "1", today)
	if decision.Validate() != nil || decision.Kind != payments.Verified {
		t.Fatalf("historical first proof invalid: %v", decision.Kind)
	}
	if err := s.FinishPaymentCheck(ctx, claim, account, payments.Decision{Kind: payments.Verified, Proof: decision.Proof}, paymentNotices(account), time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE subscriptions SET payment_date=$2::date,next_invoice_date=$3::date WHERE client_id=$1`, ids[0], paidDate, firstTarget); err != nil {
		t.Fatal(err)
	}
	if n, err := s.SeedActivations(ctx, renewalWorkflowBotID, []int64{renewalWorkflowActor}, activationTestPolicy); err != nil || n != 1 {
		t.Fatalf("historical activation seed=%d: %v", n, err)
	}
	claims, err := s.ClaimActivations(ctx, renewalWorkflowBotID, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim historical activation: %v", err)
	}
	snap, err := s.LoadActivation(ctx, claims[0].Lease, renewalWorkflowBotID)
	if err != nil {
		t.Fatal(err)
	}
	product := invoiceninja.Product{ID: "renewal-workflow-product", ProductKey: "monthly", Notes: "Synthetic service", Price: "125.25", TaxRate1: "0", TaxRate2: "0", TaxRate3: "0"}
	draft, err := invoiceninja.BuildRecurringDraft(snap.Account.CustomerID, paidDate, paidDate, product, false, activation.Marker(snap.Token))
	if err != nil {
		t.Fatal(err)
	}
	createRaw, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	suiDraft, err := sui.NewDisabledDraft([]int64{1})
	if err != nil {
		t.Fatal(err)
	}
	suiRaw, err := json.Marshal(suiDraft)
	if err != nil {
		t.Fatal(err)
	}
	snap.Steps[activation.SUICreate] = activation.Intent{Payload: suiRaw, ExternalID: "71", DispatchAttempt: claims[0].Attempt}
	snap.Steps[activation.RecurringCreate] = activation.Intent{Payload: createRaw, ExternalID: "recurring-synthetic", DispatchAttempt: claims[0].Attempt, RecurringAmount: "125.25", RecurringDatetime: draft.NextSendDate + "T12:00:00Z"}
	beforeRaw, _ := json.Marshal(suiDraft)
	var recordMap map[string]json.RawMessage
	if err := json.Unmarshal(beforeRaw, &recordMap); err != nil {
		t.Fatal(err)
	}
	recordMap["id"] = json.RawMessage("71")
	beforeRaw, err = json.Marshal(recordMap)
	if err != nil {
		t.Fatal(err)
	}
	before, err := sui.ParseClientRecord(beforeRaw)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := activation.SUIBinding(snap)
	if err != nil {
		t.Fatal(err)
	}
	enable, err := sui.PrepareEnable(before, binding)
	if err != nil {
		t.Fatal(err)
	}
	enableRaw, _ := json.Marshal(enable)
	snap.Steps[activation.SUIEnable] = activation.Intent{Payload: enableRaw, ExternalID: "71", DispatchAttempt: claims[0].Attempt}
	startRaw, _ := json.Marshal(activation.StartRequest{IDs: []string{"recurring-synthetic"}, Action: "start"})
	snap.Steps[activation.RecurringStart] = activation.Intent{Payload: startRaw, ExternalID: "recurring-synthetic", DispatchAttempt: claims[0].Attempt, RecurringAmount: "125.25", RecurringDatetime: draft.NextSendDate + "T12:00:00Z"}
	for _, step := range activation.OrderedSteps() {
		if err := activation.ValidateIntent(snap, step, snap.Steps[step].Payload); err != nil {
			t.Fatalf("historical %s request invalid: %v", step, err)
		}
	}
	binding, err = activation.SUIBinding(snap)
	if err != nil {
		t.Fatal(err)
	}
	// This is a closed historical journal, not an action pre-completed for the
	// renewal under test. Every original request is a valid, exact production DTO.
	for _, step := range activation.OrderedSteps() {
		i := snap.Steps[step]
		if _, err := s.pool.Exec(ctx, `INSERT INTO activation_steps(operation_id,step,request,external_id,dispatch_attempt,recurring_amount,recurring_datetime,confirmed_at) VALUES($1,$2,$3::jsonb,$4,$5,NULLIF($6,'')::numeric,NULLIF($7,''),clock_timestamp())`, claims[0].ID, string(step), string(i.Payload), i.ExternalID, claims[0].Attempt, i.RecurringAmount, i.RecurringDatetime); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO activation_sui_bindings(client_id,bot_id,sui_id,name,credentials_hash,operation_id) VALUES($1,$2,$3,$4,$5,$6)`, ids[0], renewalWorkflowBotID, binding.ID, binding.Name, binding.CredentialsHash[:], claims[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE clients SET sui_id='71' WHERE id=$1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE subscriptions SET recurring_id='recurring-synthetic',state='active',desired_access=true WHERE client_id=$1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE operations SET status='completed',completed_at=clock_timestamp(),locked_until=NULL WHERE id=$1`, claims[0].ID); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.LoadActivation(ctx, claims[0].Lease, renewalWorkflowBotID)
	// LoadActivation requires the old lease to remain usable; validate the pure
	// completed audit from the frozen data instead of fabricating renewal state.
	if err != nil {
		loaded = snap
		loaded.Account.State = "active"
		loaded.Account.DesiredAccess = true
		loaded.Account.SUIID = "71"
		loaded.Account.RecurringID = "recurring-synthetic"
	}
	if err := activation.ValidateCompletedSnapshot(loaded, binding); err != nil {
		t.Fatalf("historical original audit does not validate: %v", err)
	}
	recordMap["enable"] = json.RawMessage("true")
	raw, err := json.Marshal(recordMap)
	if err != nil {
		t.Fatal(err)
	}
	panel, err := sui.ParseClientRecord(raw)
	if err != nil {
		t.Fatal(err)
	}
	return ids[0], loaded, binding, draft, panel
}

type renewalWorkflowProtectedTransport struct {
	unknown      bool
	chats, sends int
	uri          string
	store        *Store
	botID, actor int64
	invoiceID    string
	renewalID    int64
	proofHash    []byte
	bindingName  string
}

func (p *renewalWorkflowProtectedTransport) GetPrivateChat(_ context.Context, id int64) error {
	if id != p.actor {
		return errors.New("wrong private chat")
	}
	p.chats++
	return nil
}
func (p *renewalWorkflowProtectedTransport) SendProtectedMessage(ctx context.Context, id int64, prepare func(context.Context) (telegram.ProtectedMessage, error)) (int64, error) {
	if id != p.actor {
		return 0, errors.New("wrong protected recipient")
	}
	m, err := prepare(ctx)
	if err != nil {
		return 0, err
	}
	if !m.NotAfter.After(time.Now()) {
		return 0, errors.New("protected message expired before send")
	}
	if strings.Contains(m.Text, "https://") {
		checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		var visible bool
		err := p.store.pool.QueryRow(checkCtx, `SELECT d.kind='secret' AND d.outcome='reserved' AND d.attempt=o.attempts
			AND o.status='running' AND o.locked_until>clock_timestamp() AND r.period_kind='renewal'
			AND r.renewal_operation_id=$3 AND r.invoice_id=$4 AND r.proof_hash=$5
			AND g.outbox_id=o.id AND g.attempt=o.attempts
			AND NOT (row_to_json(r)::text||row_to_json(d)::text||o.body LIKE '%'||$6||'%' OR row_to_json(r)::text||row_to_json(d)::text||o.body LIKE '%https://%')
			FROM outbox o JOIN configuration_requests r ON r.outbox_id=o.id
			JOIN configuration_dispatches d ON d.outbox_id=o.id
			JOIN configuration_chat_guards g ON g.bot_id=o.bot_id AND g.chat_id=o.chat_id
			WHERE o.bot_id=$1 AND o.chat_id=$2 AND o.status='running' AND o.protected_configuration`, p.botID, id, p.renewalID, p.invoiceID, p.proofHash, p.bindingName).Scan(&visible)
		cancel()
		if err != nil || !visible {
			return 0, errors.New("protected send preceded current renewal dispatch authorization")
		}
	}
	p.sends++
	if strings.Contains(m.Text, "https://") {
		lines := strings.Split(m.Text, "\n")
		if len(lines) > 1 {
			p.uri = lines[1]
		}
	}
	if p.unknown {
		return 0, errors.New("synthetic uncertain Telegram receipt")
	}
	return 4242, nil
}

func TestRealRenewalToProtectedConfigurationWorkflow(t *testing.T) {
	for _, tc := range []struct {
		name string
		late bool
	}{{name: "active reschedule"}, {name: "late overdue restoration", late: true}} {
		t.Run(tc.name, func(t *testing.T) { runRenewalProtectedConfigurationCase(t, tc.late) })
	}
}

func runRenewalProtectedConfigurationCase(t *testing.T, late bool) {
	s, ctx := activationTestStore(t)
	clientID, firstActivation, binding, firstDraft, panelRecord := renewalWorkflowHistoricalFirst(t, s, ctx)
	today := time.Now().In(time.Local).Format(time.DateOnly)
	target, err := billing.NextBillingDate(today)
	if err != nil {
		t.Fatal(err)
	}
	var customer string
	if err = s.pool.QueryRow(ctx, `SELECT invoice_ninja_id FROM clients WHERE id=$1`, clientID).Scan(&customer); err != nil {
		t.Fatal(err)
	}
	currentID := "invoice-current"
	currentPay := renewalProofPaymentAmount("payment-current", "allocation-current", currentID, today, "125.25")
	currentPay.ClientID = customer
	currentInvoice := invoiceninja.SettlementInvoice{ID: currentID, ClientID: customer, RecurringID: "recurring-synthetic", Number: "SYNTHETIC-CURRENT", StatusID: "4", Date: today, DueDate: target, Amount: "125.25", Balance: "0", PaidToDate: "125.25", ExchangeRate: "1"}
	cust := invoiceninja.SettlementCustomer{ID: customer, IDNumber: "77101", CurrencyID: "1"}
	var account payments.Snapshot
	if err = s.pool.QueryRow(ctx, `SELECT c.id,c.telegram_id,c.chat_id,pc.bot_id,c.invoice_ninja_id,$2,s.first_invoice_id,s.recurring_id,s.state,s.desired_access FROM clients c JOIN subscriptions s ON s.client_id=c.id JOIN payment_confirmations pc ON pc.client_id=c.id WHERE c.id=$1`, clientID, currentID).Scan(&account.ClientID, &account.TelegramID, &account.ChatID, &account.BotID, &account.CustomerID, &account.InvoiceID, &account.FirstInvoiceID, &account.RecurringID, &account.State, &account.DesiredAccess); err != nil {
		t.Fatal(err)
	}
	account.InvoiceID = currentID
	account.FirstInvoiceID = "invoice-77101"
	account.RecurringID = "recurring-synthetic"
	account.State = "active"
	account.DesiredAccess = true
	decision := payments.Evaluate(account, cust, currentInvoice, []invoiceninja.SettlementPayment{currentPay}, "1", today)
	if decision.Kind != payments.Verified || decision.Validate() != nil {
		t.Fatalf("current renewal proof invalid: %v", decision.Kind)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO payment_confirmations(invoice_id,client_id,bot_id,customer_id,invoice_number,amount,currency_id,payment_date,next_invoice_date,proof_hash,evidence) VALUES($1,$2,$3,$4,$5,$6::numeric,$7,$8::date,$9::date,$10,$11::jsonb)`, currentID, clientID, renewalWorkflowBotID, customer, "SYNTHETIC-CURRENT", decision.Proof.Amount, decision.Proof.CurrencyID, today, decision.Proof.NextInvoiceDate, decision.Proof.Hash[:], string(decision.Proof.JSON)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO invoice_lifecycle_invoices(invoice_id,client_id,bot_id,customer_id,recurring_id,invoice_number,status,sent_date,amount,balance,paid_to_date,notice_state) VALUES($1,$2,$3,$4,$5,'SYNTHETIC-CURRENT','paid',$6::date,125.25,0,125.25,'settled')`, currentID, clientID, renewalWorkflowBotID, customer, "recurring-synthetic", today); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE subscriptions SET current_invoice_id=$2,payment_date=$3::date,next_invoice_date=$4::date WHERE client_id=$1`, clientID, currentID, today, decision.Proof.NextInvoiceDate); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE payment_checks SET review_code=0 WHERE client_id=$1`, clientID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO invoice_lifecycle_checks(client_id,bot_id) VALUES($1,$2) ON CONFLICT(client_id) DO UPDATE SET review_code=0`, clientID, renewalWorkflowBotID); err != nil {
		t.Fatal(err)
	}
	// Reuse the loopback adapter peer, but replace its financial and recurring
	// state with the real current paid cycle and the exact original draft.
	state := &renewalLoopback{invoiceDate: today, invoiceNext: target, customerID: customer, invoiceID: currentID, marker: activation.Marker(firstActivation.Token), invoiceNumber: "SYNTHETIC-CURRENT", telegramID: renewalWorkflowActor, expectedPageSize: 20, paymentID: "payment-current", allocationID: "allocation-current", panel: mustJSON(t, panelRecord), recurring: activationWorkflowRecurring(firstDraft, "recurring-synthetic")}
	state.recurring.StatusID = "2"
	state.recurring.NextSendDate = firstDraft.NextSendDate
	state.recurring.NextSendDatetime = firstDraft.NextSendDate + "T12:00:00Z"
	state.recurring.LastSentDate = ""
	if late {
		seedRenewalWorkflowOverduePrefix(t, s, ctx, clientID, renewalWorkflowBotID, renewalWorkflowActor, state, binding)
		state.recurring.StatusID = "3"
		state.recurring.NextSendDatetime = firstDraft.NextSendDate + "T08:00:00Z"
	}
	state.verifyBeforeWrite = func(step string) error {
		var opID, owner int64
		var outcome, status, guardStep, request string
		var attempts, dispatchAttempt int
		var confirmed, requiresPause, requiresSchedule, requiresEnable bool
		var pauseConfirmed, scheduleConfirmed, enableConfirmed bool
		var requestHash []byte
		err := s.pool.QueryRow(ctx, `SELECT a.operation_id,a.outcome,o.status,o.attempts,g.operation_id,COALESCE(g.step,''),st.request::text,st.dispatch_attempt,st.confirmed_at IS NOT NULL,i.request_hash,
			a.requires_pause,a.requires_schedule,a.requires_enable,COALESCE(p.confirmed_at IS NOT NULL,false),COALESCE(sc.confirmed_at IS NOT NULL,false),COALESCE(en.confirmed_at IS NOT NULL,false)
			FROM invoice_renewal_actions a JOIN operations o ON o.id=a.operation_id
			JOIN invoice_renewal_steps st ON st.operation_id=a.operation_id AND st.step=$3
			JOIN external_write_intents i ON i.operation_id=st.operation_id AND i.step=st.step AND i.origin='lifecycle'
			CROSS JOIN activation_write_guard g
			LEFT JOIN invoice_renewal_steps p ON p.operation_id=a.operation_id AND p.step='pause_recurring'
			LEFT JOIN invoice_renewal_steps sc ON sc.operation_id=a.operation_id AND sc.step='schedule_recurring'
			LEFT JOIN invoice_renewal_steps en ON en.operation_id=a.operation_id AND en.step='enable_sui'
			WHERE a.client_id=$1 AND a.invoice_id=$2 AND g.id=1`, clientID, currentID, step).Scan(&opID, &outcome, &status, &attempts, &owner, &guardStep, &request, &dispatchAttempt, &confirmed, &requestHash, &requiresPause, &requiresSchedule, &requiresEnable, &pauseConfirmed, &scheduleConfirmed, &enableConfirmed)
		if err != nil {
			return err
		}
		hash := sha256.Sum256([]byte(request))
		if opID != owner || guardStep != step || outcome != "pending" || status != "running" || attempts != dispatchAttempt || dispatchAttempt <= 0 || confirmed || string(requestHash) != string(hash[:]) {
			return errors.New("renewal write lacked committed intent and shared guard")
		}
		if step == string(lifecycle.RenewalSchedule) {
			if requiresPause && !pauseConfirmed {
				return errors.New("schedule write preceded committed pause receipt")
			}
			var exact bool
			if err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM jsonb_object_keys(st.request))=3 AND st.request ? 'before' AND st.request ? 'template'
				AND st.request->>'desired_date'=$2 AND (st.request->'before')->>'id'='recurring-synthetic'
				AND (st.request->'template')->>'client_id'=$3
				FROM invoice_renewal_steps st WHERE st.operation_id=$1 AND st.step='schedule_recurring'`, opID, target, customer).Scan(&exact); err != nil {
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) {
					t.Logf("renewal_schedule_callback SQLSTATE=%s", pgErr.Code)
				} else {
					t.Log("renewal_schedule_callback SQL failed outside PostgreSQL")
				}
				return errors.New("schedule intent SQL verification failed")
			}
			if !exact {
				return errors.New("schedule intent did not freeze the expected sparse recurring date")
			}
		}
		if step == string(lifecycle.RenewalEnable) && requiresSchedule && !scheduleConfirmed {
			return errors.New("enable write preceded recurring schedule receipt")
		}
		if step == string(lifecycle.RenewalResume) {
			var scheduled bool
			if err := s.pool.QueryRow(ctx, `SELECT confirmed_at IS NOT NULL AND request->>'desired_date'=$2 FROM invoice_renewal_steps WHERE operation_id=$1 AND step='schedule_recurring'`, opID, target).Scan(&scheduled); err != nil || !scheduled || requiresEnable && !enableConfirmed {
				return errors.New("resume preceded confirmed future schedule or enable receipt")
			}
		}
		return nil
	}
	ninja := httptest.NewServer(state.ninjaHandler())
	defer ninja.Close()
	panelHTTP := httptest.NewServer(state.panelHandler())
	defer panelHTTP.Close()
	cycle, err := invoiceninja.NewCycleReader(ninja.URL, "synthetic", 2*time.Second, 20, 10)
	if err != nil {
		t.Fatal(err)
	}
	settle, err := invoiceninja.NewSettlementClient(ninja.URL, "synthetic", 2*time.Second, 20, 10)
	if err != nil {
		t.Fatal(err)
	}
	recurring, err := invoiceninja.NewRecurringClient(ninja.URL, "synthetic", 2*time.Second, 20, 10)
	if err != nil {
		t.Fatal(err)
	}
	managed, err := sui.NewManagedClient(panelHTTP.URL, "synthetic", "1.6.4", 2*time.Second, 100)
	if err != nil {
		t.Fatal(err)
	}
	cfg := lifecycle.RenewalConfig{Enabled: true, ExclusiveWriter: true, BaseURL: ninja.URL, Token: "synthetic", InvoiceNinjaVersion: "5.13.47", SUIBaseURL: panelHTTP.URL, SUIToken: "synthetic", SUIVersion: "1.6.4", AllowedTelegramIDs: []int64{renewalWorkflowActor}, Workers: 1, MaxAccounts: 20, PollInterval: 100 * time.Millisecond, HTTPTimeout: 2 * time.Second}
	worker, err := lifecycle.NewRenewalWorker(s, cycle, settle, recurring, managed, renewalWorkflowBotID, []int64{renewalWorkflowAdmin}, "1", cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	var originalHash []byte
	if err = s.pool.QueryRow(ctx, `SELECT proof_hash FROM payment_confirmations WHERE invoice_id=$1`, account.FirstInvoiceID).Scan(&originalHash); err != nil {
		t.Fatal(err)
	}
	renewalWorkflowAssertInitialSnapshot(t, s, ctx, clientID, late, decision.Proof.Hash[:], firstActivation, firstDraft, binding)
	workerCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	done := make(chan error, 1)
	go func() { done <- worker.Run(workerCtx) }()
	joined := false
	defer func() {
		cancel()
		if !joined {
			select {
			case e := <-done:
				if e != nil {
					t.Errorf("renewal worker stopped: %v", e)
				}
			case <-time.After(3 * time.Second):
				t.Error("renewal worker did not join")
			}
		}
	}()
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	var renewalID int64
	var outcome string
	for outcome != "completed" && outcome != "review" {
		select {
		case <-poll.C:
			_ = s.pool.QueryRow(workerCtx, `SELECT a.operation_id,a.outcome FROM invoice_renewal_actions a WHERE a.client_id=$1 AND a.invoice_id=$2`, clientID, currentID).Scan(&renewalID, &outcome)
		case <-workerCtx.Done():
			break
		}
		if workerCtx.Err() != nil {
			break
		}
	}
	cancel()
	select {
	case e := <-done:
		joined = true
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("renewal worker did not join")
	}
	if outcome != "completed" {
		var code, lastError int
		var status string
		var steps, unconfirmed int
		err := s.pool.QueryRow(ctx, `SELECT COALESCE(a.review_code,0),o.status,COALESCE(o.last_error_code,0),
			(SELECT count(*) FROM invoice_renewal_steps st WHERE st.operation_id=a.operation_id),
			(SELECT count(*) FROM invoice_renewal_steps st WHERE st.operation_id=a.operation_id AND st.confirmed_at IS NULL)
			FROM invoice_renewal_actions a JOIN operations o ON o.id=a.operation_id WHERE a.client_id=$1 AND a.invoice_id=$2`, clientID, currentID).Scan(&code, &status, &lastError, &steps, &unconfirmed)
		t.Fatalf("real renewal did not complete: outcome=%q review_code=%d operation=%s last_error=%d steps=%d unconfirmed=%d late=%v diagnostic_err=%v", outcome, code, status, lastError, steps, unconfirmed, late, err)
	}
	state.mu.Lock()
	pauseCalls, scheduleCalls, resumeCalls, panelWrites := state.pauseCalls, state.scheduleCalls, state.resumeCalls, state.panelWrites
	state.mu.Unlock()
	wantPause, wantPanel := 1, 0
	if late {
		wantPause, wantPanel = 0, 1
	}
	if pauseCalls != wantPause || scheduleCalls != 1 || resumeCalls != 1 || panelWrites != wantPanel {
		t.Fatalf("late=%v writes pause/schedule/enable/resume=%d/%d/%d/%d", late, pauseCalls, scheduleCalls, panelWrites, resumeCalls)
	}
	var firstHashAgain []byte
	if err = s.pool.QueryRow(ctx, `SELECT proof_hash FROM payment_confirmations WHERE invoice_id=$1`, account.FirstInvoiceID).Scan(&firstHashAgain); err != nil || string(originalHash) != string(firstHashAgain) {
		t.Fatal("historical first proof changed")
	}
	var validCompletion bool
	if err = s.pool.QueryRow(ctx, `SELECT o.status='completed' AND a.outcome='completed' AND b.sui_id=$2 AND b.name=$3 AND b.credentials_hash=$4
		AND a.requires_pause=$5 AND a.requires_schedule AND a.requires_enable=$6 AND a.requires_resume
		AND (SELECT count(*)=4 AND bool_and(confirmed_at IS NOT NULL) FROM activation_steps WHERE operation_id=f.operation_id)
		AND (SELECT count(*)=3 AND bool_and(confirmed_at IS NOT NULL) FROM invoice_renewal_steps WHERE operation_id=a.operation_id)
		FROM invoice_renewal_actions a JOIN operations o ON o.id=a.operation_id JOIN subscriptions sub ON sub.client_id=a.client_id
		JOIN activation_flows f ON f.invoice_id=sub.first_invoice_id JOIN activation_sui_bindings b ON b.client_id=a.client_id WHERE a.operation_id=$1`, renewalID, int64(71), binding.Name, binding.CredentialsHash[:], !late, late).Scan(&validCompletion); err != nil || !validCompletion {
		t.Fatalf("completed renewal changed first audit/binding, plan or step receipts: %v", err)
	}
	var durableRenewal int64
	var mode, requestInvoice string
	var requestHash []byte
	serviceCfg := configdelivery.Config{Enabled: true, TelegramEnabled: true, PaymentsEnabled: true, Revision: 5, BaseURL: ninja.URL, Token: "synthetic", CurrencyID: "1", TelegramIDField: "id_number", SUIBaseURL: panelHTTP.URL, SUIToken: "synthetic", SUIVersion: "1.6.4", SubscriptionBaseURL: "https://subscription.example.test/prefix/", AllowedTelegramIDs: []int64{renewalWorkflowActor}, HTTPTimeout: 2 * time.Second, PageSize: 20, MaxPages: 10}
	policy := serviceCfg.Policy()
	if err = s.InstallConfigurationPolicy(ctx, renewalWorkflowBotID, policy); err != nil {
		t.Fatal(err)
	}
	// Install the protected configuration policy and enqueue through the real menu/store path.
	u, prepared := renewalWorkflowPrepared(t, s, ctx, 99001, policy)
	if err = s.ApplyMenuUpdate(ctx, renewalWorkflowBotID, menuTestUsername, u, prepared); err != nil {
		t.Fatal(err)
	}
	messages, err := s.ClaimOutbox(ctx, renewalWorkflowBotID, 50, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var message queue.Message
	for _, m := range messages {
		if m.ProtectedConfiguration {
			message = m
		} else {
			_ = s.CompleteOutbox(ctx, m.Lease, 9)
		}
	}
	if message.ID == 0 {
		t.Fatal("protected configuration request not claimable")
	}
	if err = s.pool.QueryRow(ctx, `SELECT period_kind,renewal_operation_id,invoice_id,proof_hash FROM configuration_requests WHERE outbox_id=$1`, message.ID).Scan(&mode, &durableRenewal, &requestInvoice, &requestHash); err != nil {
		t.Fatal(err)
	}
	if mode != "renewal" || durableRenewal != renewalID || requestInvoice != currentID || string(requestHash) != string(decision.Proof.Hash[:]) {
		t.Fatalf("request binding mismatch mode=%s renewal=%d invoice=%s", mode, durableRenewal, requestInvoice)
	}
	providers := configdelivery.Providers{Payments: settle, SUI: mustRecordReader(t, panelHTTP.URL), Recurring: mustRecurringReader(t, ninja.URL)}
	transport := &renewalWorkflowProtectedTransport{store: s, botID: renewalWorkflowBotID, actor: renewalWorkflowActor, invoiceID: currentID, renewalID: renewalID, proofHash: decision.Proof.Hash[:], bindingName: binding.Name}
	service, err := configdelivery.New(s, providers, transport, renewalWorkflowBotID, serviceCfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Deliver(ctx, message); err != nil {
		t.Fatal(err)
	}
	expectedURL, urlErr := sui.SubscriptionURL(serviceCfg.SubscriptionBaseURL, binding.Name)
	if urlErr != nil || transport.chats != 1 || transport.sends != 1 || transport.uri != expectedURL {
		t.Fatal("real protected service did not deliver the exact saved binding URL")
	}
	var durableSecretSafe bool
	if err = s.pool.QueryRow(ctx, `SELECT d.kind='secret' AND d.outcome='reserved' AND g.outbox_id=o.id AND o.status='running'
		AND NOT (row_to_json(r)::text||row_to_json(d)::text||o.body LIKE '%'||$2||'%' OR row_to_json(r)::text||row_to_json(d)::text||o.body LIKE '%'||$3||'%')
		FROM outbox o JOIN configuration_requests r ON r.outbox_id=o.id JOIN configuration_dispatches d ON d.outbox_id=o.id
		JOIN configuration_chat_guards g ON g.bot_id=o.bot_id AND g.chat_id=o.chat_id WHERE o.id=$1`, message.ID, binding.Name, transport.uri).Scan(&durableSecretSafe); err != nil || !durableSecretSafe {
		t.Fatalf("protected dispatch was not committed safely before send: %v", err)
	}
	if err = s.CompleteOutbox(ctx, message.Lease, 4242); err != nil {
		t.Fatal(err)
	}
	// A second explicit request can reserve once; an uncertain transport result
	// pins the chat guard, and subsequent requests cannot replay the URL.
	u, prepared = renewalWorkflowPrepared(t, s, ctx, 99002, policy)
	if err = s.ApplyMenuUpdate(ctx, renewalWorkflowBotID, menuTestUsername, u, prepared); err != nil {
		t.Fatal(err)
	}
	messages, err = s.ClaimOutbox(ctx, renewalWorkflowBotID, 50, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	message = queue.Message{}
	for _, m := range messages {
		if m.ProtectedConfiguration {
			message = m
		} else {
			_ = s.CompleteOutbox(ctx, m.Lease, 9)
		}
	}
	transport.unknown = true
	if _, err = service.Deliver(ctx, message); err == nil {
		t.Fatal("synthetic uncertain Telegram result was treated as success")
	}
	if err = s.FailConfigurationOutbox(ctx, message.Lease, 0, 0, false); err != nil {
		t.Fatal(err)
	}
	transport.unknown = false
	beforeBlockedSend := transport.sends
	beforeBlockedURI := transport.uri
	u, prepared = renewalWorkflowPrepared(t, s, ctx, 99003, policy)
	if err = s.ApplyMenuUpdate(ctx, renewalWorkflowBotID, menuTestUsername, u, prepared); err != nil {
		t.Fatal(err)
	}
	messages, err = s.ClaimOutbox(ctx, renewalWorkflowBotID, 50, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range messages {
		if m.ProtectedConfiguration {
			message = m
		} else {
			_ = s.CompleteOutbox(ctx, m.Lease, 9)
		}
	}
	blocked, err := s.LoadConfiguration(ctx, message)
	if err != nil {
		t.Fatal(err)
	}
	if !blocked.ChatBlocked {
		t.Fatal("unknown protected send did not pin chat guard")
	}
	if _, err = service.Deliver(ctx, message); err != nil {
		t.Fatalf("safe denial after unknown send failed: %v", err)
	}
	if transport.sends != beforeBlockedSend+1 || transport.chats != 2 || transport.uri != beforeBlockedURI {
		t.Fatal("unknown guard did not allow only a protected denial without another private-chat check")
	}
	if err = s.CompleteOutbox(ctx, message.Lease, 4242); err != nil {
		t.Fatal(err)
	}
}

func seedRenewalWorkflowOverduePrefix(t *testing.T, s *Store, ctx context.Context, clientID, botID, actor int64, state *renewalLoopback, binding sui.Binding) {
	t.Helper()
	yesterday := time.Now().In(time.Local).AddDate(0, 0, -1).Format(time.DateOnly)
	if _, err := s.pool.Exec(ctx, `UPDATE invoice_lifecycle_invoices SET status='sent',sent_date=$2::date,balance=amount,paid_to_date=0,notice_state='issued' WHERE client_id=$1 AND invoice_id=$3`, clientID, yesterday, "invoice-current"); err != nil {
		t.Fatal(err)
	}
	if count, err := s.SeedOverdueOperations(ctx, botID, []int64{actor}, time.Now().In(time.Local).Format(time.DateOnly), 10); err != nil || count != 1 {
		t.Fatalf("seed owned overdue prefix=%d err=%v", count, err)
	}
	claims, err := s.ClaimOperations(ctx, []string{lifecycle.OverdueOperationKind}, 1, time.Minute)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim owned overdue action: %v", err)
	}
	claim := claims[0]
	for _, step := range []struct{ name, external string }{{"pause_recurring", "recurring-synthetic"}, {"disable_sui", "71"}} {
		if _, err = s.pool.Exec(ctx, `INSERT INTO invoice_overdue_steps(operation_id,step,request,dispatch_attempt,external_id,confirmed_at) VALUES($1,$2,$3::jsonb,$4,$5,clock_timestamp())`, claim.ID, step.name, `{"synthetic":"known owned overdue receipt"}`, claim.Attempt, step.external); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.pool.Exec(ctx, `UPDATE invoice_overdue_actions SET outcome='completed',finished_at=clock_timestamp() WHERE operation_id=$1 AND outcome='pending'`, claim.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE operations SET status='completed',locked_until=NULL,completed_at=clock_timestamp() WHERE id=$1 AND status='running'`, claim.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE subscriptions SET state='overdue',desired_access=false WHERE client_id=$1`, clientID); err != nil {
		t.Fatal(err)
	}
	var panel map[string]any
	if err := json.Unmarshal(state.panel, &panel); err != nil {
		t.Fatal(err)
	}
	panel["enable"] = false
	raw, err := json.Marshal(panel)
	if err != nil {
		t.Fatal(err)
	}
	record, err := sui.ParseClientRecord(raw)
	if err != nil {
		t.Fatal(err)
	}
	if record.CredentialsHash() != binding.CredentialsHash || record.ID() != binding.ID || record.Name() != binding.Name {
		t.Fatal("late restore changed the owned SUI identity or credentials")
	}
	state.panel = raw
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func mustRecordReader(t *testing.T, base string) interface {
	GetRecord(context.Context, int64) (sui.ClientRecord, error)
} {
	t.Helper()
	v, e := sui.NewRecordReader(base, "synthetic", "1.6.4", 2*time.Second)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func mustRecurringReader(t *testing.T, base string) interface {
	Get(context.Context, string) (invoiceninja.RecurringRecord, error)
} {
	t.Helper()
	v, e := invoiceninja.NewRecurringReader(base, "synthetic", 2*time.Second)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
