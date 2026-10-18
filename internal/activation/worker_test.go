package activation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

var workerOutage = errors.New("synthetic remote secret must never appear in logging")

type workerStore struct {
	s                                                  Snapshot
	prepareCalls, confirmCalls, finishCalls, holdCalls int
	reviewCode                                         int
	guard                                              bool
	prepareError, confirmError, retryError, loadError  error
	commitBeforeError, rejectPrepare                   bool
	retries                                            []time.Duration
	retryCodes                                         []int
	notices                                            []queue.Draft
	finalContexts                                      []context.Context
	prepareContext                                     context.Context
	prepareToday, finishToday                          string
	seedBot, claimBot                                  int64
	seedIDs                                            []int64
	seedPolicy                                         [32]byte
	claimLimit                                         int
	claims                                             []queue.Operation
	claimHook                                          func(context.Context)
	prepareHook                                        func(context.Context)
	confirmHook                                        func(context.Context) error
	recurringConfirmations                             []postResult
	finishHook                                         func()
}

func (f *workerStore) SeedActivations(_ context.Context, bot int64, allowed []int64, policy [32]byte) (int, error) {
	f.seedBot, f.seedIDs, f.seedPolicy = bot, slices.Clone(allowed), policy
	return 0, nil
}
func (f *workerStore) ClaimActivations(ctx context.Context, bot int64, limit int) ([]queue.Operation, error) {
	f.claimBot, f.claimLimit = bot, limit
	if f.claimHook != nil {
		f.claimHook(ctx)
	}
	claims := f.claims
	f.claims = nil
	return claims, nil
}
func (f *workerStore) RetryOperation(ctx context.Context, _ queue.Lease, delay time.Duration, code int) error {
	f.finalContexts = append(f.finalContexts, ctx)
	f.retries = append(f.retries, delay)
	f.retryCodes = append(f.retryCodes, code)
	return f.retryError
}
func (f *workerStore) LoadActivation(_ context.Context, _ queue.Lease, _ int64) (Snapshot, error) {
	s := f.s
	s.WriterBusy = s.WriterBusy || f.guard
	s.Steps = make(map[Step]Intent, len(f.s.Steps))
	for step, intent := range f.s.Steps {
		intent.Payload = bytes.Clone(intent.Payload)
		s.Steps[step] = intent
	}
	return s, f.loadError
}
func (f *workerStore) PrepareActivationWrite(ctx context.Context, lease queue.Lease, expected Snapshot, step Step, raw json.RawMessage, today string) (bool, error) {
	f.prepareCalls++
	f.prepareContext, f.prepareToday = ctx, today
	if f.prepareError != nil && !f.commitBeforeError {
		return false, f.prepareError
	}
	if f.rejectPrepare {
		return false, nil
	}
	if _, exists := f.s.Steps[step]; exists {
		return false, nil
	}
	if f.guard || expected.WriteEpoch != f.s.WriteEpoch {
		return false, ErrGuardBusy
	}
	f.guard = true
	f.s.WriteEpoch++
	f.s.Steps[step] = Intent{Payload: bytes.Clone(raw), DispatchAttempt: lease.Attempt}
	if f.prepareHook != nil {
		f.prepareHook(ctx)
	}
	if f.prepareError != nil {
		return false, f.prepareError
	}
	return true, nil
}
func (f *workerStore) ConfirmActivationStep(ctx context.Context, lease queue.Lease, step Step, id string) error {
	if step != SUICreate && step != SUIEnable {
		return queue.ErrConflict
	}
	return f.confirm(ctx, lease, step, id)
}
func (f *workerStore) ConfirmRecurringActivationStep(ctx context.Context, lease queue.Lease, step Step, id string, amount json.Number, datetime string) error {
	if step != RecurringCreate && step != RecurringStart {
		return queue.ErrConflict
	}
	savedAmount, savedDatetime, err := NormalizeRecurringFacts(amount, datetime)
	if err != nil {
		return err
	}
	f.recurringConfirmations = append(f.recurringConfirmations, postResult{id: id, amount: amount, datetime: datetime})
	if err := f.confirm(ctx, lease, step, id); err != nil {
		return err
	}
	intent := f.s.Steps[step]
	intent.RecurringAmount, intent.RecurringDatetime = savedAmount, savedDatetime
	f.s.Steps[step] = intent
	return nil
}
func (f *workerStore) confirm(ctx context.Context, lease queue.Lease, step Step, id string) error {
	f.confirmCalls++
	f.finalContexts = append(f.finalContexts, ctx)
	if f.confirmHook != nil {
		if err := f.confirmHook(ctx); err != nil {
			return err
		}
	}
	if f.confirmError != nil {
		return f.confirmError
	}
	intent := f.s.Steps[step]
	if intent.DispatchAttempt != lease.Attempt {
		return queue.ErrLeaseLost
	}
	intent.ExternalID = id
	f.s.Steps[step] = intent
	f.guard = false
	return nil
}
func (f *workerStore) HoldActivation(ctx context.Context, _ queue.Lease, code int, notices []queue.Draft) error {
	f.finalContexts = append(f.finalContexts, ctx)
	f.holdCalls++
	f.reviewCode = code
	f.s.ReviewCode = code
	if code == ReviewPayment {
		f.s.PaymentReview = true
	}
	f.notices = notices
	return nil
}
func (f *workerStore) FinishActivation(ctx context.Context, _ queue.Lease, _ Snapshot, today string, notices []queue.Draft) error {
	f.finalContexts = append(f.finalContexts, ctx)
	f.finishCalls++
	f.finishToday = today
	f.notices = notices
	if f.finishHook != nil {
		f.finishHook()
	}
	return nil
}

type workerPayments struct {
	customer  invoiceninja.SettlementCustomer
	invoice   invoiceninja.SettlementInvoice
	cash      []invoiceninja.SettlementPayment
	calls     []string
	errorAt   int
	readError error
	hook      func(int)
}

func (f *workerPayments) read(kind string) error {
	f.calls = append(f.calls, kind)
	if f.hook != nil {
		f.hook(len(f.calls))
	}
	if f.errorAt == len(f.calls) {
		if f.readError != nil {
			return f.readError
		}
		return workerOutage
	}
	return nil
}
func (f *workerPayments) GetSettlementCustomer(context.Context, string) (invoiceninja.SettlementCustomer, error) {
	err := f.read("customer")
	return f.customer, err
}
func (f *workerPayments) GetSettlementInvoice(context.Context, string) (invoiceninja.SettlementInvoice, error) {
	err := f.read("invoice")
	return f.invoice, err
}
func (f *workerPayments) ListSettlementPayments(context.Context, string) ([]invoiceninja.SettlementPayment, error) {
	err := f.read("payments")
	cash := slices.Clone(f.cash)
	for i := range cash {
		cash[i].Allocations = slices.Clone(cash[i].Allocations)
	}
	return cash, err
}

type workerAPIs struct {
	t                                                 *testing.T
	store                                             *workerStore
	panel                                             sui.ClientRecord
	recurring                                         invoiceninja.RecurringRecord
	product                                           invoiceninja.Product
	reads, writes, productReads, finds, panelFinds    int
	steps                                             []Step
	preflightError, getError, findError, productError error
	preflight                                         func(context.Context, Step) error
	afterPermission                                   func(Step)
	afterWrite                                        func(Step)
	postError                                         error
	badReceipt                                        bool
	foreign                                           []invoiceninja.RecurringRecord
	repeatPermission, skipPermission                  bool
	callbackResults                                   []error
	startReceipt                                      invoiceninja.RecurringReceipt
}

