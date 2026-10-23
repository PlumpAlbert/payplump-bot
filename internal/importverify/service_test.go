package importverify

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/importplan"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

type noCall struct{ t *testing.T }

func (n noCall) FindCustomers(context.Context, int64) ([]invoiceninja.Customer, error) {
	n.t.Fatal("external read before hash rejection")
	return nil, nil
}
func (n noCall) ListRecurring(context.Context, string) ([]invoiceninja.RecurringRecord, error) {
	n.t.Fatal("unexpected read")
	return nil, nil
}

type recNoCall struct{ t *testing.T }

func (n recNoCall) Get(context.Context, string) (invoiceninja.RecurringRecord, error) {
	n.t.Fatal("unexpected read")
	return invoiceninja.RecurringRecord{}, nil
}

type invNoCall struct{ t *testing.T }

func (n invNoCall) ListInvoices(context.Context, string) ([]invoiceninja.CycleInvoice, error) {
	n.t.Fatal("unexpected read")
	return nil, nil
}
func (n invNoCall) GetInvoice(context.Context, string) (invoiceninja.CycleInvoice, error) {
	n.t.Fatal("unexpected read")
	return invoiceninja.CycleInvoice{}, nil
}

type payNoCall struct{ t *testing.T }

func (n payNoCall) GetSettlementCustomer(context.Context, string) (invoiceninja.SettlementCustomer, error) {
	n.t.Fatal("unexpected read")
	return invoiceninja.SettlementCustomer{}, nil
}
func (n payNoCall) GetSettlementInvoice(context.Context, string) (invoiceninja.SettlementInvoice, error) {
	n.t.Fatal("unexpected read")
	return invoiceninja.SettlementInvoice{}, nil
}
func (n payNoCall) ListSettlementPayments(context.Context, string) ([]invoiceninja.SettlementPayment, error) {
	n.t.Fatal("unexpected read")
	return nil, nil
}

type idsNoCall struct{ t *testing.T }

func (n idsNoCall) ListIdentities(context.Context, int) ([]sui.Identity, error) {
	n.t.Fatal("unexpected read")
	return nil, nil
}

type recordsNoCall struct{ t *testing.T }

func (n recordsNoCall) GetRecord(context.Context, int64) (sui.ClientRecord, error) {
	n.t.Fatal("unexpected read")
	return sui.ClientRecord{}, nil
}

type settingsNoCall struct{ t *testing.T }

func (n settingsNoCall) GetSettings(context.Context) (sui.Settings, error) {
	n.t.Fatal("unexpected read")
	return sui.Settings{}, nil
}
func validPolicy() Policy {
	return Policy{BotID: 1, SourceScopeHash: strings.Repeat("a", 64), PilotIDs: []string{"42"}, NinjaVersion: invoiceninja.SupportedRecurringVersion, SUIVersion: "1.6.4", NativeCurrencyID: "1", NativeCurrencyCode: "USD", ProductKey: "legacy", PublicSubscriptionBase: "https://subs.invalid/", ExpectedInboundIDs: []int64{1}, MaxRecords: 10, MaxInventoryRecords: 100, MaxPages: 2, MaxHistoricalAnchors: 10, EvidenceTTL: time.Minute}
}
func TestHashRejectedBeforeAnyProviderRead(t *testing.T) {
	p := validPolicy()
	providers := Providers{Customers: noCall{t}, Recurring: recNoCall{t}, Invoices: invNoCall{t}, Payments: payNoCall{t}, Identities: idsNoCall{t}, Records: recordsNoCall{t}, Settings: settingsNoCall{t}}
	svc, err := New(p, providers)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = svc.Verify(context.Background(), importplan.Plan{}, strings.Repeat("0", 64), time.Now())
	if err == nil {
		t.Fatal("expected digest rejection")
	}
}
func TestEvidenceHashCoversPrivateFacts(t *testing.T) {
	base := PrivateEvidence{SourceScope: strings.Repeat("a", 64), Proof: json.RawMessage(`{"proof":"one"}`), SourceFactsHash: strings.Repeat("b", 64), PaidAnchorInvoiceID: "invoice-1", VerifiedAt: time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)}
	baseline, err := hashEvidence([]PrivateEvidence{base})
	if err != nil {
		t.Fatal(err)
	}
	variants := []PrivateEvidence{base, base, base, base}
	variants[1].Proof = json.RawMessage(`{"proof":"two"}`)
	variants[2].SourceFactsHash = strings.Repeat("c", 64)
	variants[3].PaidAnchorInvoiceID = "invoice-2"
	for i := 1; i < len(variants); i++ {
		got, err := hashEvidence([]PrivateEvidence{variants[i]})
		if err != nil || got == baseline {
			t.Fatalf("private evidence variant %d did not change aggregate hash", i)
		}
	}
	variants[0].VerifiedAt = variants[0].VerifiedAt.Add(time.Second)
	got, err := hashEvidence([]PrivateEvidence{variants[0]})
	if err != nil || got == baseline {
		t.Fatal("verified time did not affect aggregate hash")
	}
}

