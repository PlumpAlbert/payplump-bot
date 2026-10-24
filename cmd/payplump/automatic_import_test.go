package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/importadmission"
	"github.com/PlumpAlbert/payplump-bot/internal/importverify"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/postgres"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

type automaticImportFailingInventory struct{ importverify.CustomerReader }

type automaticImportExtraCustomer struct {
	importverify.CustomerReader
	inventory importverify.CustomerInventory
	customer  invoiceninja.Customer
}

type automaticImportToggleInventory struct {
	importverify.CustomerReader
	inventory importverify.CustomerInventory
	empty     bool
}

func (a automaticImportToggleInventory) ListCustomers(ctx context.Context) ([]invoiceninja.Customer, error) {
	if a.empty {
		return []invoiceninja.Customer{}, nil
	}
	return a.inventory.ListCustomers(ctx)
}

type automaticImportToggleIdentities struct {
	importverify.IdentityReader
	empty bool
}

func (a automaticImportToggleIdentities) ListIdentities(ctx context.Context, max int) ([]sui.Identity, error) {
	if a.empty {
		return []sui.Identity{}, nil
	}
	return a.IdentityReader.ListIdentities(ctx, max)
}

func (a automaticImportExtraCustomer) ListCustomers(ctx context.Context) ([]invoiceninja.Customer, error) {
	customers, err := a.inventory.ListCustomers(ctx)
	if err != nil {
		return nil, err
	}
	return append(customers, a.customer), nil
}

func (automaticImportFailingInventory) ListCustomers(context.Context) ([]invoiceninja.Customer, error) {
	return nil, errors.New("synthetic secret response")
}

type automaticImportProviderStubs struct {
	importverify.RecurringReader
	importverify.InvoiceReader
	importverify.PaymentReader
	importverify.IdentityReader
	importverify.RecordReader
	importverify.SettingsReader
}

type automaticImportTestStore struct {
	known        map[int64]bool
	issues       []automaticImportIssue
	resolved     []string
	applyCalls   int
	prepared     []importadmission.PreparedPlan
	issueState   map[string]automaticImportIssue
	notices      []automaticImportIssue
	scanStarted  time.Time
	reconciledAt time.Time
}
type automaticImportIssue struct {
	actor              int64
	key, reason, facts string
	resolved           bool
	updatedAt          time.Time
}

func (s *automaticImportTestStore) AutomaticImportScanStart(context.Context) (time.Time, error) {
	if !s.scanStarted.IsZero() {
		return s.scanStarted, nil
	}
	return time.Now().UTC(), nil
}
func (s *automaticImportTestStore) AutomaticImportKnownActors(context.Context) (map[int64]bool, error) {
	return s.known, nil
}
func (s *automaticImportTestStore) ApplyVerifiedImport(_ context.Context, prepared importadmission.PreparedPlan) (postgres.ImportAdmissionResult, error) {
	s.applyCalls++
	if err := prepared.Validate(); err != nil {
		return postgres.ImportAdmissionResult{}, err
	}
	rows := prepared.Rows()
	if len(rows) != 1 || rows[0].CustomerID != "customer-42" || rows[0].RecurringID != "recurring-42" || rows[0].CurrentInvoiceID != "invoice-42" || len(rows[0].Proof) == 0 {
		return postgres.ImportAdmissionResult{}, errors.New("prepared facts failed test assertion")
	}
	actor, err := strconv.ParseInt(rows[0].TelegramID, 10, 64)
	if err != nil {
		return postgres.ImportAdmissionResult{}, err
	}
	s.known[actor] = true
	s.prepared = append(s.prepared, prepared)
	return postgres.ImportAdmissionResult{Admitted: 1}, nil
}
func (s *automaticImportTestStore) RecordAutomaticImportIssue(_ context.Context, _ int64, actor int64, key, reason, facts string, admins []int64) (bool, error) {
	if len(admins) == 0 && reason != importverify.ReasonSourceFailure {
		return false, errors.New("admin recipients required for candidate issue")
	}
	if s.issueState == nil {
		s.issueState = make(map[string]automaticImportIssue)
	}
	old, exists := s.issueState[key]
	if exists && !old.resolved && old.reason == reason && old.facts == facts {
		return false, nil
	}
	issue := automaticImportIssue{actor: actor, key: key, reason: reason, facts: facts, updatedAt: time.Time{}}
	s.issueState[key] = issue
	s.issues = append(s.issues, issue)
	if len(admins) > 0 {
		s.notices = append(s.notices, issue)
	}
	return true, nil
}
func (s *automaticImportTestStore) ResolveAutomaticImportIssue(_ context.Context, _ int64, key string, started time.Time) error {
	s.resolved = append(s.resolved, key)
	issue, ok := s.issueState[key]
	if ok && issue.updatedAt.Before(started) {
		issue.resolved = true
		s.issueState[key] = issue
	}
	return nil
}
func (s *automaticImportTestStore) ReconcileAutomaticImportIssues(_ context.Context, _ int64, active []string, started time.Time) error {
	s.reconciledAt = started
	keep := make(map[string]bool, len(active))
	for _, key := range active {
		keep[key] = true
	}
	for key, issue := range s.issueState {
		if !issue.resolved && !keep[key] && issue.updatedAt.Before(started) {
			issue.resolved = true
			s.issueState[key] = issue
			s.resolved = append(s.resolved, key)
		}
	}
	return nil
}

