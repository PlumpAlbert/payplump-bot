//go:build integration

package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/lifecycle"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
)

func invoiceLifecycleTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	if os.Getenv("TEST_DATABASE_URL") == "" && (os.Getenv("PGHOST") == "" || os.Getenv("PGUSER") == "" || os.Getenv("PGDATABASE") == "") {
		t.Skip("requires disposable TEST_DATABASE_URL or PGHOST/PGUSER/PGDATABASE")
	}
	s, setup := testStore(t)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(setup), 4*time.Minute)
	t.Cleanup(cancel)
	return s, ctx
}

func seedInvoiceLifecycleAccount(t *testing.T, s *Store, ctx context.Context, withCurrentProof bool) (int64, int64) {
	t.Helper()
	const botID, telegramID = int64(71), int64(101)
	if _, err := s.pool.Exec(ctx, `INSERT INTO telegram_bots (bot_id) VALUES ($1) ON CONFLICT DO NOTHING`, botID); err != nil {
		t.Fatal(err)
	}
	var clientID int64
	if err := s.pool.QueryRow(ctx, `INSERT INTO clients (telegram_id, chat_id, display_name, invoice_ninja_id, sui_id)
        VALUES ($1, $1, 'Synthetic', 'customer-synthetic', '71') RETURNING id`, telegramID).Scan(&clientID); err != nil {
		t.Fatal(err)
	}
	currentInvoiceID := "invoice-first"
	if !withCurrentProof {
		currentInvoiceID = "invoice-current"
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO subscriptions
        (client_id, state, desired_access, first_invoice_id, current_invoice_id, recurring_id)
        VALUES ($1, 'active', true, 'invoice-first', $2, 'recurring-synthetic')`, clientID, currentInvoiceID); err != nil {
		t.Fatal(err)
	}
	var registrationID int64
	if err := s.pool.QueryRow(ctx, `INSERT INTO operations (client_id, operation_key, kind, status, completed_at)
        VALUES ($1, 'lifecycle:registration:synthetic', 'register_client', 'completed', clock_timestamp()) RETURNING id`, clientID).Scan(&registrationID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO registration_flows (operation_id, bot_id, token)
        VALUES ($1, $2, 'synthetic_registration_token_123456')`, registrationID, botID); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct{ name, externalID string }{{"customer_create", "customer-synthetic"}, {"invoice_create", "invoice-first"}, {"invoice_issue", "invoice-first"}} {
		if _, err := s.pool.Exec(ctx, `INSERT INTO operation_steps (operation_id, step, request, external_id, confirmed_at)
            VALUES ($1, $2, '{}'::jsonb, $3, clock_timestamp())`, registrationID, step.name, step.externalID); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := preparePaymentBot(ctx, tx, botID); err != nil {
		rollback(tx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO payment_checks (client_id, bot_id) VALUES ($1, $2)`, clientID, botID); err != nil {
		t.Fatal(err)
	}
	invoices := []string{"invoice-first"}
	for _, invoiceID := range invoices {
		if _, err := s.pool.Exec(ctx, `INSERT INTO payment_confirmations (invoice_id, client_id, bot_id, customer_id, invoice_number,
            amount, currency_id, payment_date, next_invoice_date, proof_hash, evidence)
            VALUES ($1, $2, $3, 'customer-synthetic', 'SYNTHETIC', 100, '1', '2040-01-01', '2040-02-01', $4, '{}'::jsonb)`,
			invoiceID, clientID, botID, make([]byte, 32)); err != nil {
			t.Fatal(err)
		}
	}
	var activationID int64
	if err := s.pool.QueryRow(ctx, `INSERT INTO operations (client_id, operation_key, kind, status, completed_at)
        VALUES ($1, 'lifecycle:activation:synthetic', 'activate_first_subscription', 'completed', clock_timestamp()) RETURNING id`, clientID).Scan(&activationID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO activation_flows (operation_id, bot_id, token, invoice_id, proof_hash, policy_hash)
        VALUES ($1, $2, 'synthetic_activation_token_123456', 'invoice-first', $3, $3)`, activationID, botID, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		name, externalID string
		recurring        bool
	}{
		{"sui_create", "71", false}, {"recurring_create", "recurring-synthetic", true},
		{"sui_enable", "71", false}, {"recurring_start", "recurring-synthetic", true},
	} {
		var amount any
		var datetime any
		if step.recurring {
			amount, datetime = 100, "2040-01-01T00:00:00Z"
		}
		request := `{"synthetic":true}`
		if step.name == "recurring_create" {
			request = `{"client_id":"customer-synthetic","frequency_id":5,"next_send_date":"2040-02-01","due_date_days":"on_receipt","auto_bill":"off","remaining_cycles":-1,"private_notes":"payplump:synthetic_activation_token_123456:recurring_create","uses_inclusive_taxes":false,"line_items":[{"product_key":"product-synthetic","notes":"subscription","cost":100,"quantity":1,"discount":0,"is_amount_discount":false,"type_id":"1","tax_name1":"","tax_name2":"","tax_name3":"","tax_rate1":0,"tax_rate2":0,"tax_rate3":0,"tax_id":""}]}`
		}
		if _, err := s.pool.Exec(ctx, `INSERT INTO activation_steps
            (operation_id, step, request, dispatch_attempt, external_id, recurring_amount, recurring_datetime, confirmed_at)
            VALUES ($1, $2, $6::jsonb, 1, $3, $4, $5, clock_timestamp())`, activationID, step.name, step.externalID, amount, datetime, request); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO activation_sui_bindings (client_id, bot_id, sui_id, name, credentials_hash, operation_id)
        VALUES ($1, $2, 71, 'pp-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', $3, $4)`, clientID, botID, make([]byte, 32), activationID); err != nil {
		t.Fatal(err)
	}
	if !withCurrentProof {
		if _, err := s.pool.Exec(ctx, `INSERT INTO invoice_lifecycle_invoices
            (invoice_id, client_id, bot_id, customer_id, recurring_id, invoice_number, status, sent_date, amount, balance, paid_to_date, notice_state)
            VALUES ('invoice-current', $1, $2, 'customer-synthetic', 'recurring-synthetic', 'SYNTHETIC-CURRENT', 'sent', '2040-01-01', 100, 100, 0, 'issued')`, clientID, botID); err != nil {
			t.Fatal(err)
		}
	}
	return clientID, botID
}

func claimInvoiceLifecycle(t *testing.T, s *Store, ctx context.Context, botID int64) (lifecycle.Claim, lifecycle.Snapshot) {
	t.Helper()
	if err := s.RecordInvoiceLifecycleWake(ctx, botID); err != nil {
		t.Fatal(err)
	}
	if n, err := s.SeedInvoiceLifecycleChecks(ctx, botID, []int64{101}, 10); err != nil || n != 1 {
		t.Fatalf("seed lifecycle count=%d err=%v", n, err)
	}
	claims, err := s.ClaimInvoiceLifecycleChecks(ctx, botID, []int64{101}, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim lifecycle count=%d err=%v", len(claims), err)
	}
	snapshot, err := s.LoadInvoiceLifecycleSnapshot(ctx, claims[0])
	if err != nil {
		t.Fatal(err)
	}
	return claims[0], snapshot
}

func TestInvoiceLifecycleSentNoticeAndPaymentHandoffAreAtomic(t *testing.T) {
	s, ctx := invoiceLifecycleTestStore(t)
	clientID, botID := seedInvoiceLifecycleAccount(t, s, ctx, true)
	claim, snapshot := claimInvoiceLifecycle(t, s, ctx, botID)
	if _, err := s.pool.Exec(ctx, `UPDATE payment_checks SET next_check_at = clock_timestamp() + interval '1 hour' WHERE client_id = $1`, clientID); err != nil {
		t.Fatal(err)
	}
	invoice := &lifecycle.Invoice{ID: "invoice-next", Number: "SYNTHETIC-NEXT", Status: "sent", SentDate: "2040-02-01", Amount: "100", Balance: "100", PaidToDate: "0"}
	notices := lifecycle.Notices{
		Client: &queue.Draft{BotID: botID, ChatID: 101, Key: "invoice-lifecycle:synthetic:issued:client", Text: "synthetic invoice notice"},
		Admins: []queue.Draft{{BotID: botID, ChatID: 9001, Key: "invoice-lifecycle:synthetic:issued:admin:9001", Text: "synthetic admin notice"}},
	}
	if err := s.FinishInvoiceLifecycleCheck(ctx, claim, snapshot, invoice, 0, notices, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	var current string
	if err := s.pool.QueryRow(ctx, `SELECT current_invoice_id FROM subscriptions WHERE client_id=$1`, clientID).Scan(&current); err != nil || current != invoice.ID {
		t.Fatalf("current invoice=%q err=%v", current, err)
	}
	var recorded, outboxCount int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM invoice_lifecycle_invoices WHERE client_id=$1 AND invoice_id=$2`, clientID, invoice.ID).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE bot_id=$1 AND message_key LIKE 'invoice-lifecycle:synthetic:%'`, botID).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	var generation int64
	var paymentDue bool
	if err := s.pool.QueryRow(ctx, `SELECT generation FROM payment_wake_state WHERE bot_id=$1`, botID).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT next_check_at <= clock_timestamp() FROM payment_checks WHERE client_id=$1`, clientID).Scan(&paymentDue); err != nil {
		t.Fatal(err)
	}
	if recorded != 1 || outboxCount != 2 || generation != 0 || !paymentDue {
		t.Fatalf("recorded=%d outbox=%d global payment wake generation=%d payment due=%v", recorded, outboxCount, generation, paymentDue)
	}
	var guardBusy bool
	if err := s.pool.QueryRow(ctx, `SELECT operation_id IS NOT NULL FROM activation_write_guard WHERE id=1`).Scan(&guardBusy); err != nil || guardBusy {
		t.Fatalf("lifecycle touched shared external-write guard: busy=%v err=%v", guardBusy, err)
	}
}

func TestInvoiceLifecyclePaidBeforeSweepHasNoUnpaidNoticeButHandsOff(t *testing.T) {
	s, ctx := invoiceLifecycleTestStore(t)
	clientID, botID := seedInvoiceLifecycleAccount(t, s, ctx, true)
	claim, snapshot := claimInvoiceLifecycle(t, s, ctx, botID)
	invoice := &lifecycle.Invoice{ID: "invoice-paid-before-sweep", Number: "SYNTHETIC-PAID", Status: "paid", SentDate: "2040-02-01", Amount: "100", Balance: "0", PaidToDate: "100"}
	if err := s.FinishInvoiceLifecycleCheck(ctx, claim, snapshot, invoice, 0, lifecycle.Notices{}, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	var current string
	if err := s.pool.QueryRow(ctx, `SELECT current_invoice_id FROM subscriptions WHERE client_id=$1`, clientID).Scan(&current); err != nil || current != invoice.ID {
		t.Fatalf("paid invoice handoff failed: %q %v", current, err)
	}
	var rows, messages int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM invoice_lifecycle_invoices WHERE invoice_id=$1 AND status='paid' AND notice_state='settled'`, invoice.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE bot_id=$1`, botID).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || messages != 0 {
		t.Fatalf("paid ledger=%d unpaid messages=%d", rows, messages)
	}
}

func TestInvoiceLifecycleDefersUntilPreviousInvoiceIsVerified(t *testing.T) {
	s, ctx := invoiceLifecycleTestStore(t)
	clientID, botID := seedInvoiceLifecycleAccount(t, s, ctx, false)
	claim, snapshot := claimInvoiceLifecycle(t, s, ctx, botID)
	invoice := &lifecycle.Invoice{ID: "invoice-next", Number: "SYNTHETIC-NEXT", Status: "sent", SentDate: "2040-02-01", Amount: "100", Balance: "100", PaidToDate: "0"}
	notices := lifecycle.Notices{Client: &queue.Draft{BotID: botID, ChatID: 101, Key: "invoice-lifecycle:defer:client", Text: "synthetic invoice notice"}, Admins: []queue.Draft{{BotID: botID, ChatID: 9001, Key: "invoice-lifecycle:defer:admin", Text: "synthetic admin notice"}}}
	if err := s.FinishInvoiceLifecycleCheck(ctx, claim, snapshot, invoice, 0, notices, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	var current string
	if err := s.pool.QueryRow(ctx, `SELECT current_invoice_id FROM subscriptions WHERE client_id=$1`, clientID).Scan(&current); err != nil {
		t.Fatal(err)
	}
	var ledger, messages int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM invoice_lifecycle_invoices WHERE client_id=$1`, clientID).Scan(&ledger); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE bot_id=$1`, botID).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if current != "invoice-current" || ledger != 1 || messages != 0 {
		t.Fatalf("unverified predecessor advanced: current=%s ledger=%d messages=%d", current, ledger, messages)
	}
}
