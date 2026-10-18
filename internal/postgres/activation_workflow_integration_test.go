//go:build integration

package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

type activationWorkflowEvent struct {
	stage string
	err   error
	code  int
	final bool
}

// Observe committed results, not elapsed time. Cancellation happens AFTER the
// final event; the caller always joins Run, including its permit holders.
type observedActivationStore struct {
	*Store
	events chan activationWorkflowEvent
	cancel context.CancelFunc
	idle   bool
}

func (s *observedActivationStore) report(e activationWorkflowEvent) {
	s.events <- e
	if e.final {
		s.cancel()
	}
}

func (s *observedActivationStore) FinishActivation(ctx context.Context, lease queue.Lease, snapshot activation.Snapshot, today string, drafts []queue.Draft) error {
	err := s.Store.FinishActivation(ctx, lease, snapshot, today, drafts)
	s.report(activationWorkflowEvent{stage: "finish", err: err, final: true})
	return err
}

func (s *observedActivationStore) HoldActivation(ctx context.Context, lease queue.Lease, code int, drafts []queue.Draft) error {
	err := s.Store.HoldActivation(ctx, lease, code, drafts)
	s.report(activationWorkflowEvent{stage: "hold", err: err, code: code, final: true})
	return err
}

func (s *observedActivationStore) RetryOperation(ctx context.Context, lease queue.Lease, delay time.Duration, code int) error {
	err := s.Store.RetryOperation(ctx, lease, delay, code)
	s.report(activationWorkflowEvent{stage: "retry", err: err, code: code, final: err != nil || delay != 0 || code != 0})
	return err
}

func (s *observedActivationStore) SeedActivations(ctx context.Context, botID int64, allowed []int64, policy [32]byte) (int, error) {
	count, err := s.Store.SeedActivations(ctx, botID, allowed, policy)
	if err != nil {
		s.report(activationWorkflowEvent{stage: "seed", err: err, final: true})
	}
	return count, err
}

func (s *observedActivationStore) ClaimActivations(ctx context.Context, botID int64, limit int) ([]queue.Operation, error) {
	work, err := s.Store.ClaimActivations(ctx, botID, limit)
	if s.idle || err != nil {
		code := len(work)
		s.report(activationWorkflowEvent{stage: "idle", err: err, code: code, final: true})
		// An unexpected claim must not dispatch after the test's idle barrier.
		return nil, err
	}
	return work, err
}

const (
	activationWorkflowNinjaToken = "synthetic-activation-ninja-token"
	activationWorkflowPanelToken = "synthetic-activation-panel-token"
)

// Only this HTTP peer is synthetic. All four provider capabilities and every
// journal/lease/payment/outbox operation below use their production types.
type activationWorkflowHTTP struct {
	mu        sync.Mutex
	t         *testing.T
	store     *Store
	server    *httptest.Server
	cfg       activation.Config
	providers activation.Providers
	logs      bytes.Buffer
	clientID  int64
	botID     int64
	proof     *payments.Proof
	baseEpoch int64
	mode      string

	customer  invoiceninja.SettlementCustomer
	invoice   invoiceninja.SettlementInvoice
	cash      []invoiceninja.SettlementPayment
	product   invoiceninja.Product
	panel     sui.ClientRecord
	draft     sui.CreateDraft
	recurring *invoiceninja.RecurringRecord

	writes          []activation.Step
	mutations, gets int
	paymentPages    [2]int
	fullReadbacks   int
	flipped         bool
	negativeReads   int
	startReads      int
}

