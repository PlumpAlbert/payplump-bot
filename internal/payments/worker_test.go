package payments

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
)

type deadlineObservation struct {
	operation string
	remaining time.Duration
	has       bool
}

func observeDeadline(op string, ctx context.Context) deadlineObservation {
	deadline, ok := ctx.Deadline()
	return deadlineObservation{op, time.Until(deadline), ok}
}

func checkDeadline(t *testing.T, observation deadlineObservation, max time.Duration) {
	t.Helper()
	if !observation.has || observation.remaining <= 0 || observation.remaining > max || observation.remaining < max-time.Second {
		t.Errorf("%s deadline remaining %s (present %v), want close to %s", observation.operation, observation.remaining, observation.has, max)
	}
}

type finishObservation struct {
	claim    Claim
	snapshot Snapshot
	decision Decision
	notices  Notices
	recheck  time.Duration
}

type deferObservation struct {
	claim Claim
	delay time.Duration
	code  int
}

type paymentStoreFake struct {
	mu        sync.Mutex
	snapshot  Snapshot
	finishes  []finishObservation
	deferrals []deferObservation
	deadlines []deadlineObservation
	seed      func(context.Context, int64) (int, error)
	claim     func(context.Context, int64, int) ([]Claim, error)
	load      func(context.Context, Claim) (Snapshot, error)
	finish    func(context.Context, finishObservation) error
	deferFn   func(context.Context, deferObservation) error
}

func (f *paymentStoreFake) note(ctx context.Context, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deadlines = append(f.deadlines, observeDeadline(name, ctx))
}
func (f *paymentStoreFake) RecordPaymentWake(context.Context, int64, [32]byte) (bool, error) {
	return false, errors.New("worker must not manufacture webhook receipts")
}
func (f *paymentStoreFake) SeedPaymentChecks(ctx context.Context, id int64) (int, error) {
	f.note(ctx, "seed")
	if f.seed != nil {
		return f.seed(ctx, id)
	}
	return 0, nil
}
func (f *paymentStoreFake) ClaimPaymentChecks(ctx context.Context, id int64, n int) ([]Claim, error) {
	f.note(ctx, "claim")
	if f.claim != nil {
		return f.claim(ctx, id, n)
	}
	return nil, nil
}
func (f *paymentStoreFake) LoadPaymentSnapshot(ctx context.Context, claim Claim) (Snapshot, error) {
	f.note(ctx, "load")
	if f.load != nil {
		return f.load(ctx, claim)
	}
	return f.snapshot, nil
}
func (f *paymentStoreFake) DeferPaymentCheck(ctx context.Context, claim Claim, delay time.Duration, code int) error {
	f.note(ctx, "defer")
	observation := deferObservation{claim, delay, code}
	f.mu.Lock()
	f.deferrals = append(f.deferrals, observation)
	f.mu.Unlock()
	if f.deferFn != nil {
		return f.deferFn(ctx, observation)
	}
	return nil
}
func (f *paymentStoreFake) FinishPaymentCheck(ctx context.Context, claim Claim, snapshot Snapshot, d Decision, n Notices, recheck time.Duration) error {
	f.note(ctx, "finish")
	observation := finishObservation{claim, snapshot, d, n, recheck}
	f.mu.Lock()
	f.finishes = append(f.finishes, observation)
	f.mu.Unlock()
	if f.finish != nil {
		return f.finish(ctx, observation)
	}
	return nil
}
func (f *paymentStoreFake) results() ([]finishObservation, []deferObservation) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.finishes), slices.Clone(f.deferrals)
}

type settlementReaderFake struct {
	mu        sync.Mutex
	fixture   settlementFixture
	calls     []string
	deadlines []deadlineObservation
	counts    map[string]int
	customer  func(context.Context, int) (invoiceninja.SettlementCustomer, error)
	invoice   func(context.Context, int) (invoiceninja.SettlementInvoice, error)
	payments  func(context.Context, int) ([]invoiceninja.SettlementPayment, error)
}