func (f *workerAPIs) GetProduct(context.Context, string) (invoiceninja.Product, error) {
	f.reads++
	f.productReads++
	return f.product, f.productError
}
func (f *workerAPIs) GetRecord(_ context.Context, id int64) (sui.ClientRecord, error) {
	f.reads++
	if id != 41 {
		f.t.Error("panel GET was not bound to saved numeric ID")
	}
	return f.panel, f.getError
}
func (f *workerAPIs) FindCreated(context.Context, sui.CreateDraft) (sui.ClientRecord, bool, error) {
	f.reads++
	f.panelFinds++
	return f.panel, f.panel.ID() > 0, f.getError
}
func (f *workerAPIs) Get(_ context.Context, id string) (invoiceninja.RecurringRecord, error) {
	f.reads++
	if id != "recurring-A" {
		f.t.Error("recurring GET was not bound to saved external ID")
	}
	return f.recurring, f.getError
}
func (f *workerAPIs) Find(context.Context, string) ([]invoiceninja.RecurringRecord, error) {
	f.reads++
	f.finds++
	return f.foreign, f.findError
}
func (f *workerAPIs) permission(ctx context.Context, step Step, authorize func(context.Context) error, preAuthorize ...func() error) error {
	f.reads++ // synthetic adapter preflight, before any permission reservation
	if authorize == nil {
		f.t.Fatal("unguarded writer")
	}
	if f.preflight != nil {
		if err := f.preflight(ctx, step); err != nil {
			return err
		}
	}
	if f.preflightError != nil {
		return f.preflightError
	}
	for _, check := range preAuthorize {
		if err := check(); err != nil {
			return err
		}
	}
	if f.skipPermission {
		return nil
	}
	err := authorize(ctx)
	f.callbackResults = append(f.callbackResults, err)
	if err != nil {
		return err
	}
	if f.repeatPermission {
		err = authorize(ctx)
		f.callbackResults = append(f.callbackResults, err)
		return err
	}
	if f.afterPermission != nil {
		f.afterPermission(step)
	}
	if f.postError != nil {
		return f.postError
	}
	f.writes++
	f.steps = append(f.steps, step)
	return nil
}
func (f *workerAPIs) CreateDisabledAuthorized(ctx context.Context, draft sui.CreateDraft, authorize func(context.Context) error) (sui.ClientRecord, error) {
	if err := f.permission(ctx, SUICreate, authorize); err != nil {
		return sui.ClientRecord{}, err
	}
	f.panel = workerPanel(f.t, draft, false)
	if f.afterWrite != nil {
		f.afterWrite(SUICreate)
	}
	if f.badReceipt {
		return sui.ClientRecord{}, nil
	}
	return f.panel, nil
}
func (f *workerAPIs) EnableAuthorized(ctx context.Context, draft sui.EnableDraft, authorize func(context.Context) error) (sui.ClientRecord, error) {
	if draft.Binding().ID != 41 {
		f.t.Error("enable was not bound to confirmed create")
	}
	if err := f.permission(ctx, SUIEnable, authorize); err != nil {
		return sui.ClientRecord{}, err
	}
	var create sui.CreateDraft
	if DecodeIntent(f.store.s.Steps[SUICreate].Payload, &create) != nil {
		f.t.Fatal("bad synthetic stored create")
	}
	f.panel = workerPanel(f.t, create, true)
	if f.afterWrite != nil {
		f.afterWrite(SUIEnable)
	}
	if f.badReceipt {
		return sui.ClientRecord{}, nil
	}
	return f.panel, nil
}
func (f *workerAPIs) CreateDraftAuthorized(ctx context.Context, draft invoiceninja.RecurringDraft, authorize func(context.Context) error) (invoiceninja.RecurringRecord, error) {
	if err := f.permission(ctx, RecurringCreate, authorize); err != nil {
		return invoiceninja.RecurringRecord{}, err
	}
	f.recurring = workerRecurring(draft)
	if f.afterWrite != nil {
		f.afterWrite(RecurringCreate)
	}
	if f.badReceipt {
		f.recurring.Balance = "0"
	}
	return f.recurring, nil
}
func (f *workerAPIs) StartFirstBoundAuthorized(ctx context.Context, id string, draft invoiceninja.RecurringDraft, receipt invoiceninja.RecurringReceipt, authorize func(context.Context) error) (invoiceninja.RecurringRecord, error) {
	if id != "recurring-A" || !reflect.DeepEqual(draft.LineItems, f.recurring.LineItems) {
		f.t.Error("start did not reuse saved recurring financial draft")
	}
	f.startReceipt = receipt
	if err := f.permission(ctx, RecurringStart, authorize, func() error {
		original := Intent{RecurringAmount: receipt.Amount.String(), RecurringDatetime: receipt.NextSendDatetime}
		if !sameRecurringFacts(original, f.recurring.Amount, f.recurring.NextSendDatetime) {
			return invoiceninja.ErrRecurringReceipt
		}
		return nil
	}); err != nil {
		return invoiceninja.RecurringRecord{}, err
	}
	f.recurring.StatusID = "2"
	if f.afterWrite != nil {
		f.afterWrite(RecurringStart)
	}
	if f.badReceipt {
		f.recurring.ID = "foreign"
	}
	return f.recurring, nil
}

func workerPanel(t *testing.T, draft sui.CreateDraft, enabled bool) sui.ClientRecord {
	t.Helper()
	raw, err := json.Marshal(draft)
	if err != nil {
		t.Fatal("marshal synthetic draft")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		t.Fatal("decode synthetic draft")
	}
	fields["id"] = json.RawMessage("41")
	fields["enable"], _ = json.Marshal(enabled)
	raw, _ = json.Marshal(fields)
	record, err := sui.ParseClientRecord(raw)
	if err != nil {
		t.Fatal("invalid synthetic panel record")
	}
	return record
}
func workerRecurring(d invoiceninja.RecurringDraft) invoiceninja.RecurringRecord {
	return invoiceninja.RecurringRecord{
		ID: "recurring-A", ClientID: d.ClientID, StatusID: "1", PrivateNotes: d.PrivateNotes, FrequencyID: "5",
		NextSendDate: d.NextSendDate, NextSendDatetime: d.NextSendDate + "T12:00:00.000000Z", DueDateDays: d.DueDateDays,
		AutoBill: d.AutoBill, RemainingCycles: d.RemainingCycles, Amount: "200", Balance: "200", UsesInclusiveTaxes: d.UsesInclusiveTaxes,
		LineItems: slices.Clone(d.LineItems), Discount: "0", TaxRate1: "0", TaxRate2: "0", TaxRate3: "0", CustomSurcharge1: "0", CustomSurcharge2: "0", CustomSurcharge3: "0", CustomSurcharge4: "0", Partial: "0", PaidToDate: "0", ExchangeRate: "1",
	}
}

type workerFixture struct {
	t   *testing.T
	w   *Worker
	s   *workerStore
	p   *workerPayments
	api *workerAPIs
	op  queue.Operation
	now time.Time
	log bytes.Buffer
}

