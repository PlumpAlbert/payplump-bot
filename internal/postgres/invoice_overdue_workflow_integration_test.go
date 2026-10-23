//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/lifecycle"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

const (
	overdueWorkflowNinjaToken = "synthetic-overdue-invoice-token"
	overdueWorkflowSUIToken   = "synthetic-overdue-panel-token"
)

type overdueWorkflowHTTP struct {
	t       *testing.T
	store   *Store
	client  int64
	bot     int64
	mu      sync.Mutex
	invoice invoiceninja.CycleInvoice
	record  invoiceninja.RecurringRecord
	panel   sui.ClientRecord
	unknown bool
	posts   []string
}

func TestInvoiceOverdueWorkerStoreAdaptersLoopbackWorkflow(t *testing.T) {
	t.Run("completes all writes before durable notices and does not replay", func(t *testing.T) {
		s, ctx, worker, provider := newOverdueWorkflowRig(t, false)
		done, stop := startOverdueWorkflowRun(t, ctx, worker)
		outcome, reviewCode := waitForOverdueOutcome(t, ctx, done, s, provider.handler.client, "completed")
		stop()
		if outcome != "completed" || reviewCode != 0 {
			t.Fatalf("action outcome=%q review_code=%d", outcome, reviewCode)
		}
		assertOverdueSuccessState(t, s, provider)

		// A fresh Run is a restart. Several bounded polls must not recreate or
		// redispatch the completed action.
		done, stop = startOverdueWorkflowRun(t, ctx, worker)
		waitForOverdueQuietRun(t, done)
		stop()
		if got := provider.handler.postSnapshot(); !reflect.DeepEqual(got, []string{"pause_recurring", "disable_sui", "close_sessions"}) {
			t.Fatalf("provider POST sequence after restart=%v", got)
		}
	})

	t.Run("unknown mutated recurring POST is reviewed and never replayed", func(t *testing.T) {
		s, ctx, worker, provider := newOverdueWorkflowRig(t, true)
		done, stop := startOverdueWorkflowRun(t, ctx, worker)
		outcome, reviewCode := waitForOverdueOutcome(t, ctx, done, s, provider.handler.client, "review")
		stop()
		if outcome != "review" || reviewCode != lifecycle.ReviewOverdueUnknownWrite {
			t.Fatalf("action outcome=%q review_code=%d", outcome, reviewCode)
		}

		// The API now reports the mutation, but a malformed/failed temporary
		// receipt remains unknown and cannot authorize replay.
		ctxRead, cancelRead := context.WithTimeout(ctx, 5*time.Second)
		paused, err := provider.recurring.Get(ctxRead, "recurring-synthetic")
		cancelRead()
		if err != nil || paused.StatusID != "3" {
			t.Fatalf("post-error recurring readback status=%q err=%v", paused.StatusID, err)
		}
		assertOverdueUnknownState(t, s, provider)

		done, stop = startOverdueWorkflowRun(t, ctx, worker)
		waitForOverdueQuietRun(t, done)
		stop()
		if got := provider.handler.postSnapshot(); !reflect.DeepEqual(got, []string{"pause_recurring"}) {
			t.Fatalf("provider POST sequence after unknown outcome=%v", got)
		}
	})
}

type overdueWorkflowProvider struct {
	handler   *overdueWorkflowHTTP
	recurring *invoiceninja.RecurringClient
}