func (f *settlementReaderFake) note(ctx context.Context, op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.counts == nil {
		f.counts = make(map[string]int)
	}
	f.counts[op]++
	f.calls = append(f.calls, op)
	f.deadlines = append(f.deadlines, observeDeadline(op, ctx))
	return f.counts[op]
}
func (f *settlementReaderFake) GetSettlementCustomer(ctx context.Context, id string) (invoiceninja.SettlementCustomer, error) {
	n := f.note(ctx, "customer")
	if id != f.fixture.local.CustomerID {
		return invoiceninja.SettlementCustomer{}, errors.New("unexpected customer selector")
	}
	if f.customer != nil {
		return f.customer(ctx, n)
	}
	return f.fixture.customer, nil
}
func (f *settlementReaderFake) GetSettlementInvoice(ctx context.Context, id string) (invoiceninja.SettlementInvoice, error) {
	n := f.note(ctx, "invoice")
	if id != f.fixture.local.InvoiceID {
		return invoiceninja.SettlementInvoice{}, errors.New("unexpected invoice selector")
	}
	if f.invoice != nil {
		return f.invoice(ctx, n)
	}
	return f.fixture.invoice, nil
}
func (f *settlementReaderFake) ListSettlementPayments(ctx context.Context, id string) ([]invoiceninja.SettlementPayment, error) {
	n := f.note(ctx, "payments")
	if id != f.fixture.local.CustomerID {
		return nil, errors.New("payments must be inventoried by exact local customer")
	}
	if f.payments != nil {
		return f.payments(ctx, n)
	}
	return cloneSettlementPayments(f.fixture.payments), nil
}
func (f *settlementReaderFake) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func cloneSettlementPayments(in []invoiceninja.SettlementPayment) []invoiceninja.SettlementPayment {
	out := slices.Clone(in)
	for i := range out {
		out[i].Allocations = slices.Clone(out[i].Allocations)
	}
	return out
}

func paymentClaim() Claim {
	return Claim{Lease: queue.Lease{ID: 7, Attempt: 3}, BotID: 42, Generation: 9}
}