func newWorkerFixture(t *testing.T) *workerFixture {
	t.Helper()
	cfg := activationTestConfig()
	local := payments.Snapshot{ClientID: 7, TelegramID: 101, ChatID: 101, BotID: 42, CustomerID: "customer-A", InvoiceID: "invoice-A", FirstInvoiceID: "invoice-A", State: "awaiting_payment"}
	p := &workerPayments{
		customer: invoiceninja.SettlementCustomer{ID: "customer-A", IDNumber: "101"},
		invoice:  invoiceninja.SettlementInvoice{ID: "invoice-A", ClientID: "customer-A", Number: "INV-200", StatusID: "4", Date: "2040-01-01", DueDate: "2040-01-31", Amount: "200", Balance: "0", PaidToDate: "200", ExchangeRate: "1"},
		cash:     []invoiceninja.SettlementPayment{{ID: "payment-A", ClientID: "customer-A", Date: "2040-01-31", StatusID: "4", CurrencyID: cfg.CurrencyID, Amount: "200", Refunded: "0", Applied: "200", ExchangeRate: "1", IsManual: true, Allocations: []invoiceninja.SettlementAllocation{{ID: "allocation-A", InvoiceID: "invoice-A", PaymentID: "payment-A", Amount: "200", Refunded: "0"}}}},
	}
	d := payments.Evaluate(local, p.customer, p.invoice, p.cash, cfg.CurrencyID, "2040-01-31")
	if d.Kind != payments.Verified {
		t.Fatal("synthetic cash proof not verified")
	}
	local.PaymentDate, local.NextInvoiceDate = d.Proof.PaymentDate, d.Proof.NextInvoiceDate
	s := &workerStore{s: Snapshot{Account: local, Token: "abcdefghijklmnopqrstuvwx", PolicyHash: cfg.PolicyHash(), Proof: *d.Proof, Steps: map[Step]Intent{}}}
	api := &workerAPIs{t: t, store: s, product: invoiceninja.Product{ID: cfg.ProductID, ProductKey: "monthly", Notes: "synthetic", Price: "200", TaxRate1: "0", TaxRate2: "0", TaxRate3: "0", TaxID: "0"}}
	f := &workerFixture{t: t, s: s, p: p, api: api, op: queue.Operation{Lease: queue.Lease{ID: 9}, ClientID: 7, Kind: Kind}, now: time.Date(2040, 1, 31, 12, 0, 0, 0, time.Local)}
	w, err := New(s, Providers{Payments: p, Products: api, SUI: api, Recurring: api}, 42, []int64{501, 502}, cfg, slog.New(slog.NewTextHandler(&f.log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	f.w = w
	w.now = func() time.Time { return f.now }
	return f
}
func (f *workerFixture) claim() { f.op.Attempt++; f.w.handle(context.Background(), f.op) }
func (f *workerFixture) progress(n int) {
	f.t.Helper()
	for range n {
		f.claim()
		if f.s.reviewCode != 0 {
			f.t.Fatalf("fixture stopped at code %d", f.s.reviewCode)
		}
	}
}
func (f *workerFixture) assertHold(code int) {
	f.t.Helper()
	if f.s.reviewCode != code || f.s.holdCalls == 0 {
		f.t.Fatalf("review = %d, want %d", f.s.reviewCode, code)
	}
}
func (f *workerFixture) clearReads() { f.api.reads = 0; f.p.calls = nil }

func TestWorkerFourClaimsThenFreshFinalization(t *testing.T) {
	f := newWorkerFixture(t)
	for i, step := range OrderedSteps() {
		f.claim()
		if f.api.writes != i+1 || f.s.confirmCalls != i+1 || f.s.finishCalls != 0 || f.s.guard {
			t.Fatalf("claim %d did not perform one confirmed write", i+1)
		}
		intent := f.s.s.Steps[step]
		if intent.ExternalID == "" || intent.DispatchAttempt != i+1 || ValidateIntent(f.s.s, step, intent.Payload) != nil {
			t.Fatalf("claim %d did not freeze a bound intent", i+1)
		}
		if f.s.retries[i] != 0 {
			t.Fatal("next step not postponed to a new claim")
		}
	}
	if !slices.Equal(f.api.steps, OrderedSteps()) || f.api.productReads != 1 {
		t.Fatal("write order or product freeze changed")
	}
	var start StartRequest
	if DecodeIntent(f.s.s.Steps[RecurringStart].Payload, &start) != nil || !slices.Equal(start.IDs, []string{"recurring-A"}) || start.Action != "start" {
		t.Fatal("invalid first-start wire journal")
	}
	f.claim()
	if f.s.finishCalls != 1 || f.api.writes != 4 || f.s.holdCalls != 0 {
		t.Fatal("did not finish locally after four distinct claims")
	}
	if f.s.finishToday != "2040-01-31" {
		t.Fatal("finish did not bind current local date")
	}
	pattern := []string{"customer", "invoice", "payments", "payments", "invoice", "customer"}
	if len(f.p.calls) != 5*len(pattern) {
		t.Fatal("each write/finalization must gather whole fresh financial evidence")
	}
	for i := 0; i < len(f.p.calls); i += len(pattern) {
		if !slices.Equal(f.p.calls[i:i+len(pattern)], pattern) {
			t.Fatal("incomplete repeated payment evidence")
		}
	}
	for _, message := range f.s.notices {
		if message.Validate() != nil || strings.Contains(message.Text, f.s.s.Token) || strings.Contains(message.Text, "http") || strings.Contains(message.Text, f.api.panel.Name()) {
			t.Fatal("sensitive/invalid completion notice")
		}
	}
	if len(f.s.notices) != 3 || !strings.Contains(f.s.notices[0].Text, "/config") || !strings.Contains(f.s.notices[0].Text, "требует отдельных проверок") || !strings.Contains(f.s.notices[0].Text, "не проверена") {
		t.Fatal("completion overclaims configuration delivery or core health")
	}
}

func TestWorkerRestartReverifiesConfirmedWithoutReplaying(t *testing.T) {
	f := newWorkerFixture(t)
	f.progress(2)
	f.api.product.Price = "999999"
	f.api.productError = workerOutage
	f.progress(2)
	f.claim()
	if f.api.productReads != 1 || f.api.writes != 4 || f.s.finishCalls != 1 {
		t.Fatal("resume refetched pricing or replayed confirmed steps")
	}
	for _, step := range OrderedSteps() {
		if f.s.s.Steps[step].DispatchAttempt <= 0 {
			t.Fatal("lost original dispatch fence")
		}
	}
}

func TestWorkerScopeAndReviewFencesHaveZeroProviderCalls(t *testing.T) {
	cases := []struct {
		name   string
		change func(*workerFixture)
		code   int
	}{
		{"excluded", func(f *workerFixture) { f.w.cfg.ExcludedTelegramIDs = []int64{101} }, ReviewExcluded},
		{"not pilot", func(f *workerFixture) { f.w.cfg.AllowedTelegramIDs = []int64{999} }, ReviewExcluded},
		{"payment review", func(f *workerFixture) { f.s.s.PaymentReview = true }, ReviewPayment},
		{"flow review", func(f *workerFixture) { f.s.s.ReviewCode = ReviewRecurring }, ReviewRecurring},
		{"policy changed", func(f *workerFixture) { f.w.cfg.InclusiveTaxes = true }, ReviewPolicy},
		{"client", func(f *workerFixture) { f.op.ClientID++ }, ReviewAssociation},
		{"bot", func(f *workerFixture) { f.s.s.Account.BotID++ }, ReviewAssociation},
		{"kind", func(f *workerFixture) { f.op.Kind = "other" }, ReviewAssociation},
		{"binding", func(f *workerFixture) { f.s.s.Account.SUIID = "99" }, ReviewAssociation},
		{"invoice no longer first", func(f *workerFixture) { f.s.s.Account.FirstInvoiceID = "old" }, ReviewAssociation},
		{"active", func(f *workerFixture) { f.s.s.Account.State = "active" }, ReviewAssociation},
		{"desired access", func(f *workerFixture) { f.s.s.Account.DesiredAccess = true }, ReviewAssociation},
		{"bad proof", func(f *workerFixture) { f.s.s.Proof.Hash[0]++ }, ReviewAssociation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newWorkerFixture(t)
			tc.change(f)
			f.claim()
			f.assertHold(tc.code)
			if f.api.reads != 0 || len(f.p.calls) != 0 || f.api.writes != 0 || f.s.prepareCalls != 0 {
				t.Fatal("a scope/review fence called a provider")
			}
		})
	}
}

func TestWorkerOldUnconfirmedIntentIsNeverLookedUpOrRearmed(t *testing.T) {
	for i, step := range OrderedSteps() {
		t.Run(string(step), func(t *testing.T) {
			f := newWorkerFixture(t)
			f.progress(i + 1)
			intent := f.s.s.Steps[step]
			intent.ExternalID, intent.RecurringAmount, intent.RecurringDatetime = "", "", ""
			f.s.s.Steps[step] = intent
			f.s.guard = true
			writes, prepares, confirms := f.api.writes, f.s.prepareCalls, f.s.confirmCalls
			f.clearReads()
			f.claim()
			f.assertHold(ReviewUnknownWrite)
			if f.api.reads != 0 || len(f.p.calls) != 0 || f.api.writes != writes || f.s.prepareCalls != prepares || f.s.confirmCalls != confirms || !f.s.guard {
				t.Fatal("unconfirmed sender was replayed/adopted, or its global guard released")
			}
		})
	}
}

func TestWorkerPriorRecordsMustRemainExactAndAcknowledged(t *testing.T) {
	cases := []struct {
		name     string
		progress int
		edit     func(*workerFixture)
		code     int
	}{
		{"missing panel", 1, func(f *workerFixture) { f.api.panel = sui.ClientRecord{} }, ReviewSUI},
		{"panel wrong owner", 1, func(f *workerFixture) {
			d, _ := sui.NewDisabledDraft(f.w.cfg.InboundIDs)
			f.api.panel = workerPanel(t, d, false)
		}, ReviewSUI},
		{"panel enabled without ack", 1, func(f *workerFixture) {
			var d sui.CreateDraft
			_ = DecodeIntent(f.s.s.Steps[SUICreate].Payload, &d)
			f.api.panel = workerPanel(t, d, true)
		}, ReviewSUI},
		{"panel disabled after ack", 3, func(f *workerFixture) {
			var d sui.CreateDraft
			_ = DecodeIntent(f.s.s.Steps[SUICreate].Payload, &d)
			f.api.panel = workerPanel(t, d, false)
		}, ReviewSUI},
		{"missing recurring", 2, func(f *workerFixture) { f.api.recurring = invoiceninja.RecurringRecord{} }, ReviewRecurring},
		{"recurring wrong ID", 2, func(f *workerFixture) { f.api.recurring.ID = "foreign" }, ReviewRecurring},
		{"recurring wrong owner", 2, func(f *workerFixture) { f.api.recurring.ClientID = "foreign" }, ReviewRecurring},
		{"recurring active without ack", 2, func(f *workerFixture) { f.api.recurring.StatusID = "2" }, ReviewRecurring},
		{"recurring deleted", 2, func(f *workerFixture) { f.api.recurring.IsDeleted = true }, ReviewRecurring},
		{"recurring archived", 2, func(f *workerFixture) { f.api.recurring.ArchivedAt = 1 }, ReviewRecurring},
		{"recurring sent", 2, func(f *workerFixture) { f.api.recurring.LastSentDate = "2040-01-31" }, ReviewRecurring},
		{"recurring changed price", 2, func(f *workerFixture) { f.api.recurring.LineItems[0].Cost = "201" }, ReviewRecurring},
		{"recurring balance zero", 2, func(f *workerFixture) { f.api.recurring.Balance = "0" }, ReviewRecurring},
		{"recurring draft after start ack", 4, func(f *workerFixture) { f.api.recurring.StatusID = "1" }, ReviewRecurring},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newWorkerFixture(t)
			f.progress(tc.progress)
			writes := f.api.writes
			tc.edit(f)
			f.claim()
			f.assertHold(tc.code)
			if f.api.writes != writes || f.s.finishCalls != 0 {
				t.Fatal("progressed after a prior confirmed receipt changed")
			}
		})
	}
}

