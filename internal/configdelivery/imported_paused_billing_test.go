package configdelivery

import (
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
)

func preparePausedBillingBaseline(t *testing.T, f *deliveryFixture, date, instant string) {
	t.Helper()
	prepareImportedBaseline(t, f)
	record := f.rec.record
	record.StatusID, record.NextSendDate, record.NextSendDatetime = "3", date, instant
	profile, err := invoiceninja.NewImportedRecurringProfile(record, "monthly")
	if err != nil {
		t.Fatal(err)
	}
	f.rec.record = record
	e := f.store.view.Entitlement
	e.Imported.Profile = profile
	e.Imported.NextSendUTC, err = invoiceninja.ParseRecurringUTC(instant)
	if err != nil && instant != "" {
		t.Fatal(err)
	}
	f.store.view.Entitlement = e
	f.store.view.Request.EntitlementHash, err = e.Fingerprint()
	if err != nil || !f.store.view.Matches() {
		t.Fatalf("paused billing lost verified entitlement binding: %v", err)
	}
}

func TestImportedPaidAccessDoesNotDependOnPausedBillingSchedule(t *testing.T) {
	for _, schedule := range [][2]string{{"2040-02-29", "2040-02-29T12:00:00Z"}, {"2039-12-01", "2039-12-01T00:00:00Z"}, {"", ""}} {
		t.Run(schedule[0], func(t *testing.T) {
			f := newDeliveryFixture(t)
			preparePausedBillingBaseline(t, f, schedule[0], schedule[1])
			deadline, err := f.store.view.Entitlement.Deadline()
			if err != nil || !deadline.After(f.now) {
				t.Fatalf("paused billing prematurely expired the paid period: %v", err)
			}
			id, err := f.deliver()
			if err != nil || id != 7001 || f.store.decision != (Decision{Kind: Secret}) || f.tg.posts != 1 || f.rec.record.StatusID != "3" {
				t.Fatalf("paused billing blocked protected paid delivery: decision=%+v err=%v", f.store.decision, err)
			}
		})
	}
}

func TestImportedPausedBillingDoesNotBypassPaymentOrSourceChecks(t *testing.T) {
	for _, change := range []string{"unpaid", "source schedule changed", "source resumed", "source resumed with matching future schedule", "period expired"} {
		t.Run(change, func(t *testing.T) {
			f := newDeliveryFixture(t)
			date, instant := "", ""
			if change == "source resumed with matching future schedule" {
				date, instant = "2040-02-29", "2040-02-29T12:00:00Z"
			}
			preparePausedBillingBaseline(t, f, date, instant)
			expected := DeniedRecurring
			switch change {
			case "unpaid":
				f.p.invoice.StatusID, f.p.invoice.Balance, f.p.invoice.PaidToDate = "2", "100", "0"
				expected = DeniedPayment
			case "source schedule changed":
				f.rec.record.NextSendDate = "2040-02-29"
			case "source resumed", "source resumed with matching future schedule":
				f.rec.record.StatusID = "2"
			case "period expired":
				deadline, err := f.store.view.Entitlement.Deadline()
				if err != nil {
					t.Fatal(err)
				}
				f.now = deadline.Add(time.Minute)
				f.store.view.Request.CreatedAt, f.store.view.Request.ExpiresAt = f.now, f.now.Add(RequestTTL)
				expected = DeniedExpired
			}
			_, err := f.deliver()
			if err != nil || f.store.decision.Kind != Denial || f.store.decision.Reason != expected || f.tg.posts != 1 {
				t.Fatalf("paused baseline bypassed %s: decision=%+v err=%v", change, f.store.decision, err)
			}
			if f.tg.content.Text != denialText {
				t.Fatal("denied paused baseline leaked a subscription URL")
			}
		})
	}
}
