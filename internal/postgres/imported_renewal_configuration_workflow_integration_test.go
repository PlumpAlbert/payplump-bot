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
	"sync/atomic"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/clientmenu"
	"github.com/PlumpAlbert/payplump-bot/internal/configdelivery"
	"github.com/PlumpAlbert/payplump-bot/internal/importadmission"
	"github.com/PlumpAlbert/payplump-bot/internal/importverify"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/lifecycle"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
	"github.com/PlumpAlbert/payplump-bot/templates"
)

// A completed native imported renewal remains eligible for protected
// configuration delivery even though billing effects have been turned off.
func TestCompletedImportedRenewalAllowsProtectedConfiguration(t *testing.T) {
	runImportedPaidRenewalHappy(t, false, false, func(rig *importedOverdueRig, prepared importadmission.PreparedPlan, botID int64) {
		writesBefore := len(rig.source.postSnapshot())
		assertImportedRenewalProtectedConfiguration(t, importedConfigurationScenario{
			store: rig.s, ctx: rig.ctx, prepared: prepared, botID: botID, ninjaBase: rig.ninjaBase,
			suiBase: strings.TrimSuffix(rig.ninjaBase, "/ninja") + "/panel", invoiceID: "invoice-imported-new", originInvoiceID: admissionFixtureInv,
			ninjaToken: importedOverdueNinjaToken, suiToken: importedOverdueSUIToken,
			sourceWrites: func() int { return len(rig.source.postSnapshot()) }, writesBefore: writesBefore,
		})
	})
}

func TestCompletedImportedInitialRestorationAllowsProtectedConfiguration(t *testing.T) {
	runInitialRestorationWorkflow(t, true, false, func(store *Store, ctx context.Context, prepared importadmission.PreparedPlan, src *initialRestorationSource, botID int64, _ payments.Snapshot) {
		writesBefore := initialRestorationWriteCount(src)
		assertImportedRenewalProtectedConfiguration(t, importedConfigurationScenario{
			store: store, ctx: ctx, prepared: prepared, botID: botID, ninjaBase: src.baseNinja,
			suiBase: src.baseSUI, invoiceID: "debt-current", originInvoiceID: "debt-current", ninjaToken: initialRestorationNinjaToken,
			suiToken:     initialRestorationSUIToken,
			sourceWrites: func() int { return initialRestorationWriteCount(src) }, writesBefore: writesBefore,
		})
	})
}

func TestUnknownInitialRestorationDeniesConfiguration(t *testing.T) {
	runInitialRestorationWorkflow(t, true, true, func(store *Store, ctx context.Context, prepared importadmission.PreparedPlan, src *initialRestorationSource, botID int64, snapshot payments.Snapshot) {
		assertUnknownInitialRestorationConfigurationDenied(t, store, ctx, prepared, src, botID, snapshot)
	})
}

func TestCompletedImportedRenewalStaleConfigurationRevisionDenied(t *testing.T) {
	runImportedPaidRenewalHappy(t, false, false, func(rig *importedOverdueRig, prepared importadmission.PreparedPlan, botID int64) {
		var revisionBefore int64
		if err := rig.s.pool.QueryRow(rig.ctx, `SELECT configuration_revision FROM clients WHERE telegram_id=42`).Scan(&revisionBefore); err != nil {
			t.Fatal(err)
		}
		assertImportedRenewalProtectedConfiguration(t, importedConfigurationScenario{
			store: rig.s, ctx: rig.ctx, prepared: prepared, botID: botID, ninjaBase: rig.ninjaBase,
			suiBase: strings.TrimSuffix(rig.ninjaBase, "/ninja") + "/panel", invoiceID: "invoice-imported-new", originInvoiceID: admissionFixtureInv,
			ninjaToken: importedOverdueNinjaToken, suiToken: importedOverdueSUIToken, staleRevision: true,
			sourceWrites: func() int { return len(rig.source.postSnapshot()) }, writesBefore: len(rig.source.postSnapshot()), revisionBefore: revisionBefore,
			afterChat: func(callCtx context.Context) error {
				if _, err := rig.s.pool.Exec(callCtx, `UPDATE clients SET chat_id=43 WHERE telegram_id=42`); err != nil {
					return err
				}
				_, err := rig.s.pool.Exec(callCtx, `UPDATE clients SET chat_id=42 WHERE telegram_id=42`)
				return err
			},
		})
	})
}

var importedConfigurationDenialText = templates.Render("configdelivery/denied", templates.Data{})

func initialRestorationWriteCount(src *initialRestorationSource) int {
	src.mu.Lock()
	defer src.mu.Unlock()
	return len(src.steps)
}

