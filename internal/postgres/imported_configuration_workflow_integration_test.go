//go:build integration

package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/clientmenu"
	"github.com/PlumpAlbert/payplump-bot/internal/configdelivery"
	"github.com/PlumpAlbert/payplump-bot/internal/importverify"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/outbox"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
	"github.com/PlumpAlbert/payplump-bot/templates"
)

func importedConfigurationWorkflowRun(t *testing.T, ctx context.Context, s *Store, sender configurationWorkflowSender, botID, target int64, failed bool, logger *slog.Logger) {
	t.Helper()
	runCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
	observed := &configurationWorkflowStore{Store: s, target: target, result: make(chan configurationWorkflowResult, 1), cancel: cancel}
	worker, err := outbox.New(observed, sender, botID, outbox.Config{Workers: 1, PollInterval: 100 * time.Millisecond}, logger)
	if err != nil {
		cancel()
		t.Fatal("construct imported configuration outbox worker")
	}
	done := make(chan error, 1)
	go func() { done <- worker.Run(runCtx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("imported configuration outbox worker returned an error")
			}
		case <-time.After(10 * time.Second):
			t.Error("imported configuration outbox worker did not join")
		}
	}()
	select {
	case result := <-observed.result:
		if result.err != nil || result.failed != failed {
			t.Fatalf("unexpected imported configuration finalization: failed=%t finalize_error=%t", result.failed, result.err != nil)
		}
	case <-runCtx.Done():
		select {
		case result := <-observed.result:
			if result.err != nil || result.failed != failed {
				t.Fatalf("unexpected imported configuration finalization: failed=%t finalize_error=%t", result.failed, result.err != nil)
			}
		default:
			t.Fatal("no durable imported configuration finalization")
		}
	}
}

// Imported configuration is exercised from the actual verifier through
// admission, ordinary baseline payment observation, private menu intent,
// protected delivery, and final database authorization. Provider handlers
// come from the same source server used by verification; this rig never
// changes credentials, bases, or evidence after Prepare.
func TestImportedConfigurationFromOriginalPaidBaseline(t *testing.T) {
	runImportedBaselineConfiguration(t, "basic")
}

func TestImportedConfigurationWithPausedBilling(t *testing.T) {
	for _, schedule := range []string{"future", "stale", "empty"} {
		t.Run(schedule, func(t *testing.T) { runImportedBaselineConfiguration(t, "paused_billing_"+schedule) })
	}
}

func TestImportedConfigurationRuntimePermissionABABeforeSend(t *testing.T) {
	runImportedBaselineConfiguration(t, "runtime_aba")
}

func TestImportedConfigurationUnknownSendPinsChatBarrier(t *testing.T) {
	runImportedBaselineConfiguration(t, "unknown_send")
}

