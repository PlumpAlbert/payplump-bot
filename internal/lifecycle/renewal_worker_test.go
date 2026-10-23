package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

var errRenewalTestUnknown = errors.New("synthetic uncertain write")
var errRenewalTestConfirm = errors.New("synthetic confirmation failure")

type renewalMemoryStore struct {
	mu                           sync.Mutex
	snapshot                     RenewalSnapshot
	claim                        queue.Operation
	outcome                      string
	reviewCode                   int
	steps                        []RenewalStep
	failAfter                    RenewalStep
	failConfirm                  bool
	cancelOnLoad                 chan struct{}
	seedCalls, claimCalls, loads int
	cancel                       context.CancelFunc
}

func (s *renewalMemoryStore) SeedRenewalOperations(context.Context, int64, []int64, int) (int, error) {
	s.mu.Lock()
	s.seedCalls++
	s.mu.Unlock()
	return 0, nil
}
func (s *renewalMemoryStore) ClaimOperations(context.Context, []string, int, time.Duration) ([]queue.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimCalls++
	if s.claimCalls == 1 {
		return []queue.Operation{s.claim}, nil
	}
	return nil, nil
}
func (s *renewalMemoryStore) LoadRenewalSnapshot(ctx context.Context, claim queue.Operation, bot int64) (RenewalSnapshot, error) {
	if s.cancelOnLoad != nil {
		select {
		case s.cancelOnLoad <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return RenewalSnapshot{}, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	if s.outcome != "" || claim.ID != s.claim.ID || claim.Attempt != s.claim.Attempt || bot != s.snapshot.BotID {
		return RenewalSnapshot{}, queue.ErrLeaseLost
	}
	return cloneRenewalSnapshot(s.snapshot), nil
}
func (s *renewalMemoryStore) FreezeRenewalPlan(_ context.Context, claim queue.Operation, bot int64, expected RenewalSnapshot, plan RenewalPlan, baseline json.RawMessage, _ []int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if claim != s.claim || bot != s.snapshot.BotID || !reflect.DeepEqual(expected, s.snapshot) {
		return queue.ErrConflict
	}
	p := plan
	s.snapshot.Plan = &p
	s.snapshot.PlanBaseline = append([]byte(nil), baseline...)
	return nil
}
func (s *renewalMemoryStore) AuthorizeRenewalWrite(_ context.Context, claim queue.Operation, bot int64, expected RenewalSnapshot, step RenewalStep, request json.RawMessage, _ string, allowed []int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if claim != s.claim || bot != s.snapshot.BotID || !reflect.DeepEqual(expected, s.snapshot) || s.snapshot.WriterBusy {
		return false, queue.ErrConflict
	}
	if !containsTestID(allowed, s.snapshot.TelegramID) {
		return false, queue.ErrConflict
	}
	next, pending, _, valid := NextRenewalStep(*s.snapshot.Plan, s.snapshot.Steps)
	if !valid || pending || next != step {
		return false, queue.ErrConflict
	}
	s.snapshot.WriterEpoch()
	s.steps = append(s.steps, step)
	s.snapshot.Steps[step] = RenewalStepRecord{Request: append([]byte(nil), request...), DispatchAttempt: claim.Attempt}
	s.snapshot.WriterBusy = true
	s.snapshot.WriterOperationID = claim.ID
	s.snapshot.WriterStep = string(step)
	return true, nil
}
func (s *renewalMemoryStore) ConfirmRenewalStep(_ context.Context, claim queue.Operation, step RenewalStep, externalID string, receipt json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if claim != s.claim || s.snapshot.WriterOperationID != claim.ID || s.snapshot.WriterStep != string(step) {
		return queue.ErrConflict
	}
	if s.failConfirm {
		return errRenewalTestConfirm
	}
	r := s.snapshot.Steps[step]
	r.Confirmed = true
	r.ExternalID = externalID
	r.Receipt = append([]byte(nil), receipt...)
	s.snapshot.Steps[step] = r
	s.snapshot.WriterBusy = false
	s.snapshot.WriterOperationID = 0
	s.snapshot.WriterStep = ""
	return nil
}
func (s *renewalMemoryStore) RetryOperation(_ context.Context, lease queue.Lease, _ time.Duration, _ int) error {
	if lease != s.claim.Lease {
		return queue.ErrLeaseLost
	}
	return nil
}
func (s *renewalMemoryStore) ReviewRenewal(_ context.Context, claim queue.Operation, _ int64, _ RenewalSnapshot, code int, _ []queue.Draft) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if claim != s.claim {
		return queue.ErrLeaseLost
	}
	s.outcome = "review"
	s.reviewCode = code
	return nil
}
func (s *renewalMemoryStore) FinishRenewal(_ context.Context, claim queue.Operation, _ int64, _ RenewalSnapshot, drafts []queue.Draft) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if claim != s.claim {
		return queue.ErrLeaseLost
	}
	if len(drafts) < 2 {
		return errors.New("missing success notices")
	}
	s.outcome = "completed"
	return nil
}
func (s *RenewalSnapshot) WriterEpoch() { s.WriteEpoch++ }
func cloneRenewalSnapshot(s RenewalSnapshot) RenewalSnapshot {
	if s.Steps != nil {
		m := make(map[RenewalStep]RenewalStepRecord, len(s.Steps))
		for k, v := range s.Steps {
			v.Request = append(json.RawMessage(nil), v.Request...)
			v.Receipt = append(json.RawMessage(nil), v.Receipt...)
			m[k] = v
		}
		s.Steps = m
	}
	if s.OverdueSteps != nil {
		m := map[string]bool{}
		for k, v := range s.OverdueSteps {
			m[k] = v
		}
		s.OverdueSteps = m
	}
	if s.Plan != nil {
		p := *s.Plan
		s.Plan = &p
	}
	s.PlanBaseline = append(json.RawMessage(nil), s.PlanBaseline...)
	return s
}