func TestAutomaticImporterFailsClosedOnGlobalInventoryFailure(t *testing.T) {
	policy := importverify.Policy{Automatic: true, BotID: 9001, SourceScopeHash: strings.Repeat("a", 64), NinjaVersion: "5.13.47", SUIVersion: "1.6.4", NativeCurrencyID: "1", NativeCurrencyCode: "USD", ProductKey: "legacy", PublicSubscriptionBase: "https://subscriptions.invalid/", ExpectedInboundIDs: []int64{1}, MaxRecords: 20, MaxInventoryRecords: 10, MaxPages: 2, MaxHistoricalAnchors: 2, EvidenceTTL: time.Minute}
	providers := importverify.Providers{Customers: automaticImportFailingInventory{}, Recurring: automaticImportProviderStubs{}, Invoices: automaticImportProviderStubs{}, Payments: automaticImportProviderStubs{}, Identities: automaticImportProviderStubs{}, Records: automaticImportProviderStubs{}, Settings: automaticImportProviderStubs{}}
	verifier, err := importverify.New(policy, providers)
	if err != nil {
		t.Fatal(err)
	}
	store := &automaticImportTestStore{known: map[int64]bool{42: true}}
	worker := &automaticImporter{store: store, verifier: verifier, policy: policy, revision: 1, admins: []int64{9002}, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC) }}
	if _, err := store.RecordAutomaticImportIssue(context.Background(), policy.BotID, 77, worker.key("customer-77"), importverify.ReasonAmbiguous, strings.Repeat("f", 64), []int64{9002}); err != nil {
		t.Fatal(err)
	}
	noticesBefore := len(store.notices)
	if err := worker.Scan(context.Background()); err != nil {
		t.Fatalf("global source failure should be durably reported, not abort scan: %v", err)
	}
	globalIssue := store.issueState[worker.key("complete-inventory")]
	if store.applyCalls != 0 || len(store.issues) != 2 || globalIssue.actor != 0 || globalIssue.reason != importverify.ReasonSourceFailure || len(store.notices) != noticesBefore+1 {
		t.Fatalf("incomplete inventory was treated as candidate-level evidence or duplicate notice: applies=%d issues=%+v notices=%+v", store.applyCalls, store.issues, store.notices)
	}
	if len(store.resolved) != 0 || store.issueState[worker.key("customer-77")].resolved {
		t.Fatalf("incomplete global inventory incorrectly reconciled previous hold: resolved=%v state=%+v", store.resolved, store.issueState)
	}
	if strings.Contains(store.issues[0].facts, "synthetic secret response") {
		t.Fatal("raw provider error leaked into durable facts")
	}
}

