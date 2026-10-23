package configdelivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
)

func deliveryTestConfig() Config {
	return Config{Enabled: true, TelegramEnabled: true, PaymentsEnabled: true, Revision: 1,
		BaseURL: "https://billing.invalid/prefix", Token: "synthetic-ninja-token", CurrencyID: "1", TelegramIDField: "id_number",
		SUIBaseURL: "http://panel.invalid/private", SUIToken: "synthetic-panel-token", SUIVersion: "1.6.4", SubscriptionBaseURL: "https://subscription.invalid/sub/",
		AllowedTelegramIDs: []int64{101}, ExcludedTelegramIDs: []int64{999}, HTTPTimeout: time.Second, PageSize: 50, MaxPages: 20}
}

type deliveryStore struct {
	f                     *deliveryFixture
	view                  View
	loadErr, authorizeErr error
	loads, authorizations int
	decision              Decision
	original              View
	permit                *Permit
	onAuthorize           func()
	reserved              bool
}

func (*deliveryStore) InstallConfigurationPolicy(context.Context, int64, Policy) error {
	return errors.New("test must not install policy")
}
func (s *deliveryStore) checkDBContext(ctx context.Context) {
	s.f.t.Helper()
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > databaseTimeout {
		s.f.t.Fatal("database operation was not bounded")
	}
}
func (s *deliveryStore) LoadConfiguration(ctx context.Context, m queue.Message) (View, error) {
	s.checkDBContext(ctx)
	s.loads++
	s.f.events = append(s.f.events, "load")
	if m != s.f.message {
		s.f.t.Fatal("message changed before load")
	}
	return s.view, s.loadErr
}
func (s *deliveryStore) AuthorizeConfiguration(ctx context.Context, m queue.Message, v View, d Decision) (Permit, error) {
	s.checkDBContext(ctx)
	s.authorizations++
	s.f.events = append(s.f.events, "authorize")
	s.decision, s.original = d, v
	if d.Validate() != nil || m != s.f.message || !reflect.DeepEqual(v, s.view) {
		s.f.t.Fatal("authorization did not retain original immutable request/view")
	}
	if s.onAuthorize != nil {
		s.onAuthorize()
	}
	if s.authorizeErr != nil {
		return Permit{}, s.authorizeErr
	}
	if s.reserved {
		return Permit{}, ErrAlreadyDispatched
	}
	s.reserved = true
	if s.permit != nil {
		return *s.permit, nil
	}
	return Permit{Kind: d.Kind, Reason: d.Reason, NotAfter: s.f.now.Add(time.Second)}, nil
}

type deliveryPayments struct {
	f                  *deliveryFixture
	customer           invoiceninja.SettlementCustomer
	invoiceID          string
	invoice            invoiceninja.SettlementInvoice
	cash               []invoiceninja.SettlementPayment
	calls, inventories int
	hook               func(string, int) error
}

func (p *deliveryPayments) call(name string) error {
	p.calls++
	p.f.events = append(p.f.events, name)
	if p.hook != nil {
		return p.hook(name, p.calls)
	}
	return nil
}
func (p *deliveryPayments) GetSettlementCustomer(_ context.Context, id string) (invoiceninja.SettlementCustomer, error) {
	if id != "customer-A" {
		p.f.t.Fatal("wrong customer read")
	}
	err := p.call("customer")
	return p.customer, err
}
func (p *deliveryPayments) GetSettlementInvoice(_ context.Context, id string) (invoiceninja.SettlementInvoice, error) {
	expected := p.invoiceID
	if expected == "" {
		expected = "invoice-A"
	}
	if id != expected {
		p.f.t.Fatal("not the requested paid invoice")
	}
	err := p.call("invoice")
	return p.invoice, err
}
func (p *deliveryPayments) ListSettlementPayments(_ context.Context, id string) ([]invoiceninja.SettlementPayment, error) {
	if id != "customer-A" {
		p.f.t.Fatal("wrong inventory read")
	}
	p.inventories++
	err := p.call("payments")
	out := slices.Clone(p.cash)
	for i := range out {
		out[i].Allocations = slices.Clone(out[i].Allocations)
	}
	return out, err
}

type deliverySUI struct {
	f      *deliveryFixture
	record sui.ClientRecord
	reads  int
	hook   func(int) error
}

func (p *deliverySUI) GetRecord(_ context.Context, id int64) (sui.ClientRecord, error) {
	p.reads++
	p.f.events = append(p.f.events, "sui")
	if id != 41 {
		p.f.t.Fatal("SUI lookup was not the exact bound numeric ID")
	}
	var err error
	if p.hook != nil {
		err = p.hook(p.reads)
	}
	return p.record, err
}

type deliveryRecurring struct {
	f      *deliveryFixture
	record invoiceninja.RecurringRecord
	reads  int
	hook   func(int) error
}

func (p *deliveryRecurring) Get(_ context.Context, id string) (invoiceninja.RecurringRecord, error) {
	p.reads++
	p.f.events = append(p.f.events, "recurring")
	if id != "recurring-A" {
		p.f.t.Fatal("recurring lookup was not the exact bound ID")
	}
	var err error
	if p.hook != nil {
		err = p.hook(p.reads)
	}
	return p.record, err
}

