package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/importorigin"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
)

type fakeLifecycleReader struct {
	inventory           []invoiceninja.CycleInvoice
	fresh               invoiceninja.CycleInvoice
	listErr, getErr     error
	listCalls, getCalls int
}

func (r *fakeLifecycleReader) ListInvoices(_ context.Context, customerID string) ([]invoiceninja.CycleInvoice, error) {
	r.listCalls++
	for _, invoice := range r.inventory {
		if invoice.ClientID != customerID {
			return nil, errors.New("foreign invoice in local selection")
		}
	}
	return append([]invoiceninja.CycleInvoice(nil), r.inventory...), r.listErr
}
func (r *fakeLifecycleReader) GetInvoice(_ context.Context, id string) (invoiceninja.CycleInvoice, error) {
	r.getCalls++
	if r.getErr != nil {
		return invoiceninja.CycleInvoice{}, r.getErr
	}
	if r.fresh.ID != id {
		return invoiceninja.CycleInvoice{}, errors.New("unexpected invoice identifier")
	}
	return r.fresh, nil
}

type fakeLifecycleStore struct {
	snapshot   Snapshot
	finished   bool
	invoice    *Invoice
	reviewCode int
	notices    Notices
	deferred   int
}

func (s *fakeLifecycleStore) RecordInvoiceLifecycleWake(context.Context, int64) error { return nil }
func (s *fakeLifecycleStore) SeedInvoiceLifecycleChecks(context.Context, int64, []int64, int) (int, error) {
	return 0, nil
}
func (s *fakeLifecycleStore) ClaimInvoiceLifecycleChecks(context.Context, int64, []int64, int) ([]Claim, error) {
	return nil, nil
}
func (s *fakeLifecycleStore) LoadInvoiceLifecycleSnapshot(context.Context, Claim) (Snapshot, error) {
	return s.snapshot, nil
}
func (s *fakeLifecycleStore) DeferInvoiceLifecycleCheck(context.Context, Claim, time.Duration, int) error {
	s.deferred++
	return nil
}
func (s *fakeLifecycleStore) FinishInvoiceLifecycleCheck(_ context.Context, _ Claim, _ Snapshot, invoice *Invoice, code int, notices Notices, _ time.Duration) error {
	s.finished, s.reviewCode, s.notices = true, code, notices
	if invoice != nil {
		copy := *invoice
		s.invoice = &copy
	}
	return nil
}

func workerConfig() Config {
	return Config{Enabled: true, BaseURL: "https://billing.example.test", Token: "synthetic-api-token", WebhookSecret: "synthetic_lifecycle_webhook_secret_123456", AllowedTelegramIDs: []int64{101}, Workers: 1, PageSize: 50, MaxPages: 20, MaxAccounts: 1000, PollInterval: time.Second, RecheckInterval: 5 * time.Minute, HTTPTimeout: 5 * time.Second}
}

func baseSnapshot() Snapshot {
	return Snapshot{ClientID: 7, TelegramID: 101, ChatID: 101, BotID: 71, CustomerID: "customer-synthetic", FirstInvoiceID: "invoice-first", CurrentInvoiceID: "invoice-first", RecurringID: "recurring-synthetic", SUIID: "71", RegistrationToken: "synthetic_registration_token_123456", ActivationToken: "synthetic_activation_token_123456", State: "active", DesiredAccess: true, ManagedProvenance: true}
}

func cycleInvoice(id, status string) invoiceninja.CycleInvoice {
	return invoiceninja.CycleInvoice{
		ID: id, ClientID: "customer-synthetic", RecurringID: "recurring-synthetic", Number: "SYNTHETIC-2040-002",
		PrivateNotes: activation.Marker("synthetic_activation_token_123456"), StatusID: status,
		Date: "2040-01-01", DueDate: "2040-02-01", Amount: json.Number("100"), Balance: json.Number("100"), PaidToDate: json.Number("0"),
		Invitations: []invoiceninja.CycleInvitation{{ID: "invitation-synthetic", SentDate: "2040-01-03 10:30:00"}},
	}
}

