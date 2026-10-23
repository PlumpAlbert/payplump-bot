package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

var (
	errRunTestIntent  = errors.New("synthetic intent commit failure")
	errRunTestConfirm = errors.New("synthetic confirmation commit failure")
)

type enforcementTestStore struct {
	snapshot         EnforcementSnapshot
	claim            queue.Operation
	attempt          int
	outcome          string
	review           int
	outbox           []queue.Draft
	retries          int
	loads            int
	failIntent       bool
	changeEpoch      bool
	paymentReviewAt  EnforcementStep
	confirmFailureAt EnforcementStep
	confirmError     error
	holdConflicts    int
	foreignEpoch     bool
}

func (s *enforcementTestStore) SeedOverdueOperations(context.Context, int64, []int64, string, int) (int, error) {
	return 0, nil
}
func (s *enforcementTestStore) ClaimOperations(context.Context, []string, int, time.Duration) ([]queue.Operation, error) {
	return nil, nil
}
func (s *enforcementTestStore) LoadOverdueSnapshot(_ context.Context, claim queue.Operation, botID int64) (EnforcementSnapshot, error) {
	s.loads++
	if s.outcome != "" || claim.ID != s.claim.ID || claim.Attempt != s.attempt || botID != s.snapshot.BotID {
		return EnforcementSnapshot{}, queue.ErrLeaseLost
	}
	return cloneOverdueTestSnapshot(s.snapshot), nil
}
func (s *enforcementTestStore) AuthorizeOverdueWrite(_ context.Context, claim queue.Operation, botID int64, expected EnforcementSnapshot, step EnforcementStep, raw json.RawMessage, _ string, allowed []int64) (bool, error) {
	if claim.ID != s.claim.ID || claim.Attempt != s.attempt {
		return false, queue.ErrLeaseLost
	}
	if botID != s.snapshot.BotID || !reflect.DeepEqual(expected, s.snapshot) {
		return false, queue.ErrConflict
	}
	if s.failIntent {
		return false, errRunTestIntent
	}
	if s.changeEpoch {
		s.snapshot.WriteEpoch++
		return false, queue.ErrConflict
	}
	if s.paymentReviewAt == step {
		s.snapshot.PaymentReview = true
		return false, ErrOverdueNotUnpaid
	}
	if s.snapshot.WriterBusy {
		return false, ErrOverdueGuardBusy
	}
	if s.snapshot.PaymentConfirmed || s.snapshot.PaymentReview || s.snapshot.PaymentLocked || s.snapshot.LifecycleReview || !containsTestID(allowed, s.snapshot.TelegramID) {
		return false, ErrOverdueNotUnpaid
	}
	next, pending, complete, valid := NextOverdueStep(s.snapshot.Steps)
	if !valid || pending || complete || next != step || !json.Valid(raw) {
		return false, ErrOverdueIntentExists
	}
	if step == OverduePauseRecurring || step == OverdueDisableSUI {
		if s.snapshot.State != "active" || !s.snapshot.DesiredAccess {
			return false, queue.ErrConflict
		}
	} else if s.snapshot.State != "overdue" || s.snapshot.DesiredAccess {
		return false, queue.ErrConflict
	}
	s.snapshot.Steps[step] = OverdueStep{Request: append(json.RawMessage(nil), raw...), DispatchAttempt: claim.Attempt}
	s.snapshot.WriteEpoch++
	s.snapshot.WriterBusy = true
	s.snapshot.WriterOperationID = claim.ID
	s.snapshot.WriterStep = string(step)
	return true, nil
}
func (s *enforcementTestStore) ConfirmOverdueStep(_ context.Context, claim queue.Operation, step EnforcementStep, externalID string) error {
	if claim.ID != s.claim.ID || claim.Attempt != s.attempt {
		return queue.ErrLeaseLost
	}
	if s.confirmFailureAt == step && s.confirmError != nil {
		return s.confirmError
	}
	receipt, ok := s.snapshot.Steps[step]
	if !ok || receipt.Confirmed {
		return queue.ErrConflict
	}
	receipt.Confirmed, receipt.ExternalID = true, externalID
	s.snapshot.Steps[step] = receipt
	if step == OverdueDisableSUI {
		s.snapshot.State, s.snapshot.DesiredAccess = "overdue", false
	}
	s.snapshot.WriterBusy, s.snapshot.WriterOperationID, s.snapshot.WriterStep = false, 0, ""
	return nil
}
func (s *enforcementTestStore) RetryOperation(_ context.Context, lease queue.Lease, _ time.Duration, _ int) error {
	if lease.ID != s.claim.ID || lease.Attempt != s.attempt {
		return queue.ErrLeaseLost
	}
	s.retries++
	return nil
}
func (s *enforcementTestStore) HoldOverdue(_ context.Context, claim queue.Operation, botID int64, expected EnforcementSnapshot, code int, drafts []queue.Draft) error {
	if claim.ID != s.claim.ID || claim.Attempt != s.attempt || s.outcome != "" {
		return queue.ErrLeaseLost
	}
	if botID != s.snapshot.BotID || !reflect.DeepEqual(expected.Steps, s.snapshot.Steps) {
		return queue.ErrConflict
	}
	if s.holdConflicts > 0 {
		s.holdConflicts--
		return queue.ErrConflict
	}
	s.outcome, s.review = "review", code
	s.outbox = append([]queue.Draft(nil), drafts...)
	return nil
}
func (s *enforcementTestStore) FinishPaidOverdue(_ context.Context, claim queue.Operation, botID int64, expected EnforcementSnapshot) error {
	if claim.ID != s.claim.ID || claim.Attempt != s.attempt || s.outcome != "" {
		return queue.ErrLeaseLost
	}
	if botID != s.snapshot.BotID || len(expected.Steps) != 0 || len(s.snapshot.Steps) != 0 {
		return queue.ErrConflict
	}
	s.outcome = "paid"
	return nil
}
func (s *enforcementTestStore) FinishOverdue(_ context.Context, claim queue.Operation, botID int64, expected EnforcementSnapshot, drafts []queue.Draft) error {
	if claim.ID != s.claim.ID || claim.Attempt != s.attempt || s.outcome != "" {
		return queue.ErrLeaseLost
	}
	if s.foreignEpoch {
		s.snapshot.WriteEpoch++
	}
	actual, want := cloneOverdueTestSnapshot(s.snapshot), cloneOverdueTestSnapshot(expected)
	actual.WriteEpoch, actual.WriterBusy, actual.WriterOperationID, actual.WriterStep = 0, false, 0, ""
	want.WriteEpoch, want.WriterBusy, want.WriterOperationID, want.WriterStep = 0, false, 0, ""
	_, pending, complete, valid := NextOverdueStep(actual.Steps)
	if botID != actual.BotID || !reflect.DeepEqual(actual, want) || !valid || pending || !complete || actual.State != "overdue" || actual.DesiredAccess || actual.PaymentConfirmed || actual.PaymentReview || actual.PaymentLocked || actual.LifecycleReview {
		return queue.ErrConflict
	}
	s.outcome = "completed"
	s.outbox = append([]queue.Draft(nil), drafts...)
	return nil
}