func TestWorkerReadFailuresNeverCreateStickyNegativePayment(t *testing.T) {
	for _, stage := range []int{0, 1, 2, 3, 4} {
		t.Run(string(rune('0'+stage)), func(t *testing.T) {
			f := newWorkerFixture(t)
			f.progress(stage)
			writes := f.api.writes
			if stage == 0 {
				f.api.preflightError = workerOutage
			} else {
				f.api.getError = &integrationhttp.HTTPError{StatusCode: 404}
			}
			f.claim()
			if f.s.holdCalls != 0 || f.s.s.PaymentReview || f.api.writes != writes || f.s.finishCalls != 0 || len(f.s.retries) != stage+1 {
				t.Fatal("GET outage became a sticky negative or an effect")
			}
		})
	}
	for read := 1; read <= 6; read++ {
		t.Run("financial "+string(rune('0'+read)), func(t *testing.T) {
			f := newWorkerFixture(t)
			f.p.errorAt = read
			f.claim()
			if f.s.prepareCalls != 0 || f.api.writes != 0 || f.s.holdCalls != 0 || len(f.s.retries) != 1 {
				t.Fatal("failed complete payment GET did not defer cleanly")
			}
		})
	}
}

func TestWorkerRateLimitedReadsDeferWithoutReservingWriter(t *testing.T) {
	limited := errors.Join(workerOutage, &integrationhttp.HTTPError{StatusCode: 429})
	for progress := 0; progress <= len(OrderedSteps()); progress++ {
		for _, read := range []int{0, 1, 6} { // adapter preflight, first/last payment reads
			if progress == len(OrderedSteps()) && read == 0 {
				continue // finalization has no writer preflight
			}
			for _, extraAttempts := range []int{0, 10} {
				t.Run(strconv.Itoa(progress)+"/read="+strconv.Itoa(read)+"/attempts="+strconv.Itoa(extraAttempts), func(t *testing.T) {
					f := newWorkerFixture(t)
					f.progress(progress)
					epoch := f.s.s.WriteEpoch
					f.clearReads()
					f.op.Attempt += extraAttempts
					if read == 0 {
						f.api.preflightError = limited
					} else {
						f.p.errorAt, f.p.readError = read, limited
					}
					f.claim()
					if f.s.holdCalls != 0 || f.s.s.PaymentReview || f.s.guard || f.s.prepareCalls != progress || f.s.confirmCalls != progress || f.api.writes != progress || f.s.finishCalls != 0 || len(f.s.s.Steps) != progress || f.s.s.WriteEpoch != epoch {
						t.Fatal("429 read reserved an intent or became a sticky negative payment")
					}
					want := max(backoff(f.op.Attempt), time.Minute)
					if len(f.s.retries) != progress+1 || f.s.retries[progress] != want || f.s.retryCodes[progress] != 429 {
						t.Fatal("wrapped read 429 did not retain its status and at least one-minute backoff")
					}
					if deadline, ok := f.s.finalContexts[len(f.s.finalContexts)-1].Deadline(); !ok || time.Until(deadline) > databaseTimeout {
						t.Fatal("429 retry changed the bounded finalization budget")
					}
					f.api.preflightError, f.p.readError, f.p.errorAt = nil, nil, 0
					f.clearReads()
					f.claim()
					if f.s.holdCalls != 0 || progress < len(OrderedSteps()) && f.api.writes != progress+1 || progress == len(OrderedSteps()) && f.s.finishCalls != 1 {
						t.Fatal("a recovered 429 became sticky or prevented a fresh authorized claim")
					}
				})
			}
		}
	}
	t.Run("guard busy retains five seconds", func(t *testing.T) {
		f := newWorkerFixture(t)
		f.api.preflightError = errors.Join(ErrGuardBusy, limited)
		f.claim()
		if !slices.Equal(f.s.retries, []time.Duration{5 * time.Second}) || f.s.prepareCalls != 0 || f.api.writes != 0 || f.s.holdCalls != 0 || f.s.guard {
			t.Fatal("429 backoff overrode the five-second guard contention policy")
		}
	})
}