func TestAutomaticImporterDiscoversPreparesAdmitsAndSkipsAfterRestart(t *testing.T) {
	f := newImportApplyFixture(t)
	f.automatic.Store(true)
	getenv := func(key string) string { return f.env[key] }
	cfg, err := loadVerifyConfig(getenv)
	if err != nil {
		t.Fatal(err)
	}
	cfg.policy.Automatic = true
	cfg.policy.PilotIDs = nil
	providers, err := newVerifyProviders(cfg)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := invoiceninja.NewCustomerInventoryReader(cfg.ninjaBase, cfg.ninjaToken, cfg.timeout, cfg.pageSize, cfg.maxPages)
	if err != nil {
		t.Fatal(err)
	}
	providers.Customers = inventory
	verifier, err := importverify.New(cfg.policy, providers)
	if err != nil {
		t.Fatal(err)
	}
	store := &automaticImportTestStore{known: map[int64]bool{}}
	worker := &automaticImporter{store: store, verifier: verifier, policy: cfg.policy, revision: 7, admins: []int64{9002}, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return f.now }}
	if err := worker.Scan(context.Background()); err != nil {
		t.Fatalf("automatic discovery/admission: %v", err)
	}
	if store.applyCalls != 1 || len(store.prepared) != 1 || len(store.issues) != 0 || !store.known[42] {
		t.Fatalf("unexpected first scan: apply=%d prepared=%d issues=%+v known=%v", store.applyCalls, len(store.prepared), store.issues, store.known)
	}
	if p := store.prepared[0]; p.BotID() != 9 || p.Revision() != 7 || p.RowCount() != 1 || p.Rows()[0].TelegramID != "42" {
		t.Fatalf("invalid opaque prepared plan reached persistence boundary: bot=%d rev=%d rows=%+v", p.BotID(), p.Revision(), p.Rows())
	}
	before := f.GETs.Load()
	// A newly constructed worker models process restart; durable known actors
	// are read again and prevent re-import or historical-notice generation.
	restarted := &automaticImporter{store: store, verifier: verifier, policy: cfg.policy, revision: 7, admins: []int64{9002}, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return f.now }}
	if err := restarted.Scan(context.Background()); err != nil {
		t.Fatalf("scan after restart: %v", err)
	}
	if store.applyCalls != 1 || len(store.issues) != 0 || f.GETs.Load() <= before {
		t.Fatalf("restart repeated import or skipped bounded source scan: applies=%d issues=%d GETs=%d->%d", store.applyCalls, len(store.issues), before, f.GETs.Load())
	}
}

func TestAutomaticImporterProcessesGoodCandidateBesideBadCandidate(t *testing.T) {
	f := newImportApplyFixture(t)
	f.automatic.Store(true)
	cfg, err := loadVerifyConfig(func(key string) string { return f.env[key] })
	if err != nil {
		t.Fatal(err)
	}
	cfg.policy.Automatic = true
	cfg.policy.PilotIDs = nil
	providers, err := newVerifyProviders(cfg)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := invoiceninja.NewCustomerInventoryReader(cfg.ninjaBase, cfg.ninjaToken, cfg.timeout, cfg.pageSize, cfg.maxPages)
	if err != nil {
		t.Fatal(err)
	}
	providers.Customers = automaticImportExtraCustomer{CustomerReader: inventory, inventory: inventory, customer: invoiceninja.Customer{ID: "customer-77", IDNumber: "77"}}
	verifier, err := importverify.New(cfg.policy, providers)
	if err != nil {
		t.Fatal(err)
	}
	store := &automaticImportTestStore{known: map[int64]bool{}}
	worker := &automaticImporter{store: store, verifier: verifier, policy: cfg.policy, revision: 7, admins: []int64{9002}, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return f.now }}
	if err := worker.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.applyCalls != 1 || !store.known[42] || len(store.issues) != 1 || store.issues[0].actor != 77 || store.issues[0].reason != importverify.ReasonAmbiguous {
		t.Fatalf("bad candidate blocked valid one or escaped reporting: applies=%d known=%v issues=%+v", store.applyCalls, store.known, store.issues)
	}
}

