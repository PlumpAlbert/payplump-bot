package registration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
)

// This fake models durable state, not a sequence of canned snapshots. A fresh
// claim fences the previous attempt; neither resume nor restart erases intents.
// API mutations consume the exact journaled payload and a one-shot permission.
// There is deliberately no S-UI, recurring-invoice or Telegram capability.
type fakeRegistrationState struct {
	snapshot                 Snapshot
	lease                    queue.Lease
	ready, held, finished    bool
	granted                  map[Step]bool
	permits                  map[Step]queue.Lease
	writes                   map[int]int
	retries, holds, finishes int
	reviewCode               int
	confirmContext           context.Context
}

type fakeStore struct {
	t                      *testing.T
	mu                     sync.Mutex
	states                 map[int64]*fakeRegistrationState
	events                 []string
	outbox                 map[string]queue.Draft
	prepareErr, confirmErr error
	prepareCommit          bool
	deny                   Step
	loadHook               func(context.Context, queue.Lease) error
	confirmHook            func(context.Context, queue.Lease, Step)
	claimCalls             int
}

func newFakeStore(t *testing.T, count int) *fakeStore {
	s := &fakeStore{t: t, states: make(map[int64]*fakeRegistrationState), outbox: make(map[string]queue.Draft)}
	for id := int64(1); id <= int64(count); id++ {
		s.states[id] = &fakeRegistrationState{
			snapshot: Snapshot{ClientID: id, BotID: 77, TelegramID: 123450 + id, ChatID: 123450 + id,
				DisplayName: "Private Customer Name", Token: "stable_private_flow_0123456789_" + strconv.FormatInt(id, 10),
				SubscriptionState: "awaiting_payment", Steps: make(map[Step]Intent)},
			ready: true, granted: make(map[Step]bool), permits: make(map[Step]queue.Lease), writes: make(map[int]int),
		}
	}
	return s
}

func cloneSnapshot(s Snapshot) Snapshot {
	copy := s
	copy.Steps = make(map[Step]Intent, len(s.Steps))
	for step, intent := range s.Steps {
		intent.Payload = slices.Clone(intent.Payload)
		copy.Steps[step] = intent
	}
	return copy
}

func checkDeadline(t *testing.T, ctx context.Context, maximum time.Duration) {
	t.Helper()
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > maximum+20*time.Millisecond {
		t.Errorf("missing or overlong %s deadline", maximum)
	}
}

func (s *fakeStore) nextLocked(id int64) queue.Operation {
	state := s.states[id]
	state.lease.ID = id
	state.lease.Attempt++
	state.ready = false
	state.confirmContext = nil
	return queue.Operation{Lease: state.lease, ClientID: id, Kind: "register_client"}
}

func (s *fakeStore) next(id int64) queue.Operation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nextLocked(id)
}

func (s *fakeStore) ClaimOperations(ctx context.Context, kinds []string, limit int, lease time.Duration) ([]queue.Operation, error) {
	checkDeadline(s.t, ctx, 8*time.Second)
	if !reflect.DeepEqual(kinds, []string{"register_client"}) || lease != 2*time.Minute || limit < 1 || limit > 4 {
		s.t.Errorf("unsafe claim: kinds=%v limit=%d lease=%s", kinds, limit, lease)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimCalls++
	var operations []queue.Operation
	for id := int64(1); id <= int64(len(s.states)) && len(operations) < limit; id++ {
		state := s.states[id]
		if state.ready && !state.held && !state.finished {
			operations = append(operations, s.nextLocked(id))
		}
	}
	return operations, ctx.Err()
}

func (s *fakeStore) stateLocked(ctx context.Context, lease queue.Lease, event string) (*fakeRegistrationState, error) {
	checkDeadline(s.t, ctx, 8*time.Second)
	s.events = append(s.events, event)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	state := s.states[lease.ID]
	if state == nil || state.lease != lease {
		return nil, queue.ErrLeaseLost
	}
	return state, nil
}

func (s *fakeStore) LoadRegistration(ctx context.Context, lease queue.Lease, botID int64) (Snapshot, error) {
	if botID != 77 {
		s.t.Errorf("unverified bot identity: %d", botID)
	}
	if s.loadHook != nil {
		if err := s.loadHook(ctx, lease); err != nil {
			return Snapshot{}, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.stateLocked(ctx, lease, "load")
	if err != nil {
		return Snapshot{}, err
	}
	return cloneSnapshot(state.snapshot), nil
}

func (s *fakeStore) PrepareRegistrationWrite(ctx context.Context, lease queue.Lease, step Step, raw json.RawMessage) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.stateLocked(ctx, lease, "prepare:"+string(step))
	if err != nil {
		return false, err
	}
	if s.prepareErr != nil && !s.prepareCommit {
		return false, s.prepareErr
	}
	if old, exists := state.snapshot.Steps[step]; exists {
		if !bytes.Equal(old.Payload, raw) {
			return false, queue.ErrConflict
		}
		return false, nil
	}
	if !step.Valid() || !json.Valid(raw) {
		s.t.Error("invalid journal request")
		return false, queue.ErrConflict
	}
	if step == InvoiceCreate && state.snapshot.Steps[CustomerCreate].ExternalID == "" || step == InvoiceIssue && state.snapshot.Steps[InvoiceCreate].ExternalID == "" {
		s.t.Error("out-of-order journal request")
		return false, queue.ErrConflict
	}
	state.snapshot.Steps[step] = Intent{Payload: slices.Clone(raw)}
	if s.prepareErr != nil {
		return false, s.prepareErr // COMMIT may have succeeded; no permission.
	}
	if state.granted[step] {
		s.t.Error("intent was rearmed")
		return false, queue.ErrConflict
	}
	state.granted[step] = true
	if s.deny == step {
		return false, nil // Another holder reserved this intent.
	}
	state.permits[step] = lease
	return true, nil
}

func (s *fakeStore) ConfirmRegistrationStep(ctx context.Context, lease queue.Lease, step Step, id string) error {
	s.mu.Lock()
	state, err := s.stateLocked(ctx, lease, "confirm:"+string(step))
	if err == nil {
		err = s.confirmErr
	}
	if err == nil {
		intent, exists := state.snapshot.Steps[step]
		if !exists || id == "" || intent.ExternalID != "" && intent.ExternalID != id {
			err = queue.ErrConflict
		} else {
			intent.ExternalID = id
			state.snapshot.Steps[step] = intent
			state.confirmContext = ctx
		}
	}
	s.mu.Unlock()
	if err == nil && s.confirmHook != nil {
		s.confirmHook(ctx, lease, step)
	}
	return err
}

func (s *fakeStore) RetryOperation(ctx context.Context, lease queue.Lease, _ time.Duration, _ int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.stateLocked(ctx, lease, "retry")
	if err != nil {
		return err
	}
	if state.confirmContext != nil && state.confirmContext != ctx {
		s.t.Error("confirmation and retry did not share one finalization context/budget")
	}
	state.retries++
	state.ready = true
	return nil
}

func (s *fakeStore) insertDraftsLocked(drafts []queue.Draft) error {
	for _, draft := range drafts {
		if err := draft.Validate(); err != nil {
			return err
		}
		if old, exists := s.outbox[draft.Key]; exists && old != draft {
			return queue.ErrConflict
		}
	}
	for _, draft := range drafts {
		s.outbox[draft.Key] = draft
	}
	return nil
}

func (s *fakeStore) HoldRegistration(ctx context.Context, lease queue.Lease, code int, drafts []queue.Draft) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.stateLocked(ctx, lease, "hold")
	if err != nil {
		return err
	}
	if err := s.insertDraftsLocked(drafts); err != nil {
		return err
	}
	state.held, state.ready, state.reviewCode = true, false, code
	state.holds++
	return nil
}

func (s *fakeStore) FinishRegistration(ctx context.Context, lease queue.Lease, customerID, invoiceID string, drafts []queue.Draft) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.stateLocked(ctx, lease, "finish")
	if err != nil {
		return err
	}
	if state.snapshot.Steps[CustomerCreate].ExternalID != customerID || state.snapshot.Steps[InvoiceCreate].ExternalID != invoiceID || state.snapshot.Steps[InvoiceIssue].ExternalID != invoiceID {
		return queue.ErrConflict
	}
	if err := s.insertDraftsLocked(drafts); err != nil {
		return err
	}
	state.finished, state.ready = true, false
	state.finishes++
	state.snapshot.ExistingCustomerID = customerID
	state.snapshot.ExistingFirstInvoiceID, state.snapshot.ExistingCurrentInvoiceID = invoiceID, invoiceID
	return nil
}

