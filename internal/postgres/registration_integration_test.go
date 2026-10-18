//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/registration"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Seed real registration work in one round trip; SQL lease changes avoid sleeps.
func seedRegistrationWork(t *testing.T, s *Store, ctx context.Context, telegramID int64) queue.Operation {
	t.Helper()
	var operation queue.Operation
	err := s.pool.QueryRow(ctx, `
        WITH client AS (
            INSERT INTO clients (telegram_id, chat_id, display_name)
            VALUES ($1, $1, 'Registration test') RETURNING id
        ), subscription AS (
            INSERT INTO subscriptions (client_id) SELECT id FROM client RETURNING client_id
        )
        INSERT INTO operations (client_id, operation_key, kind, status, attempts, locked_until)
        SELECT client_id, 'register:' || client_id, 'register_client', 'running', 1,
            clock_timestamp() + interval '5 minutes' FROM subscription
        RETURNING id, attempts, client_id, kind`, telegramID).
		Scan(&operation.ID, &operation.Attempt, &operation.ClientID, &operation.Kind)
	if err != nil {
		t.Fatal(err)
	}
	return operation
}

func seedRegistrationFlow(t *testing.T, s *Store, ctx context.Context, operationID int64) {
	t.Helper()
	_, err := s.pool.Exec(ctx, `
        WITH bot AS (
            INSERT INTO telegram_bots (bot_id) VALUES (123)
            ON CONFLICT (bot_id) DO UPDATE SET bot_id = EXCLUDED.bot_id RETURNING bot_id
        )
        INSERT INTO registration_flows (operation_id, bot_id, token)
        SELECT $1::bigint, bot_id, 'integration-registration-token-' || ($1::bigint)::text FROM bot`, operationID)
	if err != nil {
		t.Fatal(err)
	}
}

func seedRegistrationConfirmations(t *testing.T, s *Store, ctx context.Context, operationID int64) {
	t.Helper()
	_, err := s.pool.Exec(ctx, `
        INSERT INTO operation_steps (operation_id, step, request, external_id, confirmed_at)
        SELECT $1, step, '{}'::jsonb, external_id, clock_timestamp()
        FROM (VALUES ('customer_create', 'customer'), ('invoice_create', 'invoice'),
            ('invoice_issue', 'invoice')) AS steps(step, external_id)`, operationID)
	if err != nil {
		t.Fatal(err)
	}
}

func assertRegistrationSQL(t *testing.T, s *Store, ctx context.Context, sql string, args ...any) {
	t.Helper()
	var ok bool
	if err := s.pool.QueryRow(ctx, sql, args...).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("registration state invariant failed")
	}
}

func TestRegistrationInputValidationNeedsNoDatabase(t *testing.T) {
	s := &Store{}
	ctx := context.Background()
	lease := queue.Lease{ID: 1, Attempt: 1}
	for _, payload := range []string{"", "null", "[]", `"object"`, "123", "{} {}", "{", "{\xff}", `{"large":"` + strings.Repeat("x", 64*1024) + `"}`} {
		if granted, err := s.PrepareRegistrationWrite(ctx, lease, registration.CustomerCreate, json.RawMessage(payload)); err == nil || granted {
			t.Fatal("accepted invalid registration request")
		}
	}
	if granted, err := s.PrepareRegistrationWrite(ctx, lease, "unknown", json.RawMessage(`{}`)); err == nil || granted {
		t.Fatal("accepted invalid registration step")
	}
	for _, id := range []string{"", "has space", " leading", "trailing ", "line\nbreak", "tab\t", "nul\x00", "unicode\u00a0space", "control\u0085", "\xff", strings.Repeat("x", 257)} {
		if err := s.ConfirmRegistrationStep(ctx, lease, registration.CustomerCreate, id); err == nil {
			t.Fatal("accepted invalid confirmation identity")
		}
		if err := s.FinishRegistration(ctx, lease, id, "invoice", nil); err == nil {
			t.Fatal("accepted invalid customer identity")
		}
		if err := s.FinishRegistration(ctx, lease, "customer", id, nil); err == nil {
			t.Fatal("accepted invalid invoice identity")
		}
	}
	if err := s.ConfirmRegistrationStep(ctx, lease, "unknown", "customer"); err == nil {
		t.Fatal("accepted invalid confirmation step")
	}
	if _, err := s.LoadRegistration(ctx, lease, 0); err == nil {
		t.Fatal("accepted invalid registration bot")
	}
	if err := s.HoldRegistration(ctx, lease, 0, nil); err == nil {
		t.Fatal("accepted empty review code")
	}
	tooMany := make([]queue.Draft, 101)
	if err := s.HoldRegistration(ctx, lease, registration.ReviewContract, tooMany); err == nil {
		t.Fatal("accepted too many review notifications")
	}
	if err := s.FinishRegistration(ctx, lease, "customer", "invoice", tooMany); err == nil {
		t.Fatal("accepted too many completion notifications")
	}
}