func TestWorkerChangedFinancialEvidenceHoldsOriginalConfirmation(t *testing.T) {
	cases := []struct {
		name string
		edit func(*workerPayments)
	}{
		{"pending", func(p *workerPayments) {
			p.invoice.StatusID = "2"
			p.invoice.Balance = "200"
			p.invoice.PaidToDate = "0"
		}},
		{"refunded source", func(p *workerPayments) { p.cash[0].Refunded = "1" }},
		{"refunded allocation", func(p *workerPayments) { p.cash[0].Allocations[0].Refunded = "1" }},
		{"changed otherwise verified hash", func(p *workerPayments) { p.cash[0].Allocations[0].ID = "allocation-B" }},
		{"changed currency", func(p *workerPayments) { p.cash[0].CurrencyID = "9" }},
		{"credit", func(p *workerPayments) { p.cash[0].Allocations[0].CreditID = "credit-A" }},
		{"incomplete whole inventory", func(p *workerPayments) { p.cash = append(p.cash, p.cash[0]) }},
	}
	for _, tc := range cases {
		for _, count := range []int{0, 3, 4} {
			t.Run(tc.name+string(rune('0'+count)), func(t *testing.T) {
				f := newWorkerFixture(t)
				f.progress(count)
				writes, prepares := f.api.writes, f.s.prepareCalls
				tc.edit(f.p)
				f.claim()
				f.assertHold(ReviewPayment)
				if !f.s.s.PaymentReview || f.s.prepareCalls != prepares || f.api.writes != writes || f.s.finishCalls != 0 {
					t.Fatal("changed cash evidence authorized progress or failed to mark original sticky")
				}
				f.clearReads()
				f.claim()
				if f.api.reads != 0 || len(f.p.calls) != 0 {
					t.Fatal("sticky payment review was released")
				}
			})
		}
	}
}

func TestWorkerUnstableRepeatedEvidenceDefersWithoutIntent(t *testing.T) {
	for _, read := range []int{4, 5, 6} {
		t.Run(string(rune('0'+read)), func(t *testing.T) {
			f := newWorkerFixture(t)
			f.p.hook = func(n int) {
				if n != read {
					return
				}
				switch n {
				case 4:
					f.p.cash[0].Allocations[0].ID = "changed"
				case 5:
					f.p.invoice.PaidToDate = "199"
				case 6:
					f.p.customer.IDNumber = "102"
				}
			}
			f.claim()
			if f.s.holdCalls != 0 || f.s.prepareCalls != 0 || f.api.writes != 0 || len(f.s.retries) != 1 {
				t.Fatal("unstable reads became authoritative or reserved a writer")
			}
		})
	}
}

func TestWorkerFinalAuthorizationFollowsAdapterPreflight(t *testing.T) {
	for i, step := range OrderedSteps() {
		t.Run(string(step), func(t *testing.T) {
			f := newWorkerFixture(t)
			f.progress(i)
			prepare := f.s.prepareCalls
			f.clearReads()
			f.api.preflight = func(_ context.Context, got Step) error {
				if got != step || f.s.prepareCalls != prepare || len(f.p.calls) != 0 {
					t.Error("reserved/observed before adapter's final read-only preflight")
				}
				return workerOutage
			}
			f.claim()
			if f.s.prepareCalls != prepare || f.s.guard || f.api.writes != i || f.s.holdCalls != 0 {
				t.Fatal("read-only adapter failure pinned the global write guard")
			}
		})
	}
	f := newWorkerFixture(t)
	f.api.preflight = func(context.Context, Step) error {
		f.p.invoice.Balance = "200"
		f.p.invoice.StatusID = "2"
		f.p.invoice.PaidToDate = "0"
		return nil
	}
	f.claim()
	f.assertHold(ReviewPayment)
	if f.api.writes != 0 || f.s.prepareCalls != 0 {
		t.Fatal("preflight-time cash change missed by final callback")
	}
}

func TestWorkerPermissionIsSingleUseEvenWithinOneClaim(t *testing.T) {
	for i, step := range OrderedSteps() {
		t.Run(string(step), func(t *testing.T) {
			f := newWorkerFixture(t)
			f.progress(i)
			f.api.repeatPermission = true
			f.claim()
			f.assertHold(ReviewUnknownWrite)
			if f.s.prepareCalls != i+1 || f.api.writes != i || len(f.api.callbackResults) != i+2 || !errors.Is(f.api.callbackResults[len(f.api.callbackResults)-1], ErrIntentExists) || !f.s.guard {
				t.Fatal("a second authorization granted permission")
			}
		})
	}
}

func TestWorkerUnknownResponsesAndPostPermissionErrorsNeverReplay(t *testing.T) {
	for i, step := range OrderedSteps() {
		for _, failure := range []string{"error", "bad receipt", "cancel before send"} {
			t.Run(string(step)+failure, func(t *testing.T) {
				f := newWorkerFixture(t)
				f.progress(i)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				switch failure {
				case "error":
					f.api.postError = workerOutage
				case "bad receipt":
					f.api.badReceipt = true
				case "cancel before send":
					f.api.afterPermission = func(Step) { cancel() }
					f.api.postError = context.Canceled
				}
				f.op.Attempt++
				f.w.handle(ctx, f.op)
				f.assertHold(ReviewUnknownWrite)
				if f.s.confirmCalls != i || !f.s.guard {
					t.Fatal("unknown outcome confirmed or guard released")
				}
				writes, prepares := f.api.writes, f.s.prepareCalls
				f.s.s.ReviewCode = 0 // even if review flag were lost, the immutable intent is enough
				f.clearReads()
				f.claim()
				if f.api.writes != writes || f.s.prepareCalls != prepares || f.api.reads != 0 || len(f.p.calls) != 0 {
					t.Fatal("ambiguous outcome was automatically rearmed/reconciled")
				}
				if strings.Contains(f.log.String(), workerOutage.Error()) {
					t.Fatal("remote error content entered logs")
				}
			})
		}
	}
}