func newActivationWorkflowHTTP(t *testing.T, s *Store, ctx context.Context, botID, telegramID int64, mode string) *activationWorkflowHTTP {
	t.Helper()
	ids := seedPaymentAccounts(t, s, ctx, botID, int(telegramID), 1)
	seedPaymentChecks(t, s, ctx, botID, 1)
	claim, snapshot := claimSinglePayment(t, s, ctx, botID)
	date := time.Now().In(time.Local).Format(time.DateOnly)
	proof := paymentProof(t, snapshot, date, "100")
	p := &activationWorkflowHTTP{t: t, store: s, clientID: ids[0], botID: botID, proof: proof, mode: mode,
		customer: invoiceninja.SettlementCustomer{ID: snapshot.CustomerID, IDNumber: strconv.FormatInt(telegramID, 10)},
		invoice: invoiceninja.SettlementInvoice{ID: snapshot.InvoiceID, ClientID: snapshot.CustomerID, Number: proof.InvoiceNumber,
			StatusID: "4", Date: date, DueDate: date, Amount: "100", Balance: "0", PaidToDate: "100", ExchangeRate: "1"},
		product: invoiceninja.Product{ID: "synthetic-monthly", ProductKey: "monthly", Notes: "Synthetic monthly service", Price: "100", TaxRate1: "0", TaxRate2: "0", TaxRate3: "0"},
	}
	// The actual cash allocation is on page TWO. An unrelated, unallocated
	// payment on page one must neither hide the cash nor enter its proof hash.
	p.cash = []invoiceninja.SettlementPayment{
		{ID: "payment-0", ClientID: snapshot.CustomerID, Date: date, StatusID: "4", CurrencyID: "1", Amount: "25", Refunded: "0", Applied: "0", ExchangeRate: "1", IsManual: true, Allocations: []invoiceninja.SettlementAllocation{}},
		{ID: "payment-1", ClientID: snapshot.CustomerID, Date: date, StatusID: "4", CurrencyID: "1", Amount: "100", Refunded: "0", Applied: "100", ExchangeRate: "1", IsManual: true,
			Allocations: []invoiceninja.SettlementAllocation{{ID: "allocation-1", PaymentID: "payment-1", InvoiceID: snapshot.InvoiceID, Amount: "100", Refunded: "0"}}},
	}
	decision := payments.Evaluate(snapshot, p.customer, p.invoice, p.cash, "1", date)
	if decision.Validate() != nil || decision.Kind != payments.Verified || decision.Proof.Hash != proof.Hash {
		t.Fatal("fixture does not reproduce the exact canonical payment proof")
	}
	if err := s.pool.QueryRow(ctx, `SELECT epoch FROM activation_write_guard WHERE id=1`).Scan(&p.baseEpoch); err != nil {
		t.Fatal("read synthetic writer epoch")
	}
	p.server = httptest.NewServer(http.HandlerFunc(p.serveHTTP))
	t.Cleanup(p.server.Close)
	p.cfg = activation.Config{
		Enabled: true, TelegramEnabled: true, PaymentsEnabled: true, ExclusiveWriter: true, TaxModeConfirmed: true, InclusiveTaxes: false,
		BaseURL: p.server.URL + "/ninja", Token: activationWorkflowNinjaToken, ProductID: p.product.ID, CurrencyID: "1", TelegramIDField: "id_number",
		SUIBaseURL: p.server.URL + "/panel", SUIToken: activationWorkflowPanelToken, SUIVersion: "1.6.4", InboundIDs: []int64{7}, AllowedTelegramIDs: []int64{telegramID},
		Workers: 1, PageSize: 1, MaxPages: 10, MaxAccounts: 10, PollInterval: 100 * time.Millisecond, HTTPTimeout: 5 * time.Second,
	}
	settlement, err := invoiceninja.NewSettlementClient(p.cfg.BaseURL, p.cfg.Token, p.cfg.HTTPTimeout, p.cfg.PageSize, p.cfg.MaxPages)
	if err != nil {
		t.Fatal("construct settlement reader")
	}
	products, err := invoiceninja.New(p.cfg.BaseURL, p.cfg.Token, p.cfg.HTTPTimeout)
	if err != nil {
		t.Fatal("construct product reader")
	}
	recurring, err := invoiceninja.NewRecurringClient(p.cfg.BaseURL, p.cfg.Token, p.cfg.HTTPTimeout, p.cfg.PageSize, p.cfg.MaxPages)
	if err != nil {
		t.Fatal("construct recurring adapter")
	}
	panel, err := sui.NewManagedClient(p.cfg.SUIBaseURL, p.cfg.SUIToken, p.cfg.SUIVersion, p.cfg.HTTPTimeout, p.cfg.MaxAccounts)
	if err != nil {
		t.Fatal("construct managed panel adapter")
	}
	p.providers = activation.Providers{Payments: settlement, Products: products, SUI: panel, Recurring: recurring}
	observed, err := payments.Observe(ctx, settlement, snapshot, p.cfg.CurrencyID, date)
	if err != nil || observed.Validate() != nil || observed.Kind != payments.Verified || observed.Proof.Hash != proof.Hash {
		t.Fatal("real settlement adapter did not reproduce the fixture proof")
	}
	// Do not insert a confirmation directly: registration OWN provenance and
	// the immutable first-payment facts pass through the real payment finalizer.
	if err := s.FinishPaymentCheck(ctx, claim, snapshot, payments.Decision{Kind: payments.Verified, Proof: proof}, paymentNotices(snapshot), time.Minute); err != nil {
		t.Fatal("confirm synthetic first payment")
	}
	return p
}

func (p *activationWorkflowHTTP) reject(w http.ResponseWriter, message string) {
	p.t.Error(message) // Never print request bodies, config, credentials or URLs.
	w.WriteHeader(http.StatusBadRequest)
}