type enforcementTestInvoices struct {
	calls  int
	states map[int]string
}

func (r *enforcementTestInvoices) GetInvoice(context.Context, string) (invoiceninja.CycleInvoice, error) {
	r.calls++
	if status, ok := r.states[r.calls]; ok {
		return overdueTestInvoice(status), nil
	}
	return overdueTestInvoice("2"), nil
}

type enforcementTestRecurring struct {
	record  invoiceninja.RecurringRecord
	unknown bool
	posts   int
}

func (r *enforcementTestRecurring) Get(context.Context, string) (invoiceninja.RecurringRecord, error) {
	return r.record, nil
}
func (r *enforcementTestRecurring) PauseAuthorized(ctx context.Context, _ invoiceninja.PauseDraft, authorize func(context.Context) error) (invoiceninja.RecurringRecord, error) {
	if err := authorize(ctx); err != nil {
		return invoiceninja.RecurringRecord{}, err
	}
	r.posts++
	if r.unknown {
		return invoiceninja.RecurringRecord{}, invoiceninja.ErrRecurringUnknown
	}
	return r.record, nil
}

type enforcementTestPanel struct {
	record      sui.ClientRecord
	unknownStep EnforcementStep
	posts       map[EnforcementStep]int
}

func (p *enforcementTestPanel) GetRecord(context.Context, int64) (sui.ClientRecord, error) {
	return p.record, nil
}
func (p *enforcementTestPanel) DisableAuthorized(ctx context.Context, _ sui.DisableDraft, authorize func(context.Context) error) (sui.ClientRecord, error) {
	if err := authorize(ctx); err != nil {
		return sui.ClientRecord{}, err
	}
	p.posts[OverdueDisableSUI]++
	if p.unknownStep == OverdueDisableSUI {
		return sui.ClientRecord{}, sui.ErrManagedUnknown
	}
	disabled, err := overdueTestPanelRecord(false)
	if err == nil {
		p.record = disabled
	}
	return disabled, err
}
func (p *enforcementTestPanel) CloseSessionsAuthorized(ctx context.Context, _ sui.CloseSessionsDraft, authorize func(context.Context) error) error {
	if err := authorize(ctx); err != nil {
		return err
	}
	p.posts[OverdueCloseSessions]++
	if p.unknownStep == OverdueCloseSessions {
		return sui.ErrManagedUnknown
	}
	return nil
}