func TestAutomaticImporterReconcilesCandidateRemovedFromBothInventories(t *testing.T) {
	f := newImportApplyFixture(t)
	f.automatic.Store(true)
	cfg, err := loadVerifyConfig(func(key string) string { return f.env[key] })
	if err != nil {
		t.Fatal(err)
	}
	cfg.policy.Automatic = true
	cfg.policy.PilotIDs = nil
	providers, err := newVerifyProviders(cfg)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := invoiceninja.NewCustomerInventoryReader(cfg.ninjaBase, cfg.ninjaToken, cfg.timeout, cfg.pageSize, cfg.maxPages)
	if err != nil {
		t.Fatal(err)
	}
	providers.Customers = automaticImportToggleInventory{CustomerReader: inventory, inventory: inventory, empty: true}
	providers.Identities = automaticImportToggleIdentities{IdentityReader: providers.Identities, empty: true}
	verifier, err := importverify.New(cfg.policy, providers)
	if err != nil {
		t.Fatal(err)
	}
	store := &automaticImportTestStore{known: map[int64]bool{}}
	worker := &automaticImporter{store: store, verifier: verifier, policy: cfg.policy, revision: 7, admins: []int64{9002}, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return f.now }}
	// Deliberately skew the application clock ahead of the database. A newer
	// concurrent issue must survive even when absent from this older snapshot.
	store.scanStarted = f.now
	worker.now = func() time.Time { return f.now.Add(time.Hour) }
	key := worker.key("customer-77")
	newer := worker.key("customer-78")
	store.issueState = map[string]automaticImportIssue{newer: {actor: 78, key: newer, reason: importverify.ReasonAmbiguous, updatedAt: f.now.Add(time.Minute)}}
	if _, err := store.RecordAutomaticImportIssue(context.Background(), cfg.policy.BotID, 77, key, importverify.ReasonAmbiguous, strings.Repeat("f", 64), []int64{9002}); err != nil {
		t.Fatal(err)
	}
	if err := worker.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	issue := store.issueState[key]
	if !issue.resolved || store.applyCalls != 0 {
		t.Fatalf("completed empty inventory did not release removed candidate hold: issue=%+v applies=%d", issue, store.applyCalls)
	}
	if !store.reconciledAt.Equal(store.scanStarted) || store.issueState[newer].resolved {
		t.Fatal("application clock skew cleared a newer database issue")
	}
}

func TestAutomaticImporterRetriesCorrectedCandidate(t *testing.T) {
	f := newImportApplyFixture(t)
	f.automatic.Store(true)
	cfg, err := loadVerifyConfig(func(key string) string { return f.env[key] })
	if err != nil {
		t.Fatal(err)
	}
	cfg.policy.Automatic = true
	cfg.policy.PilotIDs = nil
	providers, err := newVerifyProviders(cfg)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := invoiceninja.NewCustomerInventoryReader(cfg.ninjaBase, cfg.ninjaToken, cfg.timeout, cfg.pageSize, cfg.maxPages)
	if err != nil {
		t.Fatal(err)
	}
	providers.Customers = inventory
	verifier, err := importverify.New(cfg.policy, providers)
	if err != nil {
		t.Fatal(err)
	}
	store := &automaticImportTestStore{known: map[int64]bool{}}
	worker := &automaticImporter{store: store, verifier: verifier, policy: cfg.policy, revision: 7, admins: []int64{9002}, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return f.now }}
	f.fault.Store(1) // complete inventory, candidate-specific payment API failure
	if err := worker.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.applyCalls != 0 || len(store.issues) != 2 || len(store.notices) != 1 || store.issueState[worker.key("customer-42")].reason != importverify.ReasonSourceFailure || store.issueState[worker.key("customer-42")].actor != 42 || store.issueState[worker.key("complete-inventory")].actor != 0 {
		t.Fatalf("source failure should persist per-actor hold and one global notice: applies=%d issues=%+v notices=%+v", store.applyCalls, store.issues, store.notices)
	}
	// Restart during continued outage: durable issue identity suppresses another
	// per-actor or global administrator notice.
	restarted := &automaticImporter{store: store, verifier: verifier, policy: cfg.policy, revision: 7, admins: []int64{9002}, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return f.now }}
	if err := restarted.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.notices) != 1 || len(store.issues) != 2 {
		t.Fatalf("restarted outage scan spammed administrators: issues=%+v notices=%+v", store.issues, store.notices)
	}
	f.fault.Store(0) // corrected provider availability; fresh discovery and Prepare must pass
	if err := worker.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.applyCalls != 1 || !store.known[42] || len(store.notices) != 1 || !store.issueState[worker.key("customer-42")].resolved || !store.issueState[worker.key("complete-inventory")].resolved {
		t.Fatalf("corrected candidate was not admitted/resolved without duplicate notice: applies=%d known=%v issues=%+v notices=%+v", store.applyCalls, store.known, store.issues, store.notices)
	}
	resolvedCandidate := false
	for _, key := range store.resolved {
		if key == worker.key("customer-42") {
			resolvedCandidate = true
		}
	}
	if !resolvedCandidate {
		t.Fatalf("candidate issue not resolved after successful admission: %v", store.resolved)
	}
}