func (p *activationWorkflowHTTP) serveHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet {
		p.mutations++
	} else {
		p.gets++
	}
	if r.Header.Get("Accept") != "application/json" || r.Header.Get("Authorization") != "" {
		p.reject(w, "unsupported synthetic provider headers")
		return
	}
	switch {
	case strings.HasPrefix(r.URL.Path, "/ninja/api/v1/"):
		if r.Header.Get("X-API-TOKEN") != p.cfg.Token || r.Header.Get("Token") != "" || r.Header.Get("X-Requested-With") != "XMLHttpRequest" {
			p.reject(w, "billing authentication crossed provider boundaries")
			return
		}
		w.Header().Set("X-APP-VERSION", "5.13.47")
		p.serveNinja(w, r, strings.TrimPrefix(r.URL.Path, "/ninja/api/v1/"))
	case strings.HasPrefix(r.URL.Path, "/panel/apiv2/"):
		if r.Header.Get("Token") != p.cfg.SUIToken || r.Header.Get("X-API-TOKEN") != "" {
			p.reject(w, "panel authentication crossed provider boundaries")
			return
		}
		p.servePanel(w, r, strings.TrimPrefix(r.URL.Path, "/panel/apiv2/"))
	default:
		p.reject(w, "provider installation prefix was lost")
	}
}

func activationWorkflowList(w http.ResponseWriter, data any, total, page, pageSize int) {
	pages := max(1, (total+pageSize-1)/pageSize)
	count := min(pageSize, max(0, total-(page-1)*pageSize))
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "meta": map[string]any{"pagination": invoiceninja.Pagination{
		Total: total, Count: count, PerPage: pageSize, CurrentPage: page, TotalPages: pages,
	}}})
}

func (p *activationWorkflowHTTP) serveNinja(w http.ResponseWriter, r *http.Request, route string) {
	if r.Method == http.MethodGet {
		var data any
		switch route {
		case "clients/" + p.customer.ID:
			data = map[string]any{"id": p.customer.ID, "id_number": p.customer.IDNumber, "settings": map[string]any{}, "group_settings_id": "", "is_deleted": false, "archived_at": 0}
		case "invoices/" + p.invoice.ID:
			if p.flipped {
				p.negativeReads++
			}
			data = p.invoice
		case "products/" + p.product.ID:
			data = p.product
		case "payments":
			q := r.URL.Query()
			page, err := strconv.Atoi(q.Get("page"))
			if err != nil || page < 1 || page > 2 || len(q) != 6 || q.Get("client_id") != p.customer.ID || q.Get("include") != "invoices" || q.Get("status") != "active,archived,deleted" || q.Get("sort") != "id|asc" || q.Get("per_page") != "1" {
				p.reject(w, "payment inventory was not complete exact-client pagination")
				return
			}
			p.paymentPages[page-1]++
			activationWorkflowList(w, p.cash[page-1:page], 2, page, p.cfg.PageSize)
			return
		case "recurring_invoices":
			want := url.Values{"client_id": {p.customer.ID}, "status": {"active,archived,deleted"}, "client_status": {"all"}, "sort": {"id|asc"}, "per_page": {"1"}, "page": {"1"}}
			if r.URL.RawQuery != want.Encode() {
				p.reject(w, "recurring inventory was not exact complete client scope")
				return
			}
			rows := []invoiceninja.RecurringRecord{}
			if p.recurring != nil {
				rows = append(rows, *p.recurring)
			}
			activationWorkflowList(w, rows, len(rows), 1, p.cfg.PageSize)
			return
		case "ping":
			data = map[string]any{}
		default:
			if p.recurring == nil || route != "recurring_invoices/"+p.recurring.ID {
				p.reject(w, "unexpected billing GET or foreign entity")
				return
			}
			if p.mode == "drift" && p.panel.Enabled() {
				p.startReads++
				if p.startReads == 2 { // Adapter's last GET, after worker verified the original receipt.
					p.recurring.Amount, p.recurring.Balance = "999", "999"
					p.flipped = true
				}
			}
			data = *p.recurring
		}
		if r.URL.RawQuery != "" {
			p.reject(w, "unexpected individual billing query")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
		return
	}
	if r.Method != http.MethodPost || r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/json" {
		p.reject(w, "unexpected billing mutation format")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 65537))
	if err != nil || len(raw) > 65536 {
		p.reject(w, "invalid bounded billing body")
		return
	}
	switch route {
	case "recurring_invoices":
		var draft invoiceninja.RecurringDraft
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&draft) != nil || invoiceninja.ValidateRecurringDraft(draft) != nil || p.recurring != nil || draft.ClientID != p.customer.ID || draft.NextSendDate != p.proof.NextInvoiceDate || draft.LineItems[0].Cost != p.product.Price {
			p.reject(w, "recurring draft did not freeze the exact positive product policy")
			return
		}
		if !p.visiblePermit(r.Context(), activation.RecurringCreate, raw) {
			p.reject(w, "recurring POST preceded committed matching permission")
			return
		}
		record := activationWorkflowRecurring(draft, "recurring-"+p.customer.ID)
		// Force the response through the same strict decoder as a real GET.
		encoded, _ := json.Marshal(record)
		var strict invoiceninja.RecurringRecord
		if json.Unmarshal(encoded, &strict) != nil || invoiceninja.ValidateRecurringForDraft(strict, draft) != nil {
			p.reject(w, "synthetic recurring response is not a full valid DTO")
			return
		}
		p.recurring = &strict
		p.writes = append(p.writes, activation.RecurringCreate)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": strict})
	case "recurring_invoices/bulk":
		var request activation.StartRequest
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&request) != nil || p.recurring == nil || request.Action != "start" || !slices.Equal(request.IDs, []string{p.recurring.ID}) || p.recurring.StatusID != "1" || !p.panel.Enabled() {
			p.reject(w, "unsupported recurring bulk action")
			return
		}
		if !p.visiblePermit(r.Context(), activation.RecurringStart, raw) {
			p.reject(w, "start POST preceded committed matching permission")
			return
		}
		// Source-pinned first start changes ONLY status. In particular, do not
		// generate an ordinary invoice or advance the first future schedule.
		p.recurring.StatusID = "2"
		// Carbon may add/remove fractional zeros: the UTC instant is unchanged.
		p.recurring.NextSendDatetime = p.proof.NextInvoiceDate + "T12:00:00.000000Z"
		p.writes = append(p.writes, activation.RecurringStart)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []invoiceninja.RecurringRecord{*p.recurring}})
	default:
		p.reject(w, "ordinary invoice, email, clone or other billing write attempted")
	}
}