type deliveryTransport struct {
	f                  *deliveryFixture
	getErr, postErr    error
	gets, sends, posts int
	chat               int64
	content            telegram.ProtectedMessage
	beforePrepare      func()
	doublePrepare      bool
}

func (p *deliveryTransport) GetPrivateChat(_ context.Context, id int64) error {
	p.gets++
	p.f.events = append(p.f.events, "private-chat")
	if id != 101 {
		p.f.t.Fatal("private chat was not the requesting actor")
	}
	return p.getErr
}
func (p *deliveryTransport) SendProtectedMessage(ctx context.Context, id int64, prepare func(context.Context) (telegram.ProtectedMessage, error)) (int64, error) {
	p.sends++
	p.chat = id
	p.f.events = append(p.f.events, "prepare")
	if p.beforePrepare != nil {
		p.beforePrepare()
	}
	message, err := prepare(ctx)
	if err != nil {
		return 0, err
	}
	if p.doublePrepare {
		_, err := prepare(ctx)
		if !errors.Is(err, ErrAlreadyDispatched) {
			p.f.t.Fatal("second callback was not rejected")
		}
		return 0, err
	}
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	p.posts++
	p.content = message
	p.f.events = append(p.f.events, "post")
	if p.postErr != nil {
		return 0, p.postErr
	}
	return 7001, nil
}

type deliveryFixture struct {
	t       *testing.T
	s       *Service
	store   *deliveryStore
	p       *deliveryPayments
	sui     *deliverySUI
	rec     *deliveryRecurring
	tg      *deliveryTransport
	cfg     Config
	message queue.Message
	now     time.Time
	events  []string
	log     bytes.Buffer
}

func deliveryJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal("cannot marshal synthetic fixture")
	}
	return raw
}
func deliveryRecord(t *testing.T, draft sui.CreateDraft, enabled bool) sui.ClientRecord {
	t.Helper()
	var fields map[string]json.RawMessage
	if json.Unmarshal(deliveryJSON(t, draft), &fields) != nil {
		t.Fatal("invalid synthetic draft")
	}
	fields["id"], fields["enable"] = json.RawMessage("41"), deliveryJSON(t, enabled)
	r, err := sui.ParseClientRecord(deliveryJSON(t, fields))
	if err != nil {
		t.Fatal("invalid synthetic SUI record")
	}
	return r
}
func deliveryMutateRecord(t *testing.T, record sui.ClientRecord, change func(map[string]json.RawMessage)) sui.ClientRecord {
	t.Helper()
	var fields map[string]json.RawMessage
	if json.Unmarshal(record.Raw(), &fields) != nil {
		t.Fatal("invalid synthetic SUI record")
	}
	change(fields)
	r, err := sui.ParseClientRecord(deliveryJSON(t, fields))
	if err != nil {
		t.Fatal("invalid synthetic record mutation")
	}
	return r
}
func newDeliveryFixture(t *testing.T) *deliveryFixture {
	t.Helper()
	f := &deliveryFixture{t: t, cfg: deliveryTestConfig(), now: time.Date(2040, 1, 31, 12, 0, 0, 0, time.Local), message: queue.Message{Lease: queue.Lease{ID: 80, Attempt: 1}, BotID: 42, ChatID: 101, Text: queue.ConfigurationIntentBody, ProtectedConfiguration: true}}
	account := payments.Snapshot{ClientID: 7, TelegramID: 101, ChatID: 101, BotID: 42, CustomerID: "customer-A", InvoiceID: "invoice-A", FirstInvoiceID: "invoice-A", State: "active", DesiredAccess: true, SUIID: "41", RecurringID: "recurring-A"}
	f.p = &deliveryPayments{f: f, invoiceID: "invoice-A",
		customer: invoiceninja.SettlementCustomer{ID: "customer-A", IDNumber: "101"},
		invoice:  invoiceninja.SettlementInvoice{ID: "invoice-A", ClientID: "customer-A", Number: "INV-200", StatusID: "4", Date: "2040-01-01", DueDate: "2040-01-31", Amount: "200", Balance: "0", PaidToDate: "200", ExchangeRate: "1"},
		cash:     []invoiceninja.SettlementPayment{{ID: "payment-A", ClientID: "customer-A", Date: "2040-01-31", StatusID: "4", CurrencyID: "1", Amount: "200", Refunded: "0", Applied: "200", ExchangeRate: "1", IsManual: true, Allocations: []invoiceninja.SettlementAllocation{{ID: "allocation-A", InvoiceID: "invoice-A", PaymentID: "payment-A", Amount: "200", Refunded: "0"}}}}}
	proof := payments.Evaluate(account, f.p.customer, f.p.invoice, f.p.cash, "1", "2040-01-31")
	if proof.Kind != payments.Verified {
		t.Fatal("synthetic payment proof was not verified")
	}
	account.PaymentDate, account.NextInvoiceDate = proof.Proof.PaymentDate, proof.Proof.NextInvoiceDate
	draft, err := sui.NewDisabledDraft([]int64{7, 9})
	if err != nil {
		t.Fatal("cannot generate synthetic SUI draft")
	}
	binding := sui.Binding{ID: 41, Name: draft.Name(), CredentialsHash: draft.CredentialsHash()}
	enable, err := sui.PrepareEnable(deliveryRecord(t, draft, false), binding)
	if err != nil {
		t.Fatal("cannot prepare synthetic enable intent")
	}
	token := "abcdefghijklmnopqrstuvwx"
	recDraft, err := invoiceninja.BuildRecurringDraft("customer-A", "2040-01-31", "2040-01-31", invoiceninja.Product{ID: "product-A", ProductKey: "monthly", Notes: "synthetic", Price: "200", TaxRate1: "0", TaxRate2: "0", TaxRate3: "0", TaxID: "0"}, false, activation.Marker(token))
	if err != nil {
		t.Fatal("cannot generate synthetic recurring draft")
	}
	record := invoiceninja.RecurringRecord{ID: "recurring-A", ClientID: "customer-A", StatusID: "2", PrivateNotes: recDraft.PrivateNotes, FrequencyID: "5", NextSendDate: recDraft.NextSendDate, NextSendDatetime: recDraft.NextSendDate + "T12:00:00Z", DueDateDays: "on_receipt", AutoBill: "off", RemainingCycles: -1, Amount: "200", Balance: "200", LineItems: slices.Clone(recDraft.LineItems), Discount: "0", TaxRate1: "0", TaxRate2: "0", TaxRate3: "0", CustomSurcharge1: "0", CustomSurcharge2: "0", CustomSurcharge3: "0", CustomSurcharge4: "0", Partial: "0", PaidToDate: "0", ExchangeRate: "1"}
	steps := map[activation.Step]activation.Intent{
		activation.SUICreate:       {Payload: deliveryJSON(t, draft), ExternalID: "41", DispatchAttempt: 1},
		activation.RecurringCreate: {Payload: deliveryJSON(t, recDraft), ExternalID: "recurring-A", DispatchAttempt: 2, RecurringAmount: "200", RecurringDatetime: record.NextSendDatetime},
		activation.SUIEnable:       {Payload: deliveryJSON(t, enable), ExternalID: "41", DispatchAttempt: 3},
		activation.RecurringStart:  {Payload: deliveryJSON(t, activation.StartRequest{IDs: []string{"recurring-A"}, Action: "start"}), ExternalID: "recurring-A", DispatchAttempt: 4, RecurringAmount: "200.00", RecurringDatetime: recDraft.NextSendDate + " 12:00:00"},
	}
	e := Entitlement{ActivationID: 9, Revision: 2, Binding: binding, Period: FirstPeriod, Activation: activation.Snapshot{Account: account, Token: token, PolicyHash: [32]byte{1}, Proof: *proof.Proof, Steps: steps}}
	hash, err := e.Fingerprint()
	if err != nil {
		t.Fatal("invalid synthetic completed entitlement")
	}
	policy := f.cfg.Policy()
	r := Request{OutboxID: 80, BotID: 42, ClientID: 7, ActorID: 101, ChatID: 101, ActivationID: 9, Period: FirstPeriod, ClientRevision: 2, PolicyRevision: policy.Revision, PolicyHash: policy.Hash, ProofHash: proof.Proof.Hash, EntitlementHash: hash, InvoiceID: "invoice-A", CreatedAt: f.now, ExpiresAt: f.now.Add(RequestTTL)}
	f.store = &deliveryStore{f: f, view: View{Request: r, Entitlement: e, Eligible: true}}
	if !f.store.view.Matches() {
		t.Fatal("synthetic view does not match")
	}
	f.sui, f.rec, f.tg = &deliverySUI{f: f, record: deliveryRecord(t, draft, true)}, &deliveryRecurring{f: f, record: record}, &deliveryTransport{f: f}
	f.s, err = New(f.store, Providers{Payments: f.p, SUI: f.sui, Recurring: f.rec}, f.tg, 42, f.cfg, slog.New(slog.NewTextHandler(&f.log, nil)))
	if err != nil {
		t.Fatal("cannot construct synthetic delivery service")
	}
	f.s.now = func() time.Time { return f.now }
	return f
}
func (f *deliveryFixture) deliver() (int64, error) {
	return f.s.Deliver(context.Background(), f.message)
}
func (f *deliveryFixture) assertDenial(reason int, changed bool) {
	f.t.Helper()
	id, err := f.deliver()
	if err != nil || id != 7001 || f.tg.posts != 1 || f.tg.content.Text != denialText || f.store.decision != (Decision{Kind: Denial, Reason: reason, PaymentChanged: changed}) || f.tg.gets != 0 {
		f.t.Fatal("expected one protected generic denial and no private-chat lookup")
	}
}