type enforcementTestRig struct {
	worker    *EnforcementWorker
	store     *enforcementTestStore
	invoices  *enforcementTestInvoices
	recurring *enforcementTestRecurring
	panel     *enforcementTestPanel
	claim     queue.Operation
}

func newEnforcementTestRig(t *testing.T) *enforcementTestRig {
	t.Helper()
	snapshot := validEnforcementSnapshot()
	record, err := overdueTestPanelRecord(true)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.SUIBinding.CredentialsHash = record.CredentialsHash()
	claim := queue.Operation{Lease: queue.Lease{ID: 51, Attempt: 1}, ClientID: snapshot.ClientID, Kind: OverdueOperationKind}
	store := &enforcementTestStore{snapshot: cloneOverdueTestSnapshot(snapshot), claim: claim, attempt: claim.Attempt}
	invoices := &enforcementTestInvoices{states: map[int]string{}}
	recurring := &enforcementTestRecurring{record: overdueTestRecurringRecord(snapshot.RecurringDraft)}
	panel := &enforcementTestPanel{record: record, posts: map[EnforcementStep]int{}}
	cfg := EnforcementConfig{Enabled: true, ExclusiveWriter: true, BaseURL: "https://billing.synthetic.invalid", Token: "synthetic-token", SUIBaseURL: "https://panel.synthetic.invalid", SUIToken: "synthetic-panel-token", SUIVersion: "1.6.4", AllowedTelegramIDs: []int64{snapshot.TelegramID}, Workers: 1, MaxAccounts: 10, PollInterval: time.Second, HTTPTimeout: 5 * time.Second}
	worker, err := NewEnforcementWorker(store, invoices, recurring, panel, snapshot.BotID, []int64{9001}, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	worker.now = func() time.Time { return time.Date(2040, 1, 2, 12, 0, 0, 0, time.Local) }
	return &enforcementTestRig{worker: worker, store: store, invoices: invoices, recurring: recurring, panel: panel, claim: claim}
}

func overdueTestInvoice(status string) invoiceninja.CycleInvoice {
	invoice := invoiceninja.CycleInvoice{ID: "invoice-overdue", ClientID: "customer-1", RecurringID: "recurring-1", Number: "SYNTHETIC-2", PrivateNotes: activation.Marker("synthetic_activation_token_123456"), StatusID: status, Date: "2040-01-01", DueDate: "2040-01-01", Amount: "100", Balance: "100", PaidToDate: "0", ExchangeRate: "1", Invitations: []invoiceninja.CycleInvitation{{ID: "invitation-1", SentDate: "2040-01-01 10:30:00"}}}
	switch status {
	case "3":
		invoice.Balance, invoice.PaidToDate = "70", "30"
	case "4":
		invoice.Balance, invoice.PaidToDate = "0", "100"
	}
	return invoice
}
func overdueTestRecurringRecord(d invoiceninja.RecurringDraft) invoiceninja.RecurringRecord {
	return invoiceninja.RecurringRecord{ID: "recurring-1", ClientID: d.ClientID, StatusID: "2", PrivateNotes: d.PrivateNotes, FrequencyID: "5", NextSendDate: d.NextSendDate, NextSendDatetime: "2040-02-01T08:00:00Z", DueDateDays: "on_receipt", AutoBill: "off", RemainingCycles: -1, Amount: "100", Balance: "100", ExchangeRate: "1", UsesInclusiveTaxes: d.UsesInclusiveTaxes, LineItems: append([]invoiceninja.InvoiceLine(nil), d.LineItems...), Discount: "0", TaxRate1: "0", TaxRate2: "0", TaxRate3: "0", CustomSurcharge1: "0", CustomSurcharge2: "0", CustomSurcharge3: "0", CustomSurcharge4: "0", Partial: "0", PaidToDate: "0"}
}
func overdueTestPanelRecord(enabled bool) (sui.ClientRecord, error) {
	raw := fmt.Sprintf(`{"id":71,"enable":%t,"name":"pp-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","config":{},"inbounds":[1],"links":[],"volume":0,"expiry":0,"down":1,"up":1,"desc":"","group":"","remark":"","createdAt":1,"onlineAt":0,"delayStart":false,"autoReset":false,"resetDays":0,"nextReset":0,"totalUp":0,"totalDown":0}`, enabled)
	return sui.ParseClientRecord([]byte(raw))
}
func cloneOverdueTestSnapshot(snapshot EnforcementSnapshot) EnforcementSnapshot {
	if snapshot.Steps != nil {
		steps := make(map[EnforcementStep]OverdueStep, len(snapshot.Steps))
		for step, value := range snapshot.Steps {
			value.Request = append(json.RawMessage(nil), value.Request...)
			steps[step] = value
		}
		snapshot.Steps = steps
	}
	return snapshot
}
func containsTestID(ids []int64, target int64) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}
func (r *enforcementTestRig) posts() int {
	return r.recurring.posts + r.panel.posts[OverdueDisableSUI] + r.panel.posts[OverdueCloseSessions]
}