func TestWorkerGlobalGuardBusyAndPrepareFencing(t *testing.T) {
	t.Run("busy", func(t *testing.T) {
		f := newWorkerFixture(t)
		f.s.guard = true
		f.claim()
		if f.api.writes != 0 || f.s.holdCalls != 0 || len(f.s.retries) != 1 || f.s.retries[0] != 5*time.Second || len(f.s.s.Steps) != 0 {
			t.Fatal("busy global guard did not delay without intent/effect")
		}
	})
	t.Run("lease lost", func(t *testing.T) {
		f := newWorkerFixture(t)
		f.s.prepareError = queue.ErrLeaseLost
		f.claim()
		if f.api.writes != 0 || f.s.holdCalls != 0 || len(f.s.retries) != 0 {
			t.Fatal("lost lease was used for an effect or finalization")
		}
	})
	t.Run("existing permission", func(t *testing.T) {
		f := newWorkerFixture(t)
		f.s.rejectPrepare = true
		f.claim()
		f.assertHold(ReviewUnknownWrite)
		if f.api.writes != 0 || f.s.confirmCalls != 0 {
			t.Fatal("false prepare acknowledgement was used to POST")
		}
	})
	for _, committed := range []bool{false, true} {
		t.Run("uncertain commit "+map[bool]string{false: "no", true: "yes"}[committed], func(t *testing.T) {
			f := newWorkerFixture(t)
			f.s.prepareError = workerOutage
			f.s.commitBeforeError = committed
			f.claim()
			if f.api.writes != 0 || f.s.holdCalls != 0 || f.s.guard != committed || len(f.s.retries) != 1 {
				t.Fatal("prepare error was treated as permission or authoritative failure")
			}
			if committed {
				f.clearReads()
				f.claim()
				f.assertHold(ReviewUnknownWrite)
				if f.api.reads != 0 || len(f.p.calls) != 0 {
					t.Fatal("uncertain committed intent was looked up/replayed")
				}
			}
		})
	}
}

func TestWorkerFirstRecurringRejectsAnyHistoryAndFreezesExactProduct(t *testing.T) {
	for _, state := range []string{"foreign", "archived", "deleted"} {
		t.Run(state, func(t *testing.T) {
			f := newWorkerFixture(t)
			f.progress(1)
			r := invoiceninja.RecurringRecord{ID: "foreign"}
			if state == "archived" {
				r.ArchivedAt = 1
			}
			if state == "deleted" {
				r.IsDeleted = true
			}
			f.api.foreign = []invoiceninja.RecurringRecord{r}
			f.claim()
			f.assertHold(ReviewRecurring)
			if f.api.productReads != 0 || f.api.writes != 1 {
				t.Fatal("existing recurring history was adopted")
			}
		})
	}
	for _, state := range []string{"wrong ID", "deleted", "archived"} {
		t.Run("product "+state, func(t *testing.T) {
			f := newWorkerFixture(t)
			f.progress(1)
			switch state {
			case "wrong ID":
				f.api.product.ID = "other"
			case "deleted":
				f.api.product.IsDeleted = true
			case "archived":
				f.api.product.ArchivedAt = 1
			}
			f.claim()
			f.assertHold(ReviewContract)
			if f.api.writes != 1 || f.s.prepareCalls != 1 {
				t.Fatal("unverified product entered journal")
			}
		})
	}
	for _, kind := range []string{"find", "product"} {
		t.Run(kind+" outage", func(t *testing.T) {
			f := newWorkerFixture(t)
			f.progress(1)
			if kind == "find" {
				f.api.findError = workerOutage
			} else {
				f.api.productError = workerOutage
			}
			f.claim()
			if f.s.holdCalls != 0 || f.api.writes != 1 || f.s.prepareCalls != 1 {
				t.Fatal("GET outage became a review/effect")
			}
		})
	}
}

func TestWorkerExpiredProofAndSchedulesNeverRewriteDates(t *testing.T) {
	for _, count := range []int{0, 1, 2, 3, 4} {
		t.Run("local expiry "+string(rune('0'+count)), func(t *testing.T) {
			f := newWorkerFixture(t)
			f.progress(count)
			f.now = time.Date(2040, 2, 29, 0, 0, 0, 0, time.Local)
			f.clearReads()
			f.claim()
			f.assertHold(ReviewPeriod)
			if f.api.writes != count || f.api.reads != 0 || len(f.p.calls) != 0 {
				t.Fatal("expired proof triggered progress or provider reads")
			}
		})
	}
	t.Run("future payment", func(t *testing.T) {
		f := newWorkerFixture(t)
		f.now = f.now.AddDate(0, 0, -1)
		f.claim()
		f.assertHold(ReviewPeriod)
		if f.api.reads != 0 || f.api.writes != 0 {
			t.Fatal("future dated proof was used")
		}
	})
	for _, stamp := range []string{"2040-01-31T00:00:00.000000Z", "2040-01-31 00:00:00"} {
		t.Run("UTC expiry "+stamp, func(t *testing.T) {
			f := newWorkerFixture(t)
			f.progress(2)
			f.api.recurring.NextSendDatetime = stamp
			f.claim()
			f.assertHold(ReviewPeriod)
			if f.api.writes != 2 {
				t.Fatal("UTC-due recurrence progressed despite future local date")
			}
		})
	}
	for _, stamp := range []string{"2040-02-29T08:00:00.123456Z", "2040-02-29 08:00:00"} {
		t.Run("future timestamp "+stamp, func(t *testing.T) {
			f := newWorkerFixture(t)
			f.progress(1)
			f.api.afterWrite = func(step Step) {
				if step == RecurringCreate {
					f.api.recurring.NextSendDatetime = stamp
				}
			}
			f.progress(3)
			f.claim()
			if f.s.finishCalls != 1 {
				t.Fatal("valid Carbon/UTC SQL datetime rejected")
			}
		})
	}
	for _, during := range []string{"preflight", "observation"} {
		t.Run("midnight "+during, func(t *testing.T) {
			f := newWorkerFixture(t)
			f.now = time.Date(2040, 2, 28, 23, 59, 59, 0, time.Local)
			cross := func() { f.now = f.now.Add(2 * time.Second) }
			if during == "preflight" {
				f.api.preflight = func(context.Context, Step) error { cross(); return nil }
			} else {
				f.p.hook = func(n int) {
					if n == 6 {
						cross()
					}
				}
			}
			f.claim()
			f.assertHold(ReviewPeriod)
			if f.api.writes != 0 || f.s.prepareCalls != 0 {
				t.Fatal("final authorization used a stale local date")
			}
		})
	}
}