func TestAutomaticImporterUsesSourceScopedStableIssueKey(t *testing.T) {
	policy := importverify.Policy{SourceScopeHash: strings.Repeat("a", 64)}
	worker := &automaticImporter{policy: policy}
	first := worker.key("customer-42")
	if len(first) != 64 || first != worker.key("customer-42") || first == worker.key("customer-43") {
		t.Fatalf("candidate issue key is not stable and distinct: %q", first)
	}
	policy.SourceScopeHash = strings.Repeat("b", 64)
	worker.policy = policy
	if first == worker.key("customer-42") {
		t.Fatal("issue key did not change with source scope")
	}
}

type cancellingAutomaticImportStore struct {
	*automaticImportTestStore
	cancel context.CancelFunc
}

func (s *cancellingAutomaticImportStore) RecordAutomaticImportIssue(ctx context.Context, bot, actor int64, key, reason, facts string, admins []int64) (bool, error) {
	changed, err := s.automaticImportTestStore.RecordAutomaticImportIssue(ctx, bot, actor, key, reason, facts, admins)
	s.cancel()
	return changed, err
}

func TestAutomaticImporterRunJoinsCancellationAfterReportingSourceFailure(t *testing.T) {
	policy := importverify.Policy{Automatic: true, BotID: 9001, SourceScopeHash: strings.Repeat("a", 64), NinjaVersion: "5.13.47", SUIVersion: "1.6.4", NativeCurrencyID: "1", NativeCurrencyCode: "USD", ProductKey: "monthly", PublicSubscriptionBase: "https://subs.invalid/", ExpectedInboundIDs: []int64{1}, MaxRecords: 20, MaxInventoryRecords: 10, MaxPages: 2, MaxHistoricalAnchors: 2, EvidenceTTL: time.Minute}
	stubs := automaticImportProviderStubs{}
	verifier, err := importverify.New(policy, importverify.Providers{Customers: automaticImportFailingInventory{}, Recurring: stubs, Invoices: stubs, Payments: stubs, Identities: stubs, Records: stubs, Settings: stubs})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &cancellingAutomaticImportStore{automaticImportTestStore: &automaticImportTestStore{known: map[int64]bool{}}, cancel: cancel}
	worker := &automaticImporter{store: store, verifier: verifier, policy: policy, revision: 1, admins: []int64{9002}, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), now: time.Now}
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("automatic importer did not join cancellation")
	}
	if len(store.notices) != 1 || store.applyCalls != 0 {
		t.Fatal("shutdown replayed work or lost the source-failure notification")
	}
}

// Keep compile-time assertions next to the fakes: service providers are read-only.
var _ importverify.CustomerInventory = automaticImportFailingInventory{}
var _ importverify.CustomerReader = automaticImportFailingInventory{}
var _ importverify.IdentityReader = automaticImportProviderStubs{}
var _ payments.Reader = automaticImportProviderStubs{}