func (s *fakeStore) resume(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[id].held, s.states[id].ready = false, true
}

func (s *fakeStore) note(event string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

func (s *fakeStore) consume(step Step, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, state := range s.states {
		intent, exists := state.snapshot.Steps[step]
		if !exists || !bytes.Equal(raw, intent.Payload) {
			continue
		}
		if state.permits[step] != state.lease || state.writes[state.lease.Attempt] != 0 {
			s.t.Error("POST without a current one-shot permit or second POST in one claim")
			return queue.ErrLeaseLost
		}
		delete(state.permits, step)
		state.writes[state.lease.Attempt]++
		s.events = append(s.events, "post:"+string(step))
		return nil
	}
	s.t.Error("POST without an immutable durable intent")
	return queue.ErrConflict
}

type fakeAPI struct {
	t             *testing.T
	store         *fakeStore
	mu            sync.Mutex
	customers     []invoiceninja.Customer
	invoices      []invoiceninja.Invoice
	product       invoiceninja.Product
	posts         map[Step]int
	writeErr      map[Step]error
	commitOnError map[Step]bool
	lookupErr     error
	afterWrite    func(context.Context, Step)
}

func newFakeAPI(t *testing.T, store *fakeStore) *fakeAPI {
	return &fakeAPI{t: t, store: store, posts: make(map[Step]int), writeErr: make(map[Step]error), commitOnError: make(map[Step]bool),
		product: invoiceninja.Product{ID: "product-1", ProductKey: "one-month", Notes: "original product notes", Price: "1000.50", TaxRate1: "0", TaxRate2: "0", TaxRate3: "0"}}
}

func cloneInvoice(i invoiceninja.Invoice) invoiceninja.Invoice {
	i.LineItems = slices.Clone(i.LineItems)
	i.Invitations = slices.Clone(i.Invitations)
	return i
}

func (a *fakeAPI) read(ctx context.Context, event string) error {
	checkDeadline(a.t, ctx, time.Minute)
	a.store.note(event)
	return ctx.Err()
}

func (a *fakeAPI) FindCustomers(ctx context.Context, telegramID int64) ([]invoiceninja.Customer, error) {
	if err := a.read(ctx, "find-customers"); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var result []invoiceninja.Customer
	for _, customer := range a.customers {
		if customer.IDNumber == strconv.FormatInt(telegramID, 10) {
			result = append(result, customer)
		}
	}
	return result, a.lookupErr
}

func (a *fakeAPI) FindInvoices(ctx context.Context, clientID string) ([]invoiceninja.Invoice, error) {
	if err := a.read(ctx, "find-invoices"); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var result []invoiceninja.Invoice
	for _, invoice := range a.invoices {
		if invoice.ClientID == clientID {
			result = append(result, cloneInvoice(invoice))
		}
	}
	return result, a.lookupErr
}

func (a *fakeAPI) GetCustomer(ctx context.Context, id string) (invoiceninja.Customer, error) {
	if err := a.read(ctx, "get-customer"); err != nil {
		return invoiceninja.Customer{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, customer := range a.customers {
		if customer.ID == id {
			return customer, nil
		}
	}
	return invoiceninja.Customer{}, errors.New("fake customer not found")
}

func (a *fakeAPI) GetInvoice(ctx context.Context, id string) (invoiceninja.Invoice, error) {
	if err := a.read(ctx, "get-invoice"); err != nil {
		return invoiceninja.Invoice{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, invoice := range a.invoices {
		if invoice.ID == id {
			return cloneInvoice(invoice), nil
		}
	}
	return invoiceninja.Invoice{}, errors.New("fake invoice not found")
}

func (a *fakeAPI) GetProduct(ctx context.Context, id string) (invoiceninja.Product, error) {
	if err := a.read(ctx, "get-product"); err != nil {
		return invoiceninja.Product{}, err
	}
	if id != "product-1" {
		a.t.Errorf("unexpected product ID: %q", id)
	}
	return a.product, nil
}

func (a *fakeAPI) write(ctx context.Context, step Step, payload any) error {
	checkDeadline(a.t, ctx, time.Minute)
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := a.store.consume(step, payload); err != nil {
		return err
	}
	a.mu.Lock()
	a.posts[step]++
	a.mu.Unlock()
	return nil
}

func receiptCustomer(d invoiceninja.CustomerDraft, id string) invoiceninja.Customer {
	// Upstream presentation can differ without changing owned identity.
	return invoiceninja.Customer{ID: id, Name: "different receipt name", IDNumber: d.IDNumber, PrivateNotes: d.PrivateNotes}
}

func receiptInvoice(d invoiceninja.InvoiceDraft, id string) invoiceninja.Invoice {
	i := invoiceninja.Invoice{ID: id, ClientID: d.ClientID, StatusID: "1", Number: "INV-001", Date: d.Date, DueDate: d.DueDate,
		Amount: "1200.600", Balance: "0", PrivateNotes: d.PrivateNotes, UsesInclusiveTaxes: d.UsesInclusiveTaxes,
		LineItems: slices.Clone(d.LineItems), Invitations: []invoiceninja.Invitation{{ID: "invitation-id", Link: "https://public.example.test/invoice/provided-link"}}}
	i.LineItems[0].Notes = "different receipt line notes"
	return i
}

func (a *fakeAPI) CreateCustomer(ctx context.Context, d invoiceninja.CustomerDraft) (invoiceninja.Customer, error) {
	if err := a.write(ctx, CustomerCreate, d); err != nil {
		return invoiceninja.Customer{}, err
	}
	a.mu.Lock()
	result := receiptCustomer(d, fmt.Sprintf("customer-%d", len(a.customers)+1))
	err := a.writeErr[CustomerCreate]
	if err == nil || a.commitOnError[CustomerCreate] {
		a.customers = append(a.customers, result)
	}
	a.mu.Unlock()
	if a.afterWrite != nil {
		a.afterWrite(ctx, CustomerCreate)
	}
	return result, err
}

func (a *fakeAPI) CreateInvoice(ctx context.Context, d invoiceninja.InvoiceDraft) (invoiceninja.Invoice, error) {
	if err := a.write(ctx, InvoiceCreate, d); err != nil {
		return invoiceninja.Invoice{}, err
	}
	a.mu.Lock()
	result := receiptInvoice(d, fmt.Sprintf("invoice-%d", len(a.invoices)+1))
	err := a.writeErr[InvoiceCreate]
	if err == nil || a.commitOnError[InvoiceCreate] {
		a.invoices = append(a.invoices, cloneInvoice(result))
	}
	a.mu.Unlock()
	if a.afterWrite != nil {
		a.afterWrite(ctx, InvoiceCreate)
	}
	return result, err
}

func (a *fakeAPI) IssueInvoice(ctx context.Context, d invoiceninja.IssueDraft) (invoiceninja.Invoice, error) {
	if err := a.write(ctx, InvoiceIssue, d); err != nil {
		return invoiceninja.Invoice{}, err
	}
	if d.Action != "mark_sent" || len(d.IDs) != 1 {
		a.t.Error("unexpected issue payload")
		return invoiceninja.Invoice{}, ErrReviewRequired
	}
	a.mu.Lock()
	var result invoiceninja.Invoice
	err := a.writeErr[InvoiceIssue]
	for index, invoice := range a.invoices {
		if invoice.ID == d.IDs[0] {
			if invoice.StatusID != "1" || !unissued(invoice) {
				a.t.Error("issue was not a positive-amount zero-balance draft")
			}
			result = cloneInvoice(invoice)
			result.StatusID, result.Balance = "2", result.Amount
			if err == nil || a.commitOnError[InvoiceIssue] {
				a.invoices[index] = cloneInvoice(result)
			}
		}
	}
	a.mu.Unlock()
	if a.afterWrite != nil {
		a.afterWrite(ctx, InvoiceIssue)
	}
	return result, err
}

type workerFixture struct {
	t      *testing.T
	store  *fakeStore
	api    *fakeAPI
	worker *Worker
	logs   bytes.Buffer
	now    time.Time
}

func newWorkerFixture(t *testing.T) *workerFixture {
	f := &workerFixture{t: t, now: time.Date(2035, 6, 12, 12, 0, 0, 0, time.Local)}
	f.store = newFakeStore(t, 1)
	f.api = newFakeAPI(t, f.store)
	f.restart()
	return f
}

func (f *workerFixture) restart() {
	f.t.Helper()
	worker, err := New(f.store, f.api, 77, []int64{92, 91, 91}, testConfig(), slog.New(slog.NewTextHandler(&f.logs, nil)))
	if err != nil {
		f.t.Fatal(err)
	}
	worker.now = func() time.Time { return f.now }
	f.worker = worker
}

func (f *workerFixture) pass() {
	f.t.Helper()
	f.worker.handle(context.Background(), f.store.next(1))
}

func (f *workerFixture) passes(count int) {
	for range count {
		f.pass()
		f.restart()
	}
}

func (f *workerFixture) wantPosts(customer, invoice, issue int) {
	f.t.Helper()
	f.api.mu.Lock()
	defer f.api.mu.Unlock()
	if f.api.posts[CustomerCreate] != customer || f.api.posts[InvoiceCreate] != invoice || f.api.posts[InvoiceIssue] != issue {
		f.t.Fatalf("POST counts: %v; want %d/%d/%d", f.api.posts, customer, invoice, issue)
	}
}

func (f *workerFixture) wantHeld(code int) {
	f.t.Helper()
	state := f.store.states[1]
	if !state.held || state.finished || state.reviewCode != code || state.snapshot.DesiredAccess || state.snapshot.ExistingSUIID != "" || state.snapshot.ExistingRecurringID != "" {
		f.t.Fatalf("expected safe hold %d, got code=%d held=%v finished=%v", code, state.reviewCode, state.held, state.finished)
	}
	for _, message := range f.store.outbox {
		if strings.Contains(message.Key, ":invoice:") {
			f.t.Fatal("held registration emitted a success notification")
		}
	}
}

func TestWorkerFourPassFlowJournalsBeforeEachWriteAndFinishesAtomically(t *testing.T) {
	f := newWorkerFixture(t)
	token := f.store.states[1].snapshot.Token
	for index := range 4 {
		f.pass()
		state := f.store.states[1]
		if state.lease.Attempt != index+1 || state.writes[index+1] > 1 || state.snapshot.Token != token || state.snapshot.BotID != 77 {
			t.Fatal("attempt fencing, flow identity or write bound violated")
		}
		if state.snapshot.SubscriptionState != "awaiting_payment" || state.snapshot.DesiredAccess || state.snapshot.ExistingSUIID != "" || state.snapshot.ExistingRecurringID != "" {
			t.Fatal("registration granted access or created recurring state")
		}
		if index < 3 && (state.finished || len(f.store.outbox) != 0 || state.snapshot.ExistingCustomerID != "" || state.snapshot.ExistingFirstInvoiceID != "" || state.snapshot.ExistingCurrentInvoiceID != "") {
			t.Fatal("partial work associated billing state or emitted success")
		}
		f.restart()
	}
	f.wantPosts(1, 1, 1)
	state := f.store.states[1]
	if !state.finished || state.finishes != 1 || state.retries != 3 || len(f.store.outbox) != 3 || state.snapshot.ExistingCustomerID != "customer-1" || state.snapshot.ExistingFirstInvoiceID != "invoice-1" || state.snapshot.ExistingCurrentInvoiceID != "invoice-1" {
		t.Fatal("did not atomically finish with customer, first/current invoice and three notifications")
	}
	wantEvents := []string{
		"load", "find-customers", "prepare:customer_create", "post:customer_create", "confirm:customer_create", "retry",
		"load", "get-customer", "find-invoices", "get-product", "prepare:invoice_create", "post:invoice_create", "confirm:invoice_create", "retry",
		"load", "get-customer", "get-invoice", "prepare:invoice_issue", "post:invoice_issue", "confirm:invoice_issue", "retry",
		"load", "get-customer", "get-invoice", "finish",
	}
	if !reflect.DeepEqual(f.store.events, wantEvents) {
		t.Fatalf("unexpected stage ordering: %v", f.store.events)
	}
	var draft invoiceninja.InvoiceDraft
	if err := json.Unmarshal(state.snapshot.Steps[InvoiceCreate].Payload, &draft); err != nil {
		t.Fatal(err)
	}
	if draft.Date != "2035-06-12" || draft.DueDate != draft.Date || draft.ClientID != "customer-1" || len(draft.LineItems) != 1 || draft.LineItems[0].Cost != f.api.product.Price || draft.LineItems[0].Quantity != "1" {
		t.Fatalf("incorrect product/date snapshot: %+v", draft)
	}
	for _, rate := range []json.Number{draft.LineItems[0].TaxRate1, draft.LineItems[0].TaxRate2, draft.LineItems[0].TaxRate3} {
		if rate != "0" {
			t.Fatal("tax rates must be explicit zeroes, not empty numbers")
		}
	}
	before := make(map[string]queue.Draft)
	for key, draft := range f.store.outbox {
		before[key] = draft
	}
	f.pass() // A lost local completion acknowledgement/restart cannot POST again.
	f.wantPosts(1, 1, 1)
	if !reflect.DeepEqual(before, f.store.outbox) {
		t.Fatal("repeat completion changed business notifications")
	}
	client := f.store.outbox["registration:"+token+":invoice:client"]
	if !strings.Contains(client.Text, "1200.6") || strings.Contains(client.Text, "1000.5") {
		t.Fatal("notification did not use the concrete invoice amount")
	}
}

func TestWorkerAmbiguousCreatesRecoverOnlyByPositiveRead(t *testing.T) {
	for _, step := range []Step{CustomerCreate, InvoiceCreate} {
		t.Run(string(step), func(t *testing.T) {
			f := newWorkerFixture(t)
			if step == InvoiceCreate {
				f.passes(1)
			}
			f.api.writeErr[step], f.api.commitOnError[step] = errors.New("lost acknowledgement"), true
			f.pass()
			intent := f.store.states[1].snapshot.Steps[step]
			if len(intent.Payload) == 0 || intent.ExternalID != "" || f.store.states[1].held {
				t.Fatal("uncertain create was not durably unresolved")
			}
			f.restart()
			f.pass()
			if f.store.states[1].snapshot.Steps[step].ExternalID == "" {
				t.Fatal("positive owned read did not reconcile create")
			}
			if step == CustomerCreate {
				f.passes(3)
			} else {
				f.passes(2)
			}
			f.wantPosts(1, 1, 1)
			if !f.store.states[1].finished {
				t.Fatal("recovered create did not finish")
			}
		})
	}
}

func TestWorkerUnknownCreateEmptyLookupHoldsAndResumeNeverRearms(t *testing.T) {
	for _, step := range []Step{CustomerCreate, InvoiceCreate} {
		t.Run(string(step), func(t *testing.T) {
			f := newWorkerFixture(t)
			if step == InvoiceCreate {
				f.passes(1)
			}
			f.api.writeErr[step] = errors.New("unknown result, no visible record")
			f.pass()
			payload := slices.Clone(f.store.states[1].snapshot.Steps[step].Payload)
			f.restart()
			f.pass()
			f.wantHeld(ReviewUnknownWrite)
			f.store.resume(1)
			f.pass()
			f.wantHeld(ReviewUnknownWrite)
			if !bytes.Equal(payload, f.store.states[1].snapshot.Steps[step].Payload) {
				t.Fatal("resume rewrote immutable intent")
			}
			// The old remote transaction becomes visible only after manual hold.
			// It does not make the held FIFO head automatically claimable.
			if step == CustomerCreate {
				var d invoiceninja.CustomerDraft
				if err := json.Unmarshal(payload, &d); err != nil {
					t.Fatal(err)
				}
				f.api.customers = append(f.api.customers, receiptCustomer(d, "late-customer"))
			} else {
				var d invoiceninja.InvoiceDraft
				if err := json.Unmarshal(payload, &d); err != nil {
					t.Fatal(err)
				}
				f.api.invoices = append(f.api.invoices, receiptInvoice(d, "late-invoice"))
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			ops, err := f.store.ClaimOperations(ctx, []string{"register_client"}, 1, 2*time.Minute)
			cancel()
			if err != nil || len(ops) != 0 {
				t.Fatal("late commit automatically released hold")
			}
			f.store.resume(1)
			f.restart()
			f.pass() // Controlled resume may adopt positive evidence, never POST.
			if step == CustomerCreate {
				f.wantPosts(1, 0, 0)
			} else {
				f.wantPosts(1, 1, 0)
			}
			if !bytes.Equal(payload, f.store.states[1].snapshot.Steps[step].Payload) {
				t.Fatal("late reconciliation rewrote intent")
			}
		})
	}
}

func TestWorkerExistingOrDuplicateCustomersHoldWithoutFakeCompletion(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(strconv.FormatBool(duplicate), func(t *testing.T) {
			f := newWorkerFixture(t)
			s := f.store.states[1].snapshot
			f.api.customers = []invoiceninja.Customer{{ID: "existing", IDNumber: strconv.FormatInt(s.TelegramID, 10), Name: s.DisplayName}}
			code := ReviewExistingCustomer
			if duplicate {
				f.api.customers = append(f.api.customers, f.api.customers[0])
				f.api.customers[1].ID = "duplicate"
				code = ReviewCustomerConflict
			}
			f.pass()
			f.wantHeld(code)
			f.wantPosts(0, 0, 0)
			if len(f.store.states[1].snapshot.Steps) != 0 || f.store.states[1].snapshot.ExistingCustomerID != "" {
				t.Fatal("unowned customer was adopted")
			}
		})
	}
}

func TestWorkerInvoiceInventoryConflictsHold(t *testing.T) {
	for _, kind := range []string{"unowned-history", "owned-without-intent", "duplicate-owned"} {
		t.Run(kind, func(t *testing.T) {
			f := newWorkerFixture(t)
			f.passes(1)
			state := f.store.states[1]
			d := invoiceDraft("customer-1", f.api.product, "2035-06-12", marker(state.snapshot.Token, InvoiceCreate), false)
			invoice := receiptInvoice(d, "old-invoice")
			if kind == "unowned-history" {
				invoice.PrivateNotes = "unowned"
			}
			f.api.invoices = []invoiceninja.Invoice{invoice}
			if kind == "duplicate-owned" {
				raw, err := json.Marshal(d)
				if err != nil {
					t.Fatal(err)
				}
				state.snapshot.Steps[InvoiceCreate] = Intent{Payload: raw}
				invoice.ID = "duplicate"
				f.api.invoices = append(f.api.invoices, invoice)
			}
			f.pass()
			f.wantHeld(ReviewInvoiceConflict)
			f.wantPosts(1, 0, 0)
		})
	}
}

func TestWorkerAnyUncertainIssueHoldsEvenWhenLaterSentAndFullyUnpaid(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(strconv.FormatBool(committed), func(t *testing.T) {
			f := newWorkerFixture(t)
			f.passes(2)
			f.api.writeErr[InvoiceIssue], f.api.commitOnError[InvoiceIssue] = errors.New("mark_sent lost its acknowledgement"), committed
			f.pass()
			f.wantHeld(ReviewUnknownWrite)
			if f.store.states[1].snapshot.Steps[InvoiceIssue].ExternalID != "" {
				t.Fatal("uncertain issue was confirmed")
			}
			// Status is claimed before upstream ledger effects. Even the expected
			// balance is not evidence that the ambiguous action completed.
			f.api.invoices[0].StatusID = "2"
			f.api.invoices[0].Balance = f.api.invoices[0].Amount
			for range 2 {
				f.store.resume(1)
				f.restart()
				f.pass()
				f.wantHeld(ReviewUnknownWrite)
			}
			f.wantPosts(1, 1, 1)
		})
	}
}

func TestWorkerIssueErrorsNeverAuthorizeAutomaticRetry(t *testing.T) {
	for _, err := range []error{
		context.DeadlineExceeded, context.Canceled, invoiceninja.ErrWriteVersion,
		&integrationhttp.HTTPError{StatusCode: 500}, errors.New("malformed acknowledgement"),
	} {
		t.Run(err.Error(), func(t *testing.T) {
			f := newWorkerFixture(t)
			f.passes(2)
			f.api.writeErr[InvoiceIssue], f.api.commitOnError[InvoiceIssue] = err, true
			f.pass()
			f.wantHeld(ReviewUnknownWrite)
			f.store.resume(1)
			f.restart()
			f.pass()
			f.wantHeld(ReviewUnknownWrite)
			f.wantPosts(1, 1, 1)
		})
	}
}

func TestWorkerIncompleteLookupsCannotAuthorizeWritesOrReconciliation(t *testing.T) {
	for _, invoiceStage := range []bool{false, true} {
		t.Run(strconv.FormatBool(invoiceStage), func(t *testing.T) {
			f := newWorkerFixture(t)
			if invoiceStage {
				f.passes(1)
			}
			f.api.lookupErr = errors.Join(invoiceninja.ErrLookupIncomplete, errors.New("truncated inventory"))
			f.pass()
			f.wantHeld(ReviewContract)
			if invoiceStage {
				f.wantPosts(1, 0, 0)
			} else {
				f.wantPosts(0, 0, 0)
			}
		})
	}
}

func TestWorkerLeaseLossAfterKnownReceiptDoesNotReplayTheIntent(t *testing.T) {
	for index, step := range []Step{CustomerCreate, InvoiceCreate, InvoiceIssue} {
		t.Run(string(step), func(t *testing.T) {
			f := newWorkerFixture(t)
			f.passes(index)
			f.store.confirmErr = queue.ErrLeaseLost
			f.pass()
			f.store.confirmErr = nil
			f.restart()
			f.pass()
			if step == InvoiceIssue {
				f.wantHeld(ReviewUnknownWrite)
				f.wantPosts(1, 1, 1)
			} else {
				if f.store.states[1].snapshot.Steps[step].ExternalID == "" {
					t.Fatal("positive create reconciliation did not confirm")
				}
				if step == CustomerCreate {
					f.wantPosts(1, 0, 0)
				} else {
					f.wantPosts(1, 1, 0)
				}
			}
		})
	}
}

func TestWorkerStalePrepareOrConfirmStopsFurtherWork(t *testing.T) {
	for index, step := range []Step{CustomerCreate, InvoiceCreate, InvoiceIssue} {
		for _, stage := range []string{"prepare", "confirm"} {
			t.Run(string(step)+"/"+stage, func(t *testing.T) {
				f := newWorkerFixture(t)
				f.passes(index)
				if stage == "prepare" {
					f.store.prepareErr = queue.ErrLeaseLost
				} else {
					f.store.confirmErr = queue.ErrLeaseLost
				}
				start := len(f.store.events)
				retries := f.store.states[1].retries
				f.pass()
				events := f.store.events[start:]
				if events[len(events)-1] != stage+":"+string(step) || f.store.states[1].retries != retries || f.store.states[1].held || f.store.states[1].finished {
					t.Fatalf("continued after lease loss: %v", events)
				}
				want := []int{0, 0, 0}
				for i := range index {
					want[i] = 1
				}
				if stage == "confirm" {
					want[index] = 1
				}
				f.wantPosts(want[0], want[1], want[2])
			})
		}
	}
}

func TestWorkerLocalStateGuardsNeverChangeAccessOrAdoptAssociations(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Snapshot)
	}{
		{"bot", func(s *Snapshot) { s.BotID++ }},
		{"telegram", func(s *Snapshot) { s.TelegramID = 0 }},
		{"nonprivate-chat", func(s *Snapshot) { s.ChatID = -100 }},
		{"state", func(s *Snapshot) { s.SubscriptionState = "active" }},
		{"desired-access", func(s *Snapshot) { s.DesiredAccess = true }},
		{"sui", func(s *Snapshot) { s.ExistingSUIID = "existing-sui" }},
		{"recurring", func(s *Snapshot) { s.ExistingRecurringID = "existing-recurring" }},
		{"customer", func(s *Snapshot) { s.ExistingCustomerID = "imported-customer" }},
		{"first-invoice", func(s *Snapshot) { s.ExistingFirstInvoiceID = "imported-invoice" }},
		{"current-invoice", func(s *Snapshot) { s.ExistingCurrentInvoiceID = "imported-invoice" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWorkerFixture(t)
			tc.edit(&f.store.states[1].snapshot)
			before := cloneSnapshot(f.store.states[1].snapshot)
			f.pass()
			state := f.store.states[1]
			if !state.held || state.reviewCode != ReviewLocalAssociation || state.finished || !reflect.DeepEqual(before, state.snapshot) {
				t.Fatal("invalid local state was adopted or changed")
			}
			f.wantPosts(0, 0, 0)
		})
	}
}

func replaceIntent(t *testing.T, state *fakeRegistrationState, step Step, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	intent := state.snapshot.Steps[step]
	intent.Payload = raw
	state.snapshot.Steps[step] = intent
}

func TestWorkerMalformedCreateIntentsHoldBeforeWriting(t *testing.T) {
	for index, step := range []Step{CustomerCreate, InvoiceCreate} {
		for _, malformed := range []string{"broken", "unknown-field", "second-object", "oversized"} {
			t.Run(string(step)+"/"+malformed, func(t *testing.T) {
				f := newWorkerFixture(t)
				f.passes(index + 1)
				state := f.store.states[1]
				intent := state.snapshot.Steps[step]
				switch malformed {
				case "broken":
					intent.Payload = json.RawMessage(`{"broken":`)
				case "unknown-field":
					intent.Payload = append(slices.Clone(intent.Payload[:len(intent.Payload)-1]), []byte(`,"unknown":true}`)...)
				case "second-object":
					intent.Payload = append(slices.Clone(intent.Payload), []byte(` {}`)...)
				case "oversized":
					intent.Payload = json.RawMessage(strings.Repeat(" ", 64*1024+1))
				}
				state.snapshot.Steps[step] = intent
				f.pass()
				f.wantHeld(ReviewContract)
				if step == CustomerCreate {
					f.wantPosts(1, 0, 0)
				} else {
					f.wantPosts(1, 1, 0)
				}
			})
		}
	}
}

func TestWorkerChangedIdentityAndInvoicePayloadsHold(t *testing.T) {
	for _, change := range []string{"customer-id-number", "customer-marker", "customer-remote-marker", "invoice-client", "invoice-marker", "invoice-cost", "invoice-tax", "invoice-date", "invoice-due-date"} {
		t.Run(change, func(t *testing.T) {
			f := newWorkerFixture(t)
			f.passes(2)
			state := f.store.states[1]
			code := ReviewContract
			var customer invoiceninja.CustomerDraft
			var invoice invoiceninja.InvoiceDraft
			if err := json.Unmarshal(state.snapshot.Steps[CustomerCreate].Payload, &customer); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(state.snapshot.Steps[InvoiceCreate].Payload, &invoice); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "customer-id-number":
				customer.IDNumber = "999"
				replaceIntent(t, state, CustomerCreate, customer)
			case "customer-marker":
				customer.PrivateNotes = marker("different_flow_token_0123456789", CustomerCreate)
				replaceIntent(t, state, CustomerCreate, customer)
			case "customer-remote-marker":
				f.api.customers[0].PrivateNotes = "unowned"
				code = ReviewCustomerConflict
			case "invoice-client":
				invoice.ClientID = "another-client"
				replaceIntent(t, state, InvoiceCreate, invoice)
			case "invoice-marker":
				invoice.PrivateNotes = marker("different_flow_token_0123456789", InvoiceCreate)
				replaceIntent(t, state, InvoiceCreate, invoice)
			case "invoice-cost":
				invoice.LineItems[0].Cost = "999"
				replaceIntent(t, state, InvoiceCreate, invoice)
				code = ReviewInvoiceConflict
			case "invoice-tax":
				invoice.UsesInclusiveTaxes = !invoice.UsesInclusiveTaxes
				replaceIntent(t, state, InvoiceCreate, invoice)
				code = ReviewInvoiceConflict
			case "invoice-date":
				f.api.invoices[0].Date = "2035-06-11"
				code = ReviewInvoiceConflict
			case "invoice-due-date":
				invoice.DueDate = "2035-06-13"
				replaceIntent(t, state, InvoiceCreate, invoice)
			}
			f.pass()
			f.wantHeld(code)
			f.wantPosts(1, 1, 0)
		})
	}
}

func TestWorkerUnsafeInvoiceLifecycleStatesHold(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		for _, state := range []string{"paid", "partially-paid", "cancelled", "future-due", "deleted", "archived", "wrong-balance", "zero-amount"} {
			t.Run(fmt.Sprintf("confirmed=%v/%s", confirmed, state), func(t *testing.T) {
				f := newWorkerFixture(t)
				f.passes(2)
				if confirmed {
					f.passes(1)
				}
				i := &f.api.invoices[0]
				code := ReviewInvoiceState
				switch state {
				case "paid":
					i.StatusID, i.Balance = "4", "0"
				case "partially-paid":
					i.StatusID, i.Balance = "3", "12.34"
				case "cancelled":
					i.StatusID = "5"
				case "future-due":
					i.DueDate = "2035-06-13"
					code = ReviewInvoiceConflict
				case "deleted":
					i.IsDeleted = true
					code = ReviewInvoiceConflict
				case "archived":
					i.ArchivedAt = 1
					code = ReviewInvoiceConflict
				case "wrong-balance":
					if confirmed {
						i.Balance = "0"
					} else {
						i.Balance = i.Amount
					}
				case "zero-amount":
					i.Amount, i.Balance = "0", "0"
				}
				f.pass()
				f.wantHeld(code)
				if confirmed {
					f.wantPosts(1, 1, 1)
				} else {
					f.wantPosts(1, 1, 0)
				}
			})
		}
	}
}

