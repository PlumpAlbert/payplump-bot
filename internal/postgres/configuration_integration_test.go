//go:build integration

package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/clientmenu"
	"github.com/PlumpAlbert/payplump-bot/internal/configdelivery"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const configActor int64 = 33000

// One owned schema and one completed first cycle serve the independent cases.
// Known receipts are inserted in one batch, then REAL FinishActivation proves
// the local active transition and exact persisted SUI binding.
func configFixture(t *testing.T) (*Store, context.Context, int64, configdelivery.Policy) {
	t.Helper()
	s, _ := activationTestStore(t)
	// Eleven database protocol cases share this audit fixture. The aggregate WAN
	// budget is independent of activation's per-operation test context; runtime
	// request/lease/transport permissions retain their original deadlines.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	t.Cleanup(cancel)
	ids := seedActivationAccounts(t, s, ctx, 123, int(configActor), 1)
	seedActivationBatch(t, s, ctx, 123, []int64{configActor}, 1)
	op, snapshot := claimOneActivation(t, s, ctx, 123)
	steps, payloads, external, amounts, instants := []string{}, []string{}, []string{}, []*string{}, []*string{}
	for _, step := range activation.OrderedSteps() {
		payload := activationPayload(t, snapshot, step)
		intent := activation.Intent{Payload: payload, ExternalID: "77", DispatchAttempt: op.Attempt}
		var amount, instant *string
		if step == activation.RecurringCreate || step == activation.RecurringStart {
			intent.ExternalID = "configuration-recurring"
			intent.RecurringAmount, intent.RecurringDatetime = "125.25", snapshot.Proof.NextInvoiceDate+"T12:00:00Z"
			amount, instant = &intent.RecurringAmount, &intent.RecurringDatetime
		}
		snapshot.Steps[step] = intent
		steps = append(steps, string(step))
		payloads = append(payloads, string(payload))
		external = append(external, intent.ExternalID)
		amounts = append(amounts, amount)
		instants = append(instants, instant)
	}
	configExec(t, s, ctx, `INSERT INTO activation_steps (operation_id,step,request,external_id,dispatch_attempt,recurring_amount,recurring_datetime,confirmed_at)
        SELECT $1,step,payload::jsonb,external_id,$2,amount::numeric,instant,clock_timestamp()
        FROM unnest($3::text[],$4::text[],$5::text[],$6::text[],$7::text[]) AS x(step,payload,external_id,amount,instant)`, op.ID, op.Attempt, steps, payloads, external, amounts, instants)
	if err := s.FinishActivation(ctx, op.Lease, snapshot, activationToday, nil); err != nil {
		t.Fatal(err)
	}
	return s, ctx, ids[0], configdelivery.Policy{Enabled: true, Revision: 5, Hash: sha256.Sum256([]byte("synthetic configuration policy"))}
}
func configExec(t *testing.T, s *Store, ctx context.Context, sql string, args ...any) {
	t.Helper()
	if _, err := s.pool.Exec(ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
}
func configAssert(t *testing.T, s *Store, ctx context.Context, sql string, args ...any) {
	t.Helper()
	var ok bool
	if err := s.pool.QueryRow(ctx, sql, args...).Scan(&ok); err != nil || !ok {
		t.Fatalf("configuration invariant failed: %v", err)
	}
}
func configPrepared(t *testing.T, s *Store, ctx context.Context, updateID, actor int64, p configdelivery.Policy) (telegram.Update, clientmenu.Prepared) {
	t.Helper()
	u := menuTestUpdate(updateID, actor, "/config")
	snapshot, duplicate, err := s.LoadMenuSnapshot(ctx, 123, updateID, actor)
	if err != nil || duplicate {
		t.Fatal("configuration menu snapshot unavailable")
	}
	return u, clientmenu.Prepared{Request: clientmenu.Request{BotID: 123, UpdateID: updateID, TelegramID: actor, ChatID: actor, Action: clientmenu.Configuration}, Snapshot: snapshot, Text: queue.ConfigurationIntentBody, ProtectedConfiguration: true, ConfigurationPolicyRevision: p.Revision, ConfigurationPolicyHash: p.Hash}
}
func configRequest(t *testing.T, s *Store, ctx context.Context, id int64, p configdelivery.Policy) queue.Message {
	t.Helper()
	u, prepared := configPrepared(t, s, ctx, id, configActor, p)
	if err := s.ApplyMenuUpdate(ctx, 123, menuTestUsername, u, prepared); err != nil {
		t.Fatal(err)
	}
	return configClaim(t, s, ctx)
}
func configClaim(t *testing.T, s *Store, ctx context.Context) queue.Message {
	t.Helper()
	messages, err := s.ClaimOutbox(ctx, 123, 50, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var found queue.Message
	for _, m := range messages {
		if m.ChatID == configActor {
			found = m
		} else if !m.ProtectedConfiguration {
			if err := s.CompleteOutbox(ctx, m.Lease, 9); err != nil {
				t.Fatal(err)
			}
		}
	}
	if found.ID == 0 {
		t.Fatal("configuration FIFO head was not claimable")
	}
	return found
}
func configView(t *testing.T, s *Store, ctx context.Context, m queue.Message) configdelivery.View {
	t.Helper()
	v, err := s.LoadConfiguration(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func configAuthorize(t *testing.T, s *Store, ctx context.Context, m queue.Message, kind configdelivery.Kind) configdelivery.Permit {
	t.Helper()
	v := configView(t, s, ctx, m)
	d := configdelivery.Decision{Kind: kind}
	if kind == configdelivery.Denial {
		d.Reason = configdelivery.DeniedUnavailable
	}
	permit, err := s.AuthorizeConfiguration(ctx, m, v, d)
	if err != nil {
		t.Fatal(err)
	}
	return permit
}
func configFinish(t *testing.T, s *Store, ctx context.Context, m queue.Message) {
	t.Helper()
	if err := s.CompleteOutbox(ctx, m.Lease, 17); err != nil {
		t.Fatal(err)
	}
}

// Force the session default OFF. Test-only triggers prove that policy changes
// and reservations override it before writing their durable intent.
func configAsyncStore(t *testing.T, s *Store, ctx context.Context) *Store {
	t.Helper()
	cfg := s.pool.Config()
	after := cfg.AfterConnect
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if after != nil {
			if err := after(ctx, conn); err != nil {
				return err
			}
		}
		_, err := conn.Exec(ctx, `SET synchronous_commit = off`)
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &Store{pool: pool}
}

func TestConfigurationProtectedDatabaseProtocol(t *testing.T) {
	s, ctx, client, p := configFixture(t)
	configExec(t, s, ctx, `CREATE FUNCTION config_test_durable() RETURNS trigger LANGUAGE plpgsql AS $$
        BEGIN IF current_setting('synchronous_commit') <> 'on' THEN RAISE EXCEPTION 'synchronous reservation required'; END IF; RETURN NEW; END; $$;
        CREATE TRIGGER config_test_durable BEFORE INSERT OR UPDATE ON configuration_policies FOR EACH ROW EXECUTE FUNCTION config_test_durable();
        CREATE TRIGGER config_test_durable BEFORE INSERT ON configuration_dispatches FOR EACH ROW EXECUTE FUNCTION config_test_durable();`)
	async := configAsyncStore(t, s, ctx)
	t.Run("monotonic policy installation and disable", func(t *testing.T) {
		s := async
		if err := s.InstallConfigurationPolicy(ctx, 123, configdelivery.Policy{}); err != nil {
			t.Fatal(err)
		}
		low := p
		low.Revision = 3
		for range 2 {
			if err := s.InstallConfigurationPolicy(ctx, 123, low); err != nil {
				t.Fatal(err)
			}
		}
		conflict := low
		conflict.Hash = sha256.Sum256([]byte("different synthetic policy"))
		if err := s.InstallConfigurationPolicy(ctx, 123, conflict); !errors.Is(err, configdelivery.ErrStalePolicy) {
			t.Fatal("equal revision overwrote policy")
		}
		older := low
		older.Revision = 2
		if err := s.InstallConfigurationPolicy(ctx, 123, older); !errors.Is(err, configdelivery.ErrStalePolicy) {
			t.Fatal("old process overwrote policy")
		}
		if err := s.InstallConfigurationPolicy(ctx, 123, configdelivery.Policy{}); err != nil {
			t.Fatal(err)
		}
		if err := s.InstallConfigurationPolicy(ctx, 123, low); !errors.Is(err, configdelivery.ErrStalePolicy) {
			t.Fatal("stale process re-enabled policy")
		}
		if err := s.InstallConfigurationPolicy(ctx, 123, configdelivery.Policy{}); err != nil {
			t.Fatal(err)
		}
		configAssert(t, s, ctx, `SELECT NOT enabled AND revision=4 AND hash=$1 FROM configuration_policies WHERE bot_id=123`, low.Hash[:])
		for _, bot := range []int64{123, 124} {
			if err := s.InstallConfigurationPolicy(ctx, bot, p); err != nil {
				t.Fatal(err)
			}
		}
		for range 2 {
			if err := s.DisableConfigurationPolicies(ctx); err != nil {
				t.Fatal(err)
			}
		}
		configAssert(t, s, ctx, `SELECT count(*)=2 AND bool_and(NOT enabled AND revision=6) FROM configuration_policies WHERE bot_id IN (123,124)`)
		if err := s.InstallConfigurationPolicy(ctx, 123, p); !errors.Is(err, configdelivery.ErrStalePolicy) {
			t.Fatal("Telegram-disabled startup left an old policy rearmable")
		}
		p.Revision = 7
		if err := s.InstallConfigurationPolicy(ctx, 123, p); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("actor scope and absent provenance", func(t *testing.T) {
		u, prepared := configPrepared(t, s, ctx, 1, configActor, p)
		forged := prepared
		forged.Request.TelegramID++
		if err := s.ApplyMenuUpdate(ctx, 123, menuTestUsername, u, forged); err == nil {
			t.Fatal("forged actor accepted")
		}
		if err := s.ApplyMenuUpdate(ctx, 124, menuTestUsername, u, prepared); err == nil {
			t.Fatal("foreign bot accepted preparation")
		}
		group := u
		message := *u.Message
		group.Message = &message
		group.ID = 2
		group.Message.Chat.Type = "group"
		if err := s.ApplyMenuUpdate(ctx, 123, menuTestUsername, group, prepared); err != nil {
			t.Fatal(err)
		}
		configAssert(t, s, ctx, `SELECT NOT EXISTS(SELECT 1 FROM configuration_requests) AND NOT EXISTS(SELECT 1 FROM outbox)
            AND NOT EXISTS(SELECT 1 FROM telegram_processed_updates WHERE update_id=1)`)
		registration, err := s.RegisterPrivateClient(ctx, TelegramIdentity{TelegramID: configActor + 1, ChatID: configActor + 1})
		if err != nil {
			t.Fatal(err)
		}
		configExec(t, s, ctx, `UPDATE clients SET sui_id='88' WHERE id=$1`, registration.Client.ID)
		configExec(t, s, ctx, `UPDATE subscriptions SET state='active',desired_access=true,recurring_id='manual-recurring' WHERE client_id=$1`, registration.Client.ID)
		u, prepared = configPrepared(t, s, ctx, 3, configActor+1, p)
		if err := s.ApplyMenuUpdate(ctx, 123, menuTestUsername, u, prepared); err != nil {
			t.Fatal(err)
		}
		configAssert(t, s, ctx, `SELECT NOT protected_configuration AND client_menu AND body=$1 FROM outbox WHERE chat_id=$2`, configurationDeniedText, configActor+1)
	})
	t.Run("configuration text cannot bypass the protected path", func(t *testing.T) {
		u, prepared := configPrepared(t, s, ctx, 4, configActor, p)
		prepared.ProtectedConfiguration = false
		prepared.Text = "untrusted prepared configuration text"
		if err := s.ApplyMenuUpdate(ctx, 123, menuTestUsername, u, prepared); err != nil {
			t.Fatal(err)
		}
		m := configClaim(t, s, ctx)
		if m.ProtectedConfiguration || m.Text != configurationDeniedText || !m.ClientMenu {
			t.Fatal("configuration was downgraded to arbitrary ordinary text")
		}
		configFinish(t, s, ctx, m)
		stale := p
		stale.Revision--
		m = configRequest(t, s, ctx, 5, stale)
		if m.ProtectedConfiguration || m.Text != configurationDeniedText {
			t.Fatal("stale prepared policy was not an ordinary denial")
		}
		configFinish(t, s, ctx, m)
	})
	t.Run("atomic dedup metadata immutable marker and equality", func(t *testing.T) {
		u, prepared := configPrepared(t, s, ctx, 10, configActor, p)
		_, err := s.EnqueueOutbox(ctx, queue.Draft{BotID: 123, ChatID: configActor, Key: prepared.Request.Key(), Text: "synthetic collision", ClientMenu: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.ApplyMenuUpdate(ctx, 123, menuTestUsername, u, prepared); !errors.Is(err, queue.ErrConflict) {
			t.Fatal("conflict did not roll back protected metadata")
		}
		configAssert(t, s, ctx, `SELECT NOT EXISTS(SELECT 1 FROM configuration_requests) AND NOT EXISTS(SELECT 1 FROM telegram_processed_updates WHERE update_id=10)`)
		configFinish(t, s, ctx, configClaim(t, s, ctx))
		m := configRequest(t, s, ctx, 11, p)
		if !m.ProtectedConfiguration || m.ClientMenu || m.Text != queue.ConfigurationIntentBody {
			t.Fatal("claim lost protected flag")
		}
		v := configView(t, s, ctx, m)
		for _, forged := range []queue.Message{
			{Lease: m.Lease, BotID: 124, ChatID: m.ChatID, Text: m.Text, ProtectedConfiguration: true},
			{Lease: m.Lease, BotID: m.BotID, ChatID: m.ChatID + 1, Text: m.Text, ProtectedConfiguration: true},
		} {
			if _, err := s.LoadConfiguration(ctx, forged); !errors.Is(err, queue.ErrLeaseLost) {
				t.Fatal("foreign bot or chat selected a protected request")
			}
		}
		if !v.Eligible || !v.Matches() {
			t.Fatal("real completed first cycle was not eligible")
		}
		if v.Request.ExpiresAt.Sub(v.Request.CreatedAt) != 5*time.Minute {
			t.Fatal("TTL not exactly five minutes")
		}
		configAssert(t, s, ctx, `SELECT count(*)=1 FROM configuration_requests WHERE outbox_id=$1`, m.ID)
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		id, err := insertOutbox(ctx, tx, queue.Draft{BotID: 123, ChatID: configActor, Key: "client-menu:123:11", Text: queue.ConfigurationIntentBody, ProtectedConfiguration: true})
		if err != nil || id != m.ID {
			rollback(tx)
			t.Fatal("same protected intent was not idempotent")
		}
		rollback(tx)
		tx, err = s.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := insertOutbox(ctx, tx, queue.Draft{BotID: 123, ChatID: configActor, Key: "client-menu:123:11", Text: queue.ConfigurationIntentBody}); !errors.Is(err, queue.ErrConflict) {
			rollback(tx)
			t.Fatal("protected flag omitted from conflict equality")
		}
		rollback(tx)
		if _, err := s.pool.Exec(ctx, `UPDATE outbox SET protected_configuration=false WHERE id=$1`, m.ID); err == nil {
			t.Fatal("protected flag downgraded")
		}
		if _, err := s.pool.Exec(ctx, `UPDATE configuration_requests SET actor_id=actor_id+1,chat_id=chat_id+1 WHERE outbox_id=$1`, m.ID); err == nil {
			t.Fatal("request was mutable")
		}
		tx, err = s.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = insertOutbox(ctx, tx, queue.Draft{BotID: 123, ChatID: configActor, Key: "orphan", Text: queue.ConfigurationIntentBody, ProtectedConfiguration: true}); err != nil {
			rollback(tx)
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err == nil {
			t.Fatal("orphan protected intent committed")
		}
		// Deduplication precedes even invalid changed preparation.
		if err := s.ApplyMenuUpdate(ctx, 123, menuTestUsername, menuTestUpdate(11, configActor, "/config"), clientmenu.Prepared{}); err != nil {
			t.Fatal("duplicate was revalidated")
		}
		if err := s.RetryOutbox(ctx, m.Lease, time.Second, 0); err == nil {
			t.Fatal("protected request blindly retried")
		}
		permit := configAuthorize(t, async, ctx, m, configdelivery.Secret)
		if permit.Kind != configdelivery.Secret || permit.NotAfter.After(v.Request.ExpiresAt) {
			t.Fatal("first send not safely bounded")
		}
		if _, err := s.AuthorizeConfiguration(ctx, m, v, configdelivery.Decision{Kind: configdelivery.Secret}); !errors.Is(err, configdelivery.ErrAlreadyDispatched) {
			t.Fatal("existing dispatch granted another send")
		}
		configAssert(t, s, ctx, `SELECT d.outcome='reserved' AND d.kind='secret' AND d.attempt=$2 AND g.outbox_id=d.outbox_id
            FROM configuration_dispatches d JOIN configuration_chat_guards g ON g.outbox_id=d.outbox_id WHERE d.outbox_id=$1`, m.ID, m.Attempt)
		configFinish(t, s, ctx, m)
		configAssert(t, s, ctx, `SELECT outbox_id IS NULL FROM configuration_chat_guards WHERE bot_id=123 AND chat_id=$1`, configActor)
	})
	t.Run("expired immutable request permits only nonsensitive denial", func(t *testing.T) {
		original := configRequest(t, s, ctx, 15, p)
		if err := s.FailConfigurationOutbox(ctx, original.Lease, 0, 0, false); err != nil {
			t.Fatal(err)
		}
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer rollback(tx)
		id, err := insertOutbox(ctx, tx, queue.Draft{BotID: 123, ChatID: configActor, Key: "synthetic-expired-request", Text: queue.ConfigurationIntentBody, ProtectedConfiguration: true})
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, `INSERT INTO configuration_requests (outbox_id,bot_id,client_id,actor_id,chat_id,activation_operation_id,
            invoice_id,proof_hash,client_revision,policy_revision,policy_hash,entitlement_hash,created_at,expires_at)
            SELECT $1,r.bot_id,r.client_id,r.actor_id,r.chat_id,r.activation_operation_id,r.invoice_id,r.proof_hash,r.client_revision,
            r.policy_revision,r.policy_hash,r.entitlement_hash,t,t+interval '5 minutes'
            FROM configuration_requests r CROSS JOIN (SELECT clock_timestamp()-interval '6 minutes' AS t) stamp WHERE r.outbox_id=$2`, id, original.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		m := configClaim(t, s, ctx)
		v := configView(t, s, ctx, m)
		if v.Request.Validate() != nil || v.Eligible || v.DenialCode != configdelivery.DeniedExpired {
			t.Fatal("expired request was not a valid denial target")
		}
		permit, err := s.AuthorizeConfiguration(ctx, m, v, configdelivery.Decision{Kind: configdelivery.Secret})
		if err != nil || permit.Kind != configdelivery.Denial || !permit.NotAfter.After(time.Now()) {
			t.Fatal("expired request granted secret or blocked denial")
		}
		configFinish(t, s, ctx, m)
	})
	t.Run("revision fences ABA but ignores polling and presentation", func(t *testing.T) {
		m := configRequest(t, s, ctx, 20, p)
		v := configView(t, s, ctx, m)
		configExec(t, s, ctx, `UPDATE payment_checks SET attempts=attempts+1,observed_generation=observed_generation+1,last_error_code=42 WHERE client_id=$1`, client)
		configExec(t, s, ctx, `UPDATE clients SET display_name='synthetic presentation' WHERE id=$1`, client)
		configAssert(t, s, ctx, `SELECT configuration_revision=$2 FROM clients WHERE id=$1`, client, v.Request.ClientRevision)
		configExec(t, s, ctx, `UPDATE payment_confirmations SET review_required=true WHERE client_id=$1`, client)
		configExec(t, s, ctx, `UPDATE payment_confirmations SET review_required=false WHERE client_id=$1`, client)
		configAssert(t, s, ctx, `SELECT configuration_revision=$2+2 FROM clients WHERE id=$1`, client, v.Request.ClientRevision)
		permit, err := s.AuthorizeConfiguration(ctx, m, v, configdelivery.Decision{Kind: configdelivery.Secret})
		if err != nil || permit.Kind != configdelivery.Denial {
			t.Fatal("ABA granted secret")
		}
		configFinish(t, s, ctx, m)
		// Exercise each remaining source without changing the eventual values.
		for _, mutation := range []struct {
			sql  string
			args []any
		}{
			{`UPDATE clients SET sui_id=CASE WHEN sui_id='77' THEN '78' ELSE '77' END WHERE id=$1`, []any{client}},
			{`UPDATE subscriptions SET desired_access=NOT desired_access WHERE client_id=$1`, []any{client}},
			{`UPDATE payment_checks SET review_code=CASE WHEN review_code=0 THEN 2007 ELSE 0 END WHERE client_id=$1`, []any{client}},
			{`UPDATE payment_checks SET bot_id=CASE WHEN bot_id=123 THEN 124 ELSE 123 END WHERE client_id=$1`, []any{client}},
			{`UPDATE activation_flows SET review_code=CASE WHEN review_code=0 THEN 3001 ELSE 0 END WHERE operation_id=(SELECT id FROM operations WHERE client_id=$1 AND kind='activate_first_subscription')`, []any{client}},
			{`UPDATE activation_steps SET dispatch_attempt=CASE WHEN dispatch_attempt=1 THEN 2 ELSE 1 END WHERE step='sui_create' AND operation_id=(SELECT id FROM operations WHERE client_id=$1 AND kind='activate_first_subscription')`, []any{client}},
			{`UPDATE activation_sui_bindings SET sui_id=CASE WHEN sui_id=77 THEN 78 ELSE 77 END WHERE client_id=$1`, []any{client}},
			{`UPDATE operations SET status=CASE WHEN status='completed' THEN 'pending' ELSE 'completed' END WHERE client_id=$1 AND kind='activate_first_subscription'`, []any{client}},
			{`UPDATE registration_flows SET review_code=CASE WHEN review_code=0 THEN 42 ELSE 0 END WHERE operation_id=(SELECT id FROM operations WHERE client_id=$1 AND kind='register_client')`, []any{client}},
			{`UPDATE operation_steps SET external_id=CASE WHEN external_id='synthetic-alternate' THEN 'customer-33000' ELSE 'synthetic-alternate' END WHERE step='customer_create' AND operation_id=(SELECT id FROM operations WHERE client_id=$1 AND kind='register_client')`, []any{client}},
		} {
			var before int64
			if err := s.pool.QueryRow(ctx, `SELECT configuration_revision FROM clients WHERE id=$1`, client).Scan(&before); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				configExec(t, s, ctx, mutation.sql, mutation.args...)
			}
			configAssert(t, s, ctx, `SELECT configuration_revision=$2+2 FROM clients WHERE id=$1`, client, before)
		}
	})
	t.Run("known rejection and rate limit are atomic", func(t *testing.T) {
		m := configRequest(t, s, ctx, 30, p)
		if configAuthorize(t, s, ctx, m, configdelivery.Secret).Kind != configdelivery.Secret {
			t.Fatal("secret denied")
		}
		if err := s.FailConfigurationOutbox(ctx, m.Lease, 429, time.Minute, true); err != nil {
			t.Fatal(err)
		}
		configAssert(t, s, ctx, `SELECT o.status='failed' AND d.outcome='rejected' AND g.outbox_id IS NULL AND b.outbox_not_before>clock_timestamp()+interval '50 seconds'
            FROM outbox o JOIN configuration_dispatches d ON d.outbox_id=o.id JOIN telegram_bots b ON b.bot_id=o.bot_id
            JOIN configuration_chat_guards g ON g.bot_id=o.bot_id AND g.chat_id=o.chat_id WHERE o.id=$1`, m.ID)
		claims, err := s.ClaimOutbox(ctx, 123, 1, time.Minute)
		if err != nil || len(claims) != 0 {
			t.Fatal("429 cooldown ignored")
		}
		configExec(t, s, ctx, `UPDATE telegram_bots SET outbox_not_before='-infinity' WHERE bot_id=123`)
	})
	t.Run("stale policy and late local period downgrade", func(t *testing.T) {
		m := configRequest(t, s, ctx, 40, p)
		v := configView(t, s, ctx, m)
		if err := s.InstallConfigurationPolicy(ctx, 123, configdelivery.Policy{}); err != nil {
			t.Fatal(err)
		}
		permit, err := s.AuthorizeConfiguration(ctx, m, v, configdelivery.Decision{Kind: configdelivery.Secret})
		if err != nil || permit.Kind != configdelivery.Denial {
			t.Fatal("disabled policy granted secret")
		}
		configFinish(t, s, ctx, m)
		p.Revision += 2
		if err := s.InstallConfigurationPolicy(ctx, 123, p); err != nil {
			t.Fatal(err)
		}
		m = configRequest(t, s, ctx, 41, p)
		v = configView(t, s, ctx, m)
		configExec(t, s, ctx, `UPDATE activation_steps SET recurring_datetime='2000-01-01T00:00:00Z' WHERE operation_id=$1 AND step IN ('recurring_create','recurring_start')`, v.Request.ActivationID)
		permit, err = s.AuthorizeConfiguration(ctx, m, v, configdelivery.Decision{Kind: configdelivery.Secret})
		if err != nil || permit.Kind != configdelivery.Denial {
			t.Fatal("changed original UTC period granted secret")
		}
		configFinish(t, s, ctx, m)
		configExec(t, s, ctx, `UPDATE activation_steps SET recurring_datetime='2039-02-28T12:00:00Z' WHERE operation_id=$1 AND step IN ('recurring_create','recurring_start')`, v.Request.ActivationID)
	})
	t.Run("lock waits recheck lease and FIFO", func(t *testing.T) {
		for i, mode := range []string{"lease", "fifo"} {
			// The first ordinary head can become unfinished while authorization
			// waits. No protected send may overtake it.
			head, err := s.EnqueueOutbox(ctx, queue.Draft{BotID: 123, ChatID: configActor, Key: fmt.Sprintf("head-%d", i), Text: "synthetic ordinary", ClientMenu: true})
			if err != nil {
				t.Fatal(err)
			}
			configFinish(t, s, ctx, configClaim(t, s, ctx))
			m := configRequest(t, s, ctx, int64(50+i), p)
			v := configView(t, s, ctx, m)
			configAtClientWait(t, s, ctx, func(bounded context.Context, tracing *Store) error {
				_, err := tracing.AuthorizeConfiguration(bounded, m, v, configdelivery.Decision{Kind: configdelivery.Secret})
				return err
			}, func() {
				if mode == "lease" {
					configExec(t, s, ctx, `UPDATE outbox SET locked_until=clock_timestamp()-interval '1 second' WHERE id=$1`, m.ID)
				} else {
					configExec(t, s, ctx, `UPDATE outbox SET status='pending',finished_at=NULL,telegram_message_id=NULL WHERE id=$1`, head)
				}
			})
			configAssert(t, s, ctx, `SELECT NOT EXISTS(SELECT 1 FROM configuration_dispatches WHERE outbox_id=$1)`, m.ID)
			if mode == "lease" {
				m = configClaim(t, s, ctx)
			}
			if err := s.FailConfigurationOutbox(ctx, m.Lease, 0, 0, false); err != nil {
				t.Fatal(err)
			}
			if mode == "fifo" {
				configFinish(t, s, ctx, configClaim(t, s, ctx))
			}
		}
	})
	t.Run("payment denial sticky only for authoritative original evidence", func(t *testing.T) {
		m := configRequest(t, s, ctx, 60, p)
		v := configView(t, s, ctx, m)
		permit, err := s.AuthorizeConfiguration(ctx, m, v, configdelivery.Decision{Kind: configdelivery.Denial, Reason: configdelivery.DeniedUnavailable})
		if err != nil || permit.Kind != configdelivery.Denial {
			t.Fatal("unavailable denial failed")
		}
		configFinish(t, s, ctx, m)
		configAssert(t, s, ctx, `SELECT NOT review_required AND review_code=0 FROM payment_confirmations WHERE client_id=$1`, client)
		m = configRequest(t, s, ctx, 61, p)
		v = configView(t, s, ctx, m)
		var other, oldRevision, newRevision int64
		if err := s.pool.QueryRow(ctx, `SELECT id,configuration_revision FROM clients WHERE telegram_id=$1`, configActor+1).Scan(&other, &newRevision); err != nil {
			t.Fatal(err)
		}
		if err := s.pool.QueryRow(ctx, `SELECT configuration_revision FROM clients WHERE id=$1`, client).Scan(&oldRevision); err != nil {
			t.Fatal(err)
		}
		configExec(t, s, ctx, `UPDATE payment_confirmations SET client_id=$2 WHERE invoice_id=$1`, v.Request.InvoiceID, other)
		permit, err = s.AuthorizeConfiguration(ctx, m, v, configdelivery.Decision{Kind: configdelivery.Denial, Reason: configdelivery.DeniedPayment, PaymentChanged: true})
		if err != nil || permit.Kind != configdelivery.Denial {
			t.Fatal("changed owner did not deny safely")
		}
		configFinish(t, s, ctx, m)
		configAssert(t, s, ctx, `SELECT NOT review_required AND review_code=0 FROM payment_confirmations WHERE invoice_id=$1`, v.Request.InvoiceID)
		configExec(t, s, ctx, `UPDATE payment_confirmations SET client_id=$2 WHERE invoice_id=$1`, v.Request.InvoiceID, client)
		configAssert(t, s, ctx, `SELECT configuration_revision=$2+2 FROM clients WHERE id=$1`, client, oldRevision)
		configAssert(t, s, ctx, `SELECT configuration_revision=$2+2 FROM clients WHERE id=$1`, other, newRevision)
		m = configRequest(t, s, ctx, 62, p)
		v = configView(t, s, ctx, m)
		permit, err = s.AuthorizeConfiguration(ctx, m, v, configdelivery.Decision{Kind: configdelivery.Denial, Reason: configdelivery.DeniedPayment, PaymentChanged: true})
		if err != nil || permit.Kind != configdelivery.Denial {
			t.Fatal("payment denial failed")
		}
		configFinish(t, s, ctx, m)
		configAssert(t, s, ctx, `SELECT pc.review_required AND pc.review_code=2007 AND p.review_code=2007
            FROM payment_confirmations pc JOIN payment_checks p ON p.client_id=pc.client_id WHERE pc.client_id=$1`, client)
		configExec(t, s, ctx, `UPDATE payment_confirmations SET review_required=false,review_code=0 WHERE client_id=$1`, client)
		configExec(t, s, ctx, `UPDATE payment_checks SET review_code=0 WHERE client_id=$1`, client)
	})
	t.Run("unknown sender pins chat forever but does not block ordinary menus", func(t *testing.T) {
		m := configRequest(t, s, ctx, 70, p)
		v := configView(t, s, ctx, m)
		if configAuthorize(t, s, ctx, m, configdelivery.Secret).Kind != configdelivery.Secret {
			t.Fatal("first secret denied")
		}
		configExec(t, s, ctx, `UPDATE outbox SET locked_until=clock_timestamp()-interval '1 second' WHERE id=$1`, m.ID)
		recovered := configClaim(t, s, ctx)
		if _, err := s.AuthorizeConfiguration(ctx, recovered, v, configdelivery.Decision{Kind: configdelivery.Secret}); !errors.Is(err, configdelivery.ErrAlreadyDispatched) {
			t.Fatal("new attempt rearmed original send")
		}
		if err := s.CompleteOutbox(ctx, recovered.Lease, 19); !errors.Is(err, queue.ErrLeaseLost) {
			t.Fatal("new attempt completed original sender")
		}
		if err := s.FailConfigurationOutbox(ctx, recovered.Lease, 400, 0, true); err != nil {
			t.Fatal(err)
		}
		configAssert(t, s, ctx, `SELECT d.outcome='unknown' AND g.outbox_id=d.outbox_id AND g.attempt=$2
            FROM configuration_dispatches d JOIN configuration_chat_guards g ON g.outbox_id=d.outbox_id WHERE d.outbox_id=$1`, m.ID, m.Attempt)
		if err := s.CompleteOutbox(ctx, m.Lease, 19); !errors.Is(err, queue.ErrLeaseLost) {
			t.Fatal("late original success cleared guard")
		}
		if err := s.InstallConfigurationPolicy(ctx, 123, configdelivery.Policy{}); err != nil {
			t.Fatal(err)
		}
		p.Revision += 2
		if err := s.InstallConfigurationPolicy(ctx, 123, p); err != nil {
			t.Fatal(err)
		}
		next := configRequest(t, s, ctx, 71, p)
		nextView := configView(t, s, ctx, next)
		if !nextView.ChatBlocked {
			t.Fatal("new request lost unknown guard")
		}
		if permit := configAuthorize(t, s, ctx, next, configdelivery.Secret); permit.Kind != configdelivery.Denial || permit.Reason != configdelivery.DeniedUnknownDelivery {
			t.Fatal("new explicit request rearmed unknown secret")
		}
		configFinish(t, s, ctx, next)
		if _, err := s.EnqueueOutbox(ctx, queue.Draft{BotID: 123, ChatID: configActor, Key: "ordinary-after-unknown", Text: "synthetic status", ClientMenu: true}); err != nil {
			t.Fatal(err)
		}
		ordinary := configClaim(t, s, ctx)
		if ordinary.ProtectedConfiguration || !ordinary.ClientMenu {
			t.Fatal("ordinary menu did not pass failed protected FIFO head")
		}
		configFinish(t, s, ctx, ordinary)
		configAssert(t, s, ctx, `SELECT outbox_id=$2 FROM configuration_chat_guards WHERE bot_id=123 AND chat_id=$1`, configActor, m.ID)
	})
}

// Pause at the actual client-lock statement without sleeps. The other session
// changes lease/FIFO before the authorizer obtains its local account snapshot.
type configClientGate struct {
	started, release chan struct{}
	once             sync.Once
}

func (g *configClientGate) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, "SELECT id FROM clients WHERE id=$1 FOR UPDATE") {
		g.once.Do(func() {
			close(g.started)
			select {
			case <-g.release:
			case <-ctx.Done():
			}
		})
	}
	return ctx
}
func (*configClientGate) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
func configAtClientWait(t *testing.T, s *Store, ctx context.Context, run func(context.Context, *Store) error, change func()) {
	t.Helper()
	gate := &configClientGate{started: make(chan struct{}), release: make(chan struct{})}
	cfg := s.pool.Config()
	cfg.MaxConns = 1
	cfg.ConnConfig.Tracer = gate
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(bounded, &Store{pool: pool}) }()
	select {
	case <-gate.started:
	case <-bounded.Done():
		close(gate.release)
		t.Fatal("authorizer never reached client wait")
	}
	change()
	close(gate.release)
	select {
	case err := <-done:
		if !errors.Is(err, queue.ErrLeaseLost) {
			t.Fatalf("late lease/FIFO change granted permit: %v", err)
		}
	case <-bounded.Done():
		t.Fatal("authorizer did not finish")
	}
}