func newOverdueWorkflowRig(t *testing.T, unknownPause bool) (*Store, context.Context, *lifecycle.EnforcementWorker, *overdueWorkflowProvider) {
	t.Helper()
	s, ctx := invoiceLifecycleTestStore(t)
	clientID, botID := seedInvoiceLifecycleAccount(t, s, ctx, false)
	yesterday := time.Now().In(time.Local).AddDate(0, 0, -1).Format(time.DateOnly)
	if _, err := s.pool.Exec(ctx, `UPDATE invoice_lifecycle_invoices SET sent_date = $1::date WHERE client_id = $2 AND invoice_id = 'invoice-current'`, yesterday, clientID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO invoice_lifecycle_checks (client_id, bot_id) VALUES ($1, $2)`, clientID, botID); err != nil {
		t.Fatal(err)
	}

	panel := overdueWorkflowSUIRecord(t)
	hash := panel.CredentialsHash()
	tag, err := s.pool.Exec(ctx, `UPDATE activation_sui_bindings SET name = $1, credentials_hash = $2
        WHERE client_id = $3 AND bot_id = $4 AND sui_id = 71`, panel.Name(), hash[:], clientID, botID)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("bind synthetic S-UI record: rows=%d err=%v", tag.RowsAffected(), err)
	}

	template := overdueWorkflowRecurringTemplate(t, s, ctx, clientID)
	record := overdueWorkflowRecurringRecord(template)
	invoice := invoiceninja.CycleInvoice{
		ID: "invoice-current", ClientID: "customer-synthetic", RecurringID: "recurring-synthetic",
		Number: "SYNTHETIC-CURRENT", PrivateNotes: activation.Marker("synthetic_activation_token_123456"), StatusID: "2",
		Date: yesterday, DueDate: yesterday, Amount: "100", Balance: "100", PaidToDate: "0", ExchangeRate: "1",
		Invitations: []invoiceninja.CycleInvitation{{ID: "invitation-synthetic", SentDate: yesterday + " 10:30:00"}},
	}
	handler := &overdueWorkflowHTTP{t: t, store: s, client: clientID, bot: botID, invoice: invoice, record: record, panel: panel, unknown: unknownPause}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	cfg := lifecycle.EnforcementConfig{
		Enabled: true, ExclusiveWriter: true, BaseURL: server.URL + "/ninja", Token: overdueWorkflowNinjaToken,
		InvoiceNinjaVersion: invoiceninja.SupportedRecurringVersion,
		SUIBaseURL:          server.URL + "/panel", SUIToken: overdueWorkflowSUIToken, SUIVersion: "1.6.4",
		AllowedTelegramIDs: []int64{101}, Workers: 1, MaxAccounts: 10,
		PollInterval: 100 * time.Millisecond, HTTPTimeout: 3 * time.Second,
	}
	cycle, err := invoiceninja.NewCycleReader(cfg.BaseURL, cfg.Token, cfg.HTTPTimeout, 1, 5)
	if err != nil {
		t.Fatal("construct real Invoice Ninja cycle reader")
	}
	recurring, err := invoiceninja.NewRecurringClient(cfg.BaseURL, cfg.Token, cfg.HTTPTimeout, 1, 5)
	if err != nil {
		t.Fatal("construct real Invoice Ninja recurring adapter")
	}
	panelClient, err := sui.NewManagedClient(cfg.SUIBaseURL, cfg.SUIToken, cfg.SUIVersion, cfg.HTTPTimeout, cfg.MaxAccounts)
	if err != nil {
		t.Fatal("construct real S-UI managed adapter")
	}
	worker, err := lifecycle.NewEnforcementWorker(s, cycle, recurring, panelClient, botID, []int64{9001}, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal("construct real overdue enforcement worker")
	}
	return s, ctx, worker, &overdueWorkflowProvider{handler: handler, recurring: recurring}
}

func overdueWorkflowRecurringTemplate(t *testing.T, s *Store, ctx context.Context, clientID int64) invoiceninja.RecurringDraft {
	t.Helper()
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT st.request FROM operations o
        JOIN activation_steps st ON st.operation_id = o.id
        WHERE o.client_id = $1 AND o.kind = 'activate_first_subscription' AND st.step = 'recurring_create'
          AND st.confirmed_at IS NOT NULL`, clientID).Scan(&raw)
	if err != nil {
		t.Fatal("load persisted recurring template")
	}
	var draft invoiceninja.RecurringDraft
	if err := json.Unmarshal(raw, &draft); err != nil || invoiceninja.ValidateRecurringDraft(draft) != nil {
		t.Fatalf("persisted recurring template is invalid: %v", err)
	}
	if draft.ClientID != "customer-synthetic" || draft.PrivateNotes != activation.Marker("synthetic_activation_token_123456") {
		t.Fatal("persisted recurring template does not match the managed account")
	}
	return draft
}