type renewalMemoryPayments struct {
	customer invoiceninja.SettlementCustomer
	invoice  invoiceninja.SettlementInvoice
	items    []invoiceninja.SettlementPayment
}

func (r *renewalMemoryPayments) GetSettlementCustomer(context.Context, string) (invoiceninja.SettlementCustomer, error) {
	return r.customer, nil
}
func (r *renewalMemoryPayments) GetSettlementInvoice(context.Context, string) (invoiceninja.SettlementInvoice, error) {
	return r.invoice, nil
}
func (r *renewalMemoryPayments) ListSettlementPayments(context.Context, string) ([]invoiceninja.SettlementPayment, error) {
	return append([]invoiceninja.SettlementPayment(nil), r.items...), nil
}

type renewalMemoryInvoice struct{ invoice invoiceninja.CycleInvoice }

func (r renewalMemoryInvoice) GetInvoice(context.Context, string) (invoiceninja.CycleInvoice, error) {
	return r.invoice, nil
}

type renewalMemoryRecurring struct {
	record    invoiceninja.RecurringRecord
	target    string
	calls     []RenewalStep
	failAfter RenewalStep
}

func (r *renewalMemoryRecurring) Get(context.Context, string) (invoiceninja.RecurringRecord, error) {
	return r.record, nil
}
func (r *renewalMemoryRecurring) PauseAuthorized(ctx context.Context, _ invoiceninja.PauseDraft, auth func(context.Context) error) (invoiceninja.RecurringRecord, error) {
	r.calls = append(r.calls, RenewalPause)
	if err := auth(ctx); err != nil {
		return invoiceninja.RecurringRecord{}, err
	}
	r.record.StatusID = "3"
	if r.failAfter == RenewalPause {
		return r.record, errRenewalTestUnknown
	}
	return r.record, nil
}
func (r *renewalMemoryRecurring) RescheduleAuthorized(ctx context.Context, _ invoiceninja.ScheduleDraft, auth func(context.Context) error) (invoiceninja.RecurringRecord, error) {
	r.calls = append(r.calls, RenewalSchedule)
	if err := auth(ctx); err != nil {
		return invoiceninja.RecurringRecord{}, err
	}
	r.record.NextSendDate = r.target
	r.record.NextSendDatetime = r.target + "T08:00:00Z"
	if r.failAfter == RenewalSchedule {
		return r.record, errRenewalTestUnknown
	}
	return r.record, nil
}
func (r *renewalMemoryRecurring) ResumeAuthorized(ctx context.Context, _ invoiceninja.ResumeDraft, auth func(context.Context) error) (invoiceninja.RecurringRecord, error) {
	r.calls = append(r.calls, RenewalResume)
	if err := auth(ctx); err != nil {
		return invoiceninja.RecurringRecord{}, err
	}
	r.record.StatusID = "2"
	if r.failAfter == RenewalResume {
		return r.record, errRenewalTestUnknown
	}
	return r.record, nil
}

