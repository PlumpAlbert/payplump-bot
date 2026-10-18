//go:build integration

package postgres

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/clientmenu"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const menuTestBot int64 = 123
const menuTestUsername = "PayplumpBot"

func menuTestUpdate(id, actor int64, command string) telegram.Update {
	return telegram.Update{ID: id, Message: &telegram.Message{
		From: &telegram.User{ID: actor, FirstName: "Клиент"},
		Chat: telegram.Chat{ID: actor, Type: "private"}, Text: command,
		Entities: []telegram.Entity{{Type: "bot_command", Offset: 0, Length: len(command)}},
	}}
}

func prepareMenuTest(t *testing.T, s *Store, ctx context.Context, update telegram.Update, text string) clientmenu.Prepared {
	t.Helper()
	identity, action, supported := update.PrivateMenu(menuTestUsername)
	if !supported {
		t.Fatal("test menu update was not recognized")
	}
	snapshot, duplicate, err := s.LoadMenuSnapshot(ctx, menuTestBot, update.ID, identity.TelegramID)
	if err != nil || duplicate {
		t.Fatalf("load menu preparation: duplicate=%v, error=%v", duplicate, err)
	}
	return clientmenu.Prepared{Request: clientmenu.Request{
		BotID: menuTestBot, UpdateID: update.ID, TelegramID: identity.TelegramID, ChatID: identity.ChatID, Action: action,
	}, Snapshot: snapshot, Text: text}
}

func applyMenuTest(t *testing.T, s *Store, ctx context.Context, update telegram.Update, prepared clientmenu.Prepared) {
	t.Helper()
	if err := s.ApplyMenuUpdate(ctx, menuTestBot, menuTestUsername, update, prepared); err != nil {
		t.Fatal(err)
	}
}

// Keep related cases in one disposable schema rather than making every small
// assertion pay for another remote migration/setup round trip.
func TestMenuAtomicRegistrationDedupAndScope(t *testing.T) {
	s, ctx := testStore(t)
	start := menuTestUpdate(0, 7001, "/start")
	prepared := prepareMenuTest(t, s, ctx, start, "Добро пожаловать")
	if prepared.Snapshot != (clientmenu.Snapshot{}) {
		t.Fatal("unknown sender did not get a zero snapshot")
	}
	applyMenuTest(t, s, ctx, start, prepared)
	var atomicStart bool
	if err := s.pool.QueryRow(ctx, `SELECT
        (SELECT count(*) = 1 FROM clients WHERE telegram_id = 7001 AND chat_id = 7001 AND display_name = 'Клиент')
        AND (SELECT count(*) = 1 FROM subscriptions WHERE state = 'awaiting_payment' AND NOT desired_access)
        AND (SELECT count(*) = 1 FROM operations WHERE kind = 'register_client' AND status = 'pending')
        AND (SELECT count(*) = 1 FROM outbox WHERE bot_id = 123 AND chat_id = 7001
            AND message_key = 'client-menu:123:0' AND body = 'Добро пожаловать' AND client_menu)
        AND (SELECT count(*) = 1 FROM telegram_processed_updates WHERE bot_id = 123 AND update_id = 0)`,
	).Scan(&atomicStart); err != nil || !atomicStart {
		t.Fatalf("new menu start did not commit all local records: %v", err)
	}

	// New /start updates a profile and replies, but never queues registration twice.
	start.ID = 1
	start.Message.From.FirstName = "Новое имя"
	prepared = prepareMenuTest(t, s, ctx, start, "Главное меню")
	if !prepared.Snapshot.Known || prepared.Snapshot.PaymentDate != "" || prepared.Snapshot.NextInvoiceDate != "" {
		t.Fatal("known unpaid snapshot or nullable dates were incorrect")
	}
	applyMenuTest(t, s, ctx, start, prepared)
	var repeated bool
	if err := s.pool.QueryRow(ctx, `SELECT
        (SELECT count(*) = 1 FROM operations WHERE kind = 'register_client')
        AND (SELECT count(*) = 1 FROM clients WHERE telegram_id = 7001 AND display_name = 'Новое имя')
        AND (SELECT count(*) = 2 FROM outbox) AND (SELECT count(*) = 2 FROM telegram_processed_updates)`,
	).Scan(&repeated); err != nil || !repeated {
		t.Fatalf("existing start repeated registration or lost profile/reply: %v", err)
	}

	// Even a changed actor/body and an invalid proof/text cannot replay a committed update.
	replay := menuTestUpdate(start.ID, 9999, "/invoice")
	applyMenuTest(t, s, ctx, replay, clientmenu.Prepared{Text: "\x00"})
	snapshot, duplicate, err := s.LoadMenuSnapshot(ctx, menuTestBot, replay.ID, 9999)
	if err != nil || !duplicate || snapshot != (clientmenu.Snapshot{}) {
		t.Fatalf("committed redelivery was not short-circuited: %v", err)
	}

	// Help is available to an unknown sender without creating a client or a job.
	help := menuTestUpdate(2, 7002, "/help")
	applyMenuTest(t, s, ctx, help, prepareMenuTest(t, s, ctx, help, "Инструкция"))
	// Unsupported raw updates are deduplicated without consulting even a forged proof.
	group := menuTestUpdate(3, 7003, "/start")
	group.Message.Chat = telegram.Chat{ID: -100, Type: "group"}
	for _, update := range []telegram.Update{group, {ID: 4}, menuTestUpdate(5, 7003, "/unrecognized")} {
		applyMenuTest(t, s, ctx, update, prepared)
	}

	// Raw sender, not prepared proof, owns the account selector. Every request
	// field is checked; all failures must roll back their deduplication insert.
	invoice := menuTestUpdate(6, 7001, "/invoice")
	proof := prepareMenuTest(t, s, ctx, invoice, "Счёт")
	forged := menuTestUpdate(6, 7002, "/invoice")
	if err := s.ApplyMenuUpdate(ctx, menuTestBot, menuTestUsername, forged, proof); err == nil {
		t.Fatal("prepared actor overrode the raw sender")
	}
	for _, change := range []func(*clientmenu.Request){
		func(r *clientmenu.Request) { r.BotID++ },
		func(r *clientmenu.Request) { r.UpdateID++ },
		func(r *clientmenu.Request) { r.TelegramID++ },
		func(r *clientmenu.Request) { r.ChatID++ },
		func(r *clientmenu.Request) { r.Action = clientmenu.Start },
	} {
		bad := proof
		change(&bad.Request)
		if err := s.ApplyMenuUpdate(ctx, menuTestBot, menuTestUsername, invoice, bad); err == nil {
			t.Fatal("forged menu proof was accepted")
		}
	}
	var scoped bool
	if err := s.pool.QueryRow(ctx, `SELECT
        (SELECT count(*) = 1 FROM clients) AND (SELECT count(*) = 1 FROM operations)
        AND (SELECT count(*) = 3 FROM outbox)
        AND (SELECT count(*) = 1 FROM outbox WHERE chat_id = 7002 AND client_menu)
        AND (SELECT count(*) = 6 FROM telegram_processed_updates)
        AND NOT EXISTS (SELECT 1 FROM telegram_processed_updates WHERE update_id = 6)`,
	).Scan(&scoped); err != nil || !scoped {
		t.Fatalf("unsupported/forged/help update escaped actor scope or deduplication: %v", err)
	}
}