func TestDeliveryPositiveOrderAndURL(t *testing.T) {
	f := newDeliveryFixture(t)
	id, err := f.deliver()
	if err != nil || id != 7001 || f.tg.posts != 1 || f.store.decision != (Decision{Kind: Secret}) || f.tg.chat != 101 {
		t.Fatal("protected delivery failed")
	}
	want := []string{"load", "sui", "recurring", "customer", "invoice", "payments", "payments", "invoice", "customer", "sui", "recurring", "private-chat", "prepare", "authorize", "post"}
	if !slices.Equal(f.events, want) {
		t.Fatal("provider/private actor/final permit ordering changed")
	}
	uri, err := sui.SubscriptionURL(f.cfg.SubscriptionBaseURL, f.store.view.Entitlement.Binding.Name)
	if err != nil || !strings.Contains(f.tg.content.Text, "\n"+uri+"\n") || !strings.Contains(f.tg.content.Text, "/help") {
		t.Fatal("unexpected protected message format")
	}
	u, err := url.Parse(uri)
	if err != nil || u.RawQuery != "format=json" || f.message.Text != queue.ConfigurationIntentBody || !reflect.DeepEqual(f.store.original, f.store.view) {
		t.Fatal("personal URI contaminated durable work")
	}
	if !f.tg.content.NotAfter.Equal(f.now.Add(time.Second)) {
		t.Fatal("permit deadline not preserved")
	}
	if f.log.Len() != 0 {
		t.Fatal("delivery logged private evidence")
	}
}

func TestDeliveryConstructorAndClonedScopes(t *testing.T) {
	f := newDeliveryFixture(t)
	providers := Providers{Payments: f.p, SUI: f.sui, Recurring: f.rec}
	for _, tc := range []struct {
		store     Store
		transport Transport
		providers Providers
		bot       int64
		cfg       Config
	}{
		{nil, f.tg, providers, 42, f.cfg}, {f.store, nil, providers, 42, f.cfg}, {f.store, f.tg, providers, 0, f.cfg}, {f.store, f.tg, Providers{}, 42, f.cfg}, {f.store, f.tg, providers, 42, Config{Enabled: true}},
	} {
		if _, err := New(tc.store, tc.providers, tc.transport, tc.bot, tc.cfg, nil); err == nil {
			t.Fatal("invalid constructor input accepted")
		}
	}
	if _, err := New(f.store, Providers{}, f.tg, 42, Config{}, nil); err != nil {
		t.Fatal("disabled constructor required providers")
	}
	f.cfg.AllowedTelegramIDs[0] = 555
	f.cfg.ExcludedTelegramIDs[0] = 101
	if !f.s.cfg.Allows(101) || !slices.Equal(f.s.cfg.ExcludedTelegramIDs, []int64{999}) {
		t.Fatal("caller mutated cloned policy scope")
	}
	if f.store.loads != 0 || len(f.events) != 0 {
		t.Fatal("constructor performed I/O")
	}
}

func TestDeliveryRejectsNonIntentEnvelopes(t *testing.T) {
	for _, change := range []func(*queue.Message){
		func(m *queue.Message) { m.ProtectedConfiguration = false }, func(m *queue.Message) { m.ClientMenu = true },
		func(m *queue.Message) { m.Text = "/configuration administrator secret-selector" }, func(m *queue.Message) { m.BotID++ },
		func(m *queue.Message) { m.ChatID = -1 }, func(m *queue.Message) { m.ID = 0 }, func(m *queue.Message) { m.Attempt = 0 },
	} {
		f := newDeliveryFixture(t)
		change(&f.message)
		if _, err := f.deliver(); err == nil || len(f.events) != 0 {
			t.Fatal("invalid protected intent reached storage/transport")
		}
	}
}

func TestDeliveryLocalDenialsHaveZeroIntegrationReads(t *testing.T) {
	cases := []struct {
		name   string
		change func(*deliveryFixture)
		reason int
	}{
		{"disabled", func(f *deliveryFixture) { f.s.cfg.Enabled = false }, DeniedPolicy},
		{"excluded", func(f *deliveryFixture) { f.s.cfg.ExcludedTelegramIDs = []int64{101} }, DeniedPolicy},
		{"nonpilot", func(f *deliveryFixture) { f.s.cfg.AllowedTelegramIDs = []int64{888} }, DeniedPolicy},
		{"policy revision", func(f *deliveryFixture) { f.store.view.Request.PolicyRevision++ }, DeniedPolicy},
		{"policy hash", func(f *deliveryFixture) { f.store.view.Request.PolicyHash[0]++ }, DeniedPolicy},
		{"review", func(f *deliveryFixture) { f.store.view.Entitlement.Activation.PaymentReview = true }, DeniedEntitlement},
		{"activation review", func(f *deliveryFixture) {
			f.store.view.Entitlement.Activation.ReviewCode = activation.ReviewUnknownWrite
		}, DeniedEntitlement},
		{"forged active", func(f *deliveryFixture) { f.store.view.Entitlement.Activation.Steps = nil }, DeniedEntitlement},
		{"renewal", func(f *deliveryFixture) { f.store.view.Entitlement.Activation.Account.InvoiceID = "invoice-renewal" }, DeniedEntitlement},
		{"binding provenance", func(f *deliveryFixture) { f.store.view.Entitlement.Binding.ID++ }, DeniedEntitlement},
		{"original start facts", func(f *deliveryFixture) {
			i := f.store.view.Entitlement.Activation.Steps[activation.RecurringStart]
			i.RecurringAmount = "201"
			f.store.view.Entitlement.Activation.Steps[activation.RecurringStart] = i
		}, DeniedEntitlement},
		{"ineligible", func(f *deliveryFixture) { f.store.view.Eligible = false; f.store.view.DenialCode = DeniedEntitlement }, DeniedEntitlement},
		{"request client revision", func(f *deliveryFixture) { f.store.view.Request.ClientRevision++ }, DeniedEntitlement},
		{"request outbox", func(f *deliveryFixture) { f.store.view.Request.OutboxID++ }, DeniedEntitlement},
		{"request bot", func(f *deliveryFixture) { f.store.view.Request.BotID++ }, DeniedEntitlement},
		{"request future", func(f *deliveryFixture) { f.now = f.now.Add(-time.Nanosecond) }, DeniedExpired},
		{"request ttl", func(f *deliveryFixture) {
			f.store.view.Request.ExpiresAt = f.store.view.Request.CreatedAt.Add(RequestTTL + time.Nanosecond)
		}, DeniedEntitlement},
		{"request expired", func(f *deliveryFixture) { f.now = f.now.Add(RequestTTL) }, DeniedExpired},
		{"period expired", func(f *deliveryFixture) {
			f.now = time.Date(2040, 3, 1, 0, 0, 0, 0, time.Local)
			f.store.view.Request.CreatedAt = f.now
			f.store.view.Request.ExpiresAt = f.now.Add(RequestTTL)
		}, DeniedExpired},
		{"unknown chat guard", func(f *deliveryFixture) { f.store.view.ChatBlocked = true }, DeniedUnknownDelivery},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeliveryFixture(t)
			tc.change(f)
			f.assertDenial(tc.reason, false)
			if !slices.Equal(f.events, []string{"load", "prepare", "authorize", "post"}) {
				t.Fatal("local denial leaked integration GETs")
			}
		})
	}
}

