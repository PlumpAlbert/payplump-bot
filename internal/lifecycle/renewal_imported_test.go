package lifecycle

import (
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/importorigin"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

func importedRenewalFixture(t *testing.T) (RenewalSnapshot, invoiceninja.RecurringRecord, sui.ClientRecord) {
	t.Helper()
	_, store, recurring, panel := renewalWorkerFixture(t, false, true)
	s := store.snapshot
	r := recurring.record
	panelRaw := panel.record.Raw()
	var panelFields map[string]any
	if err := json.Unmarshal(panelRaw, &panelFields); err != nil {
		t.Fatal(err)
	}
	panelFields["name"] = "101"
	panelRaw, err := json.Marshal(panelFields)
	if err != nil {
		t.Fatal(err)
	}
	importedPanel, err := sui.ParseClientRecord(panelRaw)
	if err != nil {
		t.Fatal(err)
	}
	s.SUIBinding.Name = importedPanel.Name()
	s.SUIBinding.CredentialsHash = importedPanel.CredentialsHash()
	profile, err := invoiceninja.NewImportedRecurringProfile(r, r.LineItems[0].ProductKey)
	if err != nil {
		t.Fatal(err)
	}
	s.FirstInvoiceID, s.RegistrationToken, s.ActivationToken = "", "", ""
	s.Payment.FirstInvoiceID = ""
	s.ImportedOrigin = importorigin.Ref{
		ClientID: s.ClientID, BotID: s.BotID, AdmissionRevision: 1,
		RuntimeRevision: 2, ConfigurationRevision: 4,
		StableHash:      sha256.Sum256([]byte("stable")),
		PolicyHash:      sha256.Sum256([]byte("policy")),
		SourceScopeHash: sha256.Sum256([]byte("scope")),
	}
	s.ImportedAdmissionPermissionRevision = 3
	s.ImportedEffectsRevision = 4
	s.ImportedEffectsAllowed = true
	s.ImportedRecurringProfile = profile
	s.ImportedPrivateNotes = r.PrivateNotes
	s.ImportedInboundIDs = []int64{1}
	s.RecurringDraft = invoiceninja.RecurringDraft{}
	s.Payment.ImportedOrigin = s.ImportedOrigin
	s.Payment.ImportedBaselineProofHash = [32]byte{}
	return s, r, importedPanel
}

func TestImportedRenewalAssociationRequiresFrozenOrigin(t *testing.T) {
	s, _, _ := importedRenewalFixture(t)
	if !validRenewalAssociation(s, 42, []int64{101}, nil) {
		t.Fatal("valid imported renewal association rejected")
	}
	badCases := []struct {
		name   string
		mutate func(*RenewalSnapshot)
	}{
		{"fake-first-invoice", func(v *RenewalSnapshot) { v.FirstInvoiceID = "invented" }},
		{"first-invoice-payment", func(v *RenewalSnapshot) { v.Payment.FirstInvoiceID = "invented" }},
		{"own-registration-token", func(v *RenewalSnapshot) { v.RegistrationToken = "unexpected" }},
		{"own-activation-token", func(v *RenewalSnapshot) { v.ActivationToken = "unexpected" }},
		{"wrong-origin-client", func(v *RenewalSnapshot) { v.ImportedOrigin.ClientID++ }},
		{"wrong-origin-bot", func(v *RenewalSnapshot) { v.ImportedOrigin.BotID++ }},
		{"missing-profile", func(v *RenewalSnapshot) { v.ImportedRecurringProfile = invoiceninja.ImportedRecurringProfile{} }},
		{"missing-current-admission-revision", func(v *RenewalSnapshot) { v.ImportedAdmissionPermissionRevision = 0 }},
		{"missing-effects-revision", func(v *RenewalSnapshot) { v.ImportedEffectsRevision = 0 }},
		{"duplicate-inbounds", func(v *RenewalSnapshot) { v.ImportedInboundIDs = []int64{1, 1} }},
		{"empty-inbounds", func(v *RenewalSnapshot) { v.ImportedInboundIDs = nil }},
	}
	for _, tc := range badCases {
		t.Run(tc.name, func(t *testing.T) {
			bad := s
			tc.mutate(&bad)
			if validRenewalAssociation(bad, 42, []int64{101}, nil) {
				t.Fatal("invalid imported association accepted")
			}
		})
	}
	// An empty first-invoice field alone does not turn an OWN account into an import.
	own := s
	own.ImportedOrigin = importorigin.Ref{}
	own.Payment.ImportedOrigin = importorigin.Ref{}
	if validRenewalAssociation(own, 42, []int64{101}, nil) {
		t.Fatal("OWN association without first-invoice provenance accepted")
	}
}

func TestImportedRenewalInvoiceMatchesFrozenLegacyNotes(t *testing.T) {
	for _, notes := range []string{"legacy note", ""} {
		t.Run(map[bool]string{true: "empty", false: "legacy"}[notes == ""], func(t *testing.T) {
			s, record, _ := importedRenewalFixture(t)
			record.PrivateNotes = notes
			profile, err := invoiceninja.NewImportedRecurringProfile(record, record.LineItems[0].ProductKey)
			if err != nil {
				t.Fatal(err)
			}
			s.ImportedRecurringProfile = profile
			s.ImportedPrivateNotes = notes
			if !validRenewalAssociation(s, 42, []int64{101}, nil) {
				t.Fatal("association with frozen legacy notes rejected")
			}
			invoice := invoiceninja.CycleInvoice{ID: s.InvoiceID, ClientID: s.CustomerID, RecurringID: s.RecurringID, PrivateNotes: notes, StatusID: "4"}
			if !renewalInvoiceMatches(invoice, s) {
				t.Fatal("invoice matching frozen legacy notes rejected")
			}
			invoice.PrivateNotes = "different"
			if renewalInvoiceMatches(invoice, s) {
				t.Fatal("invoice with changed private notes accepted")
			}
			invoice.PrivateNotes = notes
			invoice.RecurringID = "foreign-recurring"
			if renewalInvoiceMatches(invoice, s) {
				t.Fatal("invoice for foreign recurring record accepted")
			}
			invoice.RecurringID = s.RecurringID
			invoice.ClientID = "foreign-customer"
			if renewalInvoiceMatches(invoice, s) {
				t.Fatal("invoice for foreign customer accepted")
			}
			invoice.ClientID = s.CustomerID
			invoice.StatusID = "2"
			if renewalInvoiceMatches(invoice, s) {
				t.Fatal("non-paid invoice accepted")
			}
		})
	}
}

func TestImportedRenewalExternalAndCompletionState(t *testing.T) {
	s, record, panel := importedRenewalFixture(t)
	if code := validRenewalExternalState(s, record, panel); code != 0 {
		t.Fatalf("valid active imported binding rejected: review=%d", code)
	}

	paused := record
	paused.StatusID = "3"
	if code := validRenewalExternalState(s, paused, panel); code == 0 {
		t.Fatal("manually paused recurring with enabled S-UI and no owned overdue prefix accepted")
	}
	panelRaw := []byte(`{"id":71,"enable":false,"name":"101","config":{},"inbounds":[1],"links":[],"volume":0,"expiry":0,"down":1,"up":1,"desc":"","group":"","remark":"","createdAt":1,"onlineAt":0,"delayStart":false,"autoReset":false,"resetDays":0,"nextReset":0,"totalUp":0,"totalDown":0}`)
	pausedPanel, err := sui.ParseClientRecord(panelRaw)
	if err != nil {
		t.Fatal(err)
	}
	if code := validRenewalExternalState(s, paused, pausedPanel); code == 0 {
		t.Fatal("manually paused recurring with disabled S-UI and no owned overdue prefix accepted")
	}

	changedInboundRaw := []byte(`{"id":71,"enable":true,"name":"101","config":{},"inbounds":[2],"links":[],"volume":0,"expiry":0,"down":1,"up":1,"desc":"","group":"","remark":"","createdAt":1,"onlineAt":0,"delayStart":false,"autoReset":false,"resetDays":0,"nextReset":0,"totalUp":0,"totalDown":0}`)
	changedInbound, err := sui.ParseClientRecord(changedInboundRaw)
	if err != nil {
		t.Fatal(err)
	}
	if changedInbound.CredentialsHash() != panel.CredentialsHash() {
		t.Fatal("fixture should isolate inbound drift from credentials hash")
	}
	if code := validRenewalExternalState(s, record, changedInbound); code == 0 {
		t.Fatal("changed imported inbounds accepted")
	}

	plan := RenewalPlan{TargetDate: "2040-02-28"}
	completed := record
	completed.StatusID = "2"
	completed.NextSendDate = plan.TargetDate
	completed.NextSendDatetime = "2040-02-28T08:00:00Z"
	now := time.Date(2040, time.February, 1, 12, 0, 0, 0, time.UTC)
	if !renewalCompletionState(s, plan, completed, panel, now) {
		t.Fatal("valid future imported renewal completion rejected")
	}
	wrongPrice := completed
	wrongPrice.Amount = "101"
	if renewalCompletionState(s, plan, wrongPrice, panel, now) {
		t.Fatal("completion with changed recurring price accepted")
	}
	if renewalCompletionState(s, plan, completed, changedInbound, now) {
		t.Fatal("completion with changed imported inbounds accepted")
	}
	past := completed
	past.NextSendDatetime = "2040-02-01T11:00:00Z"
	if renewalCompletionState(s, plan, past, panel, now) {
		t.Fatal("completion with elapsed UTC receipt accepted")
	}
	past = completed
	past.NextSendDate = "2040-02-01"
	if renewalCompletionState(s, plan, past, panel, now) {
		t.Fatal("completion with non-future local date accepted")
	}
}
