package lifecycle

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

func importedInitialRenewalFixture(t *testing.T) (RenewalSnapshot, invoiceninja.RecurringRecord, sui.ClientRecord) {
	t.Helper()
	s, record, panel := importedRenewalFixture(t)
	s.State, s.DesiredAccess = "overdue", false
	s.Payment.State, s.Payment.DesiredAccess = "overdue", false
	s.ImportedInitialPaused = true
	s.ImportedInitialDebt = invoiceninja.CycleInvoice{
		ID: s.InvoiceID, ClientID: s.CustomerID, RecurringID: s.RecurringID,
		Number: s.Proof.InvoiceNumber, PrivateNotes: "historic debt notes", StatusID: "2",
		Date: "2040-01-01", Amount: "100", Balance: "100", PaidToDate: "0",
	}
	s.ImportedPaidAnchorInvoiceID = "historical-paid-anchor"
	s.LedgerCustomerID, s.LedgerRecurringID, s.LedgerInvoiceNumber, s.LedgerStatus = "", "", "", ""
	s.LedgerAmount, s.LedgerBalance, s.LedgerPaidToDate, s.LedgerSentDate = "", "", "", ""
	record.StatusID = "3"
	panelRaw, err := json.Marshal(map[string]any{
		"id": panel.ID(), "enable": false, "name": panel.Name(), "config": map[string]any{},
		"inbounds": []int64{1}, "links": []any{}, "volume": 0, "expiry": 0,
		"down": 1, "up": 1, "desc": "", "group": "", "remark": "", "createdAt": 1,
		"onlineAt": 0, "delayStart": false, "autoReset": false, "resetDays": 0,
		"nextReset": 0, "totalUp": 0, "totalDown": 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	panel, err = sui.ParseClientRecord(panelRaw)
	if err != nil {
		t.Fatal(err)
	}
	return s, record, panel
}

func TestImportedInitialPausedDebtMustBeCurrentUnpaidDebtNotAnchor(t *testing.T) {
	s, _, _ := importedInitialRenewalFixture(t)
	if !ValidImportedInitialAuthority(s) || !validRenewalAssociation(s, 42, []int64{101}, nil) {
		t.Fatal("valid frozen current debt was rejected")
	}
	paidInvoice := invoiceninja.CycleInvoice{ID: s.InvoiceID, ClientID: s.CustomerID, RecurringID: s.RecurringID, Number: s.Proof.InvoiceNumber, PrivateNotes: s.ImportedInitialDebt.PrivateNotes, StatusID: "4", Date: s.ImportedInitialDebt.Date, Amount: "100", Balance: "0", PaidToDate: "100"}
	if !renewalInvoiceMatches(paidInvoice, s) {
		t.Fatal("fresh paid current-debt invoice with its frozen legacy notes rejected")
	}
	paidInvoice.PrivateNotes = s.ImportedPrivateNotes
	if renewalInvoiceMatches(paidInvoice, s) {
		t.Fatal("recurring-template notes incorrectly replaced current-debt invoice notes")
	}
	badCases := []struct {
		name   string
		mutate func(*RenewalSnapshot)
	}{
		{"historical-anchor-as-debt", func(v *RenewalSnapshot) { v.ImportedInitialDebt.ID = v.ImportedPaidAnchorInvoiceID }},
		{"foreign-debt-client", func(v *RenewalSnapshot) { v.ImportedInitialDebt.ClientID = "foreign" }},
		{"wrong-recurring", func(v *RenewalSnapshot) { v.ImportedInitialDebt.RecurringID = "foreign" }},
		{"paid-baseline", func(v *RenewalSnapshot) {
			v.ImportedInitialDebt.PaidToDate = "100"
			v.ImportedInitialDebt.Balance = "0"
		}},
		{"wrong-baseline-amount", func(v *RenewalSnapshot) { v.ImportedInitialDebt.Amount = "99" }},
		{"wrong-current-debt", func(v *RenewalSnapshot) { v.ImportedInitialDebt.ID = "other" }},
		{"ledger-fabrication", func(v *RenewalSnapshot) { v.LedgerStatus = "paid" }},
		{"missing-anchor", func(v *RenewalSnapshot) { v.ImportedPaidAnchorInvoiceID = "" }},
		{"foreign-current-proof", func(v *RenewalSnapshot) { v.Proof.InvoiceID = "foreign" }},
	}
	for _, tc := range badCases {
		t.Run(tc.name, func(t *testing.T) {
			bad := s
			tc.mutate(&bad)
			if ValidImportedInitialAuthority(bad) || validRenewalAssociation(bad, 42, []int64{101}, nil) {
				t.Fatal("invalid initial authority accepted")
			}
		})
	}
}

func TestImportedInitialPausedRestorationAllowsEmptyOrPastScheduleBaseline(t *testing.T) {
	s, record, panel := importedInitialRenewalFixture(t)
	if code := validRenewalExternalState(s, record, panel); code != 0 {
		t.Fatalf("verified frozen source rejected: %d", code)
	}
	changedRecord := record
	changedRecord.PrivateNotes = "changed legacy source notes"
	if code := validRenewalExternalState(s, changedRecord, panel); code == 0 {
		t.Fatal("source recurring notes drift accepted")
	}
	for _, field := range []string{"inbounds", "config"} {
		var raw map[string]any
		if err := json.Unmarshal(panel.Raw(), &raw); err != nil {
			t.Fatal(err)
		}
		if field == "inbounds" {
			raw[field] = []int64{2}
		} else {
			raw[field] = map[string]any{"vless": map[string]any{"uuid": "changed"}}
		}
		changedRaw, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		changedPanel, err := sui.ParseClientRecord(changedRaw)
		if err != nil {
			t.Fatal(err)
		}
		if code := validRenewalExternalState(s, record, changedPanel); code == 0 {
			t.Fatalf("source %s drift accepted", field)
		}
	}
	now := time.Date(2040, time.February, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ name, date, datetime string }{
		{"empty", "", ""},
		{"past", "2040-01-15", "2040-01-15T08:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := record
			candidate.NextSendDate, candidate.NextSendDatetime = tc.date, tc.datetime
			if code := validRenewalExternalState(s, candidate, panel); code != 0 {
				t.Fatalf("verified initial paused source rejected: %d", code)
			}
			plan, _, code := makeRenewalPlan(s, candidate, panel, now)
			if code != 0 || !plan.Schedule || plan.Pause || !plan.Enable || !plan.Resume || plan.TargetDate != "2040-02-29" {
				t.Fatalf("unexpected restoration plan: %+v review=%d", plan, code)
			}
		})
	}
	if _, _, code := makeRenewalPlan(s, record, panel, time.Date(2040, time.March, 1, 0, 0, 0, 0, time.UTC)); code != ReviewRenewalTarget {
		t.Fatalf("expired payment-derived target was not held: %d", code)
	}
}

func TestImportedInitialModeDoesNotAuthorizeManualPausedRenewal(t *testing.T) {
	s, record, panel := importedInitialRenewalFixture(t)
	s.ImportedInitialPaused = false
	s.ImportedInitialDebt = invoiceninja.CycleInvoice{}
	if code := validRenewalExternalState(s, record, panel); code == 0 {
		t.Fatal("manual paused imported source without own overdue prefix accepted")
	}
	activeOrigin := s
	activeOrigin.ImportedInitialPaused = true
	activeOrigin.State = "active"
	activeOrigin.DesiredAccess = true
	activeOrigin.Payment.State = "active"
	activeOrigin.Payment.DesiredAccess = true
	if ValidImportedInitialAuthority(activeOrigin) {
		t.Fatal("initial authority accepted a non-overdue subscription")
	}
}