func workerForTest(t *testing.T, store Store, reader Reader, cfg Config, logs *bytes.Buffer) *Worker {
	t.Helper()
	if logs == nil {
		logs = new(bytes.Buffer)
	}
	w, err := New(store, reader, 42, []int64{99, 98, 99}, cfg, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	w.now = func() time.Time { return time.Date(2026, 3, 15, 12, 0, 0, 0, time.Local) }
	return w
}

func TestWorkerVerifiedRereadsAllResourcesBeforeFinish(t *testing.T) {
	f := cleanSettlement()
	store := &paymentStoreFake{snapshot: f.local}
	reader := &settlementReaderFake{fixture: f}
	store.finish = func(_ context.Context, observation finishObservation) error {
		want := []string{"customer", "invoice", "payments", "payments", "invoice", "customer"}
		if got := reader.callList(); !reflect.DeepEqual(got, want) {
			t.Errorf("reads at finish = %v, want %v", got, want)
		}
		return nil
	}
	w := workerForTest(t, store, reader, validPaymentConfig(), nil)
	w.handle(context.Background(), paymentClaim())
	finished, deferred := store.results()
	if len(finished) != 1 || len(deferred) != 0 {
		t.Fatalf("finishes=%d defers=%d", len(finished), len(deferred))
	}
	got := finished[0]
	expectDecision(t, got.decision, Verified, 0)
	if got.claim != paymentClaim() || got.snapshot != f.local || got.recheck != w.cfg.RecheckInterval {
		t.Fatal("lost claim fencing or local snapshot")
	}
	if got.snapshot.PaymentDate != "" || got.snapshot.NextInvoiceDate != "" || got.snapshot.DesiredAccess {
		t.Fatal("worker mutated local dates/access instead of passing proof to store")
	}
	if got.decision.Proof.PaymentDate != "2026-01-20" || got.decision.Proof.NextInvoiceDate != "2026-02-20" {
		t.Fatal("worker used its observation time instead of Payment.date")
	}
	if len(got.notices.Confirmed) != 3 {
		t.Fatalf("confirmed notices=%d, want client and two unique admins", len(got.notices.Confirmed))
	}
	for _, n := range append(slices.Clone(got.notices.Confirmed), got.notices.Review...) {
		if err := n.Validate(); err != nil {
			t.Errorf("invalid notice: %v", err)
		}
		if n.BotID != 42 {
			t.Error("notice escaped the verified bot")
		}
	}
	for _, d := range store.deadlines {
		checkDeadline(t, d, 8*time.Second)
	}
	for _, d := range reader.deadlines {
		checkDeadline(t, d, time.Minute)
	}
}

func TestWorkerChangedEvidenceDefersWithoutAnyFinish(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*settlementReaderFake)
	}{
		{"allocation amount", func(r *settlementReaderFake) {
			r.payments = func(_ context.Context, n int) ([]invoiceninja.SettlementPayment, error) {
				p := cloneSettlementPayments(r.fixture.payments)
				if n == 2 {
					p[0].Allocations[0].Amount = "99"
				}
				return p, nil
			}
		}},
		{"allocation date", func(r *settlementReaderFake) {
			r.payments = func(_ context.Context, n int) ([]invoiceninja.SettlementPayment, error) {
				p := cloneSettlementPayments(r.fixture.payments)
				if n == 2 {
					p[0].Date = "2026-01-18"
				}
				return p, nil
			}
		}},
		{"whole root amount even unchanged applied proof", func(r *settlementReaderFake) {
			r.payments = func(_ context.Context, n int) ([]invoiceninja.SettlementPayment, error) {
				p := cloneSettlementPayments(r.fixture.payments)
				if n == 2 {
					p[0].Amount = "101"
				}
				return p, nil
			}
		}},
		{"whole other invoice allocation inventory", func(r *settlementReaderFake) {
			r.payments = func(_ context.Context, n int) ([]invoiceninja.SettlementPayment, error) {
				p := cloneSettlementPayments(r.fixture.payments)
				p[0].Amount, p[0].Applied = "150", "150"
				id := "other-invoice-A"
				if n == 2 {
					id = "other-invoice-B"
				}
				p[0].Allocations = append(p[0].Allocations, invoiceninja.SettlementAllocation{ID: "other-allocation", InvoiceID: id, PaymentID: p[0].ID, Amount: "50", Refunded: "0"})
				return p, nil
			}
		}},
		{"whole ignored pending inventory", func(r *settlementReaderFake) {
			r.payments = func(_ context.Context, n int) ([]invoiceninja.SettlementPayment, error) {
				p := cloneSettlementPayments(r.fixture.payments)
				if n == 1 {
					pending := cashPayment("pending", "pending-a", "1", "2026-01-21")
					pending.StatusID, pending.CurrencyID = "1", ""
					p = append(p, pending)
				}
				return p, nil
			}
		}},
		{"whole invoice facts", func(r *settlementReaderFake) {
			r.invoice = func(_ context.Context, n int) (invoiceninja.SettlementInvoice, error) {
				i := r.fixture.invoice
				if n == 2 {
					i.Number = "renumbered"
				}
				return i, nil
			}
		}},
		{"whole customer facts", func(r *settlementReaderFake) {
			r.customer = func(_ context.Context, n int) (invoiceninja.SettlementCustomer, error) {
				c := r.fixture.customer
				if n == 2 {
					c.IDNumber = "someone-else"
				}
				return c, nil
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := cleanSettlement()
			store := &paymentStoreFake{snapshot: f.local}
			reader := &settlementReaderFake{fixture: f}
			tc.setup(reader)
			workerForTest(t, store, reader, validPaymentConfig(), nil).handle(context.Background(), paymentClaim())
			finished, deferred := store.results()
			if len(finished) != 0 || len(deferred) != 1 {
				t.Fatalf("unstable WHOLE settlement inventory: finishes=%d defers=%d; must defer before any Finish", len(finished), len(deferred))
			}
		})
	}
}