func overdueWorkflowRecurringRecord(d invoiceninja.RecurringDraft) invoiceninja.RecurringRecord {
	return invoiceninja.RecurringRecord{
		ID: "recurring-synthetic", ClientID: d.ClientID, StatusID: "2", PrivateNotes: d.PrivateNotes,
		FrequencyID: strconv.Itoa(d.FrequencyID), NextSendDate: d.NextSendDate,
		NextSendDatetime: d.NextSendDate + "T12:00:00Z", DueDateDays: d.DueDateDays,
		AutoBill: d.AutoBill, RemainingCycles: d.RemainingCycles, UsesInclusiveTaxes: d.UsesInclusiveTaxes,
		LineItems: append([]invoiceninja.InvoiceLine(nil), d.LineItems...),
		Amount:    "100", Balance: "100", ExchangeRate: "1", Discount: "0", IsAmountDiscount: false,
		TaxRate1: "0", TaxRate2: "0", TaxRate3: "0", CustomSurcharge1: "0", CustomSurcharge2: "0",
		CustomSurcharge3: "0", CustomSurcharge4: "0", Partial: "0", PaidToDate: "0",
	}
}

func overdueWorkflowSUIRecord(t *testing.T) sui.ClientRecord {
	t.Helper()
	draft, err := sui.NewDisabledDraft([]int64{7})
	if err != nil {
		t.Fatal("create synthetic S-UI draft")
	}
	raw, err := json.Marshal(draft)
	if err != nil {
		t.Fatal("encode synthetic S-UI draft")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal("decode synthetic S-UI draft")
	}
	fields["id"] = json.RawMessage("71")
	fields["enable"] = json.RawMessage("true")
	raw, err = json.Marshal(fields)
	if err != nil {
		t.Fatal("encode synthetic S-UI record")
	}
	record, err := sui.ParseClientRecord(raw)
	if err != nil || record.ID() != 71 || !record.Enabled() {
		t.Fatalf("parse synthetic S-UI record: id=%d enabled=%v err=%v", record.ID(), record.Enabled(), err)
	}
	return record
}

func (h *overdueWorkflowHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Accept") != "application/json" || r.Header.Get("Authorization") != "" {
		h.reject(w, r, "unexpected common request headers")
		return
	}
	switch {
	case strings.HasPrefix(r.URL.Path, "/ninja/api/v1/"):
		if r.Header.Get("X-API-TOKEN") != overdueWorkflowNinjaToken || r.Header.Get("X-Requested-With") != "XMLHttpRequest" || r.Header.Get("Token") != "" {
			h.reject(w, r, "Invoice Ninja authentication crossed provider boundaries")
			return
		}
		w.Header().Set("X-APP-VERSION", invoiceninja.SupportedRecurringVersion)
		h.serveNinja(w, r, strings.TrimPrefix(r.URL.Path, "/ninja/api/v1/"))
	case strings.HasPrefix(r.URL.Path, "/panel/apiv2/"):
		if r.Header.Get("Token") != overdueWorkflowSUIToken || r.Header.Get("X-API-TOKEN") != "" {
			h.reject(w, r, "S-UI authentication crossed provider boundaries")
			return
		}
		h.servePanel(w, r, strings.TrimPrefix(r.URL.Path, "/panel/apiv2/"))
	default:
		h.reject(w, r, "unexpected provider installation prefix")
	}
}