func activationWorkflowRecurring(d invoiceninja.RecurringDraft, id string) invoiceninja.RecurringRecord {
	return invoiceninja.RecurringRecord{
		ID: id, ClientID: d.ClientID, StatusID: "1", PrivateNotes: d.PrivateNotes, FrequencyID: strconv.Itoa(d.FrequencyID),
		NextSendDate: d.NextSendDate, NextSendDatetime: d.NextSendDate + "T12:00:00Z", DueDateDays: d.DueDateDays,
		AutoBill: d.AutoBill, RemainingCycles: d.RemainingCycles, UsesInclusiveTaxes: d.UsesInclusiveTaxes, LineItems: d.LineItems,
		Amount: d.LineItems[0].Cost, Balance: d.LineItems[0].Cost, Discount: "0", TaxRate1: "0", TaxRate2: "0", TaxRate3: "0",
		CustomSurcharge1: "0", CustomSurcharge2: "0", CustomSurcharge3: "0", CustomSurcharge4: "0", Partial: "0", PaidToDate: "0", ExchangeRate: "1",
	}
}

func (p *activationWorkflowHTTP) servePanel(w http.ResponseWriter, r *http.Request, route string) {
	reply := func(obj any) { _ = json.NewEncoder(w).Encode(map[string]any{"success": true, "obj": obj}) }
	if r.Method == http.MethodGet {
		switch route {
		case "status":
			if r.URL.RawQuery != "r=sys" {
				p.reject(w, "missing panel version scope")
				return
			}
			if p.mode == "unpaid" && p.recurring != nil && !p.panel.Enabled() {
				// This is the enable adapter's LAST preflight, after its full
				// ownership reads. Its authorization callback must now re-read
				// financial evidence, before reserving even a third intent.
				p.invoice.StatusID, p.invoice.Balance, p.invoice.PaidToDate = "2", "100", "0"
				p.flipped = true
			}
			reply(map[string]any{"sys": map[string]any{"appVersion": "1.6.4"}})
		case "inbounds":
			if r.URL.RawQuery != "" {
				p.reject(w, "unexpected inbound query")
				return
			}
			reply(map[string]any{"inbounds": []any{map[string]any{"id": 7, "type": "vless"}}})
		case "clients":
			list := []any{}
			if r.URL.RawQuery == "" {
				if p.panel.ID() != 0 {
					list = append(list, map[string]any{"id": p.panel.ID(), "name": p.panel.Name()})
				}
			} else if r.URL.RawQuery == "id=17" && p.panel.ID() == 17 {
				p.fullReadbacks++
				list = append(list, p.panel)
			} else {
				p.reject(w, "panel full read was not the allocated exact identity")
				return
			}
			reply(map[string]any{"clients": list})
		default:
			p.reject(w, "unexpected panel GET")
		}
		return
	}
	if r.Method != http.MethodPost || route != "save" || r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.ParseForm() != nil || len(r.PostForm) != 3 || r.PostForm.Get("object") != "clients" {
		p.reject(w, "unexpected panel mutation format")
		return
	}
	raw := []byte(r.PostForm.Get("data"))
	switch r.PostForm.Get("action") {
	case "new":
		if p.panel.ID() != 0 || json.Unmarshal(raw, &p.draft) != nil {
			p.reject(w, "panel create was not a complete disabled draft")
			return
		}
		if !p.visiblePermit(r.Context(), activation.SUICreate, raw) {
			p.reject(w, "panel create preceded committed matching permission")
			return
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			p.reject(w, "invalid panel create payload")
			return
		}
		fields["id"] = json.RawMessage("17")
		encoded, _ := json.Marshal(fields)
		record, err := sui.ParseClientRecord(encoded)
		if err != nil || sui.ValidateRecordForDraft(record, p.draft, false) != nil {
			p.reject(w, "allocated panel record did not preserve the exact posted draft")
			return
		}
		p.panel = record
		p.writes = append(p.writes, activation.SUICreate)
		if p.mode == "unknown" {
			// The save persisted, but even a later mandatory positive readback
			// must not turn this malformed HTTP 200 into a known receipt.
			_, _ = io.WriteString(w, `{"success":true,"obj":`)
			return
		}
	case "edit":
		record, err := sui.ParseClientRecord(raw)
		if err != nil || p.panel.ID() != 17 || p.panel.Enabled() || !record.Enabled() || sui.ValidateRecordForDraft(record, p.draft, true) != nil {
			p.reject(w, "panel edit changed more than enable")
			return
		}
		if !p.visiblePermit(r.Context(), activation.SUIEnable, raw) {
			p.reject(w, "panel enable preceded committed matching permission")
			return
		}
		p.panel = record
		p.writes = append(p.writes, activation.SUIEnable)
	default:
		p.reject(w, "unsupported panel mutation action")
		return
	}
	reply(map[string]any{"clients": []any{map[string]any{"id": p.panel.ID(), "name": p.panel.Name()}}})
}