type renewalMemoryPanel struct {
	record sui.ClientRecord
	calls  int
	fail   bool
}

func (r *renewalMemoryPanel) GetRecord(context.Context, int64) (sui.ClientRecord, error) {
	return r.record, nil
}
func (r *renewalMemoryPanel) EnableAuthorized(ctx context.Context, _ sui.EnableDraft, auth func(context.Context) error) (sui.ClientRecord, error) {
	r.calls++
	if err := auth(ctx); err != nil {
		return sui.ClientRecord{}, err
	}
	var value map[string]any
	if json.Unmarshal(r.record.Raw(), &value) != nil {
		return sui.ClientRecord{}, sui.ErrManagedResponse
	}
	value["enable"] = true
	raw, _ := json.Marshal(value)
	r.record, _ = sui.ParseClientRecord(raw)
	if r.fail {
		return r.record, sui.ErrManagedUnknown
	}
	return r.record, nil
}

func TestRenewalWorkerNoOpNormalPaidPeriodDoesNotWrite(t *testing.T) {
	w, store, recurring, panel := renewalWorkerFixture(t, false, false)
	w.handle(context.Background(), store.claim)
	if store.outcome != "completed" || len(recurring.calls) != 0 || panel.calls != 0 {
		t.Fatalf("outcome=%q recurring=%v enable=%d", store.outcome, recurring.calls, panel.calls)
	}
}

func TestRenewalWorkerOrdersChangedScheduleAndOverdueRestoration(t *testing.T) {
	for _, tc := range []struct {
		name    string
		overdue bool
		want    []RenewalStep
	}{{"active changed date", false, []RenewalStep{RenewalPause, RenewalSchedule, RenewalResume}}, {"late overdue restoration", true, []RenewalStep{RenewalSchedule, RenewalEnable, RenewalResume}}} {
		t.Run(tc.name, func(t *testing.T) {
			w, store, recurring, panel := renewalWorkerFixture(t, tc.overdue, true)
			w.handle(context.Background(), store.claim)
			if store.outcome != "completed" || !reflect.DeepEqual(recurring.calls, filterRecurring(tc.want)) || panel.calls != countStep(tc.want, RenewalEnable) {
				t.Fatalf("outcome=%q recurring=%v enable=%d", store.outcome, recurring.calls, panel.calls)
			}
			if !reflect.DeepEqual(store.steps, tc.want) {
				t.Fatalf("write order=%v want=%v", store.steps, tc.want)
			}
		})
	}
}

func TestRenewalWorkerRejectsExpiredInitialTimestampEvenWhenRescheduling(t *testing.T) {
	worker, store, recurring, _ := renewalWorkerFixture(t, false, true)
	recurring.record.NextSendDatetime = "2040-01-30T08:00:00Z"
	worker.handle(context.Background(), store.claim)
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.outcome != "review" || store.reviewCode != ReviewRenewalTarget || len(store.steps) != 0 || len(recurring.calls) != 0 {
		t.Fatalf("expired source timestamp was not held before writes: outcome=%q code=%d intents=%v calls=%v", store.outcome, store.reviewCode, store.steps, recurring.calls)
	}
}