func TestWorkerIssuanceUsesLocalDateAndHoldsAcrossMidnight(t *testing.T) {
	t.Run("local-not-returned-clock-zone", func(t *testing.T) {
		f := newWorkerFixture(t)
		local := time.Date(2035, 6, 12, 23, 59, 0, 0, time.Local)
		_, offset := local.Zone()
		f.now = local.In(time.FixedZone("clock-return-zone", offset+12*60*60))
		if f.now.Format(time.DateOnly) == local.Format(time.DateOnly) {
			t.Fatal("test did not distinguish clock zone from local date")
		}
		f.passes(4)
		if f.api.invoices[0].Date != "2035-06-12" || f.api.invoices[0].DueDate != "2035-06-12" || !f.store.states[1].finished {
			t.Fatal("invoice date/deadline did not use local date")
		}
	})
	for _, kind := range []string{"midnight", "future-intent", "past-intent"} {
		t.Run(kind, func(t *testing.T) {
			f := newWorkerFixture(t)
			f.now = time.Date(2035, 6, 12, 23, 59, 59, 0, time.Local)
			f.passes(2)
			if kind == "midnight" {
				f.now = f.now.Add(time.Second)
			} else {
				date := "2035-06-13"
				if kind == "past-intent" {
					date = "2035-06-11"
				}
				var d invoiceninja.InvoiceDraft
				if err := json.Unmarshal(f.store.states[1].snapshot.Steps[InvoiceCreate].Payload, &d); err != nil {
					t.Fatal(err)
				}
				d.Date, d.DueDate = date, date
				replaceIntent(t, f.store.states[1], InvoiceCreate, d)
				f.api.invoices[0].Date, f.api.invoices[0].DueDate = date, date
			}
			f.pass()
			f.wantHeld(ReviewInvoiceState)
			f.wantPosts(1, 1, 0)
		})
	}
}

