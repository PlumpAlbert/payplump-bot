//go:build integration

package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/configdelivery"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/outbox"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
	"github.com/PlumpAlbert/payplump-bot/templates"
)

type configurationWorkflowRoundTrip func(*http.Request) (*http.Response, error)

func (f configurationWorkflowRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type configurationWorkflowSender struct {
	*telegram.Client
	service *configdelivery.Service
	logger  *slog.Logger
}

func (s configurationWorkflowSender) SendConfiguration(ctx context.Context, m queue.Message) (int64, error) {
	started := time.Now()
	id, err := s.service.Deliver(ctx, m)
	if err != nil {
		s.logger.Info("configuration workflow send failed", "elapsed", time.Since(started), "deadline", errors.Is(err, context.DeadlineExceeded), "canceled", errors.Is(err, context.Canceled), "lease_lost", errors.Is(err, queue.ErrLeaseLost))
	}
	return id, err
}

// The real private-chat GET finishes before this local ABA. The service still
// holds its original view when its final authorization callback is invoked.
type configurationWorkflowTransport struct {
	*telegram.Client
	afterChat func(context.Context) error
}

func (s configurationWorkflowTransport) GetPrivateChat(ctx context.Context, chat int64) error {
	if err := s.Client.GetPrivateChat(ctx, chat); err != nil {
		return err
	}
	if s.afterChat != nil {
		return s.afterChat(ctx)
	}
	return nil
}

type configurationWorkflowResult struct {
	failed bool
	err    error
}
type configurationWorkflowStore struct {
	*Store
	target int64
	result chan configurationWorkflowResult
	cancel context.CancelFunc
}

func (s *configurationWorkflowStore) report(id int64, failed bool, err error) {
	if id == s.target {
		s.result <- configurationWorkflowResult{failed, err}
		s.cancel()
	}
}
func (s *configurationWorkflowStore) CompleteOutbox(ctx context.Context, lease queue.Lease, id int64) error {
	err := s.Store.CompleteOutbox(ctx, lease, id)
	s.report(lease.ID, false, err)
	return err
}
func (s *configurationWorkflowStore) FailConfigurationOutbox(ctx context.Context, lease queue.Lease, code int, delay time.Duration, rejected bool) error {
	err := s.Store.FailConfigurationOutbox(ctx, lease, code, delay, rejected)
	s.report(lease.ID, true, err)
	return err
}

func configurationWorkflowRun(t *testing.T, ctx context.Context, s *Store, sender configurationWorkflowSender, target int64, failed bool, logger *slog.Logger) {
	t.Helper()
	runCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
	observed := &configurationWorkflowStore{Store: s, target: target, result: make(chan configurationWorkflowResult, 1), cancel: cancel}
	worker, err := outbox.New(observed, sender, 123, outbox.Config{Workers: 1, PollInterval: 100 * time.Millisecond}, logger)
	if err != nil {
		cancel()
		t.Fatal("construct configuration outbox worker")
	}
	done := make(chan error, 1)
	go func() { done <- worker.Run(runCtx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("outbox worker returned an error")
			}
		case <-time.After(10 * time.Second):
			t.Error("outbox worker did not join")
		}
	}()
	select {
	case result := <-observed.result:
		if result.err != nil || result.failed != failed {
			t.Fatalf("unexpected durable configuration finalization: failed=%t finalize_error=%t", result.failed, result.err != nil)
		}
	case <-runCtx.Done():
		// report queues the committed result before canceling Run.
		select {
		case result := <-observed.result:
			if result.err != nil || result.failed != failed {
				t.Fatalf("unexpected durable configuration finalization: failed=%t finalize_error=%t", result.failed, result.err != nil)
			}
		default:
			t.Fatal("no durable configuration finalization")
		}
	}
}

