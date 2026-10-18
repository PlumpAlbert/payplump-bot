package activation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
)

func TestWorkerPrepareWaitCrossesDeadlineWithoutSendingOrRearming(t *testing.T) {
	for _, deadlineKind := range []string{"local period", "UTC send"} {
		for i, step := range OrderedSteps() {
			// There is no known recurring send instant before its create receipt.
			if deadlineKind == "UTC send" && i < 2 {
				continue
			}
			t.Run(deadlineKind+"/"+string(step), func(t *testing.T) {
				f := newWorkerFixture(t)
				deadline, err := time.ParseInLocation(time.DateOnly, f.s.s.Proof.NextInvoiceDate, time.Local)
				if err != nil {
					t.Fatal(err)
				}
				if deadlineKind == "UTC send" {
					deadline = f.now.Add(time.Second)
					f.api.afterWrite = func(step Step) {
						if step == RecurringCreate {
							f.api.recurring.NextSendDatetime = deadline.UTC().Format(time.RFC3339Nano)
						}
					}
				}
				f.progress(i)
				f.now = deadline.Add(-time.Nanosecond)
				f.s.prepareHook = func(context.Context) { f.now = deadline }
				f.claim()
				f.assertHold(ReviewUnknownWrite)
				intent := f.s.s.Steps[step]
				if f.api.writes != i || f.s.prepareCalls != i+1 || f.s.confirmCalls != i || f.s.finishCalls != 0 || !f.s.guard || len(f.s.s.Steps) != i+1 || len(f.s.retries) != i {
					t.Fatal("deadline crossed during Prepare sent, confirmed, retried, or released its durable reservation")
				}
				if intent.ExternalID != "" || intent.RecurringAmount != "" || intent.RecurringDatetime != "" || intent.DispatchAttempt != i+1 {
					t.Fatal("denied POST did not retain exactly one unconfirmed intent")
				}
				var denied reviewError
				if !errors.As(f.api.callbackResults[len(f.api.callbackResults)-1], &denied) || denied.code != ReviewPeriod {
					t.Fatal("authorization failed to recheck the expired deadline after Prepare")
				}
				// Losing even the review flag cannot turn a reserved permission into
				// another POST, nor can elapsed wall time release its barrier.
				f.s.s.ReviewCode = 0
				f.now = f.now.Add(24 * time.Hour)
				f.clearReads()
				f.claim()
				f.assertHold(ReviewUnknownWrite)
				if f.api.reads != 0 || len(f.p.calls) != 0 || f.api.writes != i || f.s.prepareCalls != i+1 || len(f.s.retries) != i || !f.s.guard {
					t.Fatal("expired reservation was looked up or rearmed")
				}
			})
		}
	}
}

func TestWorkerCancellationDuringPrepareRetainsUnknownReservation(t *testing.T) {
	for i, step := range OrderedSteps() {
		t.Run(string(step), func(t *testing.T) {
			f := newWorkerFixture(t)
			f.progress(i)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.s.prepareHook = func(context.Context) { cancel() }
			f.op.Attempt++
			f.w.handle(ctx, f.op)
			f.assertHold(ReviewUnknownWrite)
			if f.api.writes != i || f.s.confirmCalls != i || f.s.prepareCalls != i+1 || !f.s.guard || f.s.s.Steps[step].ExternalID != "" || len(f.s.retries) != i {
				t.Fatal("cancellation after durable Prepare sent or discarded the unknown permission")
			}
			if !errors.Is(f.api.callbackResults[len(f.api.callbackResults)-1], context.Canceled) {
				t.Fatal("authorization did not recheck cancellation after Prepare")
			}
		})
	}
}