func TestWorkerStableOutOfOrderInventoryCanFinish(t *testing.T) {
	f := cleanSettlement()
	f.payments[0].Amount, f.payments[0].Applied = "150", "150"
	f.payments[0].Allocations = append(f.payments[0].Allocations, invoiceninja.SettlementAllocation{ID: "other", PaymentID: "payment-A", InvoiceID: "other-invoice", Amount: "50", Refunded: "0"})
	store := &paymentStoreFake{snapshot: f.local}
	reader := &settlementReaderFake{fixture: f}
	reader.payments = func(_ context.Context, n int) ([]invoiceninja.SettlementPayment, error) {
		p := cloneSettlementPayments(f.payments)
		if n == 2 {
			for i := range p {
				slices.Reverse(p[i].Allocations)
			}
			slices.Reverse(p)
		}
		return p, nil
	}
	workerForTest(t, store, reader, validPaymentConfig(), nil).handle(context.Background(), paymentClaim())
	finished, deferred := store.results()
	if len(finished) != 1 || len(deferred) != 0 {
		t.Fatalf("reordered equivalent inventory: finishes=%d defers=%d", len(finished), len(deferred))
	}
	expectDecision(t, finished[0].decision, Verified, 0)
}

func TestWorkerNegativeAndReadFailuresNeverCreatePositiveProof(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(*paymentStoreFake, *settlementReaderFake, *Config)
		kind     Kind
		code     int
		deferred bool
	}{
		{"pending", func(_ *paymentStoreFake, r *settlementReaderFake, _ *Config) {
			r.fixture.invoice.StatusID, r.fixture.invoice.Balance, r.fixture.invoice.PaidToDate = "2", "200", "0"
		}, Pending, 0, false},
		{"zero balance no cash", func(_ *paymentStoreFake, r *settlementReaderFake, _ *Config) { r.fixture.payments = nil }, NeedsReview, ReviewAllocations, false},
		{"wrong local bot", func(s *paymentStoreFake, _ *settlementReaderFake, _ *Config) { s.snapshot.BotID++ }, NeedsReview, ReviewAssociation, false},
		{"wrong local client", func(s *paymentStoreFake, _ *settlementReaderFake, _ *Config) { s.snapshot.ClientID++ }, NeedsReview, ReviewAssociation, false},
		{"excluded telegram", func(_ *paymentStoreFake, r *settlementReaderFake, c *Config) {
			c.ExcludedTelegramIDs = []int64{r.fixture.local.TelegramID}
		}, NeedsReview, ReviewExcluded, false},
		{"customer contract", func(_ *paymentStoreFake, r *settlementReaderFake, _ *Config) {
			r.customer = func(context.Context, int) (invoiceninja.SettlementCustomer, error) {
				return invoiceninja.SettlementCustomer{}, fmt.Errorf("wrapped: %w", invoiceninja.ErrSettlementContract)
			}
		}, "", 0, true},
		{"version mismatch", func(_ *paymentStoreFake, r *settlementReaderFake, _ *Config) {
			r.invoice = func(context.Context, int) (invoiceninja.SettlementInvoice, error) {
				return invoiceninja.SettlementInvoice{}, invoiceninja.ErrSettlementVersion
			}
		}, "", 0, true},
		{"incomplete inventory", func(_ *paymentStoreFake, r *settlementReaderFake, _ *Config) {
			r.payments = func(context.Context, int) ([]invoiceninja.SettlementPayment, error) {
				return nil, invoiceninja.ErrSettlementIncomplete
			}
		}, "", 0, true},
		{"invalid transport contract", func(_ *paymentStoreFake, r *settlementReaderFake, _ *Config) {
			r.payments = func(context.Context, int) ([]invoiceninja.SettlementPayment, error) {
				return nil, integrationhttp.ErrInvalidResponse
			}
		}, "", 0, true},
		{"transient remote failure", func(_ *paymentStoreFake, r *settlementReaderFake, _ *Config) {
			r.payments = func(context.Context, int) ([]invoiceninja.SettlementPayment, error) {
				return nil, errors.New("offline")
			}
		}, "", 0, true},
		{"second read failure", func(_ *paymentStoreFake, r *settlementReaderFake, _ *Config) {
			r.payments = func(_ context.Context, n int) ([]invoiceninja.SettlementPayment, error) {
				if n == 2 {
					return nil, errors.New("second read failed")
				}
				return cloneSettlementPayments(r.fixture.payments), nil
			}
		}, "", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := cleanSettlement()
			store := &paymentStoreFake{snapshot: f.local}
			reader := &settlementReaderFake{fixture: f}
			cfg := validPaymentConfig()
			tc.setup(store, reader, &cfg)
			workerForTest(t, store, reader, cfg, nil).handle(context.Background(), paymentClaim())
			finished, deferred := store.results()
			if tc.deferred {
				if len(finished) != 0 || len(deferred) != 1 {
					t.Fatalf("finishes=%d defers=%d", len(finished), len(deferred))
				}
				return
			}
			if len(finished) != 1 || len(deferred) != 0 {
				t.Fatalf("finishes=%d defers=%d", len(finished), len(deferred))
			}
			expectDecision(t, finished[0].decision, tc.kind, tc.code)
			if len(finished[0].notices.Confirmed) != 0 {
				t.Fatal("negative observation queued a confirmation")
			}
			if tc.code == ReviewAssociation || tc.code == ReviewExcluded {
				if len(reader.callList()) != 0 {
					t.Error("local exclusion/association guard still contacted provider")
				}
			}
			if tc.kind == Pending && !reflect.DeepEqual(reader.callList(), []string{"customer", "invoice"}) {
				t.Error("pending invoice unnecessarily inventoried payment evidence")
			}
		})
	}
}