func (h *overdueWorkflowHTTP) serveNinja(w http.ResponseWriter, r *http.Request, route string) {
	if r.Method == http.MethodGet {
		if r.URL.RawQuery != "" {
			h.reject(w, r, "unexpected Invoice Ninja GET query")
			return
		}
		switch route {
		case "ping":
			writeOverdueJSON(w, map[string]any{"data": map[string]any{}})
		case "invoices/invoice-current":
			writeOverdueJSON(w, map[string]any{"data": h.invoice})
		case "recurring_invoices/recurring-synthetic":
			writeOverdueJSON(w, map[string]any{"data": h.record})
		default:
			h.reject(w, r, "unexpected Invoice Ninja GET route")
		}
		return
	}
	if r.Method != http.MethodPost || route != "recurring_invoices/bulk" || r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/json" {
		h.reject(w, r, "unexpected Invoice Ninja mutation route or format")
		return
	}
	var command struct {
		IDs    []string `json:"ids"`
		Action string   `json:"action"`
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 65537))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&command) != nil || decoder.Decode(new(any)) != io.EOF || !reflect.DeepEqual(command.IDs, []string{"recurring-synthetic"}) || command.Action != "stop" {
		h.reject(w, r, "recurring bulk command did not match the pinned stop contract")
		return
	}
	if !h.visibleIntent(r.Context(), lifecycle.OverduePauseRecurring, "recurring-synthetic", 0, "active", true) {
		h.reject(w, r, "recurring POST preceded committed intent and writer guard")
		return
	}
	h.posts = append(h.posts, "pause_recurring")
	h.record.StatusID = "3"
	if h.unknown {
		// The synthetic API mutates first but returns a temporary error. The
		// adapter must treat the receipt as unknown and the worker must HOLD.
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"synthetic temporary response"}`)
		return
	}
	writeOverdueJSON(w, map[string]any{"data": []invoiceninja.RecurringRecord{h.record}})
}

func (h *overdueWorkflowHTTP) servePanel(w http.ResponseWriter, r *http.Request, route string) {
	reply := func(obj any) { writeOverdueJSON(w, map[string]any{"success": true, "obj": obj}) }
	if r.Method == http.MethodGet {
		switch route {
		case "status":
			if r.URL.RawQuery != "r=sys" {
				h.reject(w, r, "S-UI version claim query did not match the pinned contract")
				return
			}
			reply(map[string]any{"sys": map[string]any{"appVersion": "1.6.4"}})
		case "clients":
			if r.URL.RawQuery != "id=71" {
				h.reject(w, r, "S-UI request was not an exact full-record read")
				return
			}
			reply(map[string]any{"clients": []sui.ClientRecord{h.panel}})
		default:
			h.reject(w, r, "unexpected S-UI GET route")
		}
		return
	}
	if r.Method != http.MethodPost || r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.ParseForm() != nil {
		h.reject(w, r, "unexpected S-UI mutation format")
		return
	}
	switch route {
	case "save":
		if len(r.PostForm) != 3 || r.PostForm.Get("object") != "clients" || r.PostForm.Get("action") != "edit" {
			h.reject(w, r, "S-UI save form did not match the pinned edit contract")
			return
		}
		updated, err := sui.ParseClientRecord([]byte(r.PostForm.Get("data")))
		if err != nil || updated.ID() != 71 || updated.Name() != h.panel.Name() || updated.Enabled() || !overdueWorkflowSameSUIExceptEnable(h.panel, updated) {
			h.reject(w, r, "S-UI edit changed fields other than enable=false")
			return
		}
		if !h.visibleIntent(r.Context(), lifecycle.OverdueDisableSUI, "71", 1, "active", true) {
			h.reject(w, r, "S-UI disable POST preceded committed intent and writer guard")
			return
		}
		h.posts = append(h.posts, "disable_sui")
		h.panel = updated
		reply(map[string]any{"clients": []map[string]any{{"id": 71, "name": h.panel.Name()}}})
	case "closeSessions":
		if len(r.PostForm) != 1 || r.PostForm.Get("u") != h.panel.Name() || h.panel.Enabled() {
			h.reject(w, r, "S-UI closeSessions did not target the bound disabled record")
			return
		}
		if !h.visibleIntent(r.Context(), lifecycle.OverdueCloseSessions, "71", 2, "overdue", false) {
			h.reject(w, r, "closeSessions POST preceded committed intent and writer guard")
			return
		}
		h.posts = append(h.posts, "close_sessions")
		writeOverdueJSON(w, map[string]any{"success": true, "msg": "ok"})
	default:
		h.reject(w, r, "unexpected S-UI mutation route")
	}
}

func (h *overdueWorkflowHTTP) visibleIntent(ctx context.Context, step lifecycle.EnforcementStep, externalID string, confirmed int, subscriptionState string, access bool) bool {
	var valid bool
	err := h.store.pool.QueryRow(ctx, `SELECT
        g.operation_id = o.id AND g.step = st.step AND g.epoch > 0
        AND o.kind = $1 AND o.status = 'running' AND o.locked_until > clock_timestamp()
        AND a.outcome = 'pending' AND a.review_code = 0
        AND st.dispatch_attempt = o.attempts AND st.external_id IS NULL AND st.confirmed_at IS NULL
        AND st.request->'before'->>'id' = $5
        AND sub.state = $6 AND sub.desired_access = $7
        AND (SELECT count(*) FROM invoice_overdue_steps WHERE operation_id = o.id AND confirmed_at IS NOT NULL) = $8
        AND (SELECT count(*) FROM invoice_overdue_steps WHERE operation_id = o.id) = $8 + 1
        AND (SELECT count(*) FROM external_write_intents WHERE operation_id = o.id AND origin = 'lifecycle') = $8 + 1
        FROM activation_write_guard g
        JOIN operations o ON o.id = g.operation_id
        JOIN invoice_overdue_actions a ON a.operation_id = o.id
        JOIN invoice_overdue_steps st ON st.operation_id = o.id AND st.step = g.step
        JOIN subscriptions sub ON sub.client_id = a.client_id
        JOIN external_write_intents e ON e.operation_id = st.operation_id AND e.step = st.step AND e.origin = 'lifecycle'
        WHERE g.id = 1 AND a.client_id = $2 AND a.bot_id = $3 AND a.invoice_id = 'invoice-current' AND g.step = $4`,
		lifecycle.OverdueOperationKind, h.client, h.bot, string(step), externalID, subscriptionState, access, confirmed).Scan(&valid)
	return err == nil && valid
}

func (h *overdueWorkflowHTTP) reject(w http.ResponseWriter, r *http.Request, message string) {
	h.t.Errorf("synthetic provider rejected %s %s: %s", r.Method, r.URL.Path, message)
	w.WriteHeader(http.StatusBadRequest)
	_, _ = io.WriteString(w, `{"error":"synthetic contract mismatch"}`)
}

func (h *overdueWorkflowHTTP) postSnapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.posts...)
}

func overdueWorkflowSameSUIExceptEnable(before, after sui.ClientRecord) bool {
	var left, right map[string]json.RawMessage
	if json.Unmarshal(before.Raw(), &left) != nil || json.Unmarshal(after.Raw(), &right) != nil {
		return false
	}
	left["enable"], right["enable"] = json.RawMessage("false"), json.RawMessage("false")
	return reflect.DeepEqual(left, right)
}

func writeOverdueJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func startOverdueWorkflowRun(t *testing.T, parent context.Context, worker *lifecycle.EnforcementWorker) (chan error, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("EnforcementWorker.Run returned error: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("EnforcementWorker.Run did not join after cancellation")
			}
		})
	}
	t.Cleanup(stop)
	return done, stop
}

func waitForOverdueOutcome(t *testing.T, parent context.Context, done chan error, s *Store, clientID int64, want string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 25*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		outcome, code, err := overdueActionResult(ctx, s, clientID)
		if err != nil {
			t.Fatal("read overdue action result")
		}
		if outcome == want {
			return outcome, code
		}
		select {
		case runErr := <-done:
			done <- runErr
			t.Fatalf("EnforcementWorker.Run exited before action reached %q: %v", want, runErr)
		case <-ctx.Done():
			t.Fatalf("timed out waiting for overdue outcome %q (last=%q)", want, outcome)
		case <-ticker.C:
		}
	}
}

func waitForOverdueQuietRun(t *testing.T, done chan error) {
	t.Helper()
	timer := time.NewTimer(750 * time.Millisecond)
	defer timer.Stop()
	select {
	case err := <-done:
		done <- err
		t.Fatalf("restarted EnforcementWorker.Run exited before cancellation: %v", err)
	case <-timer.C:
	}
}

func overdueActionResult(ctx context.Context, s *Store, clientID int64) (string, int, error) {
	var outcome string
	var code int
	err := s.pool.QueryRow(ctx, `SELECT COALESCE((SELECT outcome FROM invoice_overdue_actions
        WHERE client_id = $1 AND invoice_id = 'invoice-current'), ''),
        COALESCE((SELECT review_code FROM invoice_overdue_actions
        WHERE client_id = $1 AND invoice_id = 'invoice-current'), 0)`, clientID).Scan(&outcome, &code)
	return outcome, code, err
}

func assertOverdueSuccessState(t *testing.T, s *Store, provider *overdueWorkflowProvider) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var outcome, operationStatus, subscriptionState string
	var reviewCode, steps, confirmed, intents, messages int
	var access, guardBusy bool
	if err := s.pool.QueryRow(ctx, `SELECT a.outcome, a.review_code, o.status, sub.state, sub.desired_access,
        (SELECT count(*) FROM invoice_overdue_steps st WHERE st.operation_id = a.operation_id),
        (SELECT count(*) FROM invoice_overdue_steps st WHERE st.operation_id = a.operation_id AND st.confirmed_at IS NOT NULL),
        (SELECT count(*) FROM external_write_intents e WHERE e.operation_id = a.operation_id AND e.origin = 'lifecycle'),
        EXISTS (SELECT 1 FROM activation_write_guard g WHERE g.id = 1 AND g.operation_id IS NOT NULL)
        FROM invoice_overdue_actions a JOIN operations o ON o.id = a.operation_id
        JOIN subscriptions sub ON sub.client_id = a.client_id
        WHERE a.client_id = $1 AND a.invoice_id = 'invoice-current'`, provider.handler.client).Scan(
		&outcome, &reviewCode, &operationStatus, &subscriptionState, &access, &steps, &confirmed, &intents, &guardBusy); err != nil {
		t.Fatal("read completed overdue state")
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE bot_id = $1 AND message_key LIKE 'invoice-overdue:%:overdue:%'`, provider.handler.bot).Scan(&messages); err != nil {
		t.Fatal("read overdue success outbox")
	}
	if outcome != "completed" || reviewCode != 0 || operationStatus != "completed" || subscriptionState != "overdue" || access || steps != 3 || confirmed != 3 || intents != 3 || guardBusy || messages != 2 {
		t.Fatalf("completed state outcome=%q review=%d operation=%q subscription=%q access=%v steps=%d confirmed=%d intents=%d guard_busy=%v outbox=%d",
			outcome, reviewCode, operationStatus, subscriptionState, access, steps, confirmed, intents, guardBusy, messages)
	}
	var recipients []int64
	rows, err := s.pool.Query(ctx, `SELECT chat_id FROM outbox WHERE bot_id = $1 AND message_key LIKE 'invoice-overdue:%:overdue:%' ORDER BY chat_id`, provider.handler.bot)
	if err != nil {
		t.Fatal("list overdue success notices")
	}
	for rows.Next() {
		var recipient int64
		if err := rows.Scan(&recipient); err != nil {
			rows.Close()
			t.Fatal("scan overdue success recipient")
		}
		recipients = append(recipients, recipient)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal("iterate overdue success notices")
	}
	rows.Close()
	if !reflect.DeepEqual(recipients, []int64{101, 9001}) {
		t.Fatalf("success notice recipients=%v", recipients)
	}
	rows, err = s.pool.Query(ctx, `SELECT step, COALESCE(external_id, ''), confirmed_at IS NOT NULL, dispatch_attempt
        FROM invoice_overdue_steps st JOIN invoice_overdue_actions a USING (operation_id)
        WHERE a.client_id = $1 AND a.invoice_id = 'invoice-current'`, provider.handler.client)
	if err != nil {
		t.Fatal("read overdue write receipts")
	}
	receipts := make(map[string]struct {
		externalID string
		confirmed  bool
		attempt    int
	})
	for rows.Next() {
		var step, externalID string
		var confirmed bool
		var attempt int
		if err := rows.Scan(&step, &externalID, &confirmed, &attempt); err != nil {
			rows.Close()
			t.Fatal("scan overdue write receipt")
		}
		receipts[step] = struct {
			externalID string
			confirmed  bool
			attempt    int
		}{externalID, confirmed, attempt}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal("iterate overdue write receipts")
	}
	rows.Close()
	if len(receipts) != 3 || receipts["pause_recurring"].externalID != "recurring-synthetic" || !receipts["pause_recurring"].confirmed || receipts["pause_recurring"].attempt != 1 ||
		receipts["disable_sui"].externalID != "71" || !receipts["disable_sui"].confirmed || receipts["disable_sui"].attempt != 1 ||
		receipts["close_sessions"].externalID != "71" || !receipts["close_sessions"].confirmed || receipts["close_sessions"].attempt != 1 {
		t.Fatalf("durable overdue receipts=%v", receipts)
	}
	var unsent int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE bot_id = $1 AND message_key LIKE 'invoice-overdue:%:overdue:%'
        AND status = 'pending' AND attempts = 0`, provider.handler.bot).Scan(&unsent); err != nil || unsent != 2 {
		t.Fatalf("outbox sender unexpectedly consumed notices: pending=%d err=%v", unsent, err)
	}
	provider.handler.mu.Lock()
	paused := provider.handler.record.StatusID == "3"
	disabled := !provider.handler.panel.Enabled()
	provider.handler.mu.Unlock()
	if !paused || !disabled || !reflect.DeepEqual(provider.handler.postSnapshot(), []string{"pause_recurring", "disable_sui", "close_sessions"}) {
		t.Fatalf("provider state paused=%v disabled=%v posts=%v", paused, disabled, provider.handler.postSnapshot())
	}
}

func assertOverdueUnknownState(t *testing.T, s *Store, provider *overdueWorkflowProvider) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var outcome, operationStatus, subscriptionState, guardStep string
	var reviewCode, intents, unconfirmed, adminMessages, clientMessages int
	var access, guardOwned bool
	if err := s.pool.QueryRow(ctx, `SELECT a.outcome, a.review_code, o.status, sub.state, sub.desired_access,
        (g.operation_id = a.operation_id), COALESCE(g.step, ''),
        (SELECT count(*) FROM external_write_intents e WHERE e.operation_id = a.operation_id AND e.origin = 'lifecycle'),
        (SELECT count(*) FROM invoice_overdue_steps st WHERE st.operation_id = a.operation_id AND st.confirmed_at IS NULL)
        FROM invoice_overdue_actions a JOIN operations o ON o.id = a.operation_id
        JOIN subscriptions sub ON sub.client_id = a.client_id CROSS JOIN activation_write_guard g
        WHERE a.client_id = $1 AND a.invoice_id = 'invoice-current' AND g.id = 1`, provider.handler.client).Scan(
		&outcome, &reviewCode, &operationStatus, &subscriptionState, &access, &guardOwned, &guardStep, &intents, &unconfirmed); err != nil {
		t.Fatal("read unknown-write review state")
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE chat_id = 9001), count(*) FILTER (WHERE chat_id = 101)
        FROM outbox WHERE bot_id = $1 AND message_key LIKE 'invoice-overdue:%:review:%'`, provider.handler.bot).Scan(&adminMessages, &clientMessages); err != nil {
		t.Fatal("read unknown-write review outbox")
	}
	if outcome != "review" || reviewCode != lifecycle.ReviewOverdueUnknownWrite || operationStatus != "completed" || subscriptionState != "active" || !access || !guardOwned || guardStep != "pause_recurring" || intents != 1 || unconfirmed != 1 || adminMessages != 1 || clientMessages != 0 {
		t.Fatalf("unknown state outcome=%q review=%d operation=%q subscription=%q access=%v guard=%v/%q intents=%d unconfirmed=%d admin=%d client=%d",
			outcome, reviewCode, operationStatus, subscriptionState, access, guardOwned, guardStep, intents, unconfirmed, adminMessages, clientMessages)
	}
	if got := provider.handler.postSnapshot(); !reflect.DeepEqual(got, []string{"pause_recurring"}) {
		t.Fatalf("unknown outcome sent further provider writes: %v", got)
	}
}