// This independent SQL read runs inside the HTTP handler, before the provider
// accepts a POST. An uncommitted preparation, wrong payload/owner, stale epoch,
// premature local entitlement or missing previous confirmation fails the test.
func (p *activationWorkflowHTTP) visiblePermit(ctx context.Context, step activation.Step, raw []byte) bool {
	index := len(p.writes)
	if index >= 4 || activation.OrderedSteps()[index] != step {
		return false
	}
	var ok bool
	err := p.store.pool.QueryRow(ctx, `SELECT
        g.operation_id=o.id AND g.step=st.step AND g.epoch=$5
        AND o.status='running' AND o.locked_until>clock_timestamp() AND st.dispatch_attempt=o.attempts
        AND st.external_id IS NULL AND st.confirmed_at IS NULL
        AND f.bot_id=$2 AND pc.proof_hash=$6 AND f.proof_hash=pc.proof_hash AND NOT pc.review_required
        AND c.sui_id IS NULL AND sub.recurring_id IS NULL AND sub.state='awaiting_payment' AND NOT sub.desired_access
        AND (SELECT count(*)=$7 FROM activation_steps WHERE operation_id=o.id AND confirmed_at IS NOT NULL)
        AND (SELECT count(*)=$7+1 FROM activation_steps WHERE operation_id=o.id)
        AND NOT EXISTS (SELECT 1 FROM outbox WHERE bot_id=$2 AND message_key LIKE 'activation:%')
        AND CASE WHEN st.step='sui_enable' THEN jsonb_set(st.request->'before', '{enable}', 'true'::jsonb)
            ELSE st.request END = $4::jsonb
        FROM operations o JOIN activation_flows f ON f.operation_id=o.id
        JOIN activation_steps st ON st.operation_id=o.id AND st.step=$3
        JOIN payment_confirmations pc ON pc.invoice_id=f.invoice_id
        JOIN clients c ON c.id=o.client_id JOIN subscriptions sub ON sub.client_id=c.id
        CROSS JOIN activation_write_guard g WHERE o.client_id=$1 AND o.kind='activate_first_subscription'`,
		p.clientID, p.cfgBotID(), string(step), string(raw), p.baseEpoch+int64(index)+1, p.proof.Hash[:], index).Scan(&ok)
	return err == nil && ok
}

func (p *activationWorkflowHTTP) cfgBotID() int64 { return p.botID }

func (p *activationWorkflowHTTP) run(t *testing.T, ctx context.Context, idle bool) []activationWorkflowEvent {
	t.Helper()
	started := time.Now()
	service, cancel := context.WithTimeout(ctx, 90*time.Second)
	observed := &observedActivationStore{Store: p.store, events: make(chan activationWorkflowEvent, 16), cancel: cancel, idle: idle}
	worker, err := activation.New(observed, p.providers, p.cfgBotID(), []int64{99001, 99002}, p.cfg, slog.New(slog.NewTextHandler(&p.logs, nil)))
	if err != nil {
		cancel()
		t.Fatal("construct real activation worker")
	}
	done := make(chan error, 1)
	go func() { done <- worker.Run(service) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("activation Run returned an error")
			}
		case <-time.After(10 * time.Second):
			t.Error("activation Run did not join its permit holders within ten seconds")
		}
		t.Logf("activation worker run: %s", time.Since(started).Round(time.Millisecond))
	}()
	var events []activationWorkflowEvent
	for {
		select {
		case event := <-observed.events:
			events = append(events, event)
			if event.final {
				return events
			}
		case <-service.Done():
			// All events preceding report's cancellation are already queued.
			// Drain them even if the receiver was descheduled until shutdown.
			for {
				select {
				case event := <-observed.events:
					events = append(events, event)
					if event.final {
						return events
					}
				default:
					t.Fatal("activation did not produce a final durable observation")
				}
			}
		}
	}
}