func TestEnforcementWorkerCompletesPrefixBeforeOutbox(t *testing.T) {
	rig := newEnforcementTestRig(t)
	rig.store.foreignEpoch = true
	rig.worker.handle(context.Background(), rig.claim)
	if rig.store.outcome != "completed" || len(rig.store.outbox) != 2 || rig.posts() != 3 || rig.store.retries != 0 {
		t.Fatalf("outcome=%q outbox=%d provider_posts=%d retries=%d", rig.store.outcome, len(rig.store.outbox), rig.posts(), rig.store.retries)
	}
	for _, step := range []EnforcementStep{OverduePauseRecurring, OverdueDisableSUI, OverdueCloseSessions} {
		if !rig.store.snapshot.Steps[step].Confirmed {
			t.Fatalf("step %s was not confirmed", step)
		}
	}
	if rig.store.snapshot.State != "overdue" || rig.store.snapshot.DesiredAccess {
		t.Fatalf("state=%q access=%v", rig.store.snapshot.State, rig.store.snapshot.DesiredAccess)
	}
}

func TestEnforcementWorkerPaymentReviewAtInitialAndFinalAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name          string
		initialStatus string
		paymentReview bool
		step          EnforcementStep
		read          int
		finalStatus   string
	}{
		{name: "initial paid", initialStatus: "4"},
		{name: "initial partial", initialStatus: "3"},
		{name: "initial payment review", paymentReview: true},
		{name: "final paid before first intent", step: OverduePauseRecurring, read: 2, finalStatus: "4"},
		{name: "final partial before first intent", step: OverduePauseRecurring, read: 2, finalStatus: "3"},
		{name: "final payment review before first intent", step: OverduePauseRecurring, read: 2, finalStatus: "2", paymentReview: true},
		{name: "final paid after pause", step: OverdueDisableSUI, read: 4, finalStatus: "4"},
		{name: "final partial after pause", step: OverdueDisableSUI, read: 4, finalStatus: "3"},
		{name: "final payment review after pause", step: OverdueDisableSUI, read: 4, finalStatus: "2", paymentReview: true},
		{name: "final paid after disable", step: OverdueCloseSessions, read: 6, finalStatus: "4"},
		{name: "final partial after disable", step: OverdueCloseSessions, read: 6, finalStatus: "3"},
		{name: "final payment review after disable", step: OverdueCloseSessions, read: 6, finalStatus: "2", paymentReview: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newEnforcementTestRig(t)
			if tc.initialStatus != "" {
				rig.invoices.states[1] = tc.initialStatus
			}
			if tc.paymentReview && tc.step == "" {
				rig.store.snapshot.PaymentReview = true
			}
			if tc.read > 0 {
				rig.invoices.states[tc.read] = tc.finalStatus
				if tc.paymentReview {
					rig.store.paymentReviewAt = tc.step
				}
			}
			rig.worker.handle(context.Background(), rig.claim)
			wantPosts := 0
			if tc.step == OverdueDisableSUI {
				wantPosts = 1
			} else if tc.step == OverdueCloseSessions {
				wantPosts = 2
			}
			if rig.posts() != wantPosts {
				t.Fatalf("provider posts=%d want=%d", rig.posts(), wantPosts)
			}
			if tc.initialStatus == "4" || tc.read == 2 && tc.finalStatus == "4" {
				if rig.store.outcome != "paid" {
					t.Fatalf("confirmed paid result outcome=%q", rig.store.outcome)
				}
				return
			}
			if rig.store.outcome != "review" || rig.store.review != ReviewOverduePayment {
				t.Fatalf("outcome=%q review=%d", rig.store.outcome, rig.store.review)
			}
		})
	}
}

