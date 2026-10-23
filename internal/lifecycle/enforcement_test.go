package lifecycle

import (
	"crypto/sha256"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/importorigin"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

func validEnforcementSnapshot() EnforcementSnapshot {
	const token = "synthetic_activation_token_123456"
	return EnforcementSnapshot{
		ClientID: 1, TelegramID: 101, ChatID: 101, BotID: 7,
		CustomerID: "customer-1", FirstInvoiceID: "invoice-first", CurrentInvoiceID: "invoice-overdue", InvoiceID: "invoice-overdue",
		RecurringID: "recurring-1", SUIID: "71", RegistrationToken: "synthetic_registration_token_123456", ActivationToken: token,
		State: "active", DesiredAccess: true, ManagedProvenance: true,
		Invoice: Invoice{ID: "invoice-overdue", Number: "SYNTHETIC-2", Status: "sent", SentDate: "2040-01-01", Amount: "100", Balance: "100", PaidToDate: "0"},
		RecurringDraft: invoiceninja.RecurringDraft{
			ClientID: "customer-1", FrequencyID: 5, NextSendDate: "2040-02-01", DueDateDays: "on_receipt", AutoBill: "off", RemainingCycles: -1,
			PrivateNotes: activation.Marker(token), UsesInclusiveTaxes: false,
			LineItems: []invoiceninja.InvoiceLine{{ProductKey: "product-1", Notes: "subscription", Cost: "100", Quantity: "1", Discount: "0", TypeID: "1", TaxRate1: "0", TaxRate2: "0", TaxRate3: "0"}},
		},
		SUIBinding: sui.Binding{ID: 71, Name: "pp-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		Steps:      make(map[EnforcementStep]OverdueStep),
	}
}

func importedEnforcementTestSnapshot(t *testing.T) EnforcementSnapshot {
	t.Helper()
	s := validEnforcementSnapshot()
	record := overdueTestRecurringRecord(s.RecurringDraft)
	profile, err := invoiceninja.NewImportedRecurringProfile(record, record.LineItems[0].ProductKey)
	if err != nil {
		t.Fatal(err)
	}
	panel, err := overdueTestPanelRecord(true)
	if err != nil {
		t.Fatal(err)
	}
	s.FirstInvoiceID, s.RegistrationToken, s.ActivationToken = "", "", ""
	s.RecurringDraft = invoiceninja.RecurringDraft{}
	s.ImportedOrigin = importorigin.Ref{ClientID: s.ClientID, BotID: s.BotID, AdmissionRevision: 1, RuntimeRevision: 2, ConfigurationRevision: 4, StableHash: sha256.Sum256([]byte("stable")), PolicyHash: sha256.Sum256([]byte("policy")), SourceScopeHash: sha256.Sum256([]byte("scope"))}
	s.ImportedAdmissionPermissionRevision = 3 // Current gate revision can differ from historical origin admission revision.
	s.ImportedEffectsRevision, s.ImportedEffectsAllowed = 3, false
	s.ImportedRecurringProfile, s.ImportedPrivateNotes, s.ImportedInboundIDs = profile, record.PrivateNotes, []int64{1}
	s.SUIBinding.CredentialsHash = panel.CredentialsHash()
	return s
}

func TestImportedOverdueSnapshotValidationIsIndependentOfCurrentGate(t *testing.T) {
	s := importedEnforcementTestSnapshot(t)
	now := time.Date(2040, 1, 2, 12, 0, 0, 0, time.Local)
	if s.ImportedOrigin.AdmissionRevision == s.ImportedAdmissionPermissionRevision {
		t.Fatal("fixture must distinguish historical admission from current permission revision")
	}
	if code := validateEnforcementSnapshot(s, s.BotID, []int64{s.TelegramID}, nil, now); code != 0 {
		t.Fatalf("immutable imported ownership rejected after gate disable: review=%d", code)
	}
	cases := []struct {
		name   string
		mutate func(*EnforcementSnapshot)
	}{
		{"wrong-client-ref", func(v *EnforcementSnapshot) { v.ImportedOrigin.ClientID++ }},
		{"wrong-bot-ref", func(v *EnforcementSnapshot) { v.ImportedOrigin.BotID++ }},
		{"no-admission-permission-revision", func(v *EnforcementSnapshot) { v.ImportedAdmissionPermissionRevision = 0 }},
		{"no-effects-revision", func(v *EnforcementSnapshot) { v.ImportedEffectsRevision = 0 }},

		{"no-profile", func(v *EnforcementSnapshot) { v.ImportedRecurringProfile = invoiceninja.ImportedRecurringProfile{} }},
		{"no-inbounds", func(v *EnforcementSnapshot) { v.ImportedInboundIDs = nil }},
		{"duplicate-inbounds", func(v *EnforcementSnapshot) { v.ImportedInboundIDs = []int64{1, 1} }},
		{"invented-first-invoice", func(v *EnforcementSnapshot) { v.FirstInvoiceID = "fake" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := s
			tc.mutate(&bad)
			if code := validateEnforcementSnapshot(bad, bad.BotID, []int64{bad.TelegramID}, nil, now); code == 0 {
				t.Fatal("invalid imported snapshot accepted")
			}
		})
	}
	if code := validateEnforcementSnapshot(s, s.BotID, nil, nil, now); code != ReviewOverdueAssociation {
		t.Fatalf("missing actor authorization accepted: review=%d", code)
	}
}

func TestOverduePolicyUsesNextLocalCalendarDay(t *testing.T) {
	s := validEnforcementSnapshot()
	allowed := []int64{101}
	midnight := time.Date(2040, 1, 2, 0, 0, 0, 0, time.Local)
	if code := validateEnforcementSnapshot(s, 7, allowed, nil, midnight); code != 0 {
		t.Fatalf("invoice was not overdue at next local midnight: review=%d", code)
	}
	before := midnight.Add(-time.Second)
	if code := validateEnforcementSnapshot(s, 7, allowed, nil, before); code != ReviewOverdueInvoice {
		t.Fatalf("invoice became overdue before next local midnight: review=%d", code)
	}
}

func TestOverduePolicyRejectsPartialAndUnboundInvoices(t *testing.T) {
	s := validEnforcementSnapshot()
	now := time.Date(2040, 1, 2, 12, 0, 0, 0, time.Local)
	s.Invoice.Status = "partial"
	if code := validateEnforcementSnapshot(s, 7, []int64{101}, nil, now); code != ReviewOverdueInvoice {
		t.Fatalf("partial invoice was eligible: review=%d", code)
	}
	s = validEnforcementSnapshot()
	s.SUIBinding.ID = 72
	if code := validateEnforcementSnapshot(s, 7, []int64{101}, nil, now); code != ReviewOverdueAssociation {
		t.Fatalf("mismatched S-UI binding was eligible: review=%d", code)
	}
	s = validEnforcementSnapshot()
	if code := validateEnforcementSnapshot(s, 7, []int64{101}, []int64{101}, now); code != ReviewOverdueAssociation {
		t.Fatalf("excluded pilot account was eligible: review=%d", code)
	}
}

func TestOverdueSequenceRequiresConfirmedPrefix(t *testing.T) {
	steps := map[EnforcementStep]OverdueStep{}
	if got, pending, complete, valid := NextOverdueStep(steps); !valid || pending || complete || got != OverduePauseRecurring {
		t.Fatalf("first step=%q pending=%v complete=%v valid=%v", got, pending, complete, valid)
	}
	steps[OverduePauseRecurring] = OverdueStep{Confirmed: true}
	if got, pending, complete, valid := NextOverdueStep(steps); !valid || pending || complete || got != OverdueDisableSUI {
		t.Fatalf("second step=%q pending=%v complete=%v valid=%v", got, pending, complete, valid)
	}
	steps[OverdueDisableSUI] = OverdueStep{Confirmed: true}
	if got, pending, complete, valid := NextOverdueStep(steps); !valid || pending || complete || got != OverdueCloseSessions {
		t.Fatalf("third step=%q pending=%v complete=%v valid=%v", got, pending, complete, valid)
	}
	steps[OverdueCloseSessions] = OverdueStep{Confirmed: true}
	if _, pending, complete, valid := NextOverdueStep(steps); !valid || pending || !complete {
		t.Fatalf("completed sequence pending=%v complete=%v valid=%v", pending, complete, valid)
	}
	steps[OverdueCloseSessions] = OverdueStep{Confirmed: false}
	if step, pending, complete, valid := NextOverdueStep(steps); !valid || !pending || complete || step != OverdueCloseSessions {
		t.Fatalf("unconfirmed intent=%q pending=%v complete=%v valid=%v", step, pending, complete, valid)
	}
	gap := map[EnforcementStep]OverdueStep{OverdueDisableSUI: {Confirmed: true}}
	if _, _, _, valid := NextOverdueStep(gap); valid {
		t.Fatal("step after missing pause was accepted")
	}
}