func activationWorkflowExpectEvents(t *testing.T, events []activationWorkflowEvent, retries int, final string, code int) {
	t.Helper()
	if len(events) != retries+1 {
		t.Fatalf("durable activation events=%d, want=%d", len(events), retries+1)
	}
	for i, event := range events {
		stage, wantCode := "retry", 0
		if i == retries {
			stage, wantCode = final, code
		}
		if event.err != nil || event.stage != stage || event.code != wantCode {
			t.Fatalf("unexpected durable activation event at index %d (stage=%s code=%d)", i, event.stage, event.code)
		}
	}
}

func (p *activationWorkflowHTTP) assertWrites(t *testing.T, want []activation.Step) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if !slices.Equal(p.writes, want) || p.mutations != len(want) {
		t.Fatalf("provider mutation order/count differs: accepted=%d attempted=%d want=%d", len(p.writes), p.mutations, len(want))
	}
	if p.paymentPages[0] == 0 || p.paymentPages[0] != p.paymentPages[1] {
		t.Fatal("payment evidence did not consume complete inventories")
	}
}

func (p *activationWorkflowHTTP) assertOriginalPayment(t *testing.T, ctx context.Context, reviewed bool) {
	t.Helper()
	paymentSQL(t, p.store, ctx, `SELECT pc.proof_hash=$2 AND pc.evidence=$3::jsonb AND pc.amount=100
        AND pc.payment_date=$4::date AND pc.next_invoice_date=$5::date AND pc.review_required=$6
        AND pc.review_code=CASE WHEN $6 THEN 2007 ELSE 0 END
        AND pc.invoice_id=sub.first_invoice_id AND pc.invoice_id=sub.current_invoice_id
        AND sub.payment_date=pc.payment_date AND sub.next_invoice_date=pc.next_invoice_date
        AND checkup.review_code=CASE WHEN $6 THEN 2007 ELSE 0 END
        FROM payment_confirmations pc JOIN subscriptions sub ON sub.client_id=pc.client_id
        JOIN payment_checks checkup ON checkup.client_id=pc.client_id WHERE pc.client_id=$1`,
		p.clientID, p.proof.Hash[:], string(p.proof.JSON), p.proof.PaymentDate, p.proof.NextInvoiceDate, reviewed)
}

func (p *activationWorkflowHTTP) assertNotices(t *testing.T, ctx context.Context, want int) {
	t.Helper()
	rows, err := p.store.pool.Query(ctx, `SELECT chat_id, message_key, body FROM outbox WHERE bot_id=$1 AND message_key LIKE 'activation:%' ORDER BY id`, p.cfgBotID())
	if err != nil {
		t.Fatal("read activation notices")
	}
	defer rows.Close()
	secrets := []string{p.server.URL, p.cfg.Token, p.cfg.SUIToken, p.draft.Name()}
	if p.recurring != nil {
		secrets = append(secrets, p.recurring.PrivateNotes)
	}
	var draft struct {
		Config map[string]map[string]any `json:"config"`
	}
	raw, _ := json.Marshal(p.draft)
	if json.Unmarshal(raw, &draft) != nil {
		t.Fatal("decode credential leak sentinels")
	}
	for _, config := range draft.Config {
		for _, key := range []string{"password", "uuid", "auth_str", "userkey"} {
			if secret, ok := config[key].(string); ok {
				secrets = append(secrets, secret)
			}
		}
	}
	count := 0
	seen := map[int64]bool{}
	for rows.Next() {
		var chatID int64
		var key, body string
		if rows.Scan(&chatID, &key, &body) != nil {
			t.Fatal("decode activation notice")
		}
		if seen[chatID] || !slices.Contains([]int64{p.cfg.AllowedTelegramIDs[0], 99001, 99002}, chatID) {
			t.Fatal("duplicate or wrongly scoped activation notice")
		}
		seen[chatID] = true
		for _, value := range append(secrets, "http://", "https://") {
			if value != "" && (strings.Contains(body, value) || strings.Contains(key, value) || strings.Contains(p.logs.String(), value)) {
				t.Fatal("activation notice or log leaked provider credentials or URL")
			}
		}
		count++
	}
	if rows.Err() != nil || count != want {
		t.Fatalf("activation notices=%d want=%d", count, want)
	}
}