func TestRenewalWorkerUnknownWriteIsReviewedAndNeverReplayed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		overdue bool
		fail    RenewalStep
	}{
		{"pause", false, RenewalPause}, {"schedule after pause", false, RenewalSchedule}, {"resume", false, RenewalResume},
		{"late schedule", true, RenewalSchedule}, {"late enable", true, RenewalEnable}, {"late resume", true, RenewalResume},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, store, recurring, panel := renewalWorkerFixture(t, tc.overdue, true)
			recurring.failAfter = tc.fail
			if tc.fail == RenewalEnable {
				panel.fail = true
			}
			w.handle(context.Background(), store.claim)
			if store.outcome != "review" || store.reviewCode != ReviewRenewalUnknownWrite || !store.snapshot.WriterBusy || store.snapshot.WriterStep != string(tc.fail) || store.snapshot.Steps[tc.fail].Confirmed {
				t.Fatalf("unknown action outcome=%q code=%d order=%v guard=%+v steps=%+v", store.outcome, store.reviewCode, store.steps, store.snapshot, store.snapshot.Steps)
			}
		})
	}
}

func TestRenewalWorkerConfirmationFailureKeepsGuardPinned(t *testing.T) {
	w, store, recurring, _ := renewalWorkerFixture(t, false, true)
	store.failConfirm = true
	w.handle(context.Background(), store.claim)
	if store.outcome != "review" || !store.snapshot.WriterBusy || store.snapshot.WriterStep != string(RenewalPause) || store.snapshot.Steps[RenewalPause].Confirmed || len(recurring.calls) != 1 {
		t.Fatalf("confirmation failure lost pinned intent: outcome=%q snapshot=%+v calls=%v", store.outcome, store.snapshot, recurring.calls)
	}
}

func TestRenewalWorkerReclaimedUnknownIntentDoesNotReplay(t *testing.T) {
	w, store, recurring, _ := renewalWorkerFixture(t, false, true)
	recurring.failAfter = RenewalPause
	w.handle(context.Background(), store.claim)
	pending := cloneRenewalSnapshot(store.snapshot)
	pendingStore := &renewalMemoryStore{snapshot: pending, claim: queue.Operation{Lease: queue.Lease{ID: store.claim.ID, Attempt: store.claim.Attempt + 1}, ClientID: store.claim.ClientID, Kind: store.claim.Kind}}
	w.store = pendingStore
	w.handle(context.Background(), pendingStore.claim)
	if pendingStore.outcome != "review" || pendingStore.reviewCode != ReviewRenewalUnknownWrite || len(recurring.calls) != 1 {
		t.Fatalf("reclaimed unknown write replayed: outcome=%q calls=%v", pendingStore.outcome, recurring.calls)
	}
}

func TestRenewalWorkerRejectsChangedAllocationProofWithoutWrites(t *testing.T) {
	w, store, recurring, panel := renewalWorkerFixture(t, false, true)
	w.payments.(*renewalMemoryPayments).items[0].Allocations[0].Amount = "99"
	w.handle(context.Background(), store.claim)
	if store.outcome != "review" || len(recurring.calls) != 0 || panel.calls != 0 {
		t.Fatalf("changed proof outcome=%q writes=%v enable=%d", store.outcome, recurring.calls, panel.calls)
	}
}

func TestRenewalWorkerRunCancellationJoinsClaimedWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &renewalMemoryStore{claim: queue.Operation{Lease: queue.Lease{ID: 41, Attempt: 1}, ClientID: 7, Kind: RenewalOperationKind}, snapshot: RenewalSnapshot{BotID: 42}, cancelOnLoad: make(chan struct{}, 1)}
	w := &RenewalWorker{store: store, botID: 42, cfg: RenewalConfig{Workers: 1, MaxAccounts: 1, PollInterval: time.Second, AllowedTelegramIDs: []int64{101}}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()
	select {
	case <-store.cancelOnLoad:
		cancel()
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("worker did not enter claimed task")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not join canceled renewal task")
	}
}

