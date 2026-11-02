package invoiceninja

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// All records and HTTP responses here are synthetic; no provider data is used.
func TestImportedAutoBillPolicies(t *testing.T) {
	hashes := map[string]bool{}
	for _, mode := range []string{"off", "always", "optin", "optout"} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", mode, enabled), func(t *testing.T) {
				_, baseline := importedFixture(t, "2")
				baseline.AutoBill, baseline.AutoBillEnabled = mode, enabled
				profile, err := NewImportedRecurringProfile(baseline, "monthly")
				if err != nil {
					t.Fatal(err)
				}
				if hashes[profile.Hash()] {
					t.Fatal("policy missing from hash")
				}
				hashes[profile.Hash()] = true
				raw, err := json.Marshal(profile)
				if err != nil {
					t.Fatal(err)
				}
				var restored ImportedRecurringProfile
				if err := json.Unmarshal(raw, &restored); err != nil || restored.Hash() != profile.Hash() || restored.baseline.AutoBill != mode || restored.baseline.AutoBillEnabled != enabled {
					t.Fatalf("profile journal: %v", err)
				}
				for field, value := range map[string]json.RawMessage{
					"auto_bill":         json.RawMessage(`"optout"`),
					"auto_bill_enabled": json.RawMessage(fmt.Sprint(!enabled)),
				} {
					if field == "auto_bill" && mode == "optout" {
						value = json.RawMessage(`"off"`)
					}
					var altered ImportedRecurringProfile
					if json.Unmarshal(mutateJournalObject(t, raw, "baseline", field, value), &altered) == nil {
						t.Fatalf("journal accepted changed %s without original hash", field)
					}
				}

				for _, operation := range []string{"pause", "schedule", "resume"} {
					for _, drift := range []string{"", "pre-mode", "pre-flag", "post-mode", "post-flag", "readback-mode", "readback-flag"} {
						t.Run(operation+"/"+drift, func(t *testing.T) {
							before := cloneLifecycleRecord(baseline)
							if operation != "pause" {
								before.StatusID = "3"
							}
							p, err := NewImportedRecurringProfile(before, "monthly")
							if err != nil {
								t.Fatal(err)
							}
							var run func(*ImportedRecurringClient, func(context.Context) error) (RecurringRecord, error)
							switch operation {
							case "pause":
								d, err := PrepareImportedPause(p, before)
								if err != nil {
									t.Fatal(err)
								}
								raw, err := json.Marshal(d)
								if err != nil {
									t.Fatal(err)
								}
								if err := json.Unmarshal(raw, &d); err != nil {
									t.Fatal(err)
								}
								run = func(c *ImportedRecurringClient, auth func(context.Context) error) (RecurringRecord, error) {
									return c.PauseAuthorized(context.Background(), d, auth)
								}
							case "schedule":
								d, err := PrepareImportedSchedule(p, before, "2040-03-31", "2040-01-01")
								if err != nil {
									t.Fatal(err)
								}
								raw, err := json.Marshal(d)
								if err != nil {
									t.Fatal(err)
								}
								if err := json.Unmarshal(raw, &d); err != nil {
									t.Fatal(err)
								}
								run = func(c *ImportedRecurringClient, auth func(context.Context) error) (RecurringRecord, error) {
									return c.RescheduleAuthorized(context.Background(), d, auth)
								}
							case "resume":
								d, err := PrepareImportedResume(p, before, RecurringReceipt{Amount: before.Amount, NextSendDatetime: before.NextSendDatetime})
								if err != nil {
									t.Fatal(err)
								}
								raw, err := json.Marshal(d)
								if err != nil {
									t.Fatal(err)
								}
								if err := json.Unmarshal(raw, &d); err != nil {
									t.Fatal(err)
								}
								run = func(c *ImportedRecurringClient, auth func(context.Context) error) (RecurringRecord, error) {
									return c.ResumeAuthorized(context.Background(), d, auth)
								}
							}
							var gets, writes, calls int
							change := func(x *RecurringRecord, stage string) {
								if drift == stage+"-mode" {
									if mode == "off" {
										x.AutoBill = "always"
									} else {
										x.AutoBill = "off"
									}
								}
								if drift == stage+"-flag" {
									x.AutoBillEnabled = !enabled
								}
							}
							after := cloneLifecycleRecord(before)
							switch operation {
							case "pause":
								after.StatusID = "3"
							case "resume":
								after.StatusID = "2"
							case "schedule":
								after.NextSendDate, after.NextSendDatetime = "2040-03-31", "2040-03-31T08:00:00Z"
							}
							srv := recurringTestServer(t, func(w http.ResponseWriter, r *http.Request) {
								switch r.Method + " " + r.URL.Path {
								case "GET /api/v1/recurring_invoices/recurring-A":
									gets++
									x := cloneLifecycleRecord(before)
									if gets == 1 {
										change(&x, "pre")
									} else {
										x = cloneLifecycleRecord(after)
										change(&x, "readback")
									}
									recurringTestReply(t, w, map[string]any{"data": x})
								case "PUT /api/v1/recurring_invoices/recurring-A":
									writes++
									if operation != "schedule" {
										t.Error("unexpected PUT")
									}
									var fields map[string]json.RawMessage
									if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
										t.Error(err)
									}
									if len(fields) != 2 || string(fields["next_send_date"]) != `"2040-03-31"` || string(fields["exchange_rate"]) != "1" {
										t.Errorf("PUT must contain only date and FX: %v", fields)
									}
									x := cloneLifecycleRecord(after)
									change(&x, "post")
									recurringTestReply(t, w, map[string]any{"data": x})
								case "POST /api/v1/recurring_invoices/bulk":
									writes++
									var payload struct {
										IDs    []string `json:"ids"`
										Action string   `json:"action"`
									}
									if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
										t.Error(err)
									}
									expected := "stop"
									if operation == "resume" {
										expected = "start"
									}
									if payload.Action != expected || len(payload.IDs) != 1 || payload.IDs[0] != before.ID {
										t.Errorf("unexpected lifecycle action: %+v", payload)
									}
									x := cloneLifecycleRecord(after)
									change(&x, "post")
									recurringTestReply(t, w, map[string]any{"data": []RecurringRecord{x}})
								default:
									t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
									http.NotFound(w, r)
								}
							})
							c, err := NewImportedRecurringClient(srv.URL, recurringTestToken, time.Second, 1, 1)
							if err != nil {
								t.Fatal(err)
							}
							c.base.now = func() time.Time { return time.Date(2040, 1, 1, 12, 0, 0, 0, time.Local) }
							got, err := run(c, func(context.Context) error { calls++; return nil })
							switch drift {
							case "":
								if err != nil || calls != 1 || writes != 1 || gets != 2 || got.AutoBill != mode || got.AutoBillEnabled != enabled || got.DueDateDays != "terms" {
									t.Fatalf("policy not preserved: err=%v calls=%d writes=%d gets=%d", err, calls, writes, gets)
								}
							case "pre-mode", "pre-flag":
								if !errors.Is(err, ErrRecurringReceipt) || calls != 0 || writes != 0 {
									t.Fatalf("drift authorized: err=%v calls=%d writes=%d", err, calls, writes)
								}
							default:
								if !errors.Is(err, ErrRecurringUnknown) || calls != 1 || writes != 1 {
									t.Fatalf("post-write drift not held unknown: err=%v calls=%d writes=%d", err, calls, writes)
								}
							}
						})
					}
				}
			})
		}
	}
	for _, mode := range []string{"", "unknown", "ALWAYS", " off"} {
		for _, enabled := range []bool{false, true} {
			_, r := importedFixture(t, "2")
			r.AutoBill, r.AutoBillEnabled = mode, enabled
			if _, err := NewImportedRecurringProfile(r, "monthly"); err == nil {
				t.Fatalf("accepted unknown mode %q", mode)
			}
		}
	}
}