func TestActivationWorkflowWithSyntheticHTTP(t *testing.T) {
	s, fixtureCtx := activationTestStore(t)
	// Reuse one isolated schema. The normal fixture's 90s deadline covers
	// setup; these multi-attempt real-adapter scenarios get a local suite budget.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(fixtureCtx), 5*time.Minute)
	defer cancel()
	for index, mode := range []string{"complete", "unpaid", "drift", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			p := newActivationWorkflowHTTP(t, s, ctx, 8201+int64(index), 48001+int64(index), mode)
			events := p.run(t, ctx, false)
			switch mode {
			case "complete":
				activationWorkflowExpectEvents(t, events, 4, "finish", 0)
				p.assertWrites(t, activation.OrderedSteps())
				p.assertOriginalPayment(t, ctx, false)
				hash := p.draft.CredentialsHash()
				_, canonicalUTC, err := activation.NormalizeRecurringFacts(p.recurring.Amount, p.recurring.NextSendDatetime)
				if err != nil {
					t.Fatal("known recurring receipt has invalid facts")
				}
				paymentSQL(t, s, ctx, `SELECT c.sui_id='17' AND sub.recurring_id=$2 AND sub.state='active' AND sub.desired_access
                    AND b.sui_id=17 AND b.name=$3 AND b.credentials_hash=$4 AND b.bot_id=$5 AND b.operation_id=o.id
                    AND o.status='completed' AND o.completed_at IS NOT NULL AND o.locked_until IS NULL AND f.review_code=0
                    AND (SELECT count(*)=4 AND bool_and(confirmed_at IS NOT NULL) FROM activation_steps WHERE operation_id=o.id)
                    AND (SELECT bool_and(external_id=CASE WHEN step IN ('sui_create','sui_enable') THEN '17' ELSE $2 END)
                        FROM activation_steps WHERE operation_id=o.id)
                    AND (SELECT bool_and(CASE WHEN step IN ('recurring_create','recurring_start')
                        THEN recurring_amount=$6::numeric AND recurring_datetime=$7
                        ELSE recurring_amount IS NULL AND recurring_datetime IS NULL END)
                        FROM activation_steps WHERE operation_id=o.id)
                    AND (SELECT operation_id IS NULL AND step IS NULL AND epoch=4 FROM activation_write_guard)
                    AND c.xmin=o.xmin AND sub.xmin=o.xmin AND b.xmin=o.xmin
                    AND (SELECT count(*)=3 AND bool_and(xmin=o.xmin) FROM outbox WHERE bot_id=$5 AND message_key LIKE 'activation:%')
                    FROM clients c JOIN subscriptions sub ON sub.client_id=c.id JOIN operations o ON o.client_id=c.id AND o.kind='activate_first_subscription'
                    JOIN activation_flows f ON f.operation_id=o.id JOIN activation_sui_bindings b ON b.client_id=c.id WHERE c.id=$1`,
					p.clientID, p.recurring.ID, p.draft.Name(), hash[:], p.cfgBotID(), p.recurring.Amount.String(), canonicalUTC)
				if !p.panel.Enabled() || p.recurring.StatusID != "2" || p.recurring.NextSendDate != p.proof.NextInvoiceDate || canonicalUTC != p.proof.NextInvoiceDate+"T12:00:00Z" || p.recurring.Amount != p.product.Price || p.recurring.Balance != p.product.Price || p.fullReadbacks == 0 {
					t.Fatal("completed provider state changed its first-cycle contract")
				}
				p.assertNotices(t, ctx, 3)
				// Run through a real seed AND empty claim, rather than sleeping
				// until an assumed no-work interval has passed.
				activationWorkflowExpectEvents(t, p.run(t, ctx, true), 0, "idle", 0)
				p.assertWrites(t, activation.OrderedSteps())
				p.assertNotices(t, ctx, 3)
				paymentSQL(t, s, ctx, `SELECT (SELECT count(*)=1 FROM activation_flows WHERE bot_id=$1)
                    AND (SELECT count(*)=4 FROM outbox WHERE bot_id=$1)`, p.cfgBotID())
			case "unpaid":
				activationWorkflowExpectEvents(t, events, 2, "hold", activation.ReviewPayment)
				p.assertWrites(t, activation.OrderedSteps()[:2])
				p.assertOriginalPayment(t, ctx, true)
				if !p.flipped || p.negativeReads == 0 || p.panel.Enabled() || p.recurring == nil || p.recurring.StatusID != "1" {
					t.Fatal("enable preflight did not force a fresh authoritative financial rejection")
				}
				paymentSQL(t, s, ctx, `SELECT f.review_code=3002 AND o.status='pending' AND o.next_attempt_at='infinity'::timestamptz
                    AND o.locked_until IS NULL AND c.sui_id IS NULL AND sub.recurring_id IS NULL
                    AND sub.state='awaiting_payment' AND NOT sub.desired_access
                    AND (SELECT count(*)=2 AND bool_and(confirmed_at IS NOT NULL) FROM activation_steps WHERE operation_id=o.id)
                    AND (SELECT operation_id IS NULL AND step IS NULL AND epoch=$2 FROM activation_write_guard)
                    FROM operations o JOIN activation_flows f ON f.operation_id=o.id JOIN clients c ON c.id=o.client_id
                    JOIN subscriptions sub ON sub.client_id=c.id WHERE c.id=$1`, p.clientID, p.baseEpoch+2)
				p.assertNotices(t, ctx, 2)
			case "drift":
				activationWorkflowExpectEvents(t, events, 3, "hold", activation.ReviewRecurring)
				p.assertWrites(t, activation.OrderedSteps()[:3])
				p.assertOriginalPayment(t, ctx, false)
				if !p.flipped || p.startReads != 2 || !p.panel.Enabled() || p.recurring.StatusID != "1" {
					t.Fatal("latest adapter GET did not expose drift before Start")
				}
				paymentSQL(t, s, ctx, `SELECT f.review_code=3005 AND o.status='pending' AND o.next_attempt_at='infinity'::timestamptz
                    AND c.sui_id IS NULL AND sub.recurring_id IS NULL AND NOT sub.desired_access
                    AND (SELECT count(*)=3 AND bool_and(confirmed_at IS NOT NULL) FROM activation_steps WHERE operation_id=o.id)
                    AND (SELECT operation_id IS NULL AND epoch=$2 FROM activation_write_guard)
                    AND (SELECT recurring_amount=100 FROM activation_steps WHERE operation_id=o.id AND step='recurring_create')
                    FROM operations o JOIN activation_flows f ON f.operation_id=o.id JOIN clients c ON c.id=o.client_id
                    JOIN subscriptions sub ON sub.client_id=c.id WHERE c.id=$1`, p.clientID, p.baseEpoch+3)
				p.assertNotices(t, ctx, 2)
			case "unknown":
				activationWorkflowExpectEvents(t, events, 0, "hold", activation.ReviewUnknownWrite)
				p.assertWrites(t, activation.OrderedSteps()[:1])
				p.assertOriginalPayment(t, ctx, false)
				if p.panel.ID() != 17 || p.panel.Enabled() || p.fullReadbacks == 0 || p.recurring != nil {
					t.Fatal("unknown create lacked mandatory positive disabled readback")
				}
				assertBarrier := func(attempt int) {
					t.Helper()
					raw, _ := json.Marshal(p.draft)
					paymentSQL(t, s, ctx, `SELECT f.review_code=3006 AND o.status='pending' AND o.next_attempt_at='infinity'::timestamptz
                        AND o.attempts=$2 AND o.locked_until IS NULL AND c.sui_id IS NULL AND sub.recurring_id IS NULL
                        AND sub.state='awaiting_payment' AND NOT sub.desired_access
                        AND g.operation_id=o.id AND g.step='sui_create' AND g.epoch=$3
                        AND st.request=$4::jsonb AND st.external_id IS NULL AND st.confirmed_at IS NULL AND st.dispatch_attempt=1
                        AND (SELECT count(*)=1 FROM activation_steps WHERE operation_id=o.id)
                        AND NOT EXISTS(SELECT 1 FROM activation_sui_bindings WHERE client_id=c.id)
                        FROM operations o JOIN activation_flows f ON f.operation_id=o.id JOIN clients c ON c.id=o.client_id
                        JOIN subscriptions sub ON sub.client_id=c.id JOIN activation_steps st ON st.operation_id=o.id
                        CROSS JOIN activation_write_guard g WHERE c.id=$1`, p.clientID, attempt, p.baseEpoch+1, string(raw))
				}
				assertBarrier(1)
				p.assertNotices(t, ctx, 2)
				gets := p.gets
				// Simulate an operator requeue, even clearing the flow's review
				// flag. The unresolved intent/global guard are manual-only facts.
				paymentExec(t, s, ctx, `WITH reset AS (
                    UPDATE activation_flows SET review_code=0 WHERE operation_id=(
                        SELECT id FROM operations WHERE client_id=$1 AND kind='activate_first_subscription') RETURNING operation_id
                    ) UPDATE operations SET next_attempt_at=clock_timestamp(), last_error_code=0
                    WHERE id IN (SELECT operation_id FROM reset)`, p.clientID)
				activationWorkflowExpectEvents(t, p.run(t, ctx, false), 0, "hold", activation.ReviewUnknownWrite)
				p.assertWrites(t, activation.OrderedSteps()[:1])
				assertBarrier(2)
				p.assertNotices(t, ctx, 2)
				p.assertOriginalPayment(t, ctx, false)
				if p.gets != gets {
					t.Fatal("unresolved intent was automatically re-preflighted")
				}
			}
		})
	}
}