func TestWorkerInvalidProductCannotReserveOrPostInvoice(t *testing.T) {
	for _, change := range []string{"deleted", "archived", "missing-rate", "zero-price", "bad-price", "missing-key"} {
		t.Run(change, func(t *testing.T) {
			f := newWorkerFixture(t)
			f.passes(1)
			switch change {
			case "deleted":
				f.api.product.IsDeleted = true
			case "archived":
				f.api.product.ArchivedAt = 1
			case "missing-rate":
				f.api.product.TaxRate2 = ""
			case "zero-price":
				f.api.product.Price = "0"
			case "bad-price":
				f.api.product.Price = "1e100000"
			case "missing-key":
				f.api.product.ProductKey = ""
			}
			f.pass()
			f.wantHeld(ReviewContract)
			f.wantPosts(1, 0, 0)
			if _, exists := f.store.states[1].snapshot.Steps[InvoiceCreate]; exists {
				t.Fatal("invalid invoice intent was reserved")
			}
		})
	}
}

func TestWorkerJournalsOnlyExplicitBillingPayloadFields(t *testing.T) {
	f := newWorkerFixture(t)
	f.passes(4)
	for step, want := range map[Step][]string{
		CustomerCreate: {"id_number", "name", "private_notes"},
		InvoiceCreate:  {"client_id", "date", "due_date", "line_items", "private_notes", "uses_inclusive_taxes"},
		InvoiceIssue:   {"action", "ids"},
	} {
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(f.store.states[1].snapshot.Steps[step].Payload, &payload); err != nil {
			t.Fatal(err)
		}
		var keys []string
		for key := range payload {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		if !reflect.DeepEqual(keys, want) {
			t.Fatalf("unexpected write flags for %s: %v", step, keys)
		}
	}
	// This asserts only our request payloads, not upstream email/event settings.
}

func TestWorkerLogsNeverIncludeSecretsPIIURLsOrRawErrors(t *testing.T) {
	f := newWorkerFixture(t)
	s := f.store.states[1].snapshot
	private := []string{testConfig().Token, s.Token, s.DisplayName, testConfig().PaymentPhone, "private_notes", "https://secret.example.test/path", "RAW-UPSTREAM-ERROR"}
	err := errors.New(strings.Join(private, " "))
	f.api.lookupErr = err
	f.pass()
	f.api.lookupErr = nil
	f.api.writeErr[CustomerCreate], f.api.commitOnError[CustomerCreate] = err, true
	f.pass()
	f.passes(2) // Positive customer reconciliation, then invoice creation.
	f.api.writeErr[InvoiceIssue] = err
	f.pass()
	log := f.logs.String()
	for _, value := range private {
		if strings.Contains(log, value) {
			t.Errorf("private data leaked in logs: %q", value)
		}
	}
	if !strings.Contains(log, "registration attempt deferred") || !strings.Contains(log, "registration create result unknown") || !strings.Contains(log, "registration issue result unknown") || !strings.Contains(log, "registration held for review") {
		t.Fatalf("did not exercise error logging paths: %s", log)
	}
}

func TestWorkerChangedOrMalformedIssueIntentsHoldEvenWithConfirmedID(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		for _, raw := range []string{
			`{"action":"mark_sent","ids":["different-invoice"]}`,
			`{"action":"email","ids":["invoice-1"]}`,
			`{"action":"mark_sent","ids":["invoice-1","invoice-2"]}`,
			`{"action":"mark_sent","ids":[]}`,
			`{"action":"mark_sent","ids":["invoice-1"],"unknown":true}`,
			`{"action":`,
			`{"action":"mark_sent","ids":["invoice-1"]} {}`,
		} {
			t.Run(fmt.Sprintf("confirmed=%v/%s", confirmed, raw), func(t *testing.T) {
				f := newWorkerFixture(t)
				f.passes(3)
				state := f.store.states[1]
				intent := state.snapshot.Steps[InvoiceIssue]
				intent.Payload = json.RawMessage(raw)
				if !confirmed {
					intent.ExternalID = ""
				}
				state.snapshot.Steps[InvoiceIssue] = intent
				f.pass()
				f.wantHeld(ReviewContract)
				f.wantPosts(1, 1, 1)
			})
		}
	}
}