func assertUnknownInitialRestorationConfigurationDenied(t *testing.T, store *Store, ctx context.Context, prepared importadmission.PreparedPlan, src *initialRestorationSource, botID int64, paymentSnapshot payments.Snapshot) {
	t.Helper()
	if paymentSnapshot.InvoiceID != "debt-current" {
		t.Fatalf("unknown restoration snapshot invoice=%q", paymentSnapshot.InvoiceID)
	}
	paymentSQL(t, store, ctx, `SELECT io.lifecycle='paused' AND io.current_invoice_id='debt-current' AND io.paid_anchor_invoice_id='paid-historical' AND io.current_invoice_id<>io.paid_anchor_invoice_id AND a.origin_mode='imported_initial_paused' AND a.ledger_invoice_id IS NULL AND a.outcome='review' AND a.review_code=$1 FROM verified_import_origins io JOIN invoice_renewal_actions a ON a.baseline_origin_client_id=io.client_id WHERE io.bot_id=$2 AND a.invoice_id='debt-current' AND encode(io.source_scope_hash,'hex')=$3`, lifecycle.ReviewRenewalUnknownWrite, botID, prepared.SourceScopeHash())
	paymentSQL(t, store, ctx, `SELECT s.state='overdue' AND NOT s.desired_access AND s.first_invoice_id IS NULL AND s.current_invoice_id='debt-current' AND g.operation_id=a.operation_id AND g.step='resume_recurring' FROM subscriptions s JOIN invoice_renewal_actions a ON a.client_id=s.client_id JOIN activation_write_guard g ON g.id=1 WHERE a.invoice_id='debt-current' AND a.outcome='review'`)
	src.mu.Lock()
	providerActive := src.recurring["status_id"] == "2" && src.suiRecord.Enabled()
	steps := append([]string(nil), src.steps...)
	src.mu.Unlock()
	if !providerActive || len(steps) != 3 || steps[2] != "resume_recurring" {
		t.Fatalf("unknown resume did not mutate native provider before ambiguity: active=%v steps=%v", providerActive, steps)
	}
	update := menuTestUpdate(88991, 42, "/config")
	identity, action, ok := update.PrivateMenu(menuTestUsername)
	if !ok || action != clientmenu.Configuration || identity.TelegramID != 42 {
		t.Fatal("unknown-restoration /config update was not private and actor-scoped")
	}
	snapshot, duplicate, err := store.LoadMenuSnapshot(ctx, botID, update.ID, 42)
	if err != nil || duplicate || !snapshot.Known || snapshot.State != "overdue" || snapshot.DesiredAccess {
		t.Fatalf("unknown restoration yielded secret-eligible local menu state: %+v duplicate=%v err=%v", snapshot, duplicate, err)
	}
	cfg := clientmenu.Config{ConfigurationEnabled: true, ConfigurationPolicyRevision: 1, ConfigurationPolicyHash: [32]byte{1}, ConfigurationAllowedTelegramIDs: []int64{42}}
	menu, err := clientmenu.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	preparedMenu, err := menu.Prepare(ctx, clientmenu.Request{BotID: botID, UpdateID: update.ID, TelegramID: 42, ChatID: 42, Action: action}, snapshot)
	if err != nil || preparedMenu.ProtectedConfiguration || preparedMenu.Text != clientmenu.ConfigurationUnavailableText {
		t.Fatalf("unknown native action produced protected menu intent: %+v err=%v", preparedMenu, err)
	}
	if err = store.ApplyMenuUpdate(ctx, botID, menuTestUsername, update, preparedMenu); err != nil {
		t.Fatal("commit ordinary fixed denial for unknown native restoration", err)
	}
	key := preparedMenu.Request.Key()
	paymentSQL(t, store, ctx, `SELECT body=$2 AND client_menu AND NOT protected_configuration FROM outbox WHERE bot_id=$1 AND message_key=$3`, botID, clientmenu.ConfigurationUnavailableText, key)
	paymentSQL(t, store, ctx, `SELECT count(*)=0 FROM configuration_requests WHERE bot_id=$1`, botID)
	paymentSQL(t, store, ctx, `SELECT count(*)=0 FROM configuration_dispatches d JOIN configuration_requests r ON r.outbox_id=d.outbox_id WHERE r.bot_id=$1`, botID)
	paymentSQL(t, store, ctx, `SELECT g.operation_id=a.operation_id AND g.step='resume_recurring' AND a.outcome='review' AND a.origin_mode='imported_initial_paused' AND a.ledger_invoice_id IS NULL FROM activation_write_guard g JOIN invoice_renewal_actions a ON a.operation_id=g.operation_id WHERE g.id=1`)
}