func TestEnforcementWorkerGuardLeaseEpochAndExclusionFences(t *testing.T) {
	for _, mode := range []string{"busy", "payment-lease", "excluded", "epoch-change", "intent-commit"} {
		t.Run(mode, func(t *testing.T) {
			rig := newEnforcementTestRig(t)
			switch mode {
			case "busy":
				rig.store.snapshot.WriterBusy, rig.store.snapshot.WriterOperationID, rig.store.snapshot.WriterStep = true, 99, "other"
			case "payment-lease":
				rig.store.snapshot.PaymentLocked = true
			case "excluded":
				rig.worker.cfg.ExcludedTelegramIDs = []int64{101}
			case "epoch-change":
				rig.store.changeEpoch = true
			case "intent-commit":
				rig.store.failIntent = true
			}
			rig.worker.handle(context.Background(), rig.claim)
			if rig.posts() != 0 || len(rig.store.snapshot.Steps) != 0 {
				t.Fatalf("external posts=%d steps=%d", rig.posts(), len(rig.store.snapshot.Steps))
			}
			if mode == "busy" || mode == "payment-lease" || mode == "epoch-change" || mode == "intent-commit" {
				if rig.store.retries != 1 {
					t.Fatalf("retries=%d", rig.store.retries)
				}
			} else if rig.store.outcome != "review" || rig.store.review != ReviewOverdueAssociation {
				t.Fatalf("excluded account outcome=%q review=%d", rig.store.outcome, rig.store.review)
			}
		})
	}
}