func TestWorkerRecurringReceiptFactsCannotDriftBetweenClaims(t *testing.T) {
	for _, change := range []string{"amount 200 to 999", "sub-float amount", "UTC instant"} {
		for _, progress := range []int{2, 3, 4} {
			t.Run(change+"/"+string(OrderedSteps()[progress-1]), func(t *testing.T) {
				f := newWorkerFixture(t)
				f.progress(progress)
				lines := slices.Clone(f.api.recurring.LineItems)
				original := f.s.s.Steps[RecurringCreate]
				switch change {
				case "amount 200 to 999":
					f.api.recurring.Amount, f.api.recurring.Balance = "999", "999"
				case "sub-float amount":
					f.api.recurring.Amount, f.api.recurring.Balance = "200.000000000000000001", "200.000000000000000001"
				case "UTC instant":
					instant, err := time.Parse(time.RFC3339Nano, original.RecurringDatetime)
					if err != nil {
						t.Fatal(err)
					}
					f.api.recurring.NextSendDatetime = instant.Add(time.Nanosecond).Format(time.RFC3339Nano)
				}
				if !reflect.DeepEqual(lines, f.api.recurring.LineItems) {
					t.Fatal("regression must leave the frozen financial line unchanged")
				}
				f.clearReads()
				f.claim()
				f.assertHold(ReviewRecurring)
				if f.api.writes != progress || f.s.prepareCalls != progress || f.s.confirmCalls != progress || f.s.finishCalls != 0 || len(f.s.retries) != progress || len(f.p.calls) != 0 || !reflect.DeepEqual(original, f.s.s.Steps[RecurringCreate]) {
					t.Fatal("changed known recurring facts authorized Enable, Start, or Finish, or replaced the original receipt")
				}
			})
		}
	}
}

func TestWorkerRecurringFactsAllowEquivalentDecimalAndUTCRepresentations(t *testing.T) {
	for _, amount := range []json.Number{"200.0000", "2e2"} {
		for _, datetime := range []string{"2040-02-29T15:00:00+03:00", "2040-02-29T12:00:00+00:00", "2040-02-29 12:00:00"} {
			for _, progress := range []int{2, 3, 4} {
				t.Run(string(amount)+"/"+datetime+"/"+string(OrderedSteps()[progress-1]), func(t *testing.T) {
					f := newWorkerFixture(t)
					f.progress(progress)
					original := f.s.s.Steps[RecurringCreate]
					f.api.recurring.Amount, f.api.recurring.Balance = amount, amount
					f.api.recurring.NextSendDatetime = datetime
					f.progress(4 - progress)
					f.claim()
					if f.s.finishCalls != 1 || f.api.writes != 4 || f.s.holdCalls != 0 || !reflect.DeepEqual(original, f.s.s.Steps[RecurringCreate]) {
						t.Fatal("equivalent decimal or UTC representation changed immutable receipt facts")
					}
				})
			}
		}
	}
}

func TestWorkerFreezesActualRecurringResponseWithoutCalculatingTaxTotal(t *testing.T) {
	f := newWorkerFixture(t)
	f.api.product.Price, f.api.product.TaxRate1 = "200.005", "20"
	f.api.afterWrite = func(step Step) {
		switch step {
		case RecurringCreate:
			f.api.recurring.Amount, f.api.recurring.Balance = "240.0100", "240.01"
			f.api.recurring.NextSendDatetime = "2040-02-29T15:00:00.123456+03:00"
		case RecurringStart:
			f.api.recurring.Amount, f.api.recurring.Balance = "2.4001e2", "240.010"
			f.api.recurring.NextSendDatetime = "2040-02-29T12:00:00.1234560Z"
		}
	}
	f.progress(4)
	want := []postResult{
		{id: "recurring-A", amount: "240.0100", datetime: "2040-02-29T15:00:00.123456+03:00"},
		{id: "recurring-A", amount: "2.4001e2", datetime: "2040-02-29T12:00:00.1234560Z"},
	}
	if !reflect.DeepEqual(f.s.recurringConfirmations, want) {
		t.Fatal("recurring confirmations did not receive the exact validated response fields")
	}
	for _, step := range []Step{RecurringCreate, RecurringStart} {
		intent := f.s.s.Steps[step]
		if intent.RecurringAmount != "240.01" || intent.RecurringDatetime != "2040-02-29T12:00:00.123456Z" {
			t.Fatal("confirmed receipt lost the authoritative provider amount or UTC instant")
		}
	}
	for _, step := range []Step{SUICreate, SUIEnable} {
		if intent := f.s.s.Steps[step]; intent.RecurringAmount != "" || intent.RecurringDatetime != "" {
			t.Fatal("SUI confirmation gained recurring receipt facts")
		}
	}
	f.claim()
	if f.s.finishCalls != 1 || f.s.holdCalls != 0 {
		t.Fatal("provider-rounded root amount could not complete")
	}
}