func TestRegistrationLoadKeepsTokenAndImportedAssociations(t *testing.T) {
	s, ctx := testStore(t)
	op := seedRegistrationWork(t, s, ctx, 42)
	if _, err := s.pool.Exec(ctx, `
        WITH client AS (
            UPDATE clients SET invoice_ninja_id = 'imported-customer', sui_id = 'imported-sui'
            WHERE id = $1 RETURNING id
        )
        UPDATE subscriptions SET first_invoice_id = 'imported-first', current_invoice_id = 'imported-current',
            recurring_id = 'imported-recurring', state = 'active', desired_access = true
        WHERE client_id IN (SELECT id FROM client)`, op.ClientID); err != nil {
		t.Fatal(err)
	}
	first, err := s.LoadRegistration(ctx, op.Lease, 123)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Token) < 20 || len(first.Token) > 128 || first.ClientID != op.ClientID || first.TelegramID != 42 || first.ChatID != 42 || first.BotID != 123 || first.DisplayName != "Registration test" ||
		first.ExistingCustomerID != "imported-customer" || first.ExistingSUIID != "imported-sui" || first.ExistingFirstInvoiceID != "imported-first" || first.ExistingCurrentInvoiceID != "imported-current" ||
		first.ExistingRecurringID != "imported-recurring" || first.SubscriptionState != "active" || !first.DesiredAccess || len(first.Steps) != 0 {
		t.Fatal("snapshot omitted existing account state or adopted it into the journal")
	}
	s.pool.Reset()
	restarted := &Store{pool: s.pool}
	second, err := restarted.LoadRegistration(ctx, op.Lease, 123)
	if err != nil || second.Token != first.Token || second.ExistingCustomerID != first.ExistingCustomerID {
		t.Fatalf("correlation changed after reconnect: %v", err)
	}
	if _, err := s.LoadRegistration(ctx, op.Lease, 456); !errors.Is(err, queue.ErrConflict) {
		t.Fatalf("flow accepted another bot: %v", err)
	}
	assertRegistrationSQL(t, s, ctx, `SELECT
        (SELECT count(*) = 1 FROM registration_flows) AND (SELECT count(*) = 0 FROM operation_steps)
        AND (SELECT count(*) = 0 FROM telegram_bots WHERE bot_id = 456)`)
}