func runImportedBaselineConfiguration(t *testing.T, scenario string) {
	t.Helper()
	var providerGETs atomic.Int64
	rig := newImportedOverdueRigWithHandler(t, false, func(src *importedOverdueSource) http.Handler {
		if strings.HasPrefix(scenario, "paused_billing_") {
			src.recurring["status_id"] = "3"
			switch scenario {
			case "paused_billing_stale":
				stale := time.Now().AddDate(0, -2, 0).Format(time.DateOnly)
				src.recurring["next_send_date"], src.recurring["next_send_datetime"] = stale, stale+"T00:00:00Z"
			case "paused_billing_empty":
				src.recurring["next_send_date"], src.recurring["next_send_datetime"] = "", ""
			}
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && (strings.HasPrefix(r.URL.Path, "/ninja/") || strings.HasPrefix(r.URL.Path, "/panel/")) {
				providerGETs.Add(1)
			}
			src.ServeHTTP(w, r)
		})
	})
	ctx := rig.ctx
	prepared, err := rig.prepare(time.Now().UTC().Add(-time.Second))
	if err != nil {
		t.Fatal("prepare same-source import", err)
	}
	botID := prepared.BotID()
	if _, err = rig.s.pool.Exec(ctx, `INSERT INTO telegram_bots(bot_id) VALUES($1) ON CONFLICT DO NOTHING`, botID); err != nil {
		t.Fatal(err)
	}
	if err = rig.s.SyncImportAdmissionPolicy(ctx, botID, prepared.Revision(), prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
		t.Fatal(err)
	}
	if result, e := rig.s.ApplyVerifiedImport(ctx, prepared); e != nil || result.Admitted != 1 {
		t.Fatalf("apply actual verified import: result=%+v err=%v", result, e)
	}
	if err = rig.s.SyncImportedRuntimePermission(ctx, botID, 1, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
		t.Fatal(err)
	}
	if count, e := rig.s.SeedPaymentChecks(ctx, botID); e != nil || count != 1 {
		t.Fatalf("seed ordinary imported baseline check: count=%d err=%v", count, e)
	}
	claim, snapshot := claimSinglePayment(t, rig.s, ctx, botID)
	decision, err := payments.Observe(ctx, rig.settlement, snapshot, "1", time.Now().In(time.Local).Format(time.DateOnly))
	if err != nil || decision.Kind != payments.Verified || decision.Proof == nil {
		t.Fatalf("ordinary current-baseline observation: decision=%+v err=%v", decision, err)
	}
	if err = rig.s.FinishPaymentCheck(ctx, claim, snapshot, decision, payments.Notices{}, time.Minute); err != nil {
		t.Fatal("finish ordinary current-baseline observation", err)
	}
	paymentSQL(t, rig.s, ctx, `SELECT current_invoice_id='invoice-42' AND first_invoice_id IS NULL AND state='active' AND desired_access FROM subscriptions WHERE client_id=(SELECT id FROM clients WHERE telegram_id=42)`)
	paymentSQL(t, rig.s, ctx, `SELECT count(*)=1 FROM payment_confirmations WHERE client_id=(SELECT id FROM clients WHERE telegram_id=42) AND invoice_id='invoice-42'`)
	paymentSQL(t, rig.s, ctx, `SELECT count(*)=0 FROM operations WHERE client_id=(SELECT id FROM clients WHERE telegram_id=42)`)

	base := strings.TrimSuffix(rig.ninjaBase, "/ninja")
	publicBase := "https://subs.invalid/"
	cfg := configdelivery.Config{
		Enabled: true, TelegramEnabled: true, PaymentsEnabled: true, Revision: 5,
		BaseURL: rig.ninjaBase, Token: importedOverdueNinjaToken, CurrencyID: "1", TelegramIDField: "id_number",
		SUIBaseURL: base + "/panel", SUIToken: importedOverdueSUIToken, SUIVersion: "1.6.4",
		SubscriptionBaseURL: publicBase, AllowedTelegramIDs: []int64{42}, HTTPTimeout: time.Second,
		PageSize: 10, MaxPages: 2,
	}
	// Assert the origin's immutable scope agrees with the exact configured
	// GET credentials/versions and public URL policy before exercising delivery.
	scope, err := importverify.SourceScope(cfg.BaseURL, cfg.Token, "5.13.47", cfg.SUIBaseURL, cfg.SUIToken, cfg.SUIVersion)
	if err != nil || scope != prepared.SourceScopeHash() {
		t.Fatal("configuration GET clients do not use the admitted source scope")
	}
	publicBaseHash := sha256.Sum256([]byte(publicBase))
	var evidencePinned bool
	if err = rig.s.pool.QueryRow(ctx, `SELECT encode(decode(evidence->>'public_base_hash','hex'),'hex')=$2 AND evidence->'inbound_ids'='[1]'::jsonb FROM verified_import_origins WHERE bot_id=$1 AND current_invoice_id='invoice-42'`, botID, fmt.Sprintf("%x", publicBaseHash)).Scan(&evidencePinned); err != nil || !evidencePinned {
		t.Fatalf("import evidence did not pin exact public base and original inbound IDs: %v", err)
	}
	if err = rig.s.InstallConfigurationPolicy(ctx, botID, cfg.Policy()); err != nil {
		t.Fatal("install independently enabled protected delivery policy", err)
	}
	settlement, err := invoiceninja.NewSettlementClient(cfg.BaseURL, cfg.Token, cfg.HTTPTimeout, cfg.PageSize, cfg.MaxPages)
	if err != nil {
		t.Fatal(err)
	}
	panel, err := sui.NewRecordReader(cfg.SUIBaseURL, cfg.SUIToken, cfg.SUIVersion, cfg.HTTPTimeout)
	if err != nil {
		t.Fatal(err)
	}
	recurring, err := invoiceninja.NewRecurringReader(cfg.BaseURL, cfg.Token, cfg.HTTPTimeout)
	if err != nil {
		t.Fatal(err)
	}

	// Private Telegram is another synthetic HTTP peer. The handler checks that
	// the protected request and chat reservation are committed before a URL is
	// made visible to the synthetic client, and never retains that URL in DB.
	var peerMu sync.Mutex
	var target, unknownTarget int64
	var sentURL string // process memory only; never written to fixtures/logs/DB.
	var sentDenials, secretSends, chatGets int
	var denialText = templates.Render("configdelivery/denied", templates.Data{})
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			t.Errorf("Telegram request method=%s", r.Method)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body map[string]json.RawMessage
		if json.NewDecoder(io.LimitReader(r.Body, 32*1024)).Decode(&body) != nil {
			t.Error("invalid synthetic Telegram JSON")
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var chat int64
		if json.Unmarshal(body["chat_id"], &chat) != nil || chat != 42 {
			t.Error("Telegram actor escaped its private chat")
			http.Error(w, "bad chat", http.StatusBadRequest)
			return
		}
		if r.URL.Path == "/botSYNTHETIC/getChat" {
			peerMu.Lock()
			chatGets++
			peerMu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"id": chat, "type": "private"}})
			return
		}
		if r.URL.Path != "/botSYNTHETIC/sendMessage" || string(body["protect_content"]) != "true" || len(body) != 5 {
			t.Error("unexpected or unprotected Telegram request")
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var text string
		if json.Unmarshal(body["text"], &text) != nil {
			t.Error("missing protected Telegram text")
			http.Error(w, "bad text", http.StatusBadRequest)
			return
		}
		peerMu.Lock()
		id := target
		unknown := false
		if text == denialText {
			sentDenials++
		} else if strings.Contains(text, "https://subs.invalid/") {
			sentURL = text
			secretSends++
			unknown = id == unknownTarget
		} else {
			t.Error("protected response was neither expected secret nor fixed denial")
		}
		peerMu.Unlock()
		if strings.Contains(text, "https://") {
			var reserved bool
			err := rig.s.pool.QueryRow(r.Context(), `SELECT d.kind='secret' AND d.outcome='reserved' AND d.attempt=o.attempts AND g.outbox_id=o.id AND o.status='running' FROM configuration_dispatches d JOIN outbox o ON o.id=d.outbox_id JOIN configuration_chat_guards g ON g.bot_id=o.bot_id AND g.chat_id=o.chat_id WHERE o.id=$1`, id).Scan(&reserved)
			if err != nil || !reserved {
				t.Error("personal URL sent before durable intent and chat guard reservation")
			}
			if unknown {
				// Telegram received the protected URL but returned an ambiguous
				// failure. The durable reservation must remain pinned.
				http.Error(w, "synthetic unknown send outcome", http.StatusServiceUnavailable)
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": 77, "chat": map[string]any{"id": chat, "type": "private"}}})
	}))
	defer peer.Close()

	originalTransport := http.DefaultTransport
	local := originalTransport.(*http.Transport).Clone()
	local.Proxy = nil
	defer local.CloseIdleConnections()
	peerURL, _ := url.Parse(peer.URL)
	providerURL, _ := url.Parse(base)
	http.DefaultTransport = configurationWorkflowRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme == "https" && r.URL.Host == "api.telegram.org" && strings.HasPrefix(r.URL.Path, "/botSYNTHETIC/") {
			clone := r.Clone(r.Context())
			u := *r.URL
			u.Scheme, u.Host = peerURL.Scheme, peerURL.Host
			clone.URL, clone.Host = &u, peerURL.Host
			return local.RoundTrip(clone)
		}
		if r.URL.Scheme == providerURL.Scheme && r.URL.Host == providerURL.Host {
			return local.RoundTrip(r)
		}
		return nil, errors.New("synthetic imported configuration blocked nonlocal HTTP")
	})
	defer func() { http.DefaultTransport = originalTransport }()

	update := menuTestUpdate(88001, 42, "/config")
	identity, action, ok := update.PrivateMenu(menuTestUsername)
	if !ok || identity.TelegramID != 42 || action != clientmenu.Configuration {
		t.Fatal("synthetic private /config update was not actor-scoped")
	}
	menuSnapshot, duplicate, err := rig.s.LoadMenuSnapshot(ctx, botID, update.ID, identity.TelegramID)
	if err != nil || duplicate || !menuSnapshot.Known || menuSnapshot.State != "active" || !menuSnapshot.DesiredAccess {
		t.Fatalf("load imported private menu snapshot: %+v duplicate=%t err=%v", menuSnapshot, duplicate, err)
	}
	menuCfg := clientmenu.Config{ConfigurationEnabled: true, ConfigurationPolicyRevision: cfg.Revision, ConfigurationPolicyHash: cfg.Policy().Hash, ConfigurationAllowedTelegramIDs: []int64{42}}
	menu, err := clientmenu.New(menuCfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	menuRequest := clientmenu.Request{BotID: botID, UpdateID: update.ID, TelegramID: identity.TelegramID, ChatID: identity.ChatID, Action: action}
	preparedMenu, err := menu.Prepare(ctx, menuRequest, menuSnapshot)
	if err != nil || !preparedMenu.ProtectedConfiguration || preparedMenu.Text != queue.ConfigurationIntentBody {
		t.Fatalf("prepare protected private intent: prepared=%+v err=%v", preparedMenu, err)
	}
	if err = rig.s.ApplyMenuUpdate(ctx, botID, menuTestUsername, update, preparedMenu); err != nil {
		t.Fatal("commit private configuration intent", err)
	}
	var outboxID int64
	if err = rig.s.pool.QueryRow(ctx, `SELECT outbox_id FROM configuration_requests WHERE bot_id=$1 AND actor_id=42 AND origin_kind='imported' AND period_kind='imported_baseline'`, botID).Scan(&outboxID); err != nil {
		t.Fatal("missing imported-baseline request", err)
	}
	peerMu.Lock()
	target = outboxID
	peerMu.Unlock()
	paymentSQL(t, rig.s, ctx, `SELECT origin_kind='imported' AND period_kind='imported_baseline' AND activation_operation_id IS NULL AND renewal_operation_id IS NULL AND imported_origin_client_id=client_id AND imported_admission_permission_revision>0 AND imported_runtime_revision=1 AND invoice_id='invoice-42' FROM configuration_requests WHERE outbox_id=$1`, outboxID)
	var requestScopeMatches bool
	if err = rig.s.pool.QueryRow(ctx, `SELECT encode(io.source_scope_hash,'hex')=$2 FROM configuration_requests r JOIN verified_import_origins io ON io.client_id=r.imported_origin_client_id AND io.bot_id=r.bot_id AND io.current_invoice_id=r.invoice_id WHERE r.outbox_id=$1`, outboxID, prepared.SourceScopeHash()).Scan(&requestScopeMatches); err != nil || !requestScopeMatches {
		t.Fatalf("request did not pin admitted source scope: %v", err)
	}

	transport := telegram.NewClient("SYNTHETIC")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service, err := configdelivery.New(rig.s, configdelivery.Providers{Payments: settlement, SUI: panel, Recurring: recurring}, transport, botID, cfg, logger)
	if err != nil {
		t.Fatal("construct real imported configuration service", err)
	}
	sender := configurationWorkflowSender{Client: transport, service: service, logger: logger}
	importedConfigurationWorkflowRun(t, ctx, rig.s, sender, botID, outboxID, false, logger)
	peerMu.Lock()
	captured := sentURL
	peerMu.Unlock()
	const expectedURL = "https://subs.invalid/42?format=json"
	u, urlErr := url.Parse(expectedURL)
	if !strings.Contains(captured, expectedURL) || captured != templates.Render("configdelivery/subscription", templates.Data{SubscriptionURL: expectedURL}) || urlErr != nil || u.Scheme != "https" || u.Host != "subs.invalid" || u.EscapedPath() != "/42" || u.RawQuery != "format=json" || u.Fragment != "" {
		t.Fatal("synthetic Telegram client did not receive the canonical Telegram-ID subscription URL")
	}
	if strings.Contains(captured, queue.ConfigurationIntentBody) {
		t.Fatal("sensitive URL entered durable intent")
	}
	paymentSQL(t, rig.s, ctx, `SELECT d.kind='secret' AND d.outcome='sent' AND o.status='sent' AND g.outbox_id IS NULL AND g.attempt IS NULL FROM configuration_dispatches d JOIN outbox o ON o.id=d.outbox_id JOIN configuration_chat_guards g ON g.bot_id=o.bot_id AND g.chat_id=o.chat_id WHERE o.id=$1`, outboxID)
	paymentSQL(t, rig.s, ctx, `SELECT body=$2 AND body NOT LIKE '%https://%' AND body NOT LIKE '%private-path%' FROM outbox WHERE id=$1`, outboxID, queue.ConfigurationIntentBody)
	paymentSQL(t, rig.s, ctx, `SELECT count(*)=0 FROM operations WHERE client_id=(SELECT id FROM clients WHERE telegram_id=42)`)
	paymentSQL(t, rig.s, ctx, `SELECT count(*)=0 FROM activation_sui_bindings WHERE client_id=(SELECT id FROM clients WHERE telegram_id=42)`)

	makeRequest := func(updateID int64, requestCfg configdelivery.Config) int64 {
		t.Helper()
		u := menuTestUpdate(updateID, 42, "/config")
		identity, action, supported := u.PrivateMenu(menuTestUsername)
		if !supported || identity.TelegramID != 42 || action != clientmenu.Configuration {
			t.Fatal("synthetic private configuration update was not actor-scoped")
		}
		snapshot, duplicate, loadErr := rig.s.LoadMenuSnapshot(ctx, botID, updateID, 42)
		if loadErr != nil || duplicate {
			t.Fatalf("load fresh configuration snapshot: duplicate=%t err=%v", duplicate, loadErr)
		}
		policy := requestCfg.Policy()
		menu, menuErr := clientmenu.New(clientmenu.Config{ConfigurationEnabled: true, ConfigurationPolicyRevision: policy.Revision, ConfigurationPolicyHash: policy.Hash, ConfigurationAllowedTelegramIDs: []int64{42}}, nil)
		if menuErr != nil {
			t.Fatal(menuErr)
		}
		prepared, menuErr := menu.Prepare(ctx, clientmenu.Request{BotID: botID, UpdateID: updateID, TelegramID: 42, ChatID: 42, Action: action}, snapshot)
		if menuErr != nil || !prepared.ProtectedConfiguration || prepared.Text != queue.ConfigurationIntentBody {
			t.Fatalf("prepare fresh protected configuration intent: protected=%t err=%v", prepared.ProtectedConfiguration, menuErr)
		}
		if applyErr := rig.s.ApplyMenuUpdate(ctx, botID, menuTestUsername, u, prepared); applyErr != nil {
			t.Fatal("apply fresh protected configuration intent", applyErr)
		}
		var id int64
		if queryErr := rig.s.pool.QueryRow(ctx, `SELECT r.outbox_id FROM configuration_requests r JOIN outbox o ON o.id=r.outbox_id WHERE r.bot_id=$1 AND o.message_key=$2`, botID, prepared.Request.Key()).Scan(&id); queryErr != nil {
			t.Fatal("find fresh protected configuration intent", queryErr)
		}
		return id
	}

	if scenario == "runtime_aba" {
		abaID := makeRequest(88002, cfg)
		paymentSQL(t, rig.s, ctx, `SELECT imported_runtime_revision=1 AND origin_kind='imported' AND activation_operation_id IS NULL FROM configuration_requests WHERE outbox_id=$1`, abaID)
		beforeGETs := providerGETs.Load()
		peerMu.Lock()
		beforeChats := chatGets
		peerMu.Unlock()
		abaTransport := configurationWorkflowTransport{Client: transport, afterChat: func(callCtx context.Context) error {
			if updateErr := rig.s.SyncImportedRuntimePermission(callCtx, botID, 1, prepared.PolicyHash(), prepared.SourceScopeHash(), false); updateErr != nil {
				return updateErr
			}
			return rig.s.SyncImportedRuntimePermission(callCtx, botID, 2, prepared.PolicyHash(), prepared.SourceScopeHash(), true)
		}}
		abaService, makeErr := configdelivery.New(rig.s, configdelivery.Providers{Payments: settlement, SUI: panel, Recurring: recurring}, abaTransport, botID, cfg, logger)
		if makeErr != nil {
			t.Fatal("construct runtime-ABA delivery service", makeErr)
		}
		peerMu.Lock()
		target = abaID
		peerMu.Unlock()
		importedConfigurationWorkflowRun(t, ctx, rig.s, configurationWorkflowSender{Client: transport, service: abaService, logger: logger}, botID, abaID, false, logger)
		if providerGETs.Load() <= beforeGETs {
			t.Fatal("runtime-ABA case did not reach provider validation before private-chat recheck")
		}
		peerMu.Lock()
		gotChats, denials, secrets := chatGets, sentDenials, secretSends
		peerMu.Unlock()
		if gotChats != beforeChats+1 || denials != 1 || secrets != 1 {
			t.Fatal("runtime-ABA request was upgraded or sent a personal URL")
		}
		paymentSQL(t, rig.s, ctx, `SELECT revision=2 AND enabled FROM imported_runtime_permissions WHERE bot_id=$1`, botID)
		paymentSQL(t, rig.s, ctx, `SELECT imported_runtime_revision=1 FROM configuration_requests WHERE outbox_id=$1`, abaID)
		paymentSQL(t, rig.s, ctx, `SELECT d.kind='denial' AND d.outcome='sent' AND o.status='sent' FROM configuration_dispatches d JOIN outbox o ON o.id=d.outbox_id WHERE o.id=$1`, abaID)
		paymentSQL(t, rig.s, ctx, `SELECT review_code=0 FROM payment_checks WHERE bot_id=$1 AND client_id=(SELECT id FROM clients WHERE telegram_id=42)`, botID)
		if posts := rig.source.postSnapshot(); len(posts) != 0 {
			t.Fatalf("runtime-ABA configuration workflow performed provider writes: %v", posts)
		}
		return
	}

	if scenario == "unknown_send" {
		unknownID := makeRequest(88002, cfg)
		peerMu.Lock()
		target, unknownTarget = unknownID, unknownID
		peerMu.Unlock()
		unknownService, makeErr := configdelivery.New(rig.s, configdelivery.Providers{Payments: settlement, SUI: panel, Recurring: recurring}, transport, botID, cfg, logger)
		if makeErr != nil {
			t.Fatal("construct unknown-send delivery service", makeErr)
		}
		importedConfigurationWorkflowRun(t, ctx, rig.s, configurationWorkflowSender{Client: transport, service: unknownService, logger: logger}, botID, unknownID, true, logger)
		paymentSQL(t, rig.s, ctx, `SELECT d.kind='secret' AND d.outcome='unknown' AND d.attempt=1 AND o.status='failed' AND g.outbox_id=o.id AND g.attempt=1 FROM configuration_dispatches d JOIN outbox o ON o.id=d.outbox_id JOIN configuration_chat_guards g ON g.bot_id=o.bot_id AND g.chat_id=o.chat_id WHERE o.id=$1`, unknownID)
		peerMu.Lock()
		secrets, chats, denials := secretSends, chatGets, sentDenials
		peerMu.Unlock()
		if secrets != 2 || chats != 2 || denials != 0 {
			t.Fatal("synthetic unknown send did not receive one additional reserved personal URL")
		}

		if err = rig.s.SyncImportedRuntimePermission(ctx, botID, 2, prepared.PolicyHash(), prepared.SourceScopeHash(), false); err != nil {
			t.Fatal("disable imported runtime permission after unknown send", err)
		}
		if err = rig.s.SyncImportedRuntimePermission(ctx, botID, 3, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
			t.Fatal("re-enable imported runtime permission after unknown send", err)
		}
		if err = rig.s.InstallConfigurationPolicy(ctx, botID, configdelivery.Policy{}); err != nil {
			t.Fatal("disable protected configuration policy after unknown send", err)
		}
		restartedCfg := cfg
		restartedCfg.Revision = cfg.Revision + 2
		if err = rig.s.InstallConfigurationPolicy(ctx, botID, restartedCfg.Policy()); err != nil {
			t.Fatal("re-enable configuration policy after unknown send", err)
		}
		restartedID := makeRequest(88003, restartedCfg)
		beforeGETs := providerGETs.Load()
		peerMu.Lock()
		beforeChats := chatGets
		target = restartedID
		peerMu.Unlock()
		restartedService, makeErr := configdelivery.New(rig.s, configdelivery.Providers{Payments: settlement, SUI: panel, Recurring: recurring}, transport, botID, restartedCfg, logger)
		if makeErr != nil {
			t.Fatal("construct restarted-worker delivery service", makeErr)
		}
		importedConfigurationWorkflowRun(t, ctx, rig.s, configurationWorkflowSender{Client: transport, service: restartedService, logger: logger}, botID, restartedID, false, logger)
		if providerGETs.Load() != beforeGETs {
			t.Fatal("unknown chat barrier did not deny before provider GET")
		}
		peerMu.Lock()
		secrets, chats, denials = secretSends, chatGets, sentDenials
		peerMu.Unlock()
		if secrets != 2 || chats != beforeChats || denials != 1 {
			t.Fatal("runtime/configuration re-enable replayed a URL or rechecked the private chat")
		}
		paymentSQL(t, rig.s, ctx, `SELECT g.outbox_id=$2 AND g.attempt=1 AND d.outcome='unknown' AND o.status='failed' FROM configuration_chat_guards g JOIN configuration_dispatches d ON d.outbox_id=g.outbox_id JOIN outbox o ON o.id=g.outbox_id WHERE g.bot_id=$1 AND g.chat_id=42`, botID, unknownID)
		paymentSQL(t, rig.s, ctx, `SELECT imported_runtime_revision=1 FROM configuration_requests WHERE outbox_id=$1`, unknownID)
		paymentSQL(t, rig.s, ctx, `SELECT imported_runtime_revision=3 AND policy_revision=$2 FROM configuration_requests WHERE outbox_id=$1`, restartedID, restartedCfg.Revision)
		paymentSQL(t, rig.s, ctx, `SELECT d.kind='denial' AND d.outcome='sent' AND o.status='sent' FROM configuration_dispatches d JOIN outbox o ON o.id=d.outbox_id WHERE o.id=$1`, restartedID)
		paymentSQL(t, rig.s, ctx, `SELECT review_code=0 FROM payment_checks WHERE bot_id=$1 AND client_id=(SELECT id FROM clients WHERE telegram_id=42)`, botID)
		if posts := rig.source.postSnapshot(); len(posts) != 0 {
			t.Fatalf("unknown-send configuration workflow performed provider writes: %v", posts)
		}
		return
	}
	if scenario != "basic" {
		t.Fatalf("unknown imported configuration scenario %q", scenario)
	}

	// Delivery is configuration-secret-policy gated, not an imported billing
	// effect. No S-UI write capability is constructed or invoked in this test.
	if n, e := rig.s.SeedRenewalOperations(ctx, botID, []int64{42}, 10); e != nil || n != 0 {
		t.Fatalf("baseline accidentally created a renewal operation: count=%d err=%v", n, e)
	}

	// A freshly permissioned/menu-authorized request with a changed configured
	// public base is denied before any Invoice Ninja or S-UI provider GET.
	deniedCfg := cfg
	deniedCfg.Revision++
	deniedCfg.SubscriptionBaseURL = "https://other-subs.invalid/"
	if err = rig.s.InstallConfigurationPolicy(ctx, botID, deniedCfg.Policy()); err != nil {
		t.Fatal("install changed public-base policy", err)
	}
	beforeGETs := providerGETs.Load()
	deniedUpdate := menuTestUpdate(88002, 42, "/config")
	deniedSnapshot, dupe, e := rig.s.LoadMenuSnapshot(ctx, botID, deniedUpdate.ID, 42)
	if e != nil || dupe {
		t.Fatalf("load fresh denial menu snapshot: duplicate=%t err=%v", dupe, e)
	}
	deniedMenu, e := clientmenu.New(clientmenu.Config{ConfigurationEnabled: true, ConfigurationPolicyRevision: deniedCfg.Revision, ConfigurationPolicyHash: deniedCfg.Policy().Hash, ConfigurationAllowedTelegramIDs: []int64{42}}, nil)
	if e != nil {
		t.Fatal(e)
	}
	deniedPrepared, e := deniedMenu.Prepare(ctx, clientmenu.Request{BotID: botID, UpdateID: deniedUpdate.ID, TelegramID: 42, ChatID: 42, Action: clientmenu.Configuration}, deniedSnapshot)
	if e != nil || !deniedPrepared.ProtectedConfiguration {
		t.Fatalf("prepare fresh imported denial intent: %+v err=%v", deniedPrepared, e)
	}
	if e = rig.s.ApplyMenuUpdate(ctx, botID, menuTestUsername, deniedUpdate, deniedPrepared); e != nil {
		t.Fatal("commit fresh changed-public-base intent", e)
	}
	var deniedID int64
	if e = rig.s.pool.QueryRow(ctx, `SELECT outbox_id FROM configuration_requests WHERE bot_id=$1 AND origin_kind='imported' AND period_kind='imported_baseline' AND policy_revision=$2`, botID, deniedCfg.Revision).Scan(&deniedID); e != nil {
		t.Fatal("find fresh denial request", e)
	}
	peerMu.Lock()
	target = deniedID
	peerMu.Unlock()
	deniedService, e := configdelivery.New(rig.s, configdelivery.Providers{Payments: settlement, SUI: panel, Recurring: recurring}, transport, botID, deniedCfg, logger)
	if e != nil {
		t.Fatal("construct changed-public-base denial service", e)
	}
	importedConfigurationWorkflowRun(t, ctx, rig.s, configurationWorkflowSender{Client: transport, service: deniedService, logger: logger}, botID, deniedID, false, logger)
	if providerGETs.Load() != beforeGETs {
		t.Fatal("changed public-base policy reached a billing or S-UI GET provider")
	}
	peerMu.Lock()
	denialCount := sentDenials
	peerMu.Unlock()
	if denialCount != 1 {
		t.Fatal("changed public-base request did not produce one fixed nonsensitive denial")
	}
	paymentSQL(t, rig.s, ctx, `SELECT d.kind='denial' AND d.outcome='sent' AND o.status='sent' FROM configuration_dispatches d JOIN outbox o ON o.id=d.outbox_id WHERE o.id=$1`, deniedID)

	// A later, correctly rebound request that observes changed source payment
	// evidence is denied and commits the ordinary sticky 2007 review. This is
	// deliberately a new request after the original baseline confirmation.
	changedCfg := cfg
	changedCfg.Revision = deniedCfg.Revision + 1
	if e = rig.s.InstallConfigurationPolicy(ctx, botID, changedCfg.Policy()); e != nil {
		t.Fatal("restore matching public-base policy with new revision", e)
	}
	changedUpdate := menuTestUpdate(88003, 42, "/config")
	changedSnapshot, dupe, e := rig.s.LoadMenuSnapshot(ctx, botID, changedUpdate.ID, 42)
	if e != nil || dupe {
		t.Fatalf("load changed-proof request snapshot: duplicate=%t err=%v", dupe, e)
	}
	changedMenu, e := clientmenu.New(clientmenu.Config{ConfigurationEnabled: true, ConfigurationPolicyRevision: changedCfg.Revision, ConfigurationPolicyHash: changedCfg.Policy().Hash, ConfigurationAllowedTelegramIDs: []int64{42}}, nil)
	if e != nil {
		t.Fatal(e)
	}
	changedPrepared, e := changedMenu.Prepare(ctx, clientmenu.Request{BotID: botID, UpdateID: changedUpdate.ID, TelegramID: 42, ChatID: 42, Action: clientmenu.Configuration}, changedSnapshot)
	if e != nil || !changedPrepared.ProtectedConfiguration {
		t.Fatalf("prepare changed-proof request: %+v err=%v", changedPrepared, e)
	}
	if e = rig.s.ApplyMenuUpdate(ctx, botID, menuTestUsername, changedUpdate, changedPrepared); e != nil {
		t.Fatal("commit changed-proof request", e)
	}
	var changedID int64
	if e = rig.s.pool.QueryRow(ctx, `SELECT outbox_id FROM configuration_requests WHERE bot_id=$1 AND origin_kind='imported' AND policy_revision=$2`, botID, changedCfg.Revision).Scan(&changedID); e != nil {
		t.Fatal("find changed-proof request", e)
	}
	rig.source.mu.Lock()
	paymentDate, _ := rig.source.payment["date"].(string)
	parsedDate, parseErr := time.Parse(time.DateOnly, paymentDate)
	if parseErr == nil {
		rig.source.payment["date"] = parsedDate.AddDate(0, 0, 1).Format(time.DateOnly)
	}
	rig.source.mu.Unlock()
	if parseErr != nil {
		t.Fatal("parse synthetic source payment date")
	}
	peerMu.Lock()
	target = changedID
	peerMu.Unlock()
	changedService, e := configdelivery.New(rig.s, configdelivery.Providers{Payments: settlement, SUI: panel, Recurring: recurring}, transport, botID, changedCfg, logger)
	if e != nil {
		t.Fatal("construct changed-payment service", e)
	}
	importedConfigurationWorkflowRun(t, ctx, rig.s, configurationWorkflowSender{Client: transport, service: changedService, logger: logger}, botID, changedID, false, logger)
	paymentSQL(t, rig.s, ctx, `SELECT review_code=2007 FROM payment_checks WHERE client_id=(SELECT id FROM clients WHERE telegram_id=42) AND bot_id=$1`, botID)
	peerMu.Lock()
	denialCount = sentDenials
	peerMu.Unlock()
	if denialCount != 2 {
		t.Fatal("changed payment proof did not produce one further fixed denial")
	}
	paymentSQL(t, rig.s, ctx, `SELECT d.kind='denial' AND d.outcome='sent' AND o.status='sent' FROM configuration_dispatches d JOIN outbox o ON o.id=d.outbox_id WHERE o.id=$1`, changedID)
	if posts := rig.source.postSnapshot(); len(posts) != 0 {
		t.Fatalf("GET-only configuration workflow performed provider writes: %v", posts)
	}
	if strings.HasPrefix(scenario, "paused_billing_") {
		rig.source.mu.Lock()
		status := rig.source.recurring["status_id"]
		rig.source.mu.Unlock()
		if status != "3" {
			t.Fatal("import/configuration delivery resumed paused billing")
		}
	}
	paymentSQL(t, rig.s, ctx, `SELECT count(*)=0 FROM invoice_renewal_actions WHERE client_id=(SELECT id FROM clients WHERE telegram_id=42)`)
	paymentSQL(t, rig.s, ctx, `SELECT count(*)=0 FROM invoice_lifecycle_invoices WHERE client_id=(SELECT id FROM clients WHERE telegram_id=42)`)
}
