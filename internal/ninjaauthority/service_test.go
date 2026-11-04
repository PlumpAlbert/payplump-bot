package ninjaauthority

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

type memoryStore struct {
	mu             sync.Mutex
	bindings       map[int64]Binding
	observations   map[int64]Observation
	intents        map[int64]CreateIntent
	notices        map[string]string
	noticeCount    int
	invoiceBatches [][]Invoice
}

func newMemoryStore() *memoryStore {
	return &memoryStore{bindings: map[int64]Binding{}, observations: map[int64]Observation{}, intents: map[int64]CreateIntent{}, notices: map[string]string{}}
}
func (s *memoryStore) WithAuthorityScan(ctx context.Context, bot int64, f func(context.Context) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return f(ctx)
}
func (s *memoryStore) LoadAuthorityBindings(context.Context, int64) (map[int64]Binding, error) {
	return s.bindings, nil
}
func (s *memoryStore) SaveAuthorityObservation(_ context.Context, _ int64, o Observation) error {
	s.observations[o.Actor] = o
	if o.Binding.SUIID > 0 {
		s.bindings[o.Actor] = o.Binding
	}
	return nil
}
func (s *memoryStore) SaveAuthorityObservationWithInvoices(ctx context.Context, bot int64, o Observation, invoices []Invoice) error {
	s.invoiceBatches = append(s.invoiceBatches, invoices)
	return s.SaveAuthorityObservation(ctx, bot, o)
}
func (s *memoryStore) LoadAuthorityWake(context.Context, int64) (WakeGeneration, error) {
	return WakeGeneration{}, nil
}
func (s *memoryStore) PrepareAuthorityCreate(_ context.Context, _ int64, o Observation, d sui.AuthorityCreateDraft) (CreateIntent, error) {
	if old, ok := s.intents[o.Actor]; ok {
		return old, nil
	}
	x := CreateIntent{Draft: d, SourceHash: o.SourceHash}
	s.intents[o.Actor] = x
	return x, nil
}
func (s *memoryStore) AuthorizeAuthorityCreate(_ context.Context, _ int64, a int64, h [32]byte) error {
	x, ok := s.intents[a]
	if !ok || x.Dispatched || x.SourceHash != h {
		return ErrUnknown
	}
	x.Dispatched = true
	s.intents[a] = x
	return nil
}
func (s *memoryStore) CompleteAuthorityCreate(_ context.Context, _ int64, a int64, r sui.ClientRecord) error {
	x := s.intents[a]
	if r.Name() != x.Draft.Name() || r.Enabled() != x.Draft.Enabled() || r.CredentialsHash() != x.Draft.CredentialsHash() {
		return ErrConflict
	}
	x.Completed = true
	s.intents[a] = x
	return nil
}
func (s *memoryStore) AuthorityNotice(_ context.Context, _ int64, key string, _ int64, reason, facts string, _ []int64) error {
	value := reason + facts
	if s.notices[key] != value {
		s.noticeCount++
	}
	s.notices[key] = value
	return nil
}
func (s *memoryStore) AuthoritySourceFailure(ctx context.Context, bot int64, key string, actor int64, d SourceFailure, admins []int64) error {
	return s.AuthorityNotice(ctx, bot, key, actor, "source_unavailable", d.Hash(), admins)
}
func (s *memoryStore) ReconcileAuthorityNotices(_ context.Context, _ int64, keys []string, _ time.Time) error {
	keep := map[string]bool{}
	for _, k := range keys {
		keep[k] = true
	}
	for k := range s.notices {
		if !keep[k] {
			delete(s.notices, k)
		}
	}
	return nil
}

type fakeNinja struct {
	customers []invoiceninja.Customer
	records   []invoiceninja.RecurringRecord
	err       error
}

func (n *fakeNinja) ListCustomers(context.Context) ([]invoiceninja.Customer, error) {
	return n.customers, n.err
}
func (n *fakeNinja) ListRecurring(_ context.Context, id string) ([]invoiceninja.RecurringRecord, error) {
	out := []invoiceninja.RecurringRecord{}
	for _, r := range n.records {
		if r.ClientID == id {
			out = append(out, r)
		}
	}
	return out, n.err
}

type fakeInvoices struct{}

func (fakeInvoices) ListInvoices(context.Context, string) ([]invoiceninja.CycleInvoice, error) {
	return nil, nil
}

type fakePanel struct {
	records      []sui.ClientRecord
	posts        int
	unknown      bool
	before       func()
	inventoryErr error
}