func TestWorkerStartResponseMustMatchOriginalRecurringReceipt(t *testing.T) {
	for _, change := range []string{"amount", "datetime"} {
		t.Run(change, func(t *testing.T) {
			f := newWorkerFixture(t)
			f.progress(3)
			original := f.s.s.Steps[RecurringCreate]
			f.api.afterWrite = func(step Step) {
				if step != RecurringStart {
					t.Fatal("unexpected write")
				}
				if change == "amount" {
					f.api.recurring.Amount, f.api.recurring.Balance = "999", "999"
				} else {
					f.api.recurring.NextSendDatetime = "2040-02-29T12:00:01Z"
				}
			}
			f.claim()
			f.assertHold(ReviewUnknownWrite)
			if f.api.writes != 4 || f.s.confirmCalls != 3 || len(f.s.recurringConfirmations) != 1 || !f.s.guard || f.s.s.Steps[RecurringStart].ExternalID != "" || !reflect.DeepEqual(original, f.s.s.Steps[RecurringCreate]) {
				t.Fatal("Start confirmed facts different from the original Create receipt")
			}
		})
	}
}

func TestWorkerBoundStartChecksOriginalFactsBeforeAuthorization(t *testing.T) {
	for _, change := range []string{"amount", "UTC instant", "equivalent representations"} {
		t.Run(change, func(t *testing.T) {
			f := newWorkerFixture(t)
			f.progress(3)
			original := f.s.s.Steps[RecurringCreate]
			epoch := f.s.s.WriteEpoch
			// Even an equivalent prior GET must not replace the original receipt.
			f.api.recurring.Amount, f.api.recurring.Balance = "2e2", "2e2"
			f.api.recurring.NextSendDatetime = "2040-02-29T15:00:00+03:00"
			f.api.preflight = func(_ context.Context, step Step) error {
				if step != RecurringStart {
					t.Fatal("unexpected write preflight")
				}
				switch change {
				case "amount":
					f.api.recurring.Amount, f.api.recurring.Balance = "999", "999"
				case "UTC instant":
					f.api.recurring.NextSendDatetime = "2040-02-29T12:00:01Z"
				case "equivalent representations":
					f.api.recurring.NextSendDatetime = "2040-02-29 12:00:00"
				}
				return nil
			}
			f.clearReads()
			f.claim()
			if f.api.startReceipt.Amount.String() != original.RecurringAmount || f.api.startReceipt.NextSendDatetime != original.RecurringDatetime || !reflect.DeepEqual(original, f.s.s.Steps[RecurringCreate]) {
				t.Fatal("Start replaced the original receipt with later GET facts")
			}
			if change == "equivalent representations" {
				if f.s.prepareCalls != 4 || f.s.confirmCalls != 4 || f.api.writes != 4 || f.s.holdCalls != 0 || f.s.guard {
					t.Fatal("equivalent last GET did not complete a known Start")
				}
				return
			}
			f.assertHold(ReviewRecurring)
			if f.s.prepareCalls != 3 || f.s.confirmCalls != 3 || f.api.writes != 3 || f.s.guard || len(f.s.s.Steps) != 3 || f.s.s.WriteEpoch != epoch || len(f.api.callbackResults) != 3 || len(f.p.calls) != 0 || len(f.s.retries) != 3 {
				t.Fatal("last preflight drift reserved an intent, authorized payment, or POSTed")
			}
		})
	}
}