func TestEnforcementWorkerUnknownOutcomeFreshReviewAndNoReplay(t *testing.T) {
	for _, step := range []EnforcementStep{OverduePauseRecurring, OverdueDisableSUI, OverdueCloseSessions} {
		t.Run(string(step), func(t *testing.T) {
			rig := newEnforcementTestRig(t)
			if step == OverduePauseRecurring {
				rig.recurring.unknown = true
			} else {
				rig.panel.unknownStep = step
			}
			rig.store.holdConflicts = 1
			rig.worker.handle(context.Background(), rig.claim)
			wantPosts := 1
			if step == OverdueDisableSUI {
				wantPosts = 2
			} else if step == OverdueCloseSessions {
				wantPosts = 3
			}
			if rig.store.outcome != "review" || rig.store.review != ReviewOverdueUnknownWrite || len(rig.store.outbox) != 1 || !rig.store.snapshot.WriterBusy || rig.posts() != wantPosts || rig.store.loads < wantPosts+2 {
				t.Fatalf("outcome=%q review=%d outbox=%d busy=%v posts=%d loads=%d", rig.store.outcome, rig.store.review, len(rig.store.outbox), rig.store.snapshot.WriterBusy, rig.posts(), rig.store.loads)
			}
			rig.worker.handle(context.Background(), rig.claim)
			if rig.posts() != wantPosts {
				t.Fatal("unknown provider write was replayed")
			}
		})
	}
}

func TestEnforcementWorkerConfirmationFailureAndRestartDoNotReplay(t *testing.T) {
	for _, step := range []EnforcementStep{OverduePauseRecurring, OverdueDisableSUI, OverdueCloseSessions} {
		t.Run(string(step)+"/confirmation-failure", func(t *testing.T) {
			rig := newEnforcementTestRig(t)
			rig.store.confirmFailureAt, rig.store.confirmError = step, errRunTestConfirm
			rig.worker.handle(context.Background(), rig.claim)
			wantPosts := 1
			if step == OverdueDisableSUI {
				wantPosts = 2
			} else if step == OverdueCloseSessions {
				wantPosts = 3
			}
			if rig.store.outcome != "review" || rig.store.review != ReviewOverdueUnknownWrite || rig.posts() != wantPosts || !rig.store.snapshot.WriterBusy {
				t.Fatalf("outcome=%q review=%d posts=%d busy=%v", rig.store.outcome, rig.store.review, rig.posts(), rig.store.snapshot.WriterBusy)
			}
			rig.worker.handle(context.Background(), rig.claim)
			if rig.posts() != wantPosts {
				t.Fatal("provider write replayed after confirmation failure")
			}
		})
		t.Run(string(step)+"/lease-lost-restart", func(t *testing.T) {
			rig := newEnforcementTestRig(t)
			rig.store.confirmFailureAt, rig.store.confirmError = step, queue.ErrLeaseLost
			rig.worker.handle(context.Background(), rig.claim)
			wantPosts := 1
			if step == OverdueDisableSUI {
				wantPosts = 2
			} else if step == OverdueCloseSessions {
				wantPosts = 3
			}
			if rig.store.outcome != "" || rig.posts() != wantPosts || !rig.store.snapshot.WriterBusy {
				t.Fatalf("outcome=%q posts=%d busy=%v", rig.store.outcome, rig.posts(), rig.store.snapshot.WriterBusy)
			}
			rig.claim.Attempt++
			rig.store.attempt = rig.claim.Attempt
			rig.worker.handle(context.Background(), rig.claim)
			if rig.store.outcome != "review" || rig.store.review != ReviewOverdueUnknownWrite || rig.posts() != wantPosts {
				t.Fatalf("restart outcome=%q review=%d posts=%d", rig.store.outcome, rig.store.review, rig.posts())
			}
		})
	}
}

func TestEnforcementConfirmReturnsLeaseLostAndNotUnpaidIsNotPaid(t *testing.T) {
	rig := newEnforcementTestRig(t)
	rig.store.confirmFailureAt, rig.store.confirmError = OverduePauseRecurring, queue.ErrLeaseLost
	if err := rig.worker.confirm(rig.claim, OverduePauseRecurring, "recurring-1"); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("confirm returned %v", err)
	}
	rig = newEnforcementTestRig(t)
	rig.worker.finishWriteAttempt(rig.claim, rig.store.snapshot, false, ErrOverdueNotUnpaid)
	if rig.store.outcome != "review" || rig.store.review != ReviewOverduePayment {
		t.Fatalf("partial result outcome=%q review=%d", rig.store.outcome, rig.store.review)
	}
	rig = newEnforcementTestRig(t)
	rig.worker.finishWriteAttempt(rig.claim, rig.store.snapshot, false, ErrOverduePaid)
	if rig.store.outcome != "paid" {
		t.Fatalf("paid result outcome=%q", rig.store.outcome)
	}
}