func (p *fakePanel) Inventory(context.Context) ([]sui.ClientRecord, error) {
	return p.records, p.inventoryErr
}
func (p *fakePanel) FindCreated(_ context.Context, d sui.AuthorityCreateDraft) (sui.ClientRecord, bool, error) {
	for _, r := range p.records {
		if r.Name() == d.Name() {
			if r.CredentialsHash() != d.CredentialsHash() || r.Enabled() != d.Enabled() {
				return sui.ClientRecord{}, false, ErrConflict
			}
			return r, true, nil
		}
	}
	return sui.ClientRecord{}, false, nil
}
func (p *fakePanel) CreateAuthorized(ctx context.Context, d sui.AuthorityCreateDraft, auth func(context.Context) error) (sui.ClientRecord, error) {
	if p.before != nil {
		p.before()
	}
	if err := auth(ctx); err != nil {
		return sui.ClientRecord{}, err
	}
	p.posts++
	if p.unknown {
		return sui.ClientRecord{}, sui.ErrManagedUnknown
	}
	r := draftRecord(d, int64(100+p.posts))
	p.records = append(p.records, r)
	return r, nil
}
func draftRecord(d sui.AuthorityCreateDraft, id int64) sui.ClientRecord {
	raw, e := json.Marshal(d)
	if e != nil {
		panic(e)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		panic("fixture")
	}
	fields["id"], _ = json.Marshal(id)
	raw, _ = json.Marshal(fields)
	r, e := sui.ParseClientRecord(raw)
	if e != nil {
		panic("fixture record")
	}
	return r
}
func fixture(t *testing.T, status string, inactive bool) (*Service, *memoryStore, *fakeNinja, *fakePanel) {
	t.Helper()
	st := newMemoryStore()
	n := &fakeNinja{customers: []invoiceninja.Customer{{ID: "customer-42", IDNumber: "42", IsDeleted: inactive}}, records: []invoiceninja.RecurringRecord{{ID: "recurring-42", ClientID: "customer-42", StatusID: status, AutoBill: "always", AutoBillEnabled: true, DueDateDays: "terms", Amount: "100", NextSendDate: "2040-12-31"}}}
	p := &fakePanel{}
	s, e := New(st, n, fakeInvoices{}, p, Config{BotID: 77, SourceScopeHash: strings.Repeat("a", 64), InboundIDs: []int64{1}, AdminIDs: []int64{99}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if e != nil {
		t.Fatal(e)
	}
	return s, st, n, p
}
func TestAuthorityMissingCreationUsesNinjaState(t *testing.T) {
	for _, tc := range []struct {
		status         string
		inactive, want bool
	}{{"2", false, true}, {"3", false, false}, {"2", true, false}, {"3", true, false}} {
		t.Run(tc.status+string(rune('0'+boolInt(tc.inactive))), func(t *testing.T) {
			s, st, _, p := fixture(t, tc.status, tc.inactive)
			if e := s.Scan(context.Background()); e != nil {
				t.Fatal(e)
			}
			if p.posts != 1 || len(p.records) != 1 || p.records[0].Enabled() != tc.want {
				t.Fatal("wrong initial enabled state")
			}
			if e := s.Scan(context.Background()); e != nil {
				t.Fatal(e)
			}
			if p.posts != 1 || !st.observations[42].StateKnown || st.observations[42].Enabled != tc.want {
				t.Fatal("replay or state adoption failed")
			}
		})
	}
}
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
func TestAuthorityExistingIsNeverModified(t *testing.T) {
	for _, status := range []string{"2", "3"} {
		for _, enabled := range []bool{true, false} {
			s, st, _, p := fixture(t, status, false)
			d, e := sui.NewAuthorityCreateDraft("42", enabled, []int64{1})
			if e != nil {
				t.Fatal(e)
			}
			r := draftRecord(d, 7)
			before := string(r.Raw())
			p.records = []sui.ClientRecord{r}
			for i := 0; i < 2; i++ {
				if e = s.Scan(context.Background()); e != nil {
					t.Fatal(e)
				}
			}
			if p.posts != 0 || string(p.records[0].Raw()) != before {
				t.Fatal("existing account modified")
			}
			want := 0
			if enabled != (status == "2") {
				want = 1
			}
			if st.noticeCount != want {
				t.Fatalf("notice count=%d want=%d", st.noticeCount, want)
			}
		}
	}
}
func TestAuthorityUnknownPostNeverRetries(t *testing.T) {
	s, st, _, p := fixture(t, "2", false)
	p.unknown = true
	for i := 0; i < 2; i++ {
		if e := s.Scan(context.Background()); e != nil {
			t.Fatal(e)
		}
	}
	if p.posts != 1 || !st.intents[42].Dispatched || st.intents[42].Completed {
		t.Fatal("unknown dispatch replayed or fabricated receipt")
	}
}
func TestAuthorityConcurrentScansCreateOnce(t *testing.T) {
	s, _, _, p := fixture(t, "2", false)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e := s.Scan(context.Background()); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	if p.posts != 1 {
		t.Fatal("duplicate creation")
	}
}
func TestAuthoritySourceDriftBeforeAuthorizationStopsCreation(t *testing.T) {
	s, st, n, p := fixture(t, "2", false)
	p.before = func() { n.records[0].StatusID = "3" }
	if e := s.Scan(context.Background()); e != nil {
		t.Fatal(e)
	}
	if p.posts != 0 || st.intents[42].Dispatched {
		t.Fatal("source drift crossed authorization")
	}
}
func TestAuthorityIncompleteInventoryAndAmbiguityStopCreation(t *testing.T) {
	for _, kind := range []string{"ninja_failure", "sui_failure", "duplicate_actor", "unknown_state"} {
		t.Run(kind, func(t *testing.T) {
			s, _, n, p := fixture(t, "2", false)
			switch kind {
			case "ninja_failure":
				n.err = errors.New("synthetic secret")
			case "sui_failure":
				p.inventoryErr = errors.New("synthetic secret")
			case "duplicate_actor":
				n.customers = append(n.customers, invoiceninja.Customer{ID: "other", IDNumber: "42"})
			case "unknown_state":
				n.records[0].StatusID = "1"
			}
			if e := s.Scan(context.Background()); e != nil {
				t.Fatal(e)
			}
			if p.posts != 0 {
				t.Fatal("incomplete/ambiguous state created account")
			}
		})
	}
}
func TestAuthorityPaidDateDoesNotDefineRecurringSchedule(t *testing.T) {
	s, st, n, _ := fixture(t, "2", false)
	n.records[0].NextSendDate = "2040-11-21"
	n.records[0].Amount = "200"
	if e := s.Scan(context.Background()); e != nil {
		t.Fatal(e)
	}
	o := st.observations[42]
	if !o.Enabled || o.NextInvoiceDate != "2040-11-21" {
		t.Fatal("source calendar was replaced")
	}
}
func TestAuthorityRejectsCustomerMovedToDifferentActor(t *testing.T) {
	s, st, n, p := fixture(t, "2", false)
	d, err := sui.NewAuthorityCreateDraft("pp-"+strings.Repeat("a", 40), true, []int64{1})
	if err != nil {
		t.Fatal(err)
	}
	r := draftRecord(d, 7)
	p.records = []sui.ClientRecord{r}
	st.bindings[42] = Binding{CustomerID: "customer-42", SUIID: 7, Name: r.Name(), CredentialsHash: r.CredentialsHash()}
	n.customers[0].IDNumber = "43"
	if err = s.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.posts != 0 || st.observations[43].Actor != 0 || st.observations[42].Enabled {
		t.Fatal("identity moved or duplicate account created")
	}
}
func TestAuthorityFreshInventoryDuplicateIDPreventsDispatch(t *testing.T) {
	s, st, n, p := fixture(t, "2", false)
	p.before = func() {
		duplicate := n.customers[0]
		duplicate.IDNumber = "43"
		n.customers = append(n.customers, duplicate)
	}
	if err := s.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.posts != 0 || st.intents[42].Dispatched {
		t.Fatal("ambiguous fresh inventory authorized creation")
	}
}
func TestAuthorityUnknownIntentDoesNotAdoptSameNameForeignCredentials(t *testing.T) {
	s, st, _, p := fixture(t, "2", false)
	d, err := sui.NewAuthorityCreateDraft("42", true, []int64{1})
	if err != nil {
		t.Fatal(err)
	}
	st.bindings[42] = Binding{CustomerID: "customer-42", Name: d.Name(), CredentialsHash: d.CredentialsHash()}
	foreign, err := sui.NewAuthorityCreateDraft("42", true, []int64{1})
	if err != nil {
		t.Fatal(err)
	}
	p.records = []sui.ClientRecord{draftRecord(foreign, 7)}
	if err = s.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.posts != 0 || st.observations[42].Actor != 0 {
		t.Fatal("unknown intent adopted a foreign same-name account")
	}
}
func TestAuthorityMissingBoundSUIKeepsIdentityAndObservesNinja(t *testing.T) {
	s, st, n, p := fixture(t, "2", false)
	if err := s.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	original := st.bindings[42]
	p.records = nil
	n.records[0].StatusID = "3"
	if err := s.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	o := st.observations[42]
	if p.posts != 1 || o.Binding != original || !o.StateKnown || o.Enabled || o.SUIEnabled {
		t.Fatal("missing binding lost, recreated or blocked Ninja observation")
	}
}
func TestAuthorityMissingCustomerInvalidatesObservation(t *testing.T) {
	s, st, n, p := fixture(t, "2", false)
	if err := s.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	n.customers = nil
	if err := s.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.posts != 1 || st.observations[42].StateKnown || st.observations[42].Enabled || st.observations[42].Binding.SUIID == 0 {
		t.Fatal("disappearance retained access or lost binding")
	}
}
func TestAuthorityReusesPersistedOwnBinding(t *testing.T) {
	s, st, _, p := fixture(t, "3", false)
	name := "pp-" + strings.Repeat("a", 40)
	d, e := sui.NewAuthorityCreateDraft(name, true, []int64{1})
	if e != nil {
		t.Fatal(e)
	}
	r := draftRecord(d, 7)
	p.records = []sui.ClientRecord{r}
	st.bindings[42] = Binding{CustomerID: "customer-42", SUIID: 7, Name: name, CredentialsHash: r.CredentialsHash()}
	if e = s.Scan(context.Background()); e != nil {
		t.Fatal(e)
	}
	if p.posts != 0 || st.observations[42].Binding.Name != name || st.noticeCount != 1 {
		t.Fatal("own binding lost, recreated or silently toggled")
	}
}
