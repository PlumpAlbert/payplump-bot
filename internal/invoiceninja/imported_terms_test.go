package invoiceninja

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestImportedBillingTermsDriftCannotBeSilentlyAccepted(t *testing.T) {
	for _, point := range []string{"before authorization", "after write"} {
		t.Run(point, func(t *testing.T) {
			profile, before := importedFixture(t, "3")
			draft, err := PrepareImportedSchedule(profile, before, "2040-03-31", "2040-02-01")
			if err != nil {
				t.Fatal(err)
			}
			var gets, writes, authorizations atomic.Int32
			server := recurringTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				current := before
				switch r.Method {
				case http.MethodGet:
					read := gets.Add(1)
					if point == "before authorization" || read > 1 {
						current.DueDateDays = "7"
					}
					if read > 1 {
						current.NextSendDate, current.NextSendDatetime = "2040-03-31", "2040-03-31T08:00:00Z"
					}
				case http.MethodPut:
					writes.Add(1)
					var payload map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || len(payload) != 2 || payload["next_send_date"] == nil || payload["exchange_rate"] == nil {
						t.Error("reschedule unexpectedly writes billing terms or other source fields")
					}
					current.DueDateDays = "7"
					current.NextSendDate, current.NextSendDatetime = "2040-03-31", "2040-03-31T08:00:00Z"
				default:
					t.Error("unexpected mutation")
				}
				recurringTestReply(t, w, map[string]any{"data": current})
			})
			client, err := NewImportedRecurringClient(server.URL, recurringTestToken, time.Second, 1, 1)
			if err != nil {
				t.Fatal(err)
			}
			client.base.now = func() time.Time { return time.Date(2040, 1, 1, 12, 0, 0, 0, time.Local) }
			_, err = client.RescheduleAuthorized(context.Background(), draft, func(context.Context) error {
				authorizations.Add(1)
				return nil
			})
			if point == "before authorization" {
				if !errors.Is(err, ErrRecurringReceipt) || writes.Load() != 0 || authorizations.Load() != 0 {
					t.Fatalf("changed terms reached authorization/write: err=%v", err)
				}
			} else if !errors.Is(err, ErrRecurringUnknown) || writes.Load() != 1 || authorizations.Load() != 1 {
				t.Fatalf("post-write terms drift lost unknown-outcome hold: err=%v", err)
			}
		})
	}
}

func TestImportedZeroTaxModeIsPreservedWithoutBroadeningTaxSupport(t *testing.T) {
	original, before := importedFixture(t, "2")
	before.UsesInclusiveTaxes = !before.UsesInclusiveTaxes
	profile, err := NewImportedRecurringProfile(before, before.LineItems[0].ProductKey)
	if err != nil {
		t.Fatalf("zero-tax mode rejected: %v", err)
	}
	if profile.baseline.UsesInclusiveTaxes != before.UsesInclusiveTaxes || profile.Hash() == original.Hash() || profile.baseline.Amount != original.baseline.Amount {
		t.Fatal("zero-tax mode was erased or changed the amount")
	}
	if _, err := PrepareImportedPause(profile, before); err != nil {
		t.Fatal(err)
	}
	changed := before
	changed.UsesInclusiveTaxes = !before.UsesInclusiveTaxes
	if _, err := PrepareImportedPause(profile, changed); err == nil {
		t.Fatal("tax mode changed after the source was frozen")
	}
	for _, lineTax := range []bool{false, true} {
		changed = before
		changed.LineItems = append([]InvoiceLine(nil), before.LineItems...)
		if lineTax {
			changed.LineItems[0].TaxRate1 = "1"
		} else {
			changed.TaxRate1 = "1"
		}
		if _, err := NewImportedRecurringProfile(changed, changed.LineItems[0].ProductKey); err == nil {
			t.Fatal("nonzero tax was admitted by zero-tax mode compatibility")
		}
	}
}

func TestImportedProfilePreservesExistingDueDateTerms(t *testing.T) {
	for _, term := range []string{"on_receipt", "0", "1", "7", "30", "", "legacy-terms"} {
		t.Run(term, func(t *testing.T) {
			original, before := importedFixture(t, "2")
			before.DueDateDays = term
			profile, err := NewImportedRecurringProfile(before, before.LineItems[0].ProductKey)
			if err != nil {
				t.Fatalf("existing terms rejected: %v", err)
			}
			if profile.baseline.DueDateDays != term || profile.baseline.DueDate != before.DueDate || profile.baseline.Amount != before.Amount || profile.baseline.NextSendDatetime != before.NextSendDatetime {
				t.Fatal("existing billing facts were changed")
			}
			if term != original.baseline.DueDateDays && profile.Hash() == original.Hash() {
				t.Fatal("terms are missing from the frozen source hash")
			}
			raw, err := json.Marshal(profile)
			if err != nil {
				t.Fatal(err)
			}
			var restored ImportedRecurringProfile
			if err := json.Unmarshal(raw, &restored); err != nil || restored.Hash() != profile.Hash() || restored.baseline.DueDateDays != term {
				t.Fatalf("profile did not retain terms across journal roundtrip: %v", err)
			}
			if _, err := PrepareImportedPause(restored, before); err != nil {
				t.Fatalf("preserved terms blocked pause preparation: %v", err)
			}
			changed := before
			changed.DueDateDays = "different-terms"
			if _, err := PrepareImportedPause(restored, changed); err == nil {
				t.Fatal("billing terms changed after the source was frozen")
			}
		})
	}
}