func TestMenuSnapshotRevalidationRollbackAndHeldFIFO(t *testing.T) {
	s, ctx := testStore(t)
	registered, err := s.RegisterPrivateClient(ctx, TelegramIdentity{TelegramID: 7101, ChatID: 7101, DisplayName: "Исходное имя"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE clients SET invoice_ninja_id = 'customer-old', sui_id = 'sui-old' WHERE id = $1`, registered.Client.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE subscriptions SET current_invoice_id = 'invoice-old', state = 'active',
        desired_access = true, payment_date = '2026-04-01', next_invoice_date = '2026-05-01' WHERE client_id = $1`, registered.Client.ID); err != nil {
		t.Fatal(err)
	}
	invoice := menuTestUpdate(10, 7101, "/invoice")
	prepared := prepareMenuTest(t, s, ctx, invoice, "Старый счёт")
	want := clientmenu.Snapshot{Known: true, ClientID: registered.Client.ID, CustomerID: "customer-old", InvoiceID: "invoice-old",
		SUIID: "sui-old", State: "active", DesiredAccess: true, PaymentDate: "2026-04-01", NextInvoiceDate: "2026-05-01"}
	if prepared.Snapshot != want {
		t.Fatal("menu snapshot did not project every account field")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE subscriptions SET current_invoice_id = 'invoice-new' WHERE client_id = $1`, registered.Client.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyMenuUpdate(ctx, menuTestBot, menuTestUsername, invoice, prepared); !errors.Is(err, clientmenu.ErrStaleSnapshot) {
		t.Fatalf("invoice rebind did not reject stale reply: %v", err)
	}
	var clean bool
	if err := s.pool.QueryRow(ctx, `SELECT NOT EXISTS (SELECT 1 FROM telegram_processed_updates)
        AND NOT EXISTS (SELECT 1 FROM outbox)`).Scan(&clean); err != nil || !clean {
		t.Fatalf("stale preparation did not roll back delivery metadata/reply: %v", err)
	}
	fresh := prepareMenuTest(t, s, ctx, invoice, "Новый счёт")
	if fresh.Snapshot.InvoiceID != "invoice-new" {
		t.Fatal("retry did not reload current invoice binding")
	}
	applyMenuTest(t, s, ctx, invoice, fresh)

	// A held registration head (and even its locked operation row) must not
	// block replies. Menu decisions do not inspect review or operation facts.
	if _, err := s.pool.Exec(ctx, `UPDATE operations SET next_attempt_at = 'infinity' WHERE client_id = $1`, registered.Client.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO registration_flows (operation_id, bot_id, token, review_code)
        SELECT id, 123, 'menu-held-registration-token', 42 FROM operations WHERE client_id = $1`, registered.Client.ID); err != nil {
		t.Fatal(err)
	}
	held, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(held)
	if _, err := held.Exec(ctx, `SELECT id FROM operations WHERE client_id = $1 FOR UPDATE`, registered.Client.ID); err != nil {
		t.Fatal(err)
	}
	status := menuTestUpdate(11, 7101, "/status")
	prepared = prepareMenuTest(t, s, ctx, status, "Состояние подписки")
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	err = s.ApplyMenuUpdate(bounded, menuTestBot, menuTestUsername, status, prepared)
	cancel()
	if err != nil {
		t.Fatalf("held registration blocked client menu: %v", err)
	}
	rollback(held)

	// An outbox conflict must undo the event, profile, subscription and job.
	start := menuTestUpdate(12, 7102, "/start")
	prepared = prepareMenuTest(t, s, ctx, start, "Новый клиент")
	if _, err := s.EnqueueOutbox(ctx, queue.Draft{BotID: menuTestBot, ChatID: 7102, Key: prepared.Request.Key(), Text: "Другая запись", ClientMenu: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyMenuUpdate(ctx, menuTestBot, menuTestUsername, start, prepared); !errors.Is(err, queue.ErrConflict) {
		t.Fatalf("outbox conflict was not preserved: %v", err)
	}
	var rolledBack bool
	if err := s.pool.QueryRow(ctx, `SELECT
        NOT EXISTS (SELECT 1 FROM clients WHERE telegram_id = 7102)
        AND (SELECT count(*) = 1 FROM subscriptions) AND (SELECT count(*) = 1 FROM operations)
        AND NOT EXISTS (SELECT 1 FROM telegram_processed_updates WHERE update_id = 12)
        AND (SELECT count(*) = 1 FROM outbox WHERE message_key = 'client-menu:123:12' AND body = 'Другая запись')
        AND (SELECT count(*) = 1 FROM registration_flows WHERE review_code = 42)
        AND (SELECT count(*) = 1 FROM operations WHERE next_attempt_at = 'infinity'::timestamptz)`,
	).Scan(&rolledBack); err != nil || !rolledBack {
		t.Fatalf("outbox conflict committed partial registration or changed held work: %v", err)
	}

	// Missing subscriptions and mismatched private-chat bindings are corruption,
	// never unknown accounts, and errors must not disclose account values.
	if _, err := s.pool.Exec(ctx, `INSERT INTO clients (telegram_id, chat_id, display_name, invoice_ninja_id)
        VALUES (7103, 7103, 'private-profile-value', 'private-billing-value')`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.LoadMenuSnapshot(ctx, menuTestBot, 13, 7103); err == nil || strings.Contains(err.Error(), "private-") {
		t.Fatal("missing subscription was accepted or disclosed private data")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE clients SET chat_id = 99999 WHERE id = $1`, registered.Client.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.LoadMenuSnapshot(ctx, menuTestBot, 14, 7101); err == nil {
		t.Fatal("mismatched private chat was accepted")
	}
	// This account is now corrupt, proving deduplication runs before projection.
	if snapshot, duplicate, err := s.LoadMenuSnapshot(ctx, menuTestBot, 10, 7101); err != nil || !duplicate || snapshot != (clientmenu.Snapshot{}) {
		t.Fatalf("duplicate load unnecessarily inspected corrupt account: %v", err)
	}
}

type menuClientInsertGate struct {
	started chan struct{}
	release chan struct{}
	seen    sync.Once
	resumed sync.Once
}

func (g *menuClientInsertGate) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "INSERT INTO clients") {
		g.seen.Do(func() {
			close(g.started)
			select {
			case <-g.release:
			case <-ctx.Done():
			}
		})
	}
	return ctx
}

func (*menuClientInsertGate) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
func (g *menuClientInsertGate) resume()                                                       { g.resumed.Do(func() { close(g.release) }) }

func TestMenuClientCreationProofUnderConcurrency(t *testing.T) {
	s, ctx := testStore(t)
	var wg sync.WaitGroup
	var createdClients, createdSubscriptions atomic.Int32
	var firstClient atomic.Int64
	for range 6 {
		wg.Go(func() {
			result, err := s.RegisterPrivateClient(ctx, TelegramIdentity{TelegramID: 7201, ChatID: 7201})
			if err != nil {
				t.Error(err)
				return
			}
			if result.ClientCreated {
				createdClients.Add(1)
			}
			if result.Created {
				createdSubscriptions.Add(1)
			}
			firstClient.CompareAndSwap(0, result.Client.ID)
			if result.Client.ID != firstClient.Load() {
				t.Error("concurrent registration selected different clients")
			}
		})
	}
	wg.Wait()
	if createdClients.Load() != 1 || createdSubscriptions.Load() != 1 {
		t.Fatalf("creation proof under contention: clients=%d, subscriptions=%d", createdClients.Load(), createdSubscriptions.Load())
	}

	// Force the narrow race after the unknown projection but before INSERT.
	// The winner creates ONLY the client. Created will be true for our attempted
	// subscription, so using that old flag alone would incorrectly accept it.
	start := menuTestUpdate(20, 7202, "/start")
	prepared := prepareMenuTest(t, s, ctx, start, "Регистрация")
	gate := &menuClientInsertGate{started: make(chan struct{}), release: make(chan struct{})}
	cfg := s.pool.Config()
	cfg.MaxConns = 1
	cfg.ConnConfig.Tracer = gate
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	defer gate.resume()
	racing := &Store{pool: pool}
	bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- racing.ApplyMenuUpdate(bounded, menuTestBot, menuTestUsername, start, prepared) }()
	select {
	case <-gate.started:
	case <-bounded.Done():
		t.Fatal("menu never reached the post-projection client insert")
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO clients (telegram_id, chat_id, display_name) VALUES (7202, 7202, 'Concurrent owner')`); err != nil {
		t.Fatal(err)
	}
	gate.resume()
	select {
	case err := <-done:
		if !errors.Is(err, clientmenu.ErrStaleSnapshot) {
			t.Fatalf("unknown start adopted concurrently inserted client: %v", err)
		}
	case <-bounded.Done():
		t.Fatal("menu did not finish after competing client insert")
	}
	var clean bool
	if err := s.pool.QueryRow(ctx, `SELECT
        (SELECT count(*) = 1 FROM clients WHERE telegram_id = 7202 AND display_name = 'Concurrent owner')
        AND NOT EXISTS (SELECT 1 FROM subscriptions s JOIN clients c ON c.id = s.client_id WHERE c.telegram_id = 7202)
        AND NOT EXISTS (SELECT 1 FROM operations o JOIN clients c ON c.id = o.client_id WHERE c.telegram_id = 7202)
        AND NOT EXISTS (SELECT 1 FROM telegram_processed_updates WHERE update_id = 20)
        AND NOT EXISTS (SELECT 1 FROM outbox)`,
	).Scan(&clean); err != nil || !clean {
		t.Fatalf("losing creation proof did not roll back every side effect: %v", err)
	}
	result, err := s.RegisterPrivateClient(ctx, TelegramIdentity{TelegramID: 7202, ChatID: 7202, DisplayName: "Recovered"})
	if err != nil || result.ClientCreated || !result.Created {
		t.Fatalf("client and subscription creation flags were conflated: result=%+v, error=%v", result, err)
	}
}

func TestMenuValidationAndSafeStageErrors(t *testing.T) {
	var s *Store // Invalid inputs must fail before touching a database.
	ctx := context.Background()
	for _, ids := range [][3]int64{{0, 0, 1}, {1, -1, 1}, {1, 0, 0}} {
		if _, _, err := s.LoadMenuSnapshot(ctx, ids[0], ids[1], ids[2]); err == nil {
			t.Fatal("invalid snapshot identity accepted")
		}
	}
	for _, input := range []struct {
		botID    int64
		name     string
		updateID int64
	}{{0, menuTestUsername, 0}, {menuTestBot, "", 0}, {menuTestBot, menuTestUsername, -1}} {
		if err := s.ApplyMenuUpdate(ctx, input.botID, input.name, telegram.Update{ID: input.updateID}, clientmenu.Prepared{}); err == nil {
			t.Fatal("invalid update identity accepted")
		}
	}
	private := &pgconn.PgError{Message: "private SQL value", Detail: "private profile and billing value"}
	if err := menuStageError("safe stage", private); err == nil || err.Error() != "safe stage" {
		t.Fatal("driver error escaped safe stage wrapping")
	}
	for _, sentinel := range []error{queue.ErrConflict, clientmenu.ErrStaleSnapshot, context.Canceled, context.DeadlineExceeded} {
		if err := menuStageError("safe stage", sentinel); !errors.Is(err, sentinel) {
			t.Fatal("safe sentinel was not preserved")
		}
	}
}