// Run spies use channels as their synchronization boundary; the blocked load
// models an active handler without involving a database or provider.
type overdueRunSeedCall struct {
	botID   int64
	allowed []int64
	today   string
	limit   int
}
type overdueRunClaimCall struct {
	kinds   []string
	workers int
	lease   time.Duration
}
type enforcementRunSpyStore struct {
	EnforcementStore
	claim        queue.Operation
	seedCalls    chan overdueRunSeedCall
	claimCalls   chan overdueRunClaimCall
	loadStarted  chan queue.Operation
	loadCanceled chan struct{}
	releaseLoad  chan struct{}
}

func (s *enforcementRunSpyStore) SeedOverdueOperations(_ context.Context, botID int64, allowed []int64, today string, limit int) (int, error) {
	s.seedCalls <- overdueRunSeedCall{botID: botID, allowed: append([]int64(nil), allowed...), today: today, limit: limit}
	return 0, nil
}
func (s *enforcementRunSpyStore) ClaimOperations(_ context.Context, kinds []string, workers int, lease time.Duration) ([]queue.Operation, error) {
	s.claimCalls <- overdueRunClaimCall{kinds: append([]string(nil), kinds...), workers: workers, lease: lease}
	return []queue.Operation{s.claim}, nil
}
func (s *enforcementRunSpyStore) LoadOverdueSnapshot(ctx context.Context, claim queue.Operation, _ int64) (EnforcementSnapshot, error) {
	s.loadStarted <- claim
	<-ctx.Done()
	close(s.loadCanceled)
	<-s.releaseLoad
	return EnforcementSnapshot{}, ctx.Err()
}
func TestEnforcementWorkerRunJoinsCanceledHandler(t *testing.T) {
	rig := newEnforcementTestRig(t)
	rig.worker.cfg.AllowedTelegramIDs = []int64{101, 102, 103}
	rig.worker.cfg.ExcludedTelegramIDs = []int64{102}
	rig.worker.cfg.MaxAccounts, rig.worker.cfg.Workers = 37, 2
	spy := &enforcementRunSpyStore{EnforcementStore: rig.store, claim: rig.claim, seedCalls: make(chan overdueRunSeedCall, 1), claimCalls: make(chan overdueRunClaimCall, 1), loadStarted: make(chan queue.Operation, 1), loadCanceled: make(chan struct{}), releaseLoad: make(chan struct{})}
	rig.worker.store = spy
	service, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- rig.worker.Run(service) }()
	seed := awaitOverdueRunValue(t, spy.seedCalls)
	if seed.botID != 7 || !reflect.DeepEqual(seed.allowed, []int64{101, 103}) || seed.today != "2040-01-02" || seed.limit != 37 {
		t.Fatalf("seed args=%#v", seed)
	}
	claimArgs := awaitOverdueRunValue(t, spy.claimCalls)
	if !reflect.DeepEqual(claimArgs.kinds, []string{OverdueOperationKind}) || claimArgs.workers != 2 || claimArgs.lease != enforcementLease {
		t.Fatalf("claim args=%#v", claimArgs)
	}
	if claim := awaitOverdueRunValue(t, spy.loadStarted); claim != rig.claim {
		t.Fatalf("loaded claim=%#v want=%#v", claim, rig.claim)
	}
	cancel()
	awaitOverdueRunSignal(t, spy.loadCanceled)
	select {
	case err := <-runDone:
		t.Fatalf("Run returned before active handler release: %v", err)
	default:
	}
	close(spy.releaseLoad)
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run error after cancellation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not join handler")
	}
}
func awaitOverdueRunValue[T any](t *testing.T, calls <-chan T) T {
	t.Helper()
	select {
	case value := <-calls:
		return value
	case <-time.After(3 * time.Second):
		var zero T
		t.Fatal("timed out waiting for worker Run call")
		return zero
	}
}
func awaitOverdueRunSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for handler cancellation")
	}
}