type importedConfigurationScenario struct {
	store                                                                *Store
	ctx                                                                  context.Context
	prepared                                                             importadmission.PreparedPlan
	botID                                                                int64
	ninjaBase, suiBase, invoiceID, originInvoiceID, ninjaToken, suiToken string
	staleRevision                                                        bool
	afterChat                                                            func(context.Context) error
	sourceWrites                                                         func() int
	writesBefore                                                         int
	revisionBefore                                                       int64
}

func assertImportedRenewalProtectedConfiguration(t *testing.T, args importedConfigurationScenario) {
	t.Helper()
	const publicBase = "https://subs.invalid/"
	store, ctx, botID := args.store, args.ctx, args.botID
	ninjaBase, suiBase, invoiceID, originInvoiceID := args.ninjaBase, args.suiBase, args.invoiceID, args.originInvoiceID
	cfg := configdelivery.Config{
		Enabled: true, TelegramEnabled: true, PaymentsEnabled: true, Revision: 1,
		BaseURL: ninjaBase, Token: args.ninjaToken, CurrencyID: "1", TelegramIDField: "id_number",
		SUIBaseURL: suiBase, SUIToken: args.suiToken, SUIVersion: "1.6.4",
		SubscriptionBaseURL: publicBase, AllowedTelegramIDs: []int64{42}, HTTPTimeout: time.Second, PageSize: 10, MaxPages: 2,
	}
	sourceScope, err := importverify.SourceScope(cfg.BaseURL, cfg.Token, "5.13.47", cfg.SUIBaseURL, cfg.SUIToken, cfg.SUIVersion)
	if err != nil || sourceScope != args.prepared.SourceScopeHash() {
		t.Fatalf("configuration GET clients differ from the admitted source scope: match=%v err=%v", sourceScope == args.prepared.SourceScopeHash(), err)
	}
	publicHash := sha256.Sum256([]byte(publicBase))
	var evidencePinned bool
	if err := store.pool.QueryRow(ctx, `SELECT encode(decode(evidence->>'public_base_hash','hex'),'hex')=$2 FROM verified_import_origins WHERE bot_id=$1 AND current_invoice_id=$3`, botID, fmt.Sprintf("%x", publicHash), originInvoiceID).Scan(&evidencePinned); err != nil || !evidencePinned {
		t.Fatalf("verified origin does not pin the configured public URL base: pinned=%v err=%v", evidencePinned, err)
	}
	for batch := 0; batch < 20; batch++ {
		messages, err := store.ClaimOutbox(ctx, botID, 50, time.Minute)
		if err != nil {
			t.Fatalf("claim preexisting setup outbox messages: %v", err)
		}
		for _, message := range messages {
			if message.ProtectedConfiguration {
				t.Fatalf("unexpected protected preexisting outbox message %d", message.ID)
			}
			if message.Text == queue.ConfigurationIntentBody || strings.Contains(message.Text, cfg.SubscriptionBaseURL) {
				t.Fatalf("unsafe preexisting setup outbox message %d", message.ID)
			}
			if err := store.CompleteOutbox(ctx, message.Lease, 9); err != nil {
				t.Fatalf("complete preexisting ordinary setup outbox message %d: %v", message.ID, err)
			}
		}
		if len(messages) == 0 {
			break
		}
		if batch == 19 {
			t.Fatal("preexisting setup outbox drain exceeded 20 batches")
		}
	}
	if err := store.InstallConfigurationPolicy(ctx, botID, cfg.Policy()); err != nil {
		t.Fatal("install protected imported-renewal configuration policy", err)
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

	var targetOutboxID atomic.Int64
	var secretSends, denialSends atomic.Int64
	var capturedURL atomic.Value
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body map[string]json.RawMessage
		if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Errorf("unexpected synthetic Telegram request: %s", r.Method)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var chat int64
		if json.Unmarshal(body["chat_id"], &chat) != nil || chat != 42 {
			t.Error("configuration delivery escaped private chat")
			http.Error(w, "bad chat", http.StatusBadRequest)
			return
		}
		if r.URL.Path == "/botSYNTHETIC/getChat" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"id": chat, "type": "private"}})
			return
		}
		if r.URL.Path != "/botSYNTHETIC/sendMessage" || string(body["protect_content"]) != "true" || len(body) != 5 {
			t.Error("unexpected or unprotected Telegram send")
			http.Error(w, "bad send", http.StatusBadRequest)
			return
		}
		var text string
		if json.Unmarshal(body["text"], &text) != nil {
			t.Error("Telegram request is missing protected text")
			http.Error(w, "bad text", http.StatusBadRequest)
			return
		}
		if text == importedConfigurationDenialText {
			denialSends.Add(1)
		} else {
			const urlText = "https://subs.invalid/42?format=json"
			u, parseErr := url.Parse(urlText)
			if !strings.Contains(text, urlText) || parseErr != nil || u.Scheme != "https" || u.Host != "subs.invalid" || u.EscapedPath() != "/42" || u.RawQuery != "format=json" || u.Fragment != "" || text != templates.Render("configdelivery/subscription", templates.Data{SubscriptionURL: urlText}) {
				t.Error("Telegram received a noncanonical personal URL")
				http.Error(w, "bad URL", http.StatusBadRequest)
				return
			}
			capturedURL.Store(urlText)
			secretSends.Add(1)
			var reserved bool
			if err := store.pool.QueryRow(r.Context(), `SELECT d.kind='secret' AND d.outcome='reserved' AND d.attempt=o.attempts AND g.outbox_id=o.id AND o.status='running' FROM configuration_dispatches d JOIN outbox o ON o.id=d.outbox_id JOIN configuration_chat_guards g ON g.bot_id=o.bot_id AND g.chat_id=o.chat_id WHERE o.id=$1`, targetOutboxID.Load()).Scan(&reserved); err != nil || !reserved {
				t.Errorf("protected URL was not preceded by committed reservation: reserved=%v err=%v", reserved, err)
				http.Error(w, "missing reservation", http.StatusConflict)
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": 78, "chat": map[string]any{"id": chat, "type": "private"}}})
	}))
	defer peer.Close()

	originalTransport := http.DefaultTransport
	local := originalTransport.(*http.Transport).Clone()
	local.Proxy = nil
	defer local.CloseIdleConnections()
	peerURL, _ := url.Parse(peer.URL)
	providerURL, _ := url.Parse(strings.TrimSuffix(ninjaBase, "/ninja"))
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
		return nil, errors.New("synthetic imported-renewal configuration blocked nonlocal HTTP")
	})
	defer func() { http.DefaultTransport = originalTransport }()

	update := menuTestUpdate(88901, 42, "/config")
	identity, action, ok := update.PrivateMenu(menuTestUsername)
	if !ok || identity.TelegramID != 42 || action != clientmenu.Configuration {
		t.Fatal("private /config update was not actor-scoped")
	}
	snapshot, duplicate, err := store.LoadMenuSnapshot(ctx, botID, update.ID, identity.TelegramID)
	if err != nil || duplicate || !snapshot.Known || snapshot.State != "active" || !snapshot.DesiredAccess {
		t.Fatalf("load completed-renewal menu snapshot: %+v duplicate=%t err=%v", snapshot, duplicate, err)
	}
	if args.staleRevision && snapshot.ConfigurationRevision != args.revisionBefore {
		t.Fatalf("request snapshot revision=%d, expected captured current revision=%d", snapshot.ConfigurationRevision, args.revisionBefore)
	}
	policy := cfg.Policy()
	menu, err := clientmenu.New(clientmenu.Config{ConfigurationEnabled: true, ConfigurationPolicyRevision: policy.Revision, ConfigurationPolicyHash: policy.Hash, ConfigurationAllowedTelegramIDs: []int64{42}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	preparedMenu, err := menu.Prepare(ctx, clientmenu.Request{BotID: botID, UpdateID: update.ID, TelegramID: 42, ChatID: 42, Action: action}, snapshot)
	if err != nil || !preparedMenu.ProtectedConfiguration || preparedMenu.Text != queue.ConfigurationIntentBody {
		t.Fatalf("prepare protected renewal configuration request: %+v err=%v", preparedMenu, err)
	}
	if err = store.ApplyMenuUpdate(ctx, botID, menuTestUsername, update, preparedMenu); err != nil {
		t.Fatal("commit protected configuration intent", err)
	}
	var outboxID int64
	if err = store.pool.QueryRow(ctx, `SELECT r.outbox_id FROM configuration_requests r JOIN outbox o ON o.id=r.outbox_id WHERE r.bot_id=$1 AND r.origin_kind='imported' AND r.period_kind='renewal' AND r.invoice_id=$2 ORDER BY o.id DESC LIMIT 1`, botID, invoiceID).Scan(&outboxID); err != nil {
		t.Fatal("find completed-renewal configuration request", err)
	}
	targetOutboxID.Store(outboxID)
	paymentSQL(t, store, ctx, `SELECT r.origin_kind='imported' AND r.period_kind='renewal' AND r.activation_operation_id IS NULL AND r.renewal_operation_id=a.operation_id AND r.invoice_id=$2 AND r.proof_hash=pc.proof_hash AND pc.invoice_id=$2 AND io.current_invoice_id=$3 AND ((a.origin_mode='ledger' AND a.ledger_invoice_id=$2 AND $2<>$3) OR (a.origin_mode='imported_initial_paused' AND a.ledger_invoice_id IS NULL AND $2=$3)) FROM configuration_requests r JOIN invoice_renewal_actions a ON a.operation_id=r.renewal_operation_id AND a.outcome='completed' JOIN payment_confirmations pc ON pc.client_id=r.client_id AND pc.bot_id=r.bot_id AND pc.invoice_id=r.invoice_id JOIN verified_import_origins io ON io.client_id=r.imported_origin_client_id AND io.bot_id=r.bot_id WHERE r.outbox_id=$1`, outboxID, invoiceID, originInvoiceID)

	transport := telegram.NewClient("SYNTHETIC")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	serviceTransport := configurationWorkflowTransport{Client: transport, afterChat: args.afterChat}
	service, err := configdelivery.New(store, configdelivery.Providers{Payments: settlement, SUI: panel, Recurring: recurring}, serviceTransport, botID, cfg, logger)
	if err != nil {
		t.Fatal("construct actual imported-renewal configuration service", err)
	}
	sender := configurationWorkflowSender{Client: transport, service: service, logger: logger}
	importedConfigurationWorkflowRun(t, ctx, store, sender, botID, outboxID, false, logger)
	if args.staleRevision {
		if denialSends.Load() != 1 || secretSends.Load() != 0 {
			t.Fatalf("stale configuration revision sends: denial=%d secret=%d", denialSends.Load(), secretSends.Load())
		}
		paymentSQL(t, store, ctx, `SELECT d.kind='denial' AND d.outcome='sent' AND o.status='sent' AND g.outbox_id IS NULL FROM configuration_dispatches d JOIN outbox o ON o.id=d.outbox_id JOIN configuration_chat_guards g ON g.bot_id=o.bot_id AND g.chat_id=o.chat_id WHERE o.id=$1`, outboxID)
		var requestRevision, currentRevision int64
		if err = store.pool.QueryRow(ctx, `SELECT client_revision FROM configuration_requests WHERE outbox_id=$1`, outboxID).Scan(&requestRevision); err != nil {
			t.Fatal(err)
		}
		if err = store.pool.QueryRow(ctx, `SELECT configuration_revision FROM clients WHERE telegram_id=42`).Scan(&currentRevision); err != nil || requestRevision != args.revisionBefore || currentRevision <= requestRevision {
			t.Fatalf("stale request/current revisions=%d/%d expected old=%d err=%v", requestRevision, currentRevision, args.revisionBefore, err)
		}
		paymentSQL(t, store, ctx, `SELECT review_code=0 FROM payment_checks WHERE bot_id=$1 AND client_id=(SELECT id FROM clients WHERE telegram_id=42)`, botID)
	} else {
		if denialSends.Load() != 0 || secretSends.Load() != 1 || capturedURL.Load() != "https://subs.invalid/42?format=json" {
			t.Fatalf("known renewal configuration sends: denial=%d secret=%d canonical_url=%t", denialSends.Load(), secretSends.Load(), capturedURL.Load() == "https://subs.invalid/42?format=json")
		}
		paymentSQL(t, store, ctx, `SELECT d.kind='secret' AND d.outcome='sent' AND o.status='sent' AND g.outbox_id IS NULL FROM configuration_dispatches d JOIN outbox o ON o.id=d.outbox_id JOIN configuration_chat_guards g ON g.bot_id=o.bot_id AND g.chat_id=o.chat_id WHERE o.id=$1`, outboxID)
		paymentSQL(t, store, ctx, `SELECT body=$2 AND body NOT LIKE '%https://%' AND body NOT LIKE '%private-path%' FROM outbox WHERE id=$1`, outboxID, queue.ConfigurationIntentBody)
	}
	if args.sourceWrites != nil && args.sourceWrites() != args.writesBefore {
		t.Fatalf("configuration delivery performed native provider writes: before=%d after=%d", args.writesBefore, args.sourceWrites())
	}
	paymentSQL(t, store, ctx, `SELECT count(*)=0 FROM operations WHERE client_id=(SELECT id FROM clients WHERE telegram_id=42) AND kind IN ('register_client','activate_first_subscription')`)
}