func receiveWithin[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case value := <-ch:
		return value
	case <-timer.C:
		t.Fatal("timed out waiting for worker synchronization")
	}
	var zero T
	return zero
}

func TestWorkerRunClaimsOnlyRegistrationAndBoundsConcurrency(t *testing.T) {
	f := newWorkerFixture(t)
	f.store = newFakeStore(t, 6)
	f.api = newFakeAPI(t, f.store)
	f.restart()
	f.worker.cfg.Workers = 3
	started := make(chan struct{}, 6)
	var active, maximum atomic.Int32
	f.store.loadHook = func(ctx context.Context, _ queue.Lease) error {
		checkDeadline(t, ctx, 8*time.Second)
		current := active.Add(1)
		for old := maximum.Load(); current > old; old = maximum.Load() {
			if maximum.CompareAndSwap(old, current) {
				break
			}
		}
		started <- struct{}{}
		<-ctx.Done()
		active.Add(-1)
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.worker.Run(ctx) }()
	t.Cleanup(func() { cancel(); receiveWithin(t, done) })
	for range 3 {
		receiveWithin(t, started)
	}
	if active.Load() != 3 || maximum.Load() != 3 {
		t.Fatal("worker did not start the bounded batch concurrently")
	}
	select {
	case <-started:
		t.Fatal("more handlers than configured workers")
	default:
	}
	cancel()
	// Leave the completion value available for cleanup, which also joins Run.
	err := receiveWithin(t, done)
	done <- err
	if err != nil || active.Load() != 0 || f.store.claimCalls != 1 {
		t.Fatal("Run did not cancel and join exactly its claimed batch")
	}
	f.wantPosts(0, 0, 0)
	for id := int64(1); id <= 6; id++ {
		want := 0
		if id <= 3 {
			want = 1
		}
		if f.store.states[id].lease.Attempt != want {
			t.Fatalf("unexpected lease attempt for %d", id)
		}
	}
}