func TestDeliveryLoadFailureAndAlreadyDispatchedAreTerminal(t *testing.T) {
	for _, err := range []error{queue.ErrLeaseLost, errors.New("synthetic-sensitive-store-failure")} {
		f := newDeliveryFixture(t)
		f.store.loadErr = err
		if _, got := f.deliver(); !errors.Is(got, err) || !slices.Equal(f.events, []string{"load"}) {
			t.Fatal("load failure did not stop all effects")
		}
	}
	f := newDeliveryFixture(t)
	f.store.view.AlreadyDispatched = true
	f.store.view.ChatBlocked = true
	if _, err := f.deliver(); !errors.Is(err, ErrAlreadyDispatched) || !slices.Equal(f.events, []string{"load"}) {
		t.Fatal("already dispatched request was read or resent")
	}
}

func TestDeliveryAuthoritativePaymentChangesOnlyAreSticky(t *testing.T) {
	cases := []struct {
		name   string
		change func(*deliveryFixture)
		reason int
		sticky bool
	}{
		{"refunded", func(f *deliveryFixture) { f.p.cash[0].Refunded = "1" }, DeniedPayment, true},
		{"pending", func(f *deliveryFixture) {
			f.p.invoice.StatusID = "2"
			f.p.invoice.Balance = "200"
			f.p.invoice.PaidToDate = "0"
		}, DeniedPayment, true},
		{"changed proof", func(f *deliveryFixture) { f.p.cash[0].Allocations[0].ID = "allocation-changed" }, DeniedPayment, true},
		{"outage", func(f *deliveryFixture) {
			f.p.hook = func(string, int) error { return errors.New("synthetic-sensitive-outage") }
		}, DeniedUnavailable, false},
		{"incomplete second inventory", func(f *deliveryFixture) {
			f.p.hook = func(name string, _ int) error {
				if name == "payments" && f.p.inventories == 2 {
					return errors.New("incomplete synthetic inventory")
				}
				return nil
			}
		}, DeniedUnavailable, false},
		{"deferred final GET", func(f *deliveryFixture) {
			f.p.hook = func(name string, n int) error {
				if name == "customer" && n == 6 {
					return errors.New("synthetic version mismatch")
				}
				return nil
			}
		}, DeniedUnavailable, false},
		{"unstable target", func(f *deliveryFixture) {
			f.p.hook = func(name string, _ int) error {
				if name == "payments" && f.p.inventories == 2 {
					f.p.cash[0].Date = "2040-01-30"
				}
				return nil
			}
		}, DeniedUnavailable, false},
		{"ignored inventory changed", func(f *deliveryFixture) {
			ignored := f.p.cash[0]
			ignored.ID = "ignored"
			ignored.Allocations = []invoiceninja.SettlementAllocation{}
			f.p.cash = append(f.p.cash, ignored)
			f.p.hook = func(name string, _ int) error {
				if name == "payments" && f.p.inventories == 2 {
					f.p.cash[1].Amount = "201"
				}
				return nil
			}
		}, DeniedUnavailable, false},
		{"surrounding invoice changed", func(f *deliveryFixture) {
			f.p.hook = func(name string, n int) error {
				if name == "invoice" && n == 5 {
					f.p.invoice.Number = "OTHER"
				}
				return nil
			}
		}, DeniedUnavailable, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { f := newDeliveryFixture(t); tc.change(f); f.assertDenial(tc.reason, tc.sticky) })
	}
}