func TestExclusionWinsOverPilotBeforeProviderRead(t *testing.T) {
	p := validPolicy()
	p.ExcludedIDs = []string{"42"}
	providers := Providers{Customers: noCall{t}, Recurring: recNoCall{t}, Invoices: invNoCall{t}, Payments: payNoCall{t}, Identities: idsNoCall{t}, Records: recordsNoCall{t}, Settings: settingsNoCall{t}}
	svc, err := New(p, providers)
	if err != nil {
		t.Fatal(err)
	}
	plan := importplan.Analyze(importplan.Manifest{Records: []importplan.Record{{TelegramID: "42", InvoiceCustomerID: "customer-1", InvoiceNumber: "42", CurrentInvoiceID: "invoice-1", InvoiceStatus: "paid", RecurringID: "recurring-1", SUIID: "1", SUIName: "42", ConfigHash: strings.Repeat("a", 64), PeriodStart: "2025-01-01", PeriodEnd: "2025-02-01", Currency: "USD", Lifecycle: "active", SourceVersion: "5.13.47", PinnedProvenance: true, CompleteProof: true, UnpaidInvoices: 0, Amount: "10"}}})
	result, _, err := svc.Verify(context.Background(), plan, plan.Hash, time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if result.Verified != 0 || result.Reasons[ReasonExcluded] != 1 {
		t.Fatalf("exclusion did not win: %#v", result)
	}
}

func TestLocalCalendarDateUsesCallerLocation(t *testing.T) {
	local := time.FixedZone("operator-local", -7*60*60)
	now := time.Date(2026, 3, 10, 23, 30, 0, 0, local)
	if got := localCalendarDate(now); got != "2026-03-10" {
		t.Fatalf("local billing date=%s", got)
	}
	if got := now.UTC().Format(time.DateOnly); got != "2026-03-11" {
		t.Fatal("test did not cross UTC date boundary")
	}
}

func TestPolicyRequiresExplicitBoundsAndCurrency(t *testing.T) {
	p := validPolicy()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	p.NativeCurrencyCode = ""
	if p.Validate() == nil {
		t.Fatal("empty currency acknowledgement accepted")
	}
	p = validPolicy()
	p.NativeCurrencyID = "01"
	if p.Validate() == nil {
		t.Fatal("noncanonical native currency ID accepted")
	}
	p = validPolicy()
	p.MaxInventoryRecords = 0
	if p.Validate() == nil {
		t.Fatal("missing inventory bound accepted")
	}
	p = validPolicy()
	p.ExpectedInboundIDs = []int64{2, 1}
	if p.Validate() == nil {
		t.Fatal("noncanonical inbound set accepted")
	}
}
func TestPrivateEvidenceGenericSerializationRedacted(t *testing.T) {
	e := PrivateEvidence{CustomerID: "secret-customer", Proof: json.RawMessage(`{"payment":"secret"}`)}
	for _, value := range []string{e.String(), e.GoString(), string(mustJSON(t, e))} {
		if strings.Contains(value, "secret-customer") || strings.Contains(value, "secret") {
			t.Fatalf("private data leaked: %s", value)
		}
	}
	raw, err := e.PrivateJSON()
	if err != nil || !strings.Contains(string(raw), "secret-customer") {
		t.Fatal("explicit private encoding unavailable")
	}
}
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