func newWorkerFixture(t *testing.T, reader Reader) (*Worker, *fakeLifecycleStore) {
	t.Helper()
	store := &fakeLifecycleStore{snapshot: baseSnapshot()}
	worker, err := New(store, reader, 71, []int64{9001}, workerConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	worker.now = func() time.Time { return time.Date(2040, 1, 4, 12, 0, 0, 0, time.UTC) }
	return worker, store
}

func runOne(t *testing.T, listed, fresh invoiceninja.CycleInvoice) (*fakeLifecycleStore, *fakeLifecycleReader) {
	t.Helper()
	reader := &fakeLifecycleReader{inventory: []invoiceninja.CycleInvoice{listed}, fresh: fresh}
	worker, store := newWorkerFixture(t, reader)
	worker.handle(context.Background(), Claim{Lease: queue.Lease{ID: 7, Attempt: 1}, BotID: 71, Generation: 0})
	return store, reader
}

func TestPaidBeforeFirstSweepHandsOffWithoutUnpaidNotice(t *testing.T) {
	paid := cycleInvoice("invoice-new", "4")
	paid.Balance, paid.PaidToDate = json.Number("0"), json.Number("100")
	store, reader := runOne(t, paid, paid)
	if !store.finished || store.reviewCode != 0 || store.invoice == nil || store.invoice.Status != "paid" || reader.getCalls != 1 {
		t.Fatalf("paid invoice was not reconciled: finished=%v code=%d invoice=%#v gets=%d", store.finished, store.reviewCode, store.invoice, reader.getCalls)
	}
	if store.notices.Client != nil || len(store.notices.Admins) != 0 {
		t.Fatalf("paid-before-sweep got an unpaid invoice notice: %#v", store.notices)
	}
}

func TestFirstInvoiceAndDraftAreSuppressed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		invoice invoiceninja.CycleInvoice
	}{
		{"first invoice", cycleInvoice("invoice-first", "2")},
		{"draft", cycleInvoice("invoice-draft", "1")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &fakeLifecycleReader{inventory: []invoiceninja.CycleInvoice{tc.invoice}, fresh: tc.invoice}
			worker, store := newWorkerFixture(t, reader)
			worker.handle(context.Background(), Claim{Lease: queue.Lease{ID: 7, Attempt: 1}, BotID: 71})
			if !store.finished || store.invoice != nil || store.reviewCode != 0 || reader.getCalls != 0 {
				t.Fatalf("suppressed invoice was processed: store=%#v gets=%d", store, reader.getCalls)
			}
		})
	}
}

func TestSentInvoiceUsesInvitationDateAndIgnoresInvoiceDateMismatch(t *testing.T) {
	invoice := cycleInvoice("invoice-new", "3")
	invoice.Balance, invoice.PaidToDate = json.Number("40"), json.Number("60")
	invoice.Invitations = append(invoice.Invitations, invoiceninja.CycleInvitation{ID: "invitation-second", SentDate: "2040-01-02T23:30:00Z"})
	store, _ := runOne(t, invoice, invoice)
	if !store.finished || store.invoice == nil || store.invoice.Status != "partial" || store.invoice.SentDate != "2040-01-02" {
		t.Fatalf("wrong lifecycle candidate: %#v", store.invoice)
	}
	if store.notices.Client == nil || len(store.notices.Admins) != 1 || len(store.notices.Review) != 1 {
		t.Fatalf("missing durable notice intents: %#v", store.notices)
	}
}

