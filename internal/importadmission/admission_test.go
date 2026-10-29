package importadmission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/importplan"
	"github.com/PlumpAlbert/payplump-bot/internal/importverify"
	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
)

func TestPreparedPlanIsOpaqueAndZeroValueInvalid(t *testing.T) {
	var p PreparedPlan
	if err := p.Validate(); err == nil {
		t.Fatal("zero value must not validate")
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"\u003cprepared verified import\u003e"` {
		t.Fatalf("unexpected serialization: %s", b)
	}
	if p.String() != "<prepared verified import>" {
		t.Fatal("unexpected string representation")
	}
}

func TestPreparedPlanDetachedCopiesAndPrivateRendering(t *testing.T) {
	plan := importplan.Analyze(importplan.Manifest{Records: []importplan.Record{{
		TelegramID: "42", InvoiceCustomerID: "customer-private", InvoiceNumber: "42",
		CurrentInvoiceID: "invoice-private", InvoiceStatus: "paid", RecurringID: "recurring-private",
		SUIID: "42", SUIName: "42", ConfigHash: strings.Repeat("a", 64),
		PeriodStart: "2026-03-01", PeriodEnd: "2026-04-01", Currency: "USD", Lifecycle: "active",
		SourceVersion: "5.13.47", PinnedProvenance: true, CompleteProof: true, Amount: "100",
	}}})
	verified := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	row := AdmissionRow{
		SourceIndex: 0, TelegramID: "42", CustomerID: "customer-private", RecurringID: "recurring-private",
		CurrentInvoiceID: "invoice-private", PaidAnchorInvoiceID: "anchor-private", SUIID: 42,
		SUIName: "42", CredentialHash: strings.Repeat("a", 64), SourceFactsHash: strings.Repeat("b", 64),
		Lifecycle: "active", PaymentDate: "2026-03-01", NextInvoiceDate: "2026-04-01", Currency: "USD", Amount: "100",
		Template: json.RawMessage(`{"private_notes":"secret-template"}`), Proof: json.RawMessage(`{"private":"secret-proof"}`),
		HistoricalInvoiceIDs: []string{"invoice-private"},
		StableHash:           strings.Repeat("c", 64),
	}
	row.Evidence = evidenceFixture(plan.Hash, row, 9, strings.Repeat("d", 64), strings.Repeat("e", 64), verified, verified.Add(5*time.Minute))
	p := PreparedPlan{botID: 9, revision: 3, planHash: plan.Hash, policyHash: strings.Repeat("d", 64), sourceScopeHash: strings.Repeat("e", 64), verifiedAt: verified, notAfter: verified.Add(5 * time.Minute), plan: clonePlan(plan), rows: []AdmissionRow{row}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}

	gotPlan := p.Plan()
	gotPlan.Candidates[0].Facts.InvoiceCustomerID = "mutated"
	gotPlan.Reasons["mutated"] = 1
	if p.Plan().Candidates[0].Facts.InvoiceCustomerID != "customer-private" || p.Plan().Reasons["mutated"] != 0 {
		t.Fatal("Plan returned mutable backing data")
	}
	if err := p.Plan().ValidateHash(plan.Hash); err != nil {
		t.Fatalf("detached plan lost its hash metadata: %v", err)
	}

	rows := p.Rows()
	rows[0].Template[0] = 'x'
	rows[0].Proof[0] = 'x'
	rows[0].Evidence[0] = 'x'
	rows[0].HistoricalInvoiceIDs[0] = "mutated"
	if string(p.Rows()[0].Template) != string(row.Template) || string(p.Rows()[0].Proof) != string(row.Proof) || string(p.Rows()[0].Evidence) != string(row.Evidence) || p.Rows()[0].HistoricalInvoiceIDs[0] != "invoice-private" {
		t.Fatal("Rows returned mutable backing data")
	}

	for _, public := range []string{fmt.Sprint(row), fmt.Sprintf("%#v", row), string(mustJSON(t, row)), slogValueString(row)} {
		for _, secret := range []string{"customer-private", "invoice-private", "secret-template", "secret-proof", "secret-evidence"} {
			if strings.Contains(public, secret) {
				t.Fatalf("row public rendering disclosed %q: %s", secret, public)
			}
		}
	}
}

func TestStableAdmissionHashBindsIdentityAndPolicyRevisionOnly(t *testing.T) {
	base := stableAdmissionHash(strings.Repeat("a", 64), 9, 4, 2, "scope", "policy", "customer", "recurring", "current", "anchor", 42, "42", "credential", "active", "100", "2026-03-01", "2026-04-01", json.RawMessage(`{"template":true}`), json.RawMessage(`{"proof":true}`))
	if !digest.MatchString(base) {
		t.Fatal("stable admission hash is not a digest")
	}
	changedRevision := stableAdmissionHash(strings.Repeat("a", 64), 9, 5, 2, "scope", "policy", "customer", "recurring", "current", "anchor", 42, "42", "credential", "active", "100", "2026-03-01", "2026-04-01", json.RawMessage(`{"template":true}`), json.RawMessage(`{"proof":true}`))
	changedBot := stableAdmissionHash(strings.Repeat("a", 64), 10, 4, 2, "scope", "policy", "customer", "recurring", "current", "anchor", 42, "42", "credential", "active", "100", "2026-03-01", "2026-04-01", json.RawMessage(`{"template":true}`), json.RawMessage(`{"proof":true}`))
	if base == changedRevision || base == changedBot {
		t.Fatal("stable hash does not bind bot and policy revision")
	}
	// Verification time, TTL and source-facts inventory hash are not hash inputs.
	repeated := stableAdmissionHash(strings.Repeat("a", 64), 9, 4, 2, "scope", "policy", "customer", "recurring", "current", "anchor", 42, "42", "credential", "active", "100", "2026-03-01", "2026-04-01", json.RawMessage(`{"template":true}`), json.RawMessage(`{"proof":true}`))
	if repeated != base {
		t.Fatal("stable hash depends on volatile verification metadata")
	}
}

func TestPreparedValidationRejectsMismatchedOrStaleMetadata(t *testing.T) {
	p := preparedFixture(t)
	p.plan.Candidates[0].Facts.Amount = "101"
	if err := p.Validate(); err == nil {
		t.Fatal("accepted plan/row metadata mismatch")
	}
	p = preparedFixture(t)
	p.notAfter = p.verifiedAt
	if err := p.Validate(); err == nil {
		t.Fatal("accepted empty evidence validity interval")
	}
	p = preparedFixture(t)
	p.rows[0].SourceFactsHash = "not-a-digest"
	if err := p.Validate(); err == nil {
		t.Fatal("accepted invalid source facts digest")
	}
}

func preparedFixture(t *testing.T) PreparedPlan {
	t.Helper()
	plan := importplan.Analyze(importplan.Manifest{Records: []importplan.Record{{
		TelegramID: "42", InvoiceCustomerID: "customer-private", InvoiceNumber: "42", CurrentInvoiceID: "invoice-private",
		InvoiceStatus: "paid", RecurringID: "recurring-private", SUIID: "42", SUIName: "42", ConfigHash: strings.Repeat("a", 64),
		PeriodStart: "2026-03-01", PeriodEnd: "2026-04-01", Currency: "USD", Lifecycle: "active", SourceVersion: "5.13.47",
		PinnedProvenance: true, CompleteProof: true, Amount: "100",
	}}})
	verified := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	row := AdmissionRow{
		SourceIndex: 0, TelegramID: "42", CustomerID: "customer-private", RecurringID: "recurring-private", CurrentInvoiceID: "invoice-private", PaidAnchorInvoiceID: "anchor-private",
		SUIID: 42, CredentialHash: strings.Repeat("a", 64), SourceFactsHash: strings.Repeat("b", 64), Lifecycle: "active", PaymentDate: "2026-03-01",
		NextInvoiceDate: "2026-04-01", Currency: "USD", Amount: "100", Template: json.RawMessage(`{"template":true}`), Proof: json.RawMessage(`{"proof":true}`),
		StableHash: strings.Repeat("c", 64),
	}
	row.Evidence = evidenceFixture(plan.Hash, row, 9, strings.Repeat("d", 64), strings.Repeat("e", 64), verified, verified.Add(5*time.Minute))
	return PreparedPlan{botID: 9, revision: 3, planHash: plan.Hash, policyHash: strings.Repeat("d", 64), sourceScopeHash: strings.Repeat("e", 64), verifiedAt: verified, notAfter: verified.Add(5 * time.Minute), plan: clonePlan(plan), rows: []AdmissionRow{row}}
}

func evidenceFixture(planHash string, row AdmissionRow, botID int64, policy, scope string, verified, notAfter time.Time) json.RawMessage {
	private := importverify.PrivateEvidence{BotID: botID, PlanHash: planHash, CandidateIndex: row.SourceIndex, PolicyHash: policy, SourceScope: scope, VerifiedAt: verified, NotAfter: notAfter, CustomerID: row.CustomerID, RecurringID: row.RecurringID, CurrentInvoiceID: row.CurrentInvoiceID, PaidAnchorInvoiceID: row.PaidAnchorInvoiceID, SUIID: row.SUIID, SUIName: row.SUIName, ConfigHash: row.CredentialHash, SourceFactsHash: row.SourceFactsHash, Lifecycle: row.Lifecycle, CurrentInvoiceStatus: row.CurrentInvoiceStatus, DebtAmount: row.DebtAmount, InvoiceWatermark: row.InvoiceWatermark, HistoricalInvoiceIDs: row.HistoricalInvoiceIDs, LegacyRecurring: row.Template, Proof: row.Proof}
	encoded, err := private.PrivateJSON()
	if err != nil {
		panic(err)
	}
	return encoded
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func slogValueString(row AdmissionRow) string {
	return row.LogValue().String()
}

// Only the first GET may be reached. Promoted nil readers panic if preparation
// proceeds past this source failure, so no downstream read or write is possible.
type rejectedAdmissionReaders struct {
	importverify.CustomerReader
	importverify.RecurringReader
	importverify.InvoiceReader
	importverify.PaymentReader
	importverify.IdentityReader
	importverify.RecordReader
	importverify.SettingsReader
	reads int
}

func (r *rejectedAdmissionReaders) FindCustomers(context.Context, int64) ([]invoiceninja.Customer, error) {
	r.reads++
	return nil, fmt.Errorf("secret-token https://private.invalid/customer-private: %w", &integrationhttp.HTTPError{StatusCode: 403})
}

func TestPreparePreservesSafeVerificationDiagnostic(t *testing.T) {
	fixture := preparedFixture(t)
	policy := importverify.Policy{
		BotID: 9, SourceScopeHash: strings.Repeat("a", 64), PilotIDs: []string{"42"},
		NinjaVersion: invoiceninja.SupportedRecurringVersion, SUIVersion: "1.6.4",
		NativeCurrencyID: "1", NativeCurrencyCode: "USD", ProductKey: "legacy",
		PublicSubscriptionBase: "https://subs.invalid/", ExpectedInboundIDs: []int64{1},
		MaxRecords: 10, MaxInventoryRecords: 100, MaxPages: 2, MaxHistoricalAnchors: 10, EvidenceTTL: time.Minute,
	}
	readers := &rejectedAdmissionReaders{}
	service, err := importverify.New(policy, importverify.Providers{
		Customers: readers, Recurring: readers, Invoices: readers, Payments: readers,
		Identities: readers, Records: readers, Settings: readers,
	})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := Prepare(context.Background(), service, fixture.Plan(), fixture.PlanHash(), 1, fixture.VerifiedAt())
	var failure VerificationFailure
	if !errors.As(err, &failure) || prepared.RowCount() != 0 || readers.reads != 1 || failure.Reasons[importverify.ReasonSourceFailure] != 1 {
		t.Fatal("preparation did not fail closed with the original reason", err)
	}
	want := importverify.Diagnostic{Stage: "customers", Check: "read", Category: "http", HTTPStatus: 403}
	if len(failure.Diagnostics) != 1 || failure.Diagnostics[0] != want {
		t.Fatalf("preparation lost GET failure diagnostic: %+v", failure.Diagnostics)
	}
	for _, public := range []string{err.Error(), fmt.Sprint(failure), fmt.Sprintf("%#v", failure), string(mustJSON(t, failure)), fmt.Sprint(failure.Diagnostics)} {
		for _, secret := range []string{"secret-token", "private.invalid", "customer-private"} {
			if strings.Contains(public, secret) {
				t.Fatalf("verification failure disclosed source data: %s", public)
			}
		}
	}
}