func TestWorkerRunCancellationJoinsKnownConfirmationWithOneEightSecondBudget(t *testing.T) {
	if leaseDuration != 2*time.Minute || attemptTimeout != time.Minute || databaseTimeout != 8*time.Second {
		t.Fatal("registration lease/attempt/finalization budgets changed")
	}
	f := newWorkerFixture(t)
	entered, ack := make(chan struct{}), make(chan struct{})
	confirming, release := make(chan context.Context, 1), make(chan struct{})
	var ackOnce, releaseOnce sync.Once
	f.api.afterWrite = func(ctx context.Context, step Step) {
		if step != CustomerCreate {
			t.Error("POST after cancellation")
			return
		}
		checkDeadline(t, ctx, time.Minute)
		close(entered)
		<-ack
	}
	f.store.confirmHook = func(ctx context.Context, _ queue.Lease, _ Step) {
		confirming <- ctx
		<-release
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.worker.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		ackOnce.Do(func() { close(ack) })
		releaseOnce.Do(func() { close(release) })
		receiveWithin(t, done)
	})
	receiveWithin(t, entered)
	cancel() // The remote customer has committed; its known receipt arrives next.
	ackOnce.Do(func() { close(ack) })
	finalCtx := receiveWithin(t, confirming)
	if finalCtx.Err() != nil {
		t.Fatal("known confirmation inherited canceled service context")
	}
	checkDeadline(t, finalCtx, 8*time.Second)
	select {
	case <-done:
		t.Fatal("Run returned before known-result confirmation completed")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	err := receiveWithin(t, done)
	done <- err
	if err != nil || f.store.states[1].snapshot.Steps[CustomerCreate].ExternalID != "customer-1" || f.store.states[1].retries != 1 {
		t.Fatal("cancellation did not finish known confirmation and retry before exit")
	}
	f.wantPosts(1, 0, 0)
	if f.store.claimCalls != 1 {
		t.Fatal("claimed more work after cancellation")
	}
	f.worker.handle(ctx, f.store.next(1))
	f.wantPosts(1, 0, 0)
}