func TestWorkerLeaseLossAndLoadFailure(t *testing.T) {
	for _, err := range []error{queue.ErrLeaseLost, errors.New("load unavailable")} {
		t.Run(err.Error(), func(t *testing.T) {
			store := &paymentStoreFake{load: func(context.Context, Claim) (Snapshot, error) { return Snapshot{}, err }}
			reader := &settlementReaderFake{fixture: cleanSettlement()}
			workerForTest(t, store, reader, validPaymentConfig(), nil).handle(context.Background(), paymentClaim())
			finished, deferred := store.results()
			want := 1
			if errors.Is(err, queue.ErrLeaseLost) {
				want = 0
			}
			if len(finished) != 0 || len(deferred) != want || len(reader.callList()) != 0 {
				t.Fatalf("finishes=%d defers=%d reads=%v", len(finished), len(deferred), reader.callList())
			}
		})
	}
}

func TestWorkerRetryBoundsAndLogsDoNotLeakRemoteData(t *testing.T) {
	for _, tc := range []struct {
		attempt, status int
		delay           time.Duration
	}{
		{1, 0, 5 * time.Second}, {2, 0, 10 * time.Second}, {20, 0, 5 * time.Minute}, {1, 429, time.Minute}, {20, 429, 5 * time.Minute},
	} {
		t.Run(fmt.Sprintf("attempt%d/status%d", tc.attempt, tc.status), func(t *testing.T) {
			f := cleanSettlement()
			store := &paymentStoreFake{snapshot: f.local}
			reader := &settlementReaderFake{fixture: f}
			cfg := validPaymentConfig()
			sensitive := "customer-private-body " + cfg.Token + " " + cfg.WebhookSecret
			reader.payments = func(context.Context, int) ([]invoiceninja.SettlementPayment, error) {
				if tc.status != 0 {
					return nil, fmt.Errorf("%s: %w", sensitive, &integrationhttp.HTTPError{StatusCode: tc.status})
				}
				return nil, errors.New(sensitive)
			}
			store.deferFn = func(context.Context, deferObservation) error { return errors.New(sensitive) }
			var logs bytes.Buffer
			w := workerForTest(t, store, reader, cfg, &logs)
			claim := paymentClaim()
			claim.Attempt = tc.attempt
			w.handle(context.Background(), claim)
			finished, deferred := store.results()
			if len(finished) != 0 || len(deferred) != 1 {
				t.Fatalf("finishes=%d defers=%d", len(finished), len(deferred))
			}
			wantCode := tc.status
			if wantCode == 0 {
				wantCode = 9000
			}
			if deferred[0].delay != tc.delay || deferred[0].code != wantCode || deferred[0].claim != claim {
				t.Fatalf("unexpected retry %+v", deferred[0])
			}
			for _, value := range []string{cfg.Token, cfg.WebhookSecret, f.local.CustomerID, f.local.InvoiceID, f.customer.IDNumber, "customer-private-body"} {
				if strings.Contains(logs.String(), value) {
					t.Errorf("logs leak sensitive value %q", value)
				}
			}
		})
	}
}