func TestWorkerKnownSuccessAfterServiceCancellationIsConfirmed(t *testing.T) {
	for i, step := range OrderedSteps() {
		t.Run(string(step), func(t *testing.T) {
			f := newWorkerFixture(t)
			f.progress(i)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.api.afterWrite = func(Step) { cancel() }
			f.s.confirmHook = func(ctx context.Context) error {
				if ctx.Err() != nil {
					t.Error("confirmation inherited service cancellation")
				}
				return nil
			}
			f.op.Attempt++
			f.w.handle(ctx, f.op)
			if f.s.confirmCalls != i+1 || f.s.s.Steps[step].ExternalID == "" || f.s.holdCalls != 0 || f.s.guard {
				t.Fatal("known response was discarded on shutdown")
			}
			contexts := f.s.finalContexts[len(f.s.finalContexts)-2:]
			if contexts[0] != contexts[1] {
				t.Fatal("Confirm and Retry did not share shutdown budget")
			}
		})
	}
}

func TestWorkerConfirmationFailureSharesOneFinalizationBudget(t *testing.T) {
	for _, err := range []error{workerOutage, queue.ErrConflict} {
		t.Run(err.Error(), func(t *testing.T) {
			f := newWorkerFixture(t)
			f.s.confirmError = err
			f.claim()
			f.assertHold(ReviewUnknownWrite)
			if !f.s.guard || f.s.s.Steps[SUICreate].ExternalID != "" || len(f.s.finalContexts) != 2 {
				t.Fatal("failed confirmation lost uncertain intent")
			}
			if f.s.finalContexts[0] != f.s.finalContexts[1] {
				t.Fatal("Hold bought another eight seconds after Confirm failed")
			}
			deadline, ok := f.s.finalContexts[0].Deadline()
			if !ok || time.Until(deadline) > databaseTimeout {
				t.Fatal("unbounded background finalization")
			}
		})
	}
	t.Run("expired budget is not renewed", func(t *testing.T) {
		f := newWorkerFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		a := &activationAttempt{w: f.w, op: queue.Operation{Lease: queue.Lease{ID: 9, Attempt: 1}, ClientID: 7, Kind: Kind}, finalCtx: ctx, finalCancel: cancel}
		defer a.close()
		f.s.confirmHook = func(ctx context.Context) error { cancel(); <-ctx.Done(); return ctx.Err() }
		err := a.advance(context.Background(), f.s.s)
		var held reviewError
		if !errors.As(err, &held) {
			t.Fatal("confirmation failure did not preserve unknown write")
		}
		a.hold(held.code)
		if len(f.s.finalContexts) != 2 || f.s.finalContexts[1] != ctx || f.s.finalContexts[1].Err() == nil {
			t.Fatal("expired shutdown budget was renewed")
		}
	})
	t.Run("lost confirm lease", func(t *testing.T) {
		f := newWorkerFixture(t)
		f.s.confirmError = queue.ErrLeaseLost
		f.claim()
		if f.s.holdCalls != 0 || len(f.s.retries) != 0 || !f.s.guard {
			t.Fatal("finalization used a lost lease or released guard")
		}
	})
}

func TestWorkerRunSeedsOnlyEffectivePilotAndJoinsActiveClaim(t *testing.T) {
	f := newWorkerFixture(t)
	f.w.cfg.AllowedTelegramIDs = []int64{101, 102}
	f.w.cfg.ExcludedTelegramIDs = []int64{102}
	f.s.s.PolicyHash = f.w.cfg.PolicyHash()
	f.op.Attempt = 1
	f.s.claims = []queue.Operation{f.op}
	entered := make(chan struct{})
	release := make(chan struct{})
	f.api.preflight = func(ctx context.Context, _ Step) error { close(entered); <-ctx.Done(); <-release; return ctx.Err() }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.w.Run(ctx) }()
	<-entered
	cancel()
	select {
	case <-done:
		t.Fatal("Run left active worker behind")
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.s.seedIDs, []int64{101}) || f.s.seedPolicy != f.w.cfg.PolicyHash() || f.s.seedBot != 42 || f.s.claimBot != 42 || f.s.claimLimit != f.w.cfg.Workers {
		t.Fatal("Run ignored effective pilot, policy, bot scope or worker limit")
	}
	if f.s.prepareCalls != 0 || f.s.guard {
		t.Fatal("cancelled read-only worker reserved a writer")
	}
}

