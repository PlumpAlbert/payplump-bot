package lifecycle

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/importorigin"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
)

func TestImportedBillingPausedNewPaidCyclePlan(t *testing.T) {
	now := time.Date(2040, time.February, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, date, utc string
		schedule        bool
	}{
		{"missing", "", "", true},
		{"stale", "2040-01-15", "2040-01-15T08:00:00Z", true},
		{"same target missing UTC", "2040-02-29", "", true},
		{"same target stale UTC", "2040-02-29", "2040-01-15T08:00:00Z", true},
		{"same target future UTC", "2040-02-29", "2040-02-29T08:00:00Z", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, r, panel := importedRenewalFixture(t)
			r.StatusID, r.NextSendDate, r.NextSendDatetime = "3", tc.date, tc.utc
			var err error
			s.ImportedRecurringProfile, err = invoiceninja.NewImportedRecurringProfile(r, r.LineItems[0].ProductKey)
			if err != nil {
				t.Fatal(err)
			}
			if !ImportedBillingWasPaused(s) || !validRenewalAssociation(s, 42, []int64{101}, nil) || validRenewalExternalState(s, r, panel) != 0 {
				t.Fatal("hash-bound paused billing baseline with active paid access rejected")
			}
			plan, baseline, code := makeRenewalPlan(s, r, panel, now)
			if code != 0 || plan.Schedule != tc.schedule || !plan.Resume || plan.Pause || plan.Enable || plan.TargetDate != s.Proof.NextInvoiceDate {
				t.Fatalf("plan=%+v review=%d", plan, code)
			}
			var facts struct {
				Status string `json:"status_id"`
				Date   string `json:"next_send_date"`
				UTC    string `json:"next_send_datetime"`
			}
			if json.Unmarshal(baseline, &facts) != nil || facts.Status != "3" || facts.Date != tc.date || facts.UTC != tc.utc || plan.BaselineReceipt.NextSendDatetime != tc.utc {
				t.Fatal("planner fabricated original schedule evidence")
			}
			if renewalCompletionState(s, plan, r, panel, now) {
				t.Fatal("paused source incorrectly accepted as completed renewal")
			}
			s.Plan = &plan
			s.Steps[RenewalResume] = RenewalStepRecord{Confirmed: true}
			if validRenewalExternalState(s, r, panel) == 0 {
				t.Fatal("unexpected pause after confirmed resume accepted")
			}
			delete(s.Steps, RenewalResume)
			completed := r
			completed.StatusID, completed.NextSendDate, completed.NextSendDatetime = "2", plan.TargetDate, "2040-02-29T08:00:00Z"
			if !renewalCompletionState(s, plan, completed, panel, now) {
				t.Fatal("actual resumed future receipt rejected")
			}
		})
	}
}

func TestImportedBillingPauseDoesNotProveManualPauseOrMalformedSchedule(t *testing.T) {
	s, r, panel := importedRenewalFixture(t)
	r.StatusID = "3"
	now := time.Date(2040, time.February, 1, 12, 0, 0, 0, time.UTC)
	if ImportedBillingWasPaused(s) || validRenewalExternalState(s, r, panel) == 0 {
		t.Fatal("ordinary active profile acquired pause authority from live status")
	}
	if _, _, code := makeRenewalPlan(s, r, panel, now); code == 0 {
		t.Fatal("manual pause without immutable paused baseline accepted by planner")
	}
	var err error
	s.ImportedRecurringProfile, err = invoiceninja.NewImportedRecurringProfile(r, r.LineItems[0].ProductKey)
	if err != nil {
		t.Fatal(err)
	}
	withoutOrigin := s
	withoutOrigin.ImportedOrigin = importorigin.Ref{}
	if ImportedBillingWasPaused(withoutOrigin) {
		t.Fatal("paused profile without valid imported origin accepted")
	}
	r.StatusID, r.NextSendDatetime = "2", ""
	if _, _, code := makeRenewalPlan(s, r, panel, now); code != ReviewRenewalTarget {
		t.Fatalf("missing UTC allowed without a live paused status: %d", code)
	}
	r.StatusID, r.NextSendDatetime = "3", "not-a-timestamp"
	if _, _, code := makeRenewalPlan(s, r, panel, now); code != ReviewRenewalTarget {
		t.Fatalf("malformed nonempty UTC accepted: %d", code)
	}
	r.NextSendDatetime = "2040-01-15T08:00:00Z"
	r.Amount = "101"
	if validRenewalExternalState(s, r, panel) == 0 {
		t.Fatal("paused baseline waived immutable amount check")
	}
}

func TestImportedBillingPauseDoesNotReplaceFreshFinancialProof(t *testing.T) {
	s, r, _ := importedRenewalFixture(t)
	r.StatusID = "3"
	var err error
	s.ImportedRecurringProfile, err = invoiceninja.NewImportedRecurringProfile(r, r.LineItems[0].ProductKey)
	if err != nil {
		t.Fatal(err)
	}
	bad := s
	bad.Proof.Hash[0] ^= 1
	if validRenewalAssociation(bad, 42, []int64{101}, nil) {
		t.Fatal("paused source replaced independently confirmed payment hash")
	}
	for _, kind := range []string{"partial", "refunded"} {
		t.Run(kind, func(t *testing.T) {
			w, _, _, _ := renewalWorkerFixture(t, false, true)
			reader := w.payments.(*renewalMemoryPayments)
			if kind == "partial" {
				reader.invoice.Balance, reader.invoice.PaidToDate = "1", "99"
				reader.items[0].Amount, reader.items[0].Applied = "99", "99"
				reader.items[0].Allocations[0].Amount = "99"
			} else {
				reader.items[0].Refunded = "1"
			}
			d, err := w.observe(context.Background(), s)
			if err != nil || d.Kind == payments.Verified {
				t.Fatalf("unproven new-cycle money accepted: decision=%+v err=%v", d, err)
			}
		})
	}
}
