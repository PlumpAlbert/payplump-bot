package invoiceninja

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestLifecycleCancellationAfterPermissionAlwaysHolds(t *testing.T) {
	for _, name := range []string{"pause", "schedule", "resume"} {
		t.Run(name, func(t *testing.T) {
			status := "3"
			if name == "pause" {
				status = "2"
			}
			d, b := lifecycleFixture(t, status)
			mutations := 0
			server := recurringTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					mutations++
					t.Error("canceled granted intent transmitted")
					http.Error(w, "unexpected", 500)
					return
				}
				if r.URL.Path == "/api/v1/ping" {
					recurringTestReply(t, w, map[string]any{})
					return
				}
				recurringTestReply(t, w, map[string]any{"data": b})
			})
			c := recurringTestClient(t, server.URL, 1, 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			permission := func(context.Context) error { cancel(); return nil }
			var err error
			switch name {
			case "pause":
				draft, e := PreparePause(b, d)
				if e != nil {
					t.Fatal(e)
				}
				_, err = c.PauseAuthorized(ctx, draft, permission)
			case "schedule":
				draft, e := PrepareSchedule(b, d, "2040-03-31", "2040-01-01")
				if e != nil {
					t.Fatal(e)
				}
				_, err = c.RescheduleAuthorized(ctx, draft, permission)
			case "resume":
				draft, e := PrepareResume(b, d, RecurringReceipt{Amount: b.Amount, NextSendDatetime: b.NextSendDatetime}, "2040-01-01")
				if e != nil {
					t.Fatal(e)
				}
				_, err = c.ResumeAuthorized(ctx, draft, permission)
			}
			if !errors.Is(err, ErrRecurringUnknown) || !errors.Is(err, context.Canceled) || mutations != 0 {
				t.Fatal("post-permission cancellation lost permanent hold classification")
			}
		})
	}
}
func TestResumeRechecksCalendarAfterPermission(t *testing.T) {
	d, b := lifecycleFixture(t, "3")
	b.NextSendDatetime = time.Date(2040, 2, 29, 12, 0, 0, 0, time.Local).UTC().Format(time.RFC3339)
	draft, err := PrepareResume(b, d, RecurringReceipt{Amount: b.Amount, NextSendDatetime: b.NextSendDatetime}, "2040-02-28")
	if err != nil {
		t.Fatal(err)
	}
	mutations := 0
	server := recurringTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mutations++
			http.Error(w, "unexpected", 500)
			return
		}
		if r.URL.Path == "/api/v1/ping" {
			recurringTestReply(t, w, map[string]any{})
			return
		}
		recurringTestReply(t, w, map[string]any{"data": b})
	})
	c := recurringTestClient(t, server.URL, 1, 1)
	now := time.Date(2040, 2, 28, 23, 59, 59, 0, time.Local)
	c.now = func() time.Time { return now }
	_, err = c.ResumeAuthorized(context.Background(), draft, func(context.Context) error { now = now.Add(time.Second); return nil })
	if !errors.Is(err, ErrRecurringUnknown) || !errors.Is(err, ErrRecurringExpired) || mutations != 0 {
		t.Fatal("resumed on the billing day after an authorization wait")
	}
}
func TestPauseRejectsContradictoryWriteReceiptBeforeReadback(t *testing.T) {
	d, b := lifecycleFixture(t, "2")
	draft, err := PreparePause(b, d)
	if err != nil {
		t.Fatal(err)
	}
	posts := 0
	server := recurringTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
			x := b
			x.StatusID = "3"
			x.Amount = "999"
			x.Balance = "999"
			recurringTestReply(t, w, map[string]any{"data": []RecurringRecord{x}})
			return
		}
		if r.URL.Path == "/api/v1/ping" {
			recurringTestReply(t, w, map[string]any{})
			return
		}
		x := b
		if posts > 0 {
			x.StatusID = "3"
		}
		recurringTestReply(t, w, map[string]any{"data": x})
	})
	c := recurringTestClient(t, server.URL, 1, 1)
	_, err = c.PauseAuthorized(context.Background(), draft, func(context.Context) error { return nil })
	if !errors.Is(err, ErrRecurringUnknown) || posts != 1 {
		t.Fatal("contradictory receipt was adopted as known pause or retried")
	}
}