func renewalWorkerFixture(t *testing.T, overdue, schedule bool) (*RenewalWorker, *renewalMemoryStore, *renewalMemoryRecurring, *renewalMemoryPanel) {
	t.Helper()
	const today = "2040-02-01"
	now := time.Date(2040, time.February, 1, 12, 0, 0, 0, time.UTC)
	token := "synthetic_activation_token_1234567890"
	marker := activation.Marker(token)
	product := invoiceninja.Product{ID: "product-1", ProductKey: "monthly", Notes: "synthetic service", Price: "100", TaxRate1: "0", TaxRate2: "0", TaxRate3: "0", TaxID: "0"}
	draft, err := invoiceninja.BuildRecurringDraft("customer-1", "2040-01-31", "2040-01-31", product, false, marker)
	if err != nil {
		t.Fatal(err)
	}
	record := invoiceninja.RecurringRecord{ID: "recurring-1", ClientID: "customer-1", StatusID: "2", PrivateNotes: marker, FrequencyID: "5", NextSendDate: draft.NextSendDate, NextSendDatetime: "2040-02-28T08:00:00Z", DueDateDays: "on_receipt", AutoBill: "off", RemainingCycles: -1, Amount: "100", Balance: "100", ExchangeRate: "1", UsesInclusiveTaxes: false, LineItems: append([]invoiceninja.InvoiceLine(nil), draft.LineItems...), Discount: "0", TaxRate1: "0", TaxRate2: "0", TaxRate3: "0", CustomSurcharge1: "0", CustomSurcharge2: "0", CustomSurcharge3: "0", CustomSurcharge4: "0", Partial: "0", PaidToDate: "0", LastSentDate: "2040-01-31T08:00:00Z"}
	if schedule {
		record.NextSendDate = "2040-02-15"
	}
	if overdue {
		record.StatusID = "3"
	}
	panelRaw := []byte(`{"id":71,"enable":true,"name":"pp-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","config":{},"inbounds":[1],"links":[],"volume":0,"expiry":0,"down":1,"up":1,"desc":"","group":"","remark":"","createdAt":1,"onlineAt":0,"delayStart":false,"autoReset":false,"resetDays":0,"nextReset":0,"totalUp":0,"totalDown":0}`)
	panelRecord, err := sui.ParseClientRecord(panelRaw)
	if err != nil {
		t.Fatal(err)
	}
	if overdue {
		panelRaw = append([]byte(nil), panelRaw...)
		var obj map[string]any
		_ = json.Unmarshal(panelRaw, &obj)
		obj["enable"] = false
		panelRaw, _ = json.Marshal(obj)
		panelRecord, err = sui.ParseClientRecord(panelRaw)
		if err != nil {
			t.Fatal(err)
		}
	}
	binding := sui.Binding{ID: panelRecord.ID(), Name: panelRecord.Name(), CredentialsHash: panelRecord.CredentialsHash()}
	local := payments.Snapshot{ClientID: 7, TelegramID: 101, ChatID: 101, BotID: 42, CustomerID: "customer-1", InvoiceID: "invoice-current", FirstInvoiceID: "invoice-first", RecurringID: "recurring-1", SUIID: "71", State: "active", DesiredAccess: true}
	if overdue {
		local.State = "overdue"
		local.DesiredAccess = false
	}
	customer := invoiceninja.SettlementCustomer{ID: "customer-1", IDNumber: "101", CurrencyID: "3"}
	settlementInvoice := invoiceninja.SettlementInvoice{ID: "invoice-current", ClientID: "customer-1", RecurringID: "recurring-1", Number: "SYNTHETIC-2", StatusID: "4", Date: "2040-01-31", DueDate: "2040-02-28", Amount: "100", Balance: "0", PaidToDate: "100", ExchangeRate: "1"}
	payment := invoiceninja.SettlementPayment{ID: "payment-1", ClientID: "customer-1", Date: "2040-01-31", StatusID: "4", CurrencyID: "3", Amount: "100", Refunded: "0", Applied: "100", ExchangeRate: "1", IsManual: true, Allocations: []invoiceninja.SettlementAllocation{{ID: "allocation-1", InvoiceID: "invoice-current", PaymentID: "payment-1", Amount: "100", Refunded: "0"}}}
	paymentSnapshot := local
	paymentSnapshot.State = "active"
	paymentSnapshot.DesiredAccess = true
	proofDecision := payments.Evaluate(paymentSnapshot, customer, settlementInvoice, []invoiceninja.SettlementPayment{payment}, "3", today)
	if proofDecision.Kind != payments.Verified {
		t.Fatalf("fixture proof decision=%+v", proofDecision)
	}
	invoice := invoiceninja.CycleInvoice{ID: "invoice-current", ClientID: "customer-1", RecurringID: "recurring-1", Number: "SYNTHETIC-2", PrivateNotes: marker, StatusID: "4", Date: "2040-01-31", DueDate: "2040-02-28", Amount: "100", Balance: "0", PaidToDate: "100", ExchangeRate: "1", Invitations: []invoiceninja.CycleInvitation{{ID: "invitation-current", SentDate: "2040-01-31 08:00:00"}}}
	snapshot := RenewalSnapshot{ClientID: 7, TelegramID: 101, ChatID: 101, BotID: 42, CustomerID: "customer-1", FirstInvoiceID: "invoice-first", CurrentInvoiceID: "invoice-current", InvoiceID: "invoice-current", RecurringID: "recurring-1", SUIID: "71", RegistrationToken: "synthetic_registration_token_123456", ActivationToken: token, State: local.State, DesiredAccess: local.DesiredAccess, ManagedProvenance: true, Payment: local, Proof: *proofDecision.Proof, RecurringDraft: draft, SUIBinding: binding, LedgerCustomerID: "customer-1", LedgerRecurringID: "recurring-1", LedgerInvoiceNumber: "SYNTHETIC-2", LedgerStatus: "paid", LedgerAmount: "100", LedgerBalance: "0", LedgerPaidToDate: "100", LedgerSentDate: "2040-01-31", Steps: map[RenewalStep]RenewalStepRecord{}, OverdueSteps: map[string]bool{}}
	if overdue {
		snapshot.OverdueSteps["pause_recurring"] = true
		snapshot.OverdueSteps["disable_sui"] = true
	}
	paymentReader := &renewalMemoryPayments{customer: customer, invoice: settlementInvoice, items: []invoiceninja.SettlementPayment{payment}}
	recurring := &renewalMemoryRecurring{record: record, target: draft.NextSendDate}
	panel := &renewalMemoryPanel{record: panelRecord}
	store := &renewalMemoryStore{snapshot: snapshot, claim: queue.Operation{Lease: queue.Lease{ID: 44, Attempt: 1}, ClientID: 7, Kind: RenewalOperationKind}}
	w := &RenewalWorker{store: store, invoices: renewalMemoryInvoice{invoice}, payments: paymentReader, recurring: recurring, panel: panel, botID: 42, admins: []int64{202}, currencyID: "3", cfg: RenewalConfig{Enabled: true, AllowedTelegramIDs: []int64{101}, Workers: 1, MaxAccounts: 1000, PollInterval: time.Second, HTTPTimeout: 5 * time.Second}, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return now }}
	return w, store, recurring, panel
}
func filterRecurring(steps []RenewalStep) []RenewalStep {
	out := []RenewalStep{}
	for _, s := range steps {
		if s != RenewalEnable {
			out = append(out, s)
		}
	}
	return out
}
func countStep(steps []RenewalStep, target RenewalStep) int {
	n := 0
	for _, s := range steps {
		if s == target {
			n++
		}
	}
	return n
}