func TestIncompleteSentDateOrAssociationRequiresReview(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*invoiceninja.CycleInvoice)
		code   int
	}{
		{"missing sent date", func(i *invoiceninja.CycleInvoice) { i.Invitations[0].SentDate = "" }, ReviewSentDate},
		{"wrong marker", func(i *invoiceninja.CycleInvoice) { i.PrivateNotes = "unowned" }, ReviewAssociation},
		{"unknown status", func(i *invoiceninja.CycleInvoice) { i.StatusID = "5" }, ReviewInvoice},
		{"inconsistent balance", func(i *invoiceninja.CycleInvoice) { i.Balance = json.Number("99") }, ReviewBalance},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invoice := cycleInvoice("invoice-new", "2")
			tc.mutate(&invoice)
			store, reader := runOne(t, invoice, invoice)
			if !store.finished || store.reviewCode != tc.code || reader.getCalls != 1 || store.notices.Client != nil || len(store.notices.Review) != 1 {
				t.Fatalf("invalid invoice not held for review: code=%d want=%d notices=%#v", store.reviewCode, tc.code, store.notices)
			}
		})
	}
}

func TestMultipleNewRecurringInvoicesRequireReview(t *testing.T) {
	first, second := cycleInvoice("invoice-new-a", "2"), cycleInvoice("invoice-new-b", "2")
	reader := &fakeLifecycleReader{inventory: []invoiceninja.CycleInvoice{first, second}}
	worker, store := newWorkerFixture(t, reader)
	worker.handle(context.Background(), Claim{Lease: queue.Lease{ID: 7, Attempt: 1}, BotID: 71})
	if !store.finished || store.reviewCode != ReviewCollision || store.notices.Client != nil || reader.getCalls != 0 {
		t.Fatalf("ambiguous inventory was not held: code=%d notice=%#v gets=%d", store.reviewCode, store.notices, reader.getCalls)
	}
}

func TestUnrecordedCurrentInvoiceRequiresReview(t *testing.T) {
	reader := &fakeLifecycleReader{}
	worker, store := newWorkerFixture(t, reader)
	store.snapshot.CurrentInvoiceID = "invoice-unrecorded"
	worker.handle(context.Background(), Claim{Lease: queue.Lease{ID: 7, Attempt: 1}, BotID: 71})
	if !store.finished || store.reviewCode != ReviewAssociation || reader.listCalls != 0 {
		t.Fatalf("unrecorded current invoice was accepted: code=%d lists=%d", store.reviewCode, reader.listCalls)
	}
}

func TestImportedSnapshotAllowsEmptyActivationTokensOnlyWithValidOrigin(t *testing.T) {
	invoice := cycleInvoice("invoice-new", "2")
	invoice.PrivateNotes = "frozen legacy recurring notes"
	reader := &fakeLifecycleReader{inventory: []invoiceninja.CycleInvoice{invoice}, fresh: invoice}
	worker, store := newWorkerFixture(t, reader)
	store.snapshot.ImportedPrivateNotes = "frozen legacy recurring notes"
	store.snapshot.FirstInvoiceID = ""
	store.snapshot.RegistrationToken, store.snapshot.ActivationToken = "", ""
	store.snapshot.SeenInvoiceIDs = []string{"invoice-first"}
	store.snapshot.ImportedOrigin = importorigin.Ref{ClientID: 7, BotID: 71, AdmissionRevision: 1, RuntimeRevision: 1, ConfigurationRevision: 1}
	store.snapshot.ImportedOrigin.StableHash[0] = 1
	store.snapshot.ImportedOrigin.PolicyHash[0] = 1
	store.snapshot.ImportedOrigin.SourceScopeHash[0] = 1
	worker.handle(context.Background(), Claim{Lease: queue.Lease{ID: 7, Attempt: 1}, BotID: 71})
	if !store.finished || store.reviewCode != 0 || store.invoice == nil || reader.listCalls != 1 {
		t.Fatalf("valid imported snapshot was rejected: code=%d invoice=%#v lists=%d", store.reviewCode, store.invoice, reader.listCalls)
	}
}