func TestWorkerConstructorRequiresExplicitCapabilityAndVerifiedIdentity(t *testing.T) {
	f := newWorkerFixture(t)
	for _, tc := range []struct {
		name   string
		store  Store
		api    API
		botID  int64
		admins []int64
		cfg    Config
	}{
		{"store", nil, f.api, 77, []int64{91}, testConfig()},
		{"api", f.store, nil, 77, []int64{91}, testConfig()},
		{"bot", f.store, f.api, 0, []int64{91}, testConfig()},
		{"admins-empty", f.store, f.api, 77, nil, testConfig()},
		{"admins-negative", f.store, f.api, 77, []int64{-1}, testConfig()},
		{"disabled", f.store, f.api, 77, []int64{91}, Config{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.store, tc.api, tc.botID, tc.admins, tc.cfg, nil); err == nil {
				t.Fatal("unsafe worker constructed")
			}
		})
	}
	ids := []int64{92, 91, 92}
	w, err := New(f.store, f.api, 77, ids, testConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ids[0] = 999
	if !reflect.DeepEqual(w.admins, []int64{91, 92}) {
		t.Fatal("administrator IDs were not cloned, sorted and deduplicated")
	}
	ids = make([]int64, 100)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	if _, err := New(f.store, f.api, 77, ids, testConfig(), nil); err == nil {
		t.Fatal("unbounded administrator notifications")
	}
}

func TestWorkerNoWriteWithoutJournalAndGrantedPermission(t *testing.T) {
	for index, step := range []Step{CustomerCreate, InvoiceCreate, InvoiceIssue} {
		for _, failure := range []string{"not-granted", "journal-failed", "journal-commit-unknown"} {
			t.Run(string(step)+"/"+failure, func(t *testing.T) {
				f := newWorkerFixture(t)
				f.passes(index)
				if failure == "not-granted" {
					f.store.deny = step
				} else {
					f.store.prepareErr = errors.New("journal unavailable")
					f.store.prepareCommit = failure == "journal-commit-unknown"
				}
				f.pass()
				want := []int{0, 0, 0}
				for i := range index {
					want[i] = 1
				}
				f.wantPosts(want[0], want[1], want[2])
				if f.store.states[1].finished {
					t.Fatal("finished without permission")
				}
				if failure != "journal-failed" {
					f.store.prepareErr = nil
					f.restart()
					f.pass()
					f.wantHeld(ReviewUnknownWrite)
					f.wantPosts(want[0], want[1], want[2])
				}
			})
		}
	}
}