func TestConfigurationWorkflowWithSyntheticHTTP(t *testing.T) {
	s, ctx := activationTestStore(t)
	p := newActivationWorkflowHTTP(t, s, ctx, 123, configActor, "")
	activationWorkflowExpectEvents(t, p.run(t, ctx, false), 4, "finish", 0)
	p.assertOriginalPayment(t, ctx, false)
	// Only nonsensitive setup notifications may be completed without a worker.
	for {
		messages, err := s.ClaimOutbox(ctx, 123, 50, time.Minute)
		if err != nil {
			t.Fatal("drain setup notifications")
		}
		if len(messages) == 0 {
			break
		}
		for _, m := range messages {
			if m.ProtectedConfiguration {
				t.Fatal("setup contained a protected request")
			}
			if err := s.CompleteOutbox(ctx, m.Lease, 9); err != nil {
				t.Fatal("complete setup notification")
			}
		}
	}
	cfg := configdelivery.Config{
		Enabled: true, TelegramEnabled: true, PaymentsEnabled: true, Revision: 5,
		BaseURL: p.cfg.BaseURL, Token: p.cfg.Token, CurrencyID: "1", TelegramIDField: "id_number",
		SUIBaseURL: p.cfg.SUIBaseURL, SUIToken: p.cfg.SUIToken, SUIVersion: p.cfg.SUIVersion,
		SubscriptionBaseURL: "https://subscription.example.test/prefix/", AllowedTelegramIDs: []int64{configActor},
		HTTPTimeout: 5 * time.Second, PageSize: 1, MaxPages: 10,
	}
	if err := s.InstallConfigurationPolicy(ctx, 123, cfg.Policy()); err != nil {
		t.Fatal("install configuration policy")
	}
	settlement, err := invoiceninja.NewSettlementClient(cfg.BaseURL, cfg.Token, cfg.HTTPTimeout, cfg.PageSize, cfg.MaxPages)
	if err != nil {
		t.Fatal("construct settlement reader")
	}
	panel, err := sui.NewRecordReader(cfg.SUIBaseURL, cfg.SUIToken, cfg.SUIVersion, cfg.HTTPTimeout)
	if err != nil {
		t.Fatal("construct panel reader")
	}
	recurring, err := invoiceninja.NewRecurringReader(cfg.BaseURL, cfg.Token, cfg.HTTPTimeout)
	if err != nil {
		t.Fatal("construct recurring reader")
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		switch a.Key {
		case "time", "level", "msg", "work_id", "attempt", "error_code", "reason_code", "elapsed", "deadline", "canceled", "lease_lost":
			return a
		case "error":
			return slog.Bool("error_present", true)
		default:
			return slog.Attr{}
		}
	}}))
	t.Cleanup(func() {
		if t.Failed() {
			t.Log("safe workflow diagnostics:\n" + logs.String())
		}
	})
	client := telegram.NewClient("SYNTHETIC")
	providers := configdelivery.Providers{Payments: settlement, SUI: panel, Recurring: recurring}
	makeSender := func(after func(context.Context) error) configurationWorkflowSender {
		service, err := configdelivery.New(s, providers, configurationWorkflowTransport{client, after}, 123, cfg, logger)
		if err != nil {
			t.Fatal("construct configuration service")
		}
		return configurationWorkflowSender{client, service, logger}
	}
	// Construct/validate providers before replacing the global transport.
	sender := makeSender(nil)
	abaSender := makeSender(func(ctx context.Context) error {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer rollback(tx)
		if _, err = tx.Exec(ctx, `SELECT id FROM clients WHERE id=$1 FOR UPDATE`, p.clientID); err != nil {
			return err
		}
		for _, code := range []int{2007, 0} {
			if _, err = tx.Exec(ctx, `UPDATE payment_checks SET review_code=$2 WHERE client_id=$1`, p.clientID, code); err != nil {
				return err
			}
		}
		return tx.Commit(ctx)
	})

	var mu sync.Mutex
	var target int64
	var unknown bool
	var secrets, denials, chats, ordinary int
	var permitVisible = true
	var denialBody = templates.Render("configdelivery/denied", templates.Data{})
	p.mu.Lock()
	bindingName := p.panel.Name()
	p.mu.Unlock()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		var body map[string]json.RawMessage
		if r.Method != http.MethodPost || json.NewDecoder(io.LimitReader(r.Body, 65536)).Decode(&body) != nil {
			t.Error("invalid synthetic Telegram request")
			w.WriteHeader(400)
			return
		}
		var chatID int64
		if json.Unmarshal(body["chat_id"], &chatID) != nil || chatID != configActor {
			t.Error("unexpected Telegram private identity")
			w.WriteHeader(400)
			return
		}
		if r.URL.Path == "/botSYNTHETIC/getChat" {
			chats++
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"id": chatID, "type": "private"}})
			return
		}
		if r.URL.Path != "/botSYNTHETIC/sendMessage" {
			t.Error("unexpected Telegram method")
			w.WriteHeader(400)
			return
		}
		var text string
		if json.Unmarshal(body["text"], &text) != nil {
			t.Error("missing Telegram text")
			w.WriteHeader(400)
			return
		}
		secret := strings.Contains(text, "https://")
		protected := string(body["protect_content"]) == "true"
		if protected {
			if len(body) != 5 || string(body["disable_notification"]) != "true" || string(body["link_preview_options"]) != `{"is_disabled":true}` {
				t.Error("protected send enabled markup, keyboard, notification or preview")
			}
			if secret {
				secrets++
				expectedURI := "https://subscription.example.test/prefix/" + url.PathEscape(bindingName) + "?format=json"
				u, parseErr := url.Parse(expectedURI)
				validURI := strings.Contains(text, expectedURI) && text == templates.Render("configdelivery/subscription", templates.Data{SubscriptionURL: expectedURI}) && parseErr == nil && u.Scheme == "https" && u.Host == "subscription.example.test" && u.EscapedPath() == "/prefix/"+url.PathEscape(bindingName) && u.RawQuery == "format=json" && u.Fragment == "" && u.User == nil
				if !validURI {
					t.Error("configuration URI changed its saved single-segment binding or public prefix")
				}
				dbCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
				var visible bool
				err := s.pool.QueryRow(dbCtx, `SELECT d.kind='secret' AND d.outcome='reserved' AND d.attempt=1 AND d.attempt=o.attempts
       AND g.outbox_id=o.id AND g.attempt=o.attempts AND o.status='running'
       AND d.not_after>clock_timestamp() AND o.locked_until>clock_timestamp()
       FROM configuration_dispatches d JOIN outbox o ON o.id=d.outbox_id
       JOIN configuration_chat_guards g ON g.bot_id=o.bot_id AND g.chat_id=o.chat_id WHERE o.id=$1`, target).Scan(&visible)
				cancel()
				if err != nil || !visible {
					permitVisible = false
					t.Error("secret POST preceded committed dispatch and chat reservation")
				}
			} else {
				denials++
				if text != denialBody || strings.Contains(text, bindingName) || strings.Contains(text, "http") {
					t.Error("denial was not constant nonsensitive text")
				}
			}
		} else {
			ordinary++
			if secret || text != "synthetic ordinary help" {
				t.Error("unexpected ordinary send")
			}
		}
		if unknown && secret {
			chatID++
		} // Received the secret, but receipt cannot prove its recipient.
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": 42, "chat": map[string]any{"id": chatID, "type": "private"}}})
	}))
	defer peer.Close()
	original := http.DefaultTransport
	local := original.(*http.Transport).Clone()
	local.Proxy = nil
	defer local.CloseIdleConnections()
	telegramURL, _ := url.Parse(peer.URL)
	providerURL, _ := url.Parse(p.server.URL)
	http.DefaultTransport = configurationWorkflowRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme == "https" && r.URL.Host == "api.telegram.org" && (r.URL.Path == "/botSYNTHETIC/getChat" || r.URL.Path == "/botSYNTHETIC/sendMessage") {
			copy := r.Clone(r.Context())
			u := *r.URL
			u.Scheme, u.Host = telegramURL.Scheme, telegramURL.Host
			copy.URL, copy.Host = &u, telegramURL.Host
			return local.RoundTrip(copy)
		}
		if r.Method == http.MethodGet && r.URL.Scheme == providerURL.Scheme && r.URL.Host == providerURL.Host {
			return local.RoundTrip(r)
		}
		t.Error("blocked unexpected or nonlocal HTTP destination")
		return nil, errors.New("synthetic workflow blocked HTTP destination")
	})
	defer func() { http.DefaultTransport = original }() // Every Run below joins before restoration.
	enqueue := func(update int64) int64 {
		u, prepared := configPrepared(t, s, ctx, update, configActor, cfg.Policy())
		if err := s.ApplyMenuUpdate(ctx, 123, menuTestUsername, u, prepared); err != nil {
			t.Fatal("enqueue configuration request")
		}
		var id int64
		if err := s.pool.QueryRow(ctx, `SELECT o.id FROM outbox o JOIN configuration_requests r ON r.outbox_id=o.id WHERE o.bot_id=123 AND o.message_key=$1`, prepared.Request.Key()).Scan(&id); err != nil {
			t.Fatal("find configuration request")
		}
		mu.Lock()
		target = id
		mu.Unlock()
		return id
	}
	counts := func() (int, int, int, int) { mu.Lock(); defer mu.Unlock(); return secrets, denials, chats, ordinary }
	assertStored := func(id int64, kind, outcome, status string) {
		t.Helper()
		configAssert(t, s, ctx, `SELECT o.body=$2 AND o.protected_configuration AND o.status=$3
    AND d.kind=$4 AND d.outcome=$5 AND d.attempt=o.attempts
    AND (CASE WHEN $5='sent' THEN d.telegram_message_id=42 ELSE d.telegram_message_id IS NULL END)
    FROM outbox o JOIN configuration_dispatches d ON d.outbox_id=o.id WHERE o.id=$1`, id, queue.ConfigurationIntentBody, status, kind, outcome)
		var metadata string
		if err := s.pool.QueryRow(ctx, `SELECT row_to_json(r)::text||row_to_json(d)::text||o.body
    FROM configuration_requests r JOIN configuration_dispatches d ON d.outbox_id=r.outbox_id JOIN outbox o ON o.id=r.outbox_id WHERE o.id=$1`, id).Scan(&metadata); err != nil {
			t.Fatal("inspect durable configuration metadata")
		}
		if strings.Contains(metadata, bindingName) || strings.Contains(metadata, "https://") {
			t.Fatal("durable configuration work contains secret material")
		}
	}
	first := enqueue(7001)
	configurationWorkflowRun(t, ctx, s, sender, first, false, logger)
	assertStored(first, "secret", "sent", "sent")
	configAssert(t, s, ctx, `SELECT outbox_id IS NULL FROM configuration_chat_guards WHERE bot_id=123 AND chat_id=$1`, configActor)
	if a, b, c, _ := counts(); a != 1 || b != 0 || c != 1 {
		t.Fatal("positive configuration did not traverse private GET and protected POST")
	}

	var revision int64
	if err := s.pool.QueryRow(ctx, `SELECT configuration_revision FROM clients WHERE id=$1`, p.clientID).Scan(&revision); err != nil {
		t.Fatal("load pre-ABA revision")
	}
	aba := enqueue(7002)
	configurationWorkflowRun(t, ctx, s, abaSender, aba, false, logger)
	assertStored(aba, "denial", "sent", "sent")
	configAssert(t, s, ctx, `SELECT configuration_revision=$2+2 FROM clients WHERE id=$1`, p.clientID, revision)
	if a, b, c, _ := counts(); a != 1 || b != 1 || c != 2 {
		t.Fatal("local ABA did not downgrade final permit")
	}

	unresolved := enqueue(7003)
	mu.Lock()
	unknown = true
	mu.Unlock()
	configurationWorkflowRun(t, ctx, s, sender, unresolved, true, logger)
	assertStored(unresolved, "secret", "unknown", "failed")
	configAssert(t, s, ctx, `SELECT outbox_id=$2 AND attempt=1 FROM configuration_chat_guards WHERE bot_id=123 AND chat_id=$1`, configActor, unresolved)
	if a, b, c, _ := counts(); a != 2 || b != 1 || c != 3 {
		t.Fatal("unknown receipt did not follow one actual secret POST")
	}
	p.mu.Lock()
	gets, mutations := p.gets, p.mutations
	p.mu.Unlock()
	blocked := enqueue(7004)
	configurationWorkflowRun(t, ctx, s, sender, blocked, false, logger)
	assertStored(blocked, "denial", "sent", "sent")
	help, err := s.EnqueueOutbox(ctx, queue.Draft{BotID: 123, ChatID: configActor, Key: "configuration-workflow-help", Text: "synthetic ordinary help"})
	if err != nil {
		t.Fatal("enqueue help behind unknown configuration")
	}
	configurationWorkflowRun(t, ctx, s, sender, help, false, logger)
	if a, b, c, d := counts(); a != 2 || b != 2 || c != 3 || d != 1 {
		t.Fatal("blocked request retried secret or prevented ordinary help")
	}
	p.mu.Lock()
	unchanged := p.gets == gets && p.mutations == mutations
	p.mu.Unlock()
	if !unchanged {
		t.Fatal("unknown guard allowed automatic provider re-preflight")
	}
	configAssert(t, s, ctx, `SELECT outbox_id=$2 AND attempt=1 FROM configuration_chat_guards WHERE bot_id=123 AND chat_id=$1`, configActor, unresolved)
	configAssert(t, s, ctx, `SELECT attempts=1 AND status='failed' FROM outbox WHERE id=$1`, unresolved)
	mu.Lock()
	visible := permitVisible
	mu.Unlock()
	if !visible {
		t.Fatal("secret was sent without a committed permit")
	}
	p.assertOriginalPayment(t, ctx, false)
}