func TestDeliveryBoundRecordsMustStayOriginal(t *testing.T) {
	cases := []struct {
		name   string
		change func(*deliveryFixture)
		reason int
	}{
		{"wrong SUI id", func(f *deliveryFixture) {
			f.sui.record = deliveryMutateRecord(t, f.sui.record, func(m map[string]json.RawMessage) { m["id"] = json.RawMessage("42") })
		}, DeniedSUI},
		{"disabled SUI", func(f *deliveryFixture) {
			f.sui.record = deliveryMutateRecord(t, f.sui.record, func(m map[string]json.RawMessage) { m["enable"] = json.RawMessage("false") })
		}, DeniedSUI},
		{"inbounds drift", func(f *deliveryFixture) {
			f.sui.record = deliveryMutateRecord(t, f.sui.record, func(m map[string]json.RawMessage) { m["inbounds"] = json.RawMessage("[7]") })
		}, DeniedSUI},
		{"credentials drift", func(f *deliveryFixture) {
			f.sui.record = deliveryMutateRecord(t, f.sui.record, func(m map[string]json.RawMessage) {
				var cfg map[string]map[string]any
				if json.Unmarshal(m["config"], &cfg) != nil {
					t.Fatal("invalid synthetic config")
				}
				cfg["mixed"]["password"] = "synthetic-drift"
				m["config"] = deliveryJSON(t, cfg)
			})
		}, DeniedSUI},
		{"wrong SUI name", func(f *deliveryFixture) {
			other, err := sui.NewDisabledDraft([]int64{7, 9})
			if err != nil {
				t.Fatal("synthetic draft generation")
			}
			f.sui.record = deliveryRecord(t, other, true)
		}, DeniedSUI},
		{"SUI version error", func(f *deliveryFixture) { f.sui.hook = func(int) error { return sui.ErrManagedVersion } }, DeniedUnavailable},
		{"wrong recurring id", func(f *deliveryFixture) { f.rec.record.ID = "another" }, DeniedRecurring},
		{"wrong recurring client", func(f *deliveryFixture) { f.rec.record.ClientID = "another" }, DeniedRecurring},
		{"recurring inactive", func(f *deliveryFixture) { f.rec.record.StatusID = "1" }, DeniedRecurring},
		{"recurring archived", func(f *deliveryFixture) { f.rec.record.ArchivedAt = 1 }, DeniedRecurring},
		{"recurring deleted", func(f *deliveryFixture) { f.rec.record.IsDeleted = true }, DeniedRecurring},
		{"recurring amount", func(f *deliveryFixture) { f.rec.record.Amount = "201"; f.rec.record.Balance = "201" }, DeniedRecurring},
		{"recurring UTC", func(f *deliveryFixture) { f.rec.record.NextSendDatetime = f.rec.record.NextSendDate + "T12:00:01Z" }, DeniedRecurring},
		{"recurring date", func(f *deliveryFixture) { f.rec.record.NextSendDate = "2040-02-28" }, DeniedRecurring},
		{"recurring schedule advanced", func(f *deliveryFixture) { f.rec.record.LastSentDate = "2040-01-31" }, DeniedRecurring},
		{"recurring version error", func(f *deliveryFixture) {
			f.rec.hook = func(int) error { return errors.New("synthetic-version-error") }
		}, DeniedUnavailable},
		{"SUI changed after proof", func(f *deliveryFixture) {
			f.sui.hook = func(n int) error {
				if n == 2 {
					f.sui.record = deliveryMutateRecord(t, f.sui.record, func(m map[string]json.RawMessage) { m["enable"] = json.RawMessage("false") })
				}
				return nil
			}
		}, DeniedSUI},
		{"recurring changed after proof", func(f *deliveryFixture) {
			f.rec.hook = func(n int) error {
				if n == 2 {
					f.rec.record.Amount = "201"
					f.rec.record.Balance = "201"
				}
				return nil
			}
		}, DeniedRecurring},
		{"SUI GET fails after proof", func(f *deliveryFixture) {
			f.sui.hook = func(n int) error {
				if n == 2 {
					return errors.New("synthetic-outage")
				}
				return nil
			}
		}, DeniedUnavailable},
		{"recurring GET fails after proof", func(f *deliveryFixture) {
			f.rec.hook = func(n int) error {
				if n == 2 {
					return errors.New("synthetic-outage")
				}
				return nil
			}
		}, DeniedUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { f := newDeliveryFixture(t); tc.change(f); f.assertDenial(tc.reason, false) })
	}
}

func TestDeliveryRecurringFactsUseExactDecimalsAndUTC(t *testing.T) {
	f := newDeliveryFixture(t)
	f.rec.record.Amount = "200.000000"
	f.rec.record.Balance = "200.0"
	f.rec.record.NextSendDatetime = f.rec.record.NextSendDate + "T15:00:00+03:00"
	if _, err := f.deliver(); err != nil || f.store.decision.Kind != Secret {
		t.Fatal("equivalent exact financial/UTC facts rejected")
	}
}