func TestWorkerRealRecurringAdapterBindsLastGETAndAcceptsEquivalentUTC(t *testing.T) {
	for _, change := range []string{"amount", "sub-float amount", "UTC instant", "equivalent Z", "equivalent offset", "equivalent SQL"} {
		t.Run(change, func(t *testing.T) {
			f := newWorkerFixture(t)
			f.progress(3)
			originalIntent := f.s.s.Steps[RecurringCreate]
			epoch := f.s.s.WriteEpoch
			original, changed := f.api.recurring, f.api.recurring
			switch change {
			case "amount":
				changed.Amount, changed.Balance = "999", "999"
			case "sub-float amount":
				changed.Amount, changed.Balance = "200.000000000000000001", "200.000000000000000001"
			case "UTC instant":
				changed.NextSendDatetime = "2040-02-29T12:00:01Z"
			case "equivalent offset":
				changed.Amount, changed.Balance = "2e2", "2e2"
				changed.NextSendDatetime = "2040-02-29T15:00:00+03:00"
			case "equivalent SQL":
				changed.NextSendDatetime = "2040-02-29 12:00:00"
			}
			after := changed
			after.StatusID = "2"
			if change == "equivalent Z" {
				after.NextSendDatetime = "2040-02-29T12:00:00Z"
			}
			if !reflect.DeepEqual(original.LineItems, changed.LineItems) {
				t.Fatal("regression must leave financial lines unchanged")
			}
			var gets, posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-APP-VERSION", invoiceninja.SupportedRecurringVersion)
				w.Header().Set("Content-Type", "application/json")
				var response any
				switch r.Method + " " + r.URL.Path {
				case "GET /api/v1/recurring_invoices/recurring-A":
					record := original
					if gets.Add(1) > 1 {
						record = changed
					}
					if posts.Load() > 0 {
						record = after
					}
					response = map[string]any{"data": record}
				case "GET /api/v1/ping":
					response = map[string]any{}
				case "POST /api/v1/recurring_invoices/bulk":
					posts.Add(1)
					response = map[string]any{"data": []invoiceninja.RecurringRecord{after}}
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
					return
				}
				if err := json.NewEncoder(w).Encode(response); err != nil {
					t.Error(err)
				}
			}))
			t.Cleanup(server.Close)
			client, err := invoiceninja.NewRecurringClient(server.URL, "synthetic-recurring-token", time.Second, 1, 1)
			if err != nil {
				t.Fatal(err)
			}
			f.w.providers.Recurring = client
			f.clearReads()
			f.claim()
			if gets.Load() != 2 || !reflect.DeepEqual(originalIntent, f.s.s.Steps[RecurringCreate]) {
				t.Fatal("worker/adapter did not retain the original facts across both GETs")
			}
			if change == "amount" || change == "sub-float amount" || change == "UTC instant" {
				f.assertHold(ReviewRecurring)
				if posts.Load() != 0 || f.s.prepareCalls != 3 || f.s.confirmCalls != 3 || f.s.guard || f.s.s.WriteEpoch != epoch || len(f.s.s.Steps) != 3 || len(f.s.retries) != 3 || len(f.p.calls) != 0 {
					t.Fatal("real adapter's changed last GET authorized or reserved a Start")
				}
				return
			}
			if posts.Load() != 1 || f.s.prepareCalls != 4 || f.s.confirmCalls != 4 || f.s.holdCalls != 0 || f.s.guard || len(f.s.s.Steps) != 4 || f.s.s.Steps[RecurringStart].ExternalID != original.ID {
				t.Fatal("equivalent real HTTP timestamps became an unknown Start")
			}
			f.claim()
			if f.s.finishCalls != 1 || f.s.holdCalls != 0 || posts.Load() != 1 {
				t.Fatal("known equivalent Start failed finalization or was replayed")
			}
		})
	}
}

func TestWorkerSavedRecurringReceiptsMustAgreeBeforeAnyProviderRead(t *testing.T) {
	for _, change := range []string{"amount", "datetime", "ID", "equivalent representations"} {
		t.Run(change, func(t *testing.T) {
			f := newWorkerFixture(t)
			f.progress(4)
			intent := f.s.s.Steps[RecurringStart]
			switch change {
			case "amount":
				intent.RecurringAmount = "999"
			case "datetime":
				intent.RecurringDatetime = "2040-02-29T12:00:01Z"
			case "ID":
				intent.ExternalID = "foreign"
			case "equivalent representations":
				intent.RecurringAmount, intent.RecurringDatetime = "2e2", "2040-02-29T15:00:00+03:00"
			}
			f.s.s.Steps[RecurringStart] = intent
			f.clearReads()
			f.claim()
			if change == "equivalent representations" {
				if f.s.finishCalls != 1 || f.s.holdCalls != 0 {
					t.Fatal("equivalent stored receipt facts failed canonical comparison")
				}
				return
			}
			code := ReviewRecurring
			if change == "ID" {
				code = ReviewAssociation
			}
			f.assertHold(code)
			if f.api.reads != 0 || len(f.p.calls) != 0 || f.api.writes != 4 || f.s.finishCalls != 0 {
				t.Fatal("inconsistent stored recurring receipts reached a provider or finalized")
			}
		})
	}
}