func TestWorkerRunClaimsIndependentPaymentChecksAndJoinsFinalization(t *testing.T) {
	for _, workers := range []int{1, 4} {
		t.Run(fmt.Sprintf("workers%d", workers), func(t *testing.T) {
			f := cleanSettlement()
			cfg := validPaymentConfig()
			cfg.Workers = workers
			store := &paymentStoreFake{snapshot: f.local}
			reader := &settlementReaderFake{fixture: f}
			entered := make(chan int64, workers)
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var claims atomic.Int64
			var active, maximum atomic.Int64
			store.seed = func(_ context.Context, botID int64) (int, error) {
				if botID != 42 {
					t.Error("wrong discovery bot")
				}
				return workers, nil
			}
			store.claim = func(_ context.Context, botID int64, n int) ([]Claim, error) {
				if botID != 42 || n != workers {
					t.Errorf("claim bot/limit %d/%d", botID, n)
				}
				if claims.Add(1) > 1 {
					return nil, nil
				}
				out := make([]Claim, workers)
				for i := range out {
					out[i] = paymentClaim()
					out[i].ID += int64(i)
				}
				return out, nil
			}
			store.load = func(_ context.Context, claim Claim) (Snapshot, error) {
				s := f.local
				s.ClientID = claim.ID
				return s, nil
			}
			store.finish = func(ctx context.Context, observation finishObservation) error {
				checkDeadline(t, observeDeadline("finish", ctx), 8*time.Second)
				n := active.Add(1)
				defer active.Add(-1)
				for old := maximum.Load(); n > old && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
				}
				entered <- observation.claim.ID
				<-release
				if ctx.Err() != nil {
					t.Error("service cancellation cancelled the known-finalization budget")
				}
				return errors.New("finalization uncertain; must not start a second DB budget")
			}
			w := workerForTest(t, store, reader, cfg, nil)
			done := make(chan error, 1)
			go func() { done <- w.Run(ctx) }()
			seen := make(map[int64]bool)
			for range workers {
				select {
				case id := <-entered:
					if seen[id] {
						t.Error("same payment claim processed twice")
					}
					seen[id] = true
				case <-time.After(3 * time.Second):
					t.Fatal("configured claims did not run concurrently")
				}
			}
			cancel()
			select {
			case <-done:
				t.Fatal("Run returned without joining known finalizations")
			default:
			}
			unblock()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Run did not join after finalization completed")
			}
			finished, deferred := store.results()
			if len(finished) != workers || len(deferred) != 0 {
				t.Fatalf("finishes=%d defers=%d; finalization must use one budget", len(finished), len(deferred))
			}
			if maximum.Load() != int64(workers) {
				t.Errorf("concurrency=%d, want %d", maximum.Load(), workers)
			}
			for _, d := range store.deadlines {
				checkDeadline(t, d, 8*time.Second)
			}
		})
	}
}

func TestWorkerNewValidatesAndClonesIndependentInputs(t *testing.T) {
	f := cleanSettlement()
	store := &paymentStoreFake{snapshot: f.local}
	reader := &settlementReaderFake{fixture: f}
	cfg := validPaymentConfig()
	cfg.ExcludedTelegramIDs = []int64{77}
	admins := []int64{99, 98, 99}
	w, err := New(store, reader, 42, admins, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	admins[0], cfg.ExcludedTelegramIDs[0] = 1, 2
	if !reflect.DeepEqual(w.admins, []int64{98, 99}) || !reflect.DeepEqual(w.cfg.ExcludedTelegramIDs, []int64{77}) {
		t.Fatal("constructor retained mutable caller slices")
	}
	for _, tc := range []struct {
		name   string
		store  Store
		reader Reader
		bot    int64
		admins []int64
		cfg    Config
	}{
		{"nil store", nil, reader, 42, []int64{99}, validPaymentConfig()},
		{"nil reader", store, nil, 42, []int64{99}, validPaymentConfig()},
		{"unverified bot", store, reader, 0, []int64{99}, validPaymentConfig()},
		{"no admin", store, reader, 42, nil, validPaymentConfig()},
		{"invalid admin", store, reader, 42, []int64{-1}, validPaymentConfig()},
		{"disabled", store, reader, 42, []int64{99}, Config{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.store, tc.reader, tc.bot, tc.admins, tc.cfg, nil); err == nil {
				t.Fatal("accepted invalid worker configuration")
			}
		})
	}
}