func TestDeliveryPrivateChatErrorsPreserveCooldownAndNeverReserve(t *testing.T) {
	for _, code := range []int{429, 401, 403, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			f := newDeliveryFixture(t)
			api := &telegram.APIError{HTTPStatus: code, Code: code, RetryAfter: time.Minute, Rejected: code < 500}
			f.tg.getErr = api
			_, err := f.deliver()
			var got *telegram.APIError
			if !errors.As(err, &got) || got != api || f.store.authorizations != 0 || f.tg.sends != 0 || f.tg.posts != 0 {
				t.Fatal("private-chat failure reserved or sent a message")
			}
		})
	}
}

func TestDeliveryFinalPermitIsOneShotAndCanOnlyDowngrade(t *testing.T) {
	for _, reason := range []int{DeniedEntitlement, DeniedPolicy, DeniedUnknownDelivery} {
		t.Run(fmt.Sprint(reason), func(t *testing.T) {
			f := newDeliveryFixture(t)
			f.store.permit = &Permit{Kind: Denial, Reason: reason, NotAfter: f.now.Add(time.Second)}
			if _, err := f.deliver(); err != nil || f.store.decision.Kind != Secret || f.tg.content.Text != denialText {
				t.Fatal("final DB downgrade was ignored")
			}
		})
	}
	for _, cause := range []error{ErrAlreadyDispatched, queue.ErrLeaseLost, ErrStalePolicy, errors.New("synthetic-cooldown")} {
		f := newDeliveryFixture(t)
		f.store.authorizeErr = cause
		if _, err := f.deliver(); !errors.Is(err, cause) || f.tg.posts != 0 || f.tg.sends != 1 || f.store.authorizations != 1 {
			t.Fatal("final authorization failure caused fallback/replay")
		}
	}
	f := newDeliveryFixture(t)
	f.tg.doublePrepare = true
	if _, err := f.deliver(); !errors.Is(err, ErrAlreadyDispatched) || f.store.authorizations != 1 || f.tg.posts != 0 {
		t.Fatal("callback was not one-shot")
	}
	f = newDeliveryFixture(t)
	f.s.cfg.Enabled = false
	f.store.permit = &Permit{Kind: Secret, NotAfter: f.now.Add(time.Second)}
	if _, err := f.deliver(); err == nil || f.tg.posts != 0 {
		t.Fatal("denial was upgraded without private actor/proof")
	}
}

func TestDeliveryPostGrantExpiryCancellationAndUnknownAreNotReplayed(t *testing.T) {
	for _, which := range []string{"permit expired", "request expired", "period expired", "cancelled", "zero deadline", "invalid kind"} {
		t.Run(which, func(t *testing.T) {
			f := newDeliveryFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.store.onAuthorize = func() {
				switch which {
				case "permit expired":
					f.store.permit = &Permit{Kind: Secret, NotAfter: f.now}
				case "request expired":
					f.now = f.store.view.Request.ExpiresAt
				case "period expired":
					deadline, _ := f.store.view.Entitlement.Deadline()
					f.now = deadline
				case "cancelled":
					cancel()
				case "zero deadline":
					f.store.permit = &Permit{Kind: Secret}
				case "invalid kind":
					f.store.permit = &Permit{Kind: "invalid", NotAfter: f.now.Add(time.Second)}
				}
			}
			if _, err := f.s.Deliver(ctx, f.message); err == nil || f.tg.posts != 0 || !f.store.reserved || f.store.authorizations != 1 || f.tg.sends != 1 {
				t.Fatal("post-grant failure sent or discarded reservation")
			}
		})
	}
	f := newDeliveryFixture(t)
	f.tg.postErr = errors.New("synthetic-unknown-delivery")
	if _, err := f.deliver(); err == nil || f.tg.posts != 1 || f.tg.sends != 1 || !f.store.reserved {
		t.Fatal("unknown result retried or released")
	}
	f.store.view.AlreadyDispatched = true
	if _, err := f.deliver(); !errors.Is(err, ErrAlreadyDispatched) || f.tg.posts != 1 {
		t.Fatal("unknown prior transmission was replayed")
	}
}

func TestDeliveryDeadlineRecheckedAfterFinancialReadsAndChat(t *testing.T) {
	f := newDeliveryFixture(t)
	f.rec.hook = func(n int) error {
		if n == 2 {
			f.now = f.store.view.Request.ExpiresAt
		}
		return nil
	}
	f.assertDenial(DeniedExpired, false)
	f = newDeliveryFixture(t)
	f.tg.beforePrepare = func() { f.now = f.store.view.Request.ExpiresAt }
	if _, err := f.deliver(); err == nil || f.tg.posts != 0 {
		t.Fatal("expiry between private-chat read and final callback sent secret")
	}
}

func TestDeliverySensitiveValuesAreRedacted(t *testing.T) {
	f := newDeliveryFixture(t)
	const sentinel = "synthetic-sensitive-error-sentinel"
	f.store.loadErr = errors.New(sentinel)
	_, err := f.deliver()
	for _, value := range []any{f.s, f.cfg, f.store.view.Request, f.store.view.Entitlement, f.store.view, err, telegram.ProtectedMessage{Text: sentinel}} {
		for _, format := range []string{"%v", "%+v", "%#v"} {
			rendered := fmt.Sprintf(format, value)
			for _, secret := range []string{sentinel, f.cfg.Token, f.cfg.SUIToken, f.store.view.Entitlement.Binding.Name, f.store.view.Entitlement.Activation.Token, f.cfg.SubscriptionBaseURL} {
				if strings.Contains(rendered, secret) {
					t.Fatal("sensitive value escaped redacted formatting")
				}
			}
		}
	}
	if f.log.Len() != 0 {
		t.Fatal("private failure was logged")
	}
}