func TestImportedFreshInvoiceMustMatchFrozenRecurringNotes(t *testing.T) {
	listed := cycleInvoice("invoice-new", "2")
	listed.PrivateNotes = "changed notes"
	reader := &fakeLifecycleReader{inventory: []invoiceninja.CycleInvoice{listed}, fresh: listed}
	worker, store := newWorkerFixture(t, reader)
	store.snapshot.FirstInvoiceID = ""
	store.snapshot.RegistrationToken, store.snapshot.ActivationToken = "", ""
	store.snapshot.SeenInvoiceIDs = []string{"invoice-first"}
	store.snapshot.ImportedPrivateNotes = "frozen legacy recurring notes"
	store.snapshot.ImportedOrigin = importorigin.Ref{ClientID: 7, BotID: 71, AdmissionRevision: 1, RuntimeRevision: 1, ConfigurationRevision: 1}
	store.snapshot.ImportedOrigin.StableHash[0] = 1
	store.snapshot.ImportedOrigin.PolicyHash[0] = 1
	store.snapshot.ImportedOrigin.SourceScopeHash[0] = 1
	worker.handle(context.Background(), Claim{Lease: queue.Lease{ID: 7, Attempt: 1}, BotID: 71})
	if !store.finished || store.reviewCode != ReviewAssociation || reader.getCalls != 1 {
		t.Fatalf("invoice with mismatched frozen recurring notes was accepted: code=%d gets=%d", store.reviewCode, reader.getCalls)
	}
}

func TestImportedSnapshotRejectsPartialReferenceAndMissingCurrentHistory(t *testing.T) {
	for _, missingHistory := range []bool{false, true} {
		reader := &fakeLifecycleReader{}
		worker, store := newWorkerFixture(t, reader)
		store.snapshot.FirstInvoiceID = ""
		store.snapshot.RegistrationToken, store.snapshot.ActivationToken = "", ""
		store.snapshot.ImportedOrigin = importorigin.Ref{ClientID: 7, BotID: 71}
		if missingHistory {
			store.snapshot.ImportedOrigin = importorigin.Ref{ClientID: 7, BotID: 71, AdmissionRevision: 1, RuntimeRevision: 1, ConfigurationRevision: 1}
			store.snapshot.ImportedOrigin.StableHash[0] = 1
			store.snapshot.ImportedOrigin.PolicyHash[0] = 1
			store.snapshot.ImportedOrigin.SourceScopeHash[0] = 1
		}
		worker.handle(context.Background(), Claim{Lease: queue.Lease{ID: 7, Attempt: 1}, BotID: 71})
		if !store.finished || store.reviewCode != ReviewAssociation || reader.listCalls != 0 {
			t.Fatalf("invalid imported reference was accepted: missingHistory=%v code=%d lists=%d", missingHistory, store.reviewCode, reader.listCalls)
		}
	}
}

func TestMissingManagedProvenanceIsReviewedBeforeAPIRead(t *testing.T) {
	reader := &fakeLifecycleReader{}
	worker, store := newWorkerFixture(t, reader)
	store.snapshot.ManagedProvenance = false
	worker.handle(context.Background(), Claim{Lease: queue.Lease{ID: 7, Attempt: 1}, BotID: 71})
	if !store.finished || store.reviewCode != ReviewAssociation || reader.listCalls != 0 || len(store.notices.Review) != 1 {
		t.Fatalf("unproven account was not held for review: code=%d lists=%d notices=%#v", store.reviewCode, reader.listCalls, store.notices.Review)
	}
}

func TestIncompleteInventoryDefersWithoutNotice(t *testing.T) {
	reader := &fakeLifecycleReader{listErr: invoiceninja.ErrCycleLookupIncomplete}
	worker, store := newWorkerFixture(t, reader)
	worker.handle(context.Background(), Claim{Lease: queue.Lease{ID: 7, Attempt: 1}, BotID: 71})
	if store.finished || store.deferred != 1 {
		t.Fatalf("incomplete inventory not deferred: %#v", store)
	}
}