func TestWorkerConstructorClonesAndValidatesWithoutExternalIO(t *testing.T) {
	f := newWorkerFixture(t)
	cfg := activationTestConfig()
	cfg.ExcludedTelegramIDs = []int64{999}
	admins := []int64{502, 501, 501}
	w, err := New(f.s, f.w.providers, 42, admins, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	admins[0] = 999
	cfg.InboundIDs[0] = 999
	cfg.AllowedTelegramIDs[0] = 999
	cfg.ExcludedTelegramIDs[0] = 1000
	if !slices.Equal(w.admins, []int64{501, 502}) || w.cfg.InboundIDs[0] != 7 || w.cfg.AllowedTelegramIDs[0] != 101 || w.cfg.ExcludedTelegramIDs[0] != 999 {
		t.Fatal("caller mutated worker's frozen configuration")
	}
	for _, ids := range [][]int64{nil, {0}, {-1}, make([]int64, 100)} {
		if _, err := New(f.s, f.w.providers, 42, ids, activationTestConfig(), nil); err == nil {
			t.Fatal("invalid administrator list accepted")
		}
	}
	cfg = activationTestConfig()
	cfg.Enabled = false
	if _, err := New(f.s, f.w.providers, 42, []int64{1}, cfg, nil); err == nil {
		t.Fatal("default-off activation constructed")
	}
	cfg = activationTestConfig()
	cfg.AllowedTelegramIDs = make([]int64, 21)
	for i := range cfg.AllowedTelegramIDs {
		cfg.AllowedTelegramIDs[i] = int64(i + 1)
	}
	if _, err := New(f.s, f.w.providers, 42, []int64{1}, cfg, nil); err == nil {
		t.Fatal("pilot larger than 20 accepted")
	}
	if f.api.reads != 0 || len(f.p.calls) != 0 {
		t.Fatal("constructor called external provider")
	}
}

func TestWorkerReviewNoticesStayImmutableAndNonSensitive(t *testing.T) {
	f := newWorkerFixture(t)
	f.s.s.PaymentReview = true
	f.claim()
	first := slices.Clone(f.s.notices)
	f.s.s.PaymentReview = false
	f.s.s.ReviewCode = ReviewSUI
	f.claim()
	if !reflect.DeepEqual(first, f.s.notices) {
		t.Fatal("same review outbox key changed text with reason")
	}
	for _, d := range first {
		if d.Validate() != nil || !strings.HasPrefix(d.Key, "activation:9:review:admin:") || !strings.Contains(d.Text, "могла сохраниться") || strings.Contains(d.Text, f.s.s.Token) || strings.Contains(d.Text, "http") {
			t.Fatal("unsafe/unstable review notice")
		}
	}
}

func TestWorkerOriginalPreflightEpochCannotBeFreshened(t *testing.T) {
	for i, step := range OrderedSteps() {
		t.Run(string(step), func(t *testing.T) {
			f := newWorkerFixture(t)
			f.progress(i)
			epoch := f.s.s.WriteEpoch
			f.api.preflight = func(context.Context, Step) error {
				// Another instance obtained its permit, wrote, and confirmed.
				// The guard is empty again, but this attempt's reads are stale.
				f.s.s.WriteEpoch++
				f.s.guard = false
				return nil
			}
			f.claim()
			if f.s.prepareCalls != i+1 || f.api.writes != i || f.s.holdCalls != 0 || len(f.s.s.Steps) != i || f.s.guard || f.s.s.WriteEpoch != epoch+1 {
				t.Fatal("stale preflight epoch was silently refreshed or created an intent")
			}
			if f.s.retries[len(f.s.retries)-1] != 5*time.Second {
				t.Fatal("epoch contention did not defer five seconds")
			}
			f.api.preflight = nil
			f.claim()
			if f.api.writes != i+1 || f.s.s.WriteEpoch != epoch+2 {
				t.Fatal("fresh claim could not acquire its new epoch")
			}
		})
	}
}

func TestWorkerBusyLoadedGuardSkipsReadsButNeverMasksOwnUnknown(t *testing.T) {
	f := newWorkerFixture(t)
	f.s.guard = true
	f.claim()
	if f.api.reads != 0 || len(f.p.calls) != 0 || f.s.prepareCalls != 0 || f.s.holdCalls != 0 || !slices.Equal(f.s.retries, []time.Duration{5 * time.Second}) {
		t.Fatal("busy guard performed preflight or reserved an intent")
	}
	f.s.guard = false
	f.progress(1)
	intent := f.s.s.Steps[SUICreate]
	intent.ExternalID = ""
	f.s.s.Steps[SUICreate] = intent
	f.s.guard = true
	f.clearReads()
	f.claim()
	f.assertHold(ReviewUnknownWrite)
	if f.api.reads != 0 || len(f.p.calls) != 0 {
		t.Fatal("busy flag masked an old unknown sender")
	}
}

func TestWorkerFinalizationIgnoresUnrelatedWriterEpoch(t *testing.T) {
	f := newWorkerFixture(t)
	f.progress(4)
	f.s.guard = true // another account's unresolved write cannot undo ours
	f.s.s.WriteEpoch++
	f.claim()
	if f.s.finishCalls != 1 || f.api.writes != 4 || f.s.holdCalls != 0 || !f.s.guard {
		t.Fatal("finalization depended on another writer's permit")
	}
}

func TestWorkerConfirmedIntentPayloadsAreMandatoryAndPolicyBound(t *testing.T) {
	for i, step := range OrderedSteps() {
		t.Run("invalid "+string(step), func(t *testing.T) {
			f := newWorkerFixture(t)
			f.progress(i + 1)
			intent := f.s.s.Steps[step]
			intent.Payload = json.RawMessage(`{}`)
			f.s.s.Steps[step] = intent
			f.clearReads()
			f.claim()
			f.assertHold(ReviewContract)
			if f.api.reads != 0 || len(f.p.calls) != 0 || f.api.writes != i+1 {
				t.Fatal("invalid confirmed payload was ignored or replaced")
			}
		})
	}
	t.Run("saved inbound scope", func(t *testing.T) {
		f := newWorkerFixture(t)
		f.progress(1)
		draft, _ := sui.NewDisabledDraft([]int64{8})
		intent := f.s.s.Steps[SUICreate]
		intent.Payload, _ = json.Marshal(draft)
		f.s.s.Steps[SUICreate] = intent
		f.clearReads()
		f.claim()
		f.assertHold(ReviewPolicy)
		if f.api.reads != 0 {
			t.Fatal("frozen inbound scope was not validated")
		}
	})
	t.Run("saved tax mode", func(t *testing.T) {
		f := newWorkerFixture(t)
		f.progress(2)
		intent := f.s.s.Steps[RecurringCreate]
		var draft invoiceninja.RecurringDraft
		_ = DecodeIntent(intent.Payload, &draft)
		draft.UsesInclusiveTaxes = !f.w.cfg.InclusiveTaxes
		intent.Payload, _ = json.Marshal(draft)
		f.s.s.Steps[RecurringCreate] = intent
		f.clearReads()
		f.claim()
		f.assertHold(ReviewPolicy)
		if f.api.reads != 0 {
			t.Fatal("saved inclusive-tax policy was not validated")
		}
	})
	t.Run("confirmed receipt association", func(t *testing.T) {
		f := newWorkerFixture(t)
		f.progress(3)
		intent := f.s.s.Steps[SUIEnable]
		intent.ExternalID = "42"
		f.s.s.Steps[SUIEnable] = intent
		f.clearReads()
		f.claim()
		f.assertHold(ReviewAssociation)
		if f.api.reads != 0 {
			t.Fatal("enable receipt bound to another panel ID")
		}
	})
}

func TestWorkerPriorPanelStaticFieldsCannotDrift(t *testing.T) {
	for _, change := range []string{"id", "inbounds", "remark", "config"} {
		t.Run(change, func(t *testing.T) {
			f := newWorkerFixture(t)
			f.progress(1)
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(f.api.panel.Raw(), &fields)
			switch change {
			case "id":
				fields[change] = json.RawMessage(`42`)
			case "inbounds":
				fields[change] = json.RawMessage(`[8]`)
			case "remark":
				fields[change] = json.RawMessage(`"changed"`)
			case "config":
				var cfg map[string]json.RawMessage
				_ = json.Unmarshal(fields[change], &cfg)
				cfg["unknown_static"] = json.RawMessage(`{"opaque": "changed"}`)
				fields[change], _ = json.Marshal(cfg)
			}
			raw, _ := json.Marshal(fields)
			record, err := sui.ParseClientRecord(raw)
			if err != nil {
				t.Fatal("invalid synthetic changed record")
			}
			f.api.panel = record
			f.claim()
			f.assertHold(ReviewSUI)
			if f.api.writes != 1 || f.api.panelFinds != 0 {
				t.Fatal("changed exact owner was fuzzily adopted")
			}
		})
	}
}

func TestWorkerFinancialReadOutageAfterConfirmedProgressRemainsTransient(t *testing.T) {
	for _, stage := range []int{1, 2, 3, 4} {
		f := newWorkerFixture(t)
		f.progress(stage)
		f.p.errorAt = len(f.p.calls) + 6
		f.claim()
		if f.s.holdCalls != 0 || f.s.s.PaymentReview || f.api.writes != stage || f.s.finishCalls != 0 || f.s.prepareCalls != stage {
			t.Fatal("fresh repeated GET failure after valid proof became a sticky failure/effect")
		}
	}
}

func TestWorkerMissingAdapterAuthorizationCannotConfirm(t *testing.T) {
	f := newWorkerFixture(t)
	f.api.skipPermission = true
	f.claim()
	f.assertHold(ReviewUnknownWrite)
	if f.s.prepareCalls != 0 || f.s.confirmCalls != 0 {
		t.Fatal("unguarded adapter result was confirmed")
	}
}

var _ Store = (*workerStore)(nil)
var _ PanelAPI = (*workerAPIs)(nil)
var _ RecurringAPI = (*workerAPIs)(nil)
var _ payments.Reader = (*workerPayments)(nil)