func TestDeliveryRequestExactTTLAndPermitBounds(t *testing.T) {
	f := newDeliveryFixture(t)
	if f.store.view.Request.Validate() != nil || f.store.view.Request.ExpiresAt.Sub(f.store.view.Request.CreatedAt) != 5*time.Minute {
		t.Fatal("exact five minute request TTL is invalid")
	}
	f.store.permit = &Permit{Kind: Secret, NotAfter: f.now.Add(time.Hour)}
	if _, err := f.deliver(); err != nil || !f.tg.content.NotAfter.Equal(f.store.view.Request.ExpiresAt) {
		t.Fatal("secret permit was not bounded by request expiry")
	}
}

func TestDeliveryAggregateBudgetAndCancellation(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := f.s.Deliver(ctx, f.message); err != nil || f.tg.posts != 1 {
		t.Fatal("delivery under the outbox aggregate budget failed")
	}
	f = newDeliveryFixture(t)
	ctx, stop := context.WithCancel(context.Background())
	stop()
	if _, err := f.s.Deliver(ctx, f.message); !errors.Is(err, context.Canceled) || len(f.events) != 0 {
		t.Fatal("cancelled delivery performed I/O")
	}
	f = newDeliveryFixture(t)
	ctx, stop = context.WithCancel(context.Background())
	defer stop()
	f.p.hook = func(string, int) error { stop(); return context.Canceled }
	if _, err := f.s.Deliver(ctx, f.message); !errors.Is(err, context.Canceled) || f.store.authorizations != 0 || f.tg.sends != 0 {
		t.Fatal("provider cancellation sent a fallback or reserved delivery")
	}
}

func TestDeliveryFinalAuthorizationUsesOriginalViewAcrossABA(t *testing.T) {
	f := newDeliveryFixture(t)
	original := f.store.view
	currentRevision := original.Entitlement.Revision
	currentHash := original.Request.EntitlementHash
	f.tg.beforePrepare = func() {
		// Other transactions change access and then restore its apparent facts.
		// The monotonic local revision still makes the original view stale.
		currentRevision += 2
		currentHash = original.Request.EntitlementHash
	}
	f.store.onAuthorize = func() {
		if currentRevision == f.store.original.Entitlement.Revision || currentHash != f.store.original.Request.EntitlementHash {
			t.Fatal("synthetic ABA did not preserve apparent facts and advance revision")
		}
		f.store.permit = &Permit{Kind: Denial, Reason: DeniedEntitlement, NotAfter: f.now.Add(time.Second)}
	}
	if _, err := f.deliver(); err != nil || f.tg.content.Text != denialText || f.store.loads != 1 || !reflect.DeepEqual(f.store.original, original) {
		t.Fatal("service refreshed its original view or ignored the ABA downgrade")
	}
}

func TestDeliveryOriginalUTCScheduleMustRemainFuture(t *testing.T) {
	f := newDeliveryFixture(t)
	instant := f.now.Add(-time.Second).UTC().Format(time.RFC3339Nano)
	for _, step := range []activation.Step{activation.RecurringCreate, activation.RecurringStart} {
		i := f.store.view.Entitlement.Activation.Steps[step]
		i.RecurringDatetime = instant
		f.store.view.Entitlement.Activation.Steps[step] = i
	}
	fingerprint, err := f.store.view.Entitlement.Fingerprint()
	if err != nil {
		t.Fatal("synthetic UTC receipt is invalid")
	}
	f.store.view.Request.EntitlementHash = fingerprint
	f.rec.record.NextSendDatetime = instant
	f.assertDenial(DeniedExpired, false)
	if f.p.calls != 0 || f.sui.reads != 0 || f.rec.reads != 0 {
		t.Fatal("expired original UTC schedule reached providers")
	}
}

func TestDeliveryPostGrantPayloadFailureDoesNotFallback(t *testing.T) {
	f := newDeliveryFixture(t)
	f.tg.beforePrepare = func() { f.s.cfg.SubscriptionBaseURL = "invalid://\nsynthetic-sensitive-uri" }
	if _, err := f.deliver(); err == nil || f.tg.posts != 0 || f.tg.sends != 1 || !f.store.reserved {
		t.Fatal("post-grant payload failure caused a fallback or discarded reservation")
	}
	f = newDeliveryFixture(t)
	f.store.onAuthorize = func() {
		calls := 0
		f.s.now = func() time.Time {
			calls++
			if calls == 1 {
				return f.now
			}
			return f.now.Add(time.Second)
		}
	}
	if _, err := f.deliver(); !errors.Is(err, context.DeadlineExceeded) || f.tg.posts != 0 || !f.store.reserved {
		t.Fatal("expiry during in-memory rendering was not fenced")
	}
}