func TestRegistrationIntentGrantsOnceAcrossLeaseAttempts(t *testing.T) {
	s, ctx := testStore(t)
	op := seedRegistrationWork(t, s, ctx, 42)
	seedRegistrationFlow(t, s, ctx, op.ID)
	payload := json.RawMessage(`{"b":2,"a":1}`)
	if granted, err := s.PrepareRegistrationWrite(ctx, op.Lease, registration.CustomerCreate, payload); err != nil || !granted {
		t.Fatalf("new intent not granted: granted=%v err=%v", granted, err)
	}
	if granted, err := s.PrepareRegistrationWrite(ctx, op.Lease, registration.CustomerCreate, json.RawMessage(`{ "a": 1.0, "b": 2 }`)); err != nil || granted {
		t.Fatalf("equivalent repeated JSON granted again: granted=%v err=%v", granted, err)
	}
	if granted, err := s.PrepareRegistrationWrite(ctx, op.Lease, registration.CustomerCreate, json.RawMessage(`{"a":9}`)); !errors.Is(err, queue.ErrConflict) || granted {
		t.Fatalf("changed intent accepted: granted=%v err=%v", granted, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE operations SET locked_until = clock_timestamp() - interval '1 second' WHERE id = $1`, op.ID); err != nil {
		t.Fatal(err)
	}
	if granted, err := s.PrepareRegistrationWrite(ctx, op.Lease, registration.CustomerCreate, payload); !errors.Is(err, queue.ErrLeaseLost) || granted {
		t.Fatalf("expired owner accepted: granted=%v err=%v", granted, err)
	}
	if err := s.pool.QueryRow(ctx, `UPDATE operations SET attempts = attempts + 1,
        locked_until = clock_timestamp() + interval '5 minutes' WHERE id = $1 RETURNING attempts`, op.ID).Scan(&op.Attempt); err != nil {
		t.Fatal(err)
	}
	if granted, err := s.PrepareRegistrationWrite(ctx, op.Lease, registration.CustomerCreate, payload); err != nil || granted {
		t.Fatalf("new owner rearmed unresolved intent: granted=%v err=%v", granted, err)
	}
	assertRegistrationSQL(t, s, ctx, `SELECT count(*) = 1 AND bool_and(request = $2::jsonb
        AND external_id IS NULL AND confirmed_at IS NULL) FROM operation_steps WHERE operation_id = $1`, op.ID, string(payload))
}

func TestRegistrationHoldBlocksFIFOAndResumeDoesNotRearm(t *testing.T) {
	s, ctx := testStore(t)
	op := seedRegistrationWork(t, s, ctx, 42)
	seedRegistrationFlow(t, s, ctx, op.ID)
	payload := json.RawMessage(`{"request":"uncertain"}`)
	if granted, err := s.PrepareRegistrationWrite(ctx, op.Lease, registration.CustomerCreate, payload); err != nil || !granted {
		t.Fatalf("prepare uncertain write: granted=%v err=%v", granted, err)
	}
	draft := queue.Draft{BotID: 456, ChatID: 99, Key: "review", Text: "Manual review required"}
	if err := s.HoldRegistration(ctx, op.Lease, registration.ReviewUnknownWrite, []queue.Draft{draft}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO operations (client_id, operation_key, kind)
        VALUES ($1, 'later-work', 'check_payment')`, op.ClientID); err != nil {
		t.Fatal(err)
	}
	assertRegistrationSQL(t, s, ctx, `SELECT o.status = 'pending' AND o.locked_until IS NULL
        AND o.next_attempt_at = 'infinity'::timestamptz AND o.completed_at IS NULL
        AND o.last_error_code = $2 AND f.review_code = $2
        AND (SELECT count(*) = 1 FROM outbox WHERE bot_id = 456 AND message_key = 'review')
        FROM operations o JOIN registration_flows f ON f.operation_id = o.id WHERE o.id = $1`, op.ID, registration.ReviewUnknownWrite)
	claimed, err := s.ClaimOperations(ctx, []string{"register_client", "check_payment"}, 10, time.Minute)
	if err != nil || len(claimed) != 0 {
		t.Fatalf("later work escaped held FIFO head: count=%d err=%v", len(claimed), err)
	}
	if err := s.ResumeRegistration(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	claimed, err = s.ClaimOperations(ctx, []string{"register_client", "check_payment"}, 10, time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].ID != op.ID || claimed[0].Attempt != 2 {
		t.Fatalf("resumed head was not reclaimed: count=%d err=%v", len(claimed), err)
	}
	if err := s.ResumeRegistration(ctx, op.ID); !errors.Is(err, queue.ErrConflict) {
		t.Fatalf("running registration resumed: %v", err)
	}
	if granted, err := s.PrepareRegistrationWrite(ctx, claimed[0].Lease, registration.CustomerCreate, payload); err != nil || granted {
		t.Fatalf("resume rearmed unknown write: granted=%v err=%v", granted, err)
	}
	assertRegistrationSQL(t, s, ctx, `SELECT f.review_code = 0 AND
        (SELECT count(*) = 1 AND bool_and(request = $2::jsonb AND external_id IS NULL
            AND confirmed_at IS NULL) FROM operation_steps WHERE operation_id = f.operation_id)
        FROM registration_flows f WHERE operation_id = $1`, op.ID, string(payload))
}

func TestRegistrationConfirmationRequiresIntentAndIsImmutable(t *testing.T) {
	s, ctx := testStore(t)
	op := seedRegistrationWork(t, s, ctx, 42)
	seedRegistrationFlow(t, s, ctx, op.ID)
	if err := s.ConfirmRegistrationStep(ctx, op.Lease, registration.CustomerCreate, "customer"); !errors.Is(err, queue.ErrConflict) {
		t.Fatalf("confirmation without intent: %v", err)
	}
	if granted, err := s.PrepareRegistrationWrite(ctx, op.Lease, registration.CustomerCreate, json.RawMessage(`{}`)); err != nil || !granted {
		t.Fatalf("prepare customer: granted=%v err=%v", granted, err)
	}
	if err := s.ConfirmRegistrationStep(ctx, op.Lease, registration.CustomerCreate, "customer"); err != nil {
		t.Fatal(err)
	}
	var confirmedAt time.Time
	if err := s.pool.QueryRow(ctx, `SELECT confirmed_at FROM operation_steps WHERE operation_id = $1`, op.ID).Scan(&confirmedAt); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmRegistrationStep(ctx, op.Lease, registration.CustomerCreate, "customer"); err != nil {
		t.Fatalf("same identity confirmation rejected: %v", err)
	}
	if err := s.ConfirmRegistrationStep(ctx, op.Lease, registration.CustomerCreate, "different"); !errors.Is(err, queue.ErrConflict) {
		t.Fatalf("identity was replaced: %v", err)
	}
	assertRegistrationSQL(t, s, ctx, `SELECT external_id = 'customer' AND confirmed_at = $2
        FROM operation_steps WHERE operation_id = $1 AND step = 'customer_create'`, op.ID, confirmedAt)
	snapshot, err := s.LoadRegistration(ctx, op.Lease, 123)
	if err != nil || snapshot.Steps[registration.CustomerCreate].ExternalID != "customer" || string(snapshot.Steps[registration.CustomerCreate].Payload) != "{}" {
		t.Fatalf("confirmed journal absent from snapshot: %v", err)
	}
}

func TestRegistrationEnforcesStepOrderAndIssuedInvoiceIdentity(t *testing.T) {
	s, ctx := testStore(t)
	op := seedRegistrationWork(t, s, ctx, 42)
	seedRegistrationFlow(t, s, ctx, op.ID)
	for _, step := range []registration.Step{registration.InvoiceCreate, registration.InvoiceIssue} {
		if granted, err := s.PrepareRegistrationWrite(ctx, op.Lease, step, json.RawMessage(`{}`)); !errors.Is(err, queue.ErrConflict) || granted {
			t.Fatalf("unordered step accepted: granted=%v err=%v", granted, err)
		}
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO operation_steps
        (operation_id, step, request, external_id, confirmed_at)
        VALUES ($1, 'customer_create', '{}', 'customer', clock_timestamp())`, op.ID); err != nil {
		t.Fatal(err)
	}
	if granted, err := s.PrepareRegistrationWrite(ctx, op.Lease, registration.InvoiceCreate, json.RawMessage(`{}`)); err != nil || !granted {
		t.Fatalf("ordered invoice intent rejected: granted=%v err=%v", granted, err)
	}
	if granted, err := s.PrepareRegistrationWrite(ctx, op.Lease, registration.InvoiceIssue, json.RawMessage(`{}`)); !errors.Is(err, queue.ErrConflict) || granted {
		t.Fatalf("unconfirmed invoice allowed issue: granted=%v err=%v", granted, err)
	}
	if err := s.ConfirmRegistrationStep(ctx, op.Lease, registration.InvoiceCreate, "invoice"); err != nil {
		t.Fatal(err)
	}
	if granted, err := s.PrepareRegistrationWrite(ctx, op.Lease, registration.InvoiceIssue, json.RawMessage(`{}`)); err != nil || !granted {
		t.Fatalf("ordered issue intent rejected: granted=%v err=%v", granted, err)
	}
	if err := s.ConfirmRegistrationStep(ctx, op.Lease, registration.InvoiceIssue, "another-invoice"); !errors.Is(err, queue.ErrConflict) {
		t.Fatalf("issued a different invoice: %v", err)
	}
	if err := s.ConfirmRegistrationStep(ctx, op.Lease, registration.InvoiceIssue, "invoice"); err != nil {
		t.Fatal(err)
	}
}

func registrationOwnedCalls(s *Store, ctx context.Context, lease queue.Lease) []func() error {
	return []func() error{
		func() error { _, err := s.LoadRegistration(ctx, lease, 123); return err },
		func() error {
			granted, err := s.PrepareRegistrationWrite(ctx, lease, registration.CustomerCreate, json.RawMessage(`{}`))
			if granted {
				return errors.New("unexpected write grant")
			}
			return err
		},
		func() error { return s.ConfirmRegistrationStep(ctx, lease, registration.CustomerCreate, "customer") },
		func() error { return s.HoldRegistration(ctx, lease, registration.ReviewContract, nil) },
		func() error { return s.FinishRegistration(ctx, lease, "customer", "invoice", nil) },
	}
}

func TestRegistrationFencesEveryMethodIncludingRepeatedSuccess(t *testing.T) {
	for _, state := range []string{"expired", "superseded", "wrong-kind"} {
		t.Run(state, func(t *testing.T) {
			s, ctx := testStore(t)
			op := seedRegistrationWork(t, s, ctx, 42)
			seedRegistrationFlow(t, s, ctx, op.ID)
			seedRegistrationConfirmations(t, s, ctx, op.ID)
			if _, err := s.pool.Exec(ctx, `UPDATE operations SET
                locked_until = CASE WHEN $2 = 'expired' THEN clock_timestamp() - interval '1 second' ELSE locked_until END,
                attempts = attempts + CASE WHEN $2 = 'superseded' THEN 1 ELSE 0 END,
                kind = CASE WHEN $2 = 'wrong-kind' THEN 'check_payment' ELSE kind END WHERE id = $1`, op.ID, state); err != nil {
				t.Fatal(err)
			}
			for i, call := range registrationOwnedCalls(s, ctx, op.Lease) {
				if err := call(); !errors.Is(err, queue.ErrLeaseLost) {
					t.Fatalf("method %d accepted invalid ownership: %v", i, err)
				}
			}
			assertRegistrationSQL(t, s, ctx, `SELECT (SELECT count(*) = 0 FROM outbox)
                AND (SELECT count(*) = 3 AND bool_and(confirmed_at IS NOT NULL) FROM operation_steps)
                AND (SELECT review_code = 0 FROM registration_flows WHERE operation_id = $1)
                AND (SELECT invoice_ninja_id IS NULL FROM clients WHERE id = $2)`, op.ID, op.ClientID)
		})
	}
}

func TestRegistrationCannotMutateAnUninitializedFlow(t *testing.T) {
	s, ctx := testStore(t)
	op := seedRegistrationWork(t, s, ctx, 42)
	for _, call := range registrationOwnedCalls(s, ctx, op.Lease)[1:] {
		if err := call(); !errors.Is(err, queue.ErrConflict) {
			t.Fatalf("uninitialized flow accepted mutation: %v", err)
		}
	}
	assertRegistrationSQL(t, s, ctx, `SELECT (SELECT count(*) = 0 FROM operation_steps)
        AND (SELECT count(*) = 0 FROM registration_flows) AND (SELECT count(*) = 0 FROM outbox)`)
}

func TestRegistrationFinishRequiresEveryConfirmationAndPreservesNoAccess(t *testing.T) {
	s, ctx := testStore(t)
	op := seedRegistrationWork(t, s, ctx, 42)
	seedRegistrationFlow(t, s, ctx, op.ID)
	seedRegistrationConfirmations(t, s, ctx, op.ID)
	if _, err := s.pool.Exec(ctx, `UPDATE operation_steps SET external_id = NULL, confirmed_at = NULL
        WHERE operation_id = $1 AND step = 'invoice_issue'`, op.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRegistration(ctx, op.Lease, "customer", "invoice", nil); !errors.Is(err, queue.ErrConflict) {
		t.Fatalf("finish accepted unconfirmed issue: %v", err)
	}
	if err := s.ConfirmRegistrationStep(ctx, op.Lease, registration.InvoiceIssue, "invoice"); err != nil {
		t.Fatal(err)
	}
	for _, ids := range [][2]string{{"different-customer", "invoice"}, {"customer", "different-invoice"}} {
		if err := s.FinishRegistration(ctx, op.Lease, ids[0], ids[1], nil); !errors.Is(err, queue.ErrConflict) {
			t.Fatalf("finish ignored confirmed identities: %v", err)
		}
	}
	drafts := []queue.Draft{
		{BotID: 123, ChatID: 42, Key: "first-invoice", Text: "Invoice ready"},
		{BotID: 456, ChatID: 99, Key: "registration-review", Text: "Registration finished"},
	}
	if err := s.FinishRegistration(ctx, op.Lease, "customer", "invoice", drafts); err != nil {
		t.Fatal(err)
	}
	assertRegistrationSQL(t, s, ctx, `SELECT c.invoice_ninja_id = 'customer' AND c.sui_id IS NULL
        AND s.first_invoice_id = 'invoice' AND s.current_invoice_id = 'invoice'
        AND s.state = 'awaiting_payment' AND NOT s.desired_access AND s.recurring_id IS NULL
        AND s.payment_date IS NULL AND s.next_invoice_date IS NULL
        AND o.status = 'completed' AND o.completed_at IS NOT NULL AND o.locked_until IS NULL
        AND o.last_error_code = 0 AND o.attempts = $2
        AND (SELECT count(*) = 2 AND bool_and(status = 'pending' AND attempts = 0) FROM outbox)
        FROM operations o JOIN clients c ON c.id = o.client_id
        JOIN subscriptions s ON s.client_id = c.id WHERE o.id = $1`, op.ID, op.Attempt)
	if err := s.FinishRegistration(ctx, op.Lease, "customer", "invoice", drafts); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("completed registration accepted another finish: %v", err)
	}
	if err := s.ResumeRegistration(ctx, op.ID); !errors.Is(err, queue.ErrConflict) {
		t.Fatalf("completed registration resumed: %v", err)
	}
}

func TestRegistrationFinishAndNotificationsRollbackTogether(t *testing.T) {
	s, ctx := testStore(t)
	op := seedRegistrationWork(t, s, ctx, 42)
	seedRegistrationFlow(t, s, ctx, op.ID)
	seedRegistrationConfirmations(t, s, ctx, op.ID)
	if _, err := s.pool.Exec(ctx, `
        INSERT INTO outbox (bot_id, chat_id, message_key, body) VALUES (123, 42, 'existing', 'unchanged');
        ALTER TABLE outbox ADD CONSTRAINT reject_registration_notification CHECK (message_key <> 'reject-secret')`); err != nil {
		t.Fatal(err)
	}
	first := queue.Draft{BotID: 456, ChatID: 42, Key: "first", Text: "Private notification"}
	for _, failing := range []queue.Draft{
		{BotID: 123, ChatID: 42, Key: "existing", Text: "changed"},
		{BotID: 456, ChatID: 42, Key: "reject-secret", Text: "secret notification"},
	} {
		err := s.FinishRegistration(ctx, op.Lease, "customer", "invoice", []queue.Draft{first, failing})
		if err == nil || (failing.Key == "existing" && !errors.Is(err, queue.ErrConflict)) {
			t.Fatalf("finish ignored notification failure: %v", err)
		}
		if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "Private notification") {
			t.Fatal("database error exposed notification data")
		}
		assertRegistrationSQL(t, s, ctx, `SELECT o.status = 'running' AND o.completed_at IS NULL
            AND o.locked_until > clock_timestamp() AND c.invoice_ninja_id IS NULL
            AND s.first_invoice_id IS NULL AND s.current_invoice_id IS NULL AND NOT s.desired_access
            AND (SELECT count(*) = 1 AND bool_and(body = 'unchanged') FROM outbox)
            AND NOT EXISTS (SELECT 1 FROM telegram_bots WHERE bot_id = 456)
            FROM operations o JOIN clients c ON c.id = o.client_id
            JOIN subscriptions s ON s.client_id = c.id WHERE o.id = $1`, op.ID)
	}
}

func TestRegistrationHoldAndReviewNotificationsRollbackTogether(t *testing.T) {
	s, ctx := testStore(t)
	op := seedRegistrationWork(t, s, ctx, 42)
	seedRegistrationFlow(t, s, ctx, op.ID)
	if _, err := s.pool.Exec(ctx, `ALTER TABLE outbox ADD CONSTRAINT reject_registration_review CHECK (false)`); err != nil {
		t.Fatal(err)
	}
	draft := queue.Draft{BotID: 456, ChatID: 99, Key: "review-secret", Text: "secret review text"}
	err := s.HoldRegistration(ctx, op.Lease, registration.ReviewUnknownWrite, []queue.Draft{draft})
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("hold ignored failure or exposed private data: %v", err)
	}
	assertRegistrationSQL(t, s, ctx, `SELECT o.status = 'running' AND o.locked_until > clock_timestamp()
        AND o.last_error_code = 0 AND f.review_code = 0 AND o.completed_at IS NULL
        AND (SELECT count(*) = 0 FROM outbox)
        AND NOT EXISTS (SELECT 1 FROM telegram_bots WHERE bot_id = 456)
        FROM operations o JOIN registration_flows f ON f.operation_id = o.id WHERE o.id = $1`, op.ID)
}

func TestRegistrationFinishProtectsExistingAssociations(t *testing.T) {
	for _, change := range []struct {
		name string
		sql  string
	}{
		{"paid state", `UPDATE subscriptions SET state = 'active' WHERE client_id = $1`},
		{"access", `UPDATE subscriptions SET desired_access = true WHERE client_id = $1`},
		{"recurring", `UPDATE subscriptions SET recurring_id = 'imported-recurring' WHERE client_id = $1`},
		{"SUI", `UPDATE clients SET sui_id = 'imported-sui' WHERE id = $1`},
		{"customer", `UPDATE clients SET invoice_ninja_id = 'imported-customer' WHERE id = $1`},
		{"first invoice", `UPDATE subscriptions SET first_invoice_id = 'imported-first' WHERE client_id = $1`},
		{"current invoice", `UPDATE subscriptions SET current_invoice_id = 'imported-current' WHERE client_id = $1`},
	} {
		t.Run(change.name, func(t *testing.T) {
			s, ctx := testStore(t)
			op := seedRegistrationWork(t, s, ctx, 42)
			seedRegistrationFlow(t, s, ctx, op.ID)
			seedRegistrationConfirmations(t, s, ctx, op.ID)
			if _, err := s.pool.Exec(ctx, change.sql, op.ClientID); err != nil {
				t.Fatal(err)
			}
			var before string
			if err := s.pool.QueryRow(ctx, `SELECT jsonb_build_array(to_jsonb(c), to_jsonb(s))::text
                FROM clients c JOIN subscriptions s ON s.client_id = c.id WHERE c.id = $1`, op.ClientID).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if err := s.FinishRegistration(ctx, op.Lease, "customer", "invoice", nil); !errors.Is(err, queue.ErrConflict) {
				t.Fatalf("finish overwrote existing association: %v", err)
			}
			assertRegistrationSQL(t, s, ctx, `SELECT jsonb_build_array(to_jsonb(c), to_jsonb(s))::text = $2
                AND o.status = 'running' AND o.completed_at IS NULL
                FROM clients c JOIN subscriptions s ON s.client_id = c.id
                JOIN operations o ON o.client_id = c.id WHERE c.id = $1`, op.ClientID, before)
		})
	}
}

func TestRegistrationFinishAllowsSameIDsButRejectsAnotherOwner(t *testing.T) {
	for _, sameOwner := range []bool{false, true} {
		name := "another owner"
		if sameOwner {
			name = "same owner"
		}
		t.Run(name, func(t *testing.T) {
			s, ctx := testStore(t)
			op := seedRegistrationWork(t, s, ctx, 42)
			seedRegistrationFlow(t, s, ctx, op.ID)
			seedRegistrationConfirmations(t, s, ctx, op.ID)
			ownerID := op.ClientID
			if !sameOwner {
				ownerID = seedRegistrationWork(t, s, ctx, 99).ClientID
			}
			if _, err := s.pool.Exec(ctx, `WITH client AS (
                UPDATE clients SET invoice_ninja_id = 'customer' WHERE id = $1 RETURNING id
            ) UPDATE subscriptions SET first_invoice_id = 'invoice', current_invoice_id = 'invoice'
                WHERE client_id IN (SELECT id FROM client)`, ownerID); err != nil {
				t.Fatal(err)
			}
			err := s.FinishRegistration(ctx, op.Lease, "customer", "invoice", nil)
			if sameOwner && err != nil {
				t.Fatal(err)
			}
			if !sameOwner && !errors.Is(err, queue.ErrConflict) {
				t.Fatalf("adopted another client's identities: %v", err)
			}
		})
	}
}

func TestRegistrationPreparesBotsBeforeLockingClient(t *testing.T) {
	for _, method := range []string{"load", "hold", "finish"} {
		t.Run(method, func(t *testing.T) {
			s, ctx := testStore(t)
			op := seedRegistrationWork(t, s, ctx, 42)
			if method != "load" {
				seedRegistrationFlow(t, s, ctx, op.ID)
				if method == "finish" {
					seedRegistrationConfirmations(t, s, ctx, op.ID)
				}
			}
			trace := &botInsertTrace{started: make(chan struct{})}
			cfg := s.pool.Config()
			cfg.MaxConns = 1
			cfg.ConnConfig.Tracer = trace
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			worker := &Store{pool: pool}
			webhook, err := s.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer rollback(webhook)
			if _, err := webhook.Exec(ctx, `INSERT INTO telegram_bots (bot_id) VALUES (456)`); err != nil {
				t.Fatal(err)
			}
			workCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				drafts := []queue.Draft{{BotID: 456, ChatID: 42, Key: "notification", Text: "Notification"}}
				switch method {
				case "load":
					_, err := worker.LoadRegistration(workCtx, op.Lease, 456)
					done <- err
				case "hold":
					done <- worker.HoldRegistration(workCtx, op.Lease, registration.ReviewContract, drafts)
				case "finish":
					done <- worker.FinishRegistration(workCtx, op.Lease, "customer", "invoice", drafts)
				}
			}()
			select {
			case <-trace.started:
			case <-workCtx.Done():
				t.Fatal("registration never reached bot preparation")
			}
			if _, err := webhook.Exec(ctx, `SELECT id FROM clients WHERE id = $1 FOR NO KEY UPDATE NOWAIT`, op.ClientID); err != nil {
				t.Fatalf("registration locked client before preparing bot: %v", err)
			}
			if err := webhook.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-workCtx.Done():
				t.Fatal("registration did not resume after webhook committed")
			}
		})
	}
}

func TestRegistrationErrorsDoNotExposePostgresDetails(t *testing.T) {
	cause := &pgconn.PgError{Code: "23514", Message: "secret request", Detail: "secret customer and notification", Where: "secret SQL"}
	err := registrationStageError("save registration intent", cause)
	if err.Error() != "save registration intent" || errors.Is(err, cause) {
		t.Fatal("registration exposed raw database error")
	}
	cause.Code = "23505"
	if err := registrationStageError("associate registration customer", cause); !errors.Is(err, queue.ErrConflict) || strings.Contains(err.Error(), "secret") {
		t.Fatal("unique association conflict was not safely classified")
	}
}
