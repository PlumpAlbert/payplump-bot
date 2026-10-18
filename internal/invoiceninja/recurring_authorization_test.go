package invoiceninja

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"
)

type recurringAuthorizationProbe struct {
	mu         sync.Mutex
	paths      []string
	posts      int
	badVersion bool
	badReceipt bool
	record     RecurringRecord
}

func recurringAuthorizationCase(t *testing.T, start bool) (*RecurringClient, *recurringAuthorizationProbe, *RecurringDraft, func(context.Context, func(context.Context) error) (RecurringRecord, error)) {
	t.Helper()
	draft := recurringTestDraft(t)
	p := &recurringAuthorizationProbe{record: recurringTestRecord(t)}
	s := recurringTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		path := r.Method + " " + r.URL.Path
		p.paths = append(p.paths, path)
		switch path {
		case "GET /api/v1/ping":
			if p.badVersion {
				w.Header().Set("X-APP-VERSION", "unsupported")
			}
			recurringTestReply(t, w, map[string]any{})
		case "GET /api/v1/recurring_invoices/recurring-A":
			recurringTestReply(t, w, map[string]any{"data": p.record})
		case "POST /api/v1/recurring_invoices":
			p.posts++
			if p.badReceipt {
				recurringTestReply(t, w, map[string]any{"data": nil})
			} else {
				recurringTestReply(t, w, map[string]any{"data": p.record})
			}
		case "POST /api/v1/recurring_invoices/bulk":
			p.posts++
			after := p.record
			after.StatusID = "2"
			if p.badReceipt {
				recurringTestReply(t, w, map[string]any{"data": []RecurringRecord{}})
			} else {
				recurringTestReply(t, w, map[string]any{"data": []RecurringRecord{after}})
			}
		default:
			t.Error("unexpected authorization test request")
			http.NotFound(w, r)
		}
	})
	c := recurringTestClient(t, s.URL, 1, 1)
	return c, p, &draft, func(ctx context.Context, authorize func(context.Context) error) (RecurringRecord, error) {
		if start {
			return c.StartFirstAuthorized(ctx, "recurring-A", draft, authorize)
		}
		return c.CreateDraftAuthorized(ctx, draft, authorize)
	}
}

func recurringAuthorizationPreflights(start bool) []string {
	if start {
		return []string{"GET /api/v1/recurring_invoices/recurring-A", "GET /api/v1/ping"}
	}
	return []string{"GET /api/v1/ping"}
}

func TestRecurringAuthorizedNilDeniesBeforeIO(t *testing.T) {
	for _, start := range []bool{false, true} {
		_, p, _, call := recurringAuthorizationCase(t, start)
		_, err := call(context.Background(), nil)
		p.mu.Lock()
		if err != errRecurringDraft || errors.Is(err, ErrRecurringUnknown) || p.posts != 0 || len(p.paths) != 0 {
			t.Error("nil authorization was not rejected before I/O")
		}
		p.mu.Unlock()
	}
}

func TestRecurringAuthorizedRunsAfterAllPreflights(t *testing.T) {
	for _, start := range []bool{false, true} {
		for _, failure := range []string{"none", "version", "invalid-draft", "expired"} {
			c, p, draft, call := recurringAuthorizationCase(t, start)
			switch failure {
			case "version":
				p.badVersion = true
			case "invalid-draft":
				draft.AutoBill = "always"
			case "expired":
				c.now = func() time.Time { return time.Date(2040, 2, 29, 0, 0, 0, 0, time.Local) }
			}
			calls := 0
			denied := errors.New("synthetic authorization rejection")
			ctx := context.WithValue(context.Background(), struct{}{}, "synthetic")
			_, err := call(ctx, func(got context.Context) error {
				calls++
				p.mu.Lock()
				defer p.mu.Unlock()
				if got != ctx || !reflect.DeepEqual(p.paths, recurringAuthorizationPreflights(start)) || p.posts != 0 {
					t.Error("authorization ran before the complete GET/probe sequence")
				}
				return denied
			})
			want := 0
			if failure == "none" {
				want = 1
				if err != denied {
					t.Fatal("callback rejection was changed")
				}
			}
			p.mu.Lock()
			if err == nil || errors.Is(err, ErrRecurringUnknown) || calls != want || p.posts != 0 {
				t.Error("preflight/rejection posted, retried or became unknown")
			}
			p.mu.Unlock()
		}
	}
	// The exact record must pass the first-start policy before authorization.
	_, p, _, call := recurringAuthorizationCase(t, true)
	p.record.StatusID = "2"
	calls := 0
	_, err := call(context.Background(), func(context.Context) error { calls++; return nil })
	if err == nil || errors.Is(err, ErrRecurringUnknown) || calls != 0 {
		t.Fatal("already-active record reached final authorization")
	}
}

func TestRecurringAuthorizedRechecksClockAndCancellationAfterCallback(t *testing.T) {
	for _, start := range []bool{false, true} {
		for _, failure := range []string{"cancel", "date-expiry", "utc-expiry"} {
			if !start && failure == "utc-expiry" {
				continue
			}
			c, p, _, call := recurringAuthorizationCase(t, start)
			p.record.NextSendDatetime = "2040-01-03T08:00:00Z"
			ctx, cancel := context.WithCancel(context.Background())
			calls := 0
			_, err := call(ctx, func(context.Context) error {
				calls++
				switch failure {
				case "cancel":
					cancel()
				case "date-expiry":
					c.now = func() time.Time { return time.Date(2040, 2, 29, 0, 0, 0, 0, time.Local) }
				case "utc-expiry":
					c.now = func() time.Time { return time.Date(2040, 1, 3, 8, 0, 0, 0, time.UTC) }
				}
				return nil
			})
			cancel()
			want := ErrRecurringExpired
			if failure == "cancel" {
				want = context.Canceled
			}
			p.mu.Lock()
			if err != want || errors.Is(err, ErrRecurringUnknown) || calls != 1 || p.posts != 0 {
				t.Error("post-authorization cancellation/expiry attempted a POST or became unknown")
			}
			p.mu.Unlock()
		}
	}
}

func TestRecurringAuthorizedSuccessAndUnknownReceiptStillUseOnePOST(t *testing.T) {
	for _, start := range []bool{false, true} {
		for _, badReceipt := range []bool{false, true} {
			_, p, draft, call := recurringAuthorizationCase(t, start)
			p.badReceipt = badReceipt
			calls := 0
			got, err := call(context.Background(), func(context.Context) error {
				calls++
				// The caller's slice cannot change the already-frozen intent.
				draft.LineItems[0].Cost = "1"
				draft.LineItems[0].ProductKey = "changed"
				return nil
			})
			if badReceipt {
				if !errors.Is(err, ErrRecurringUnknown) {
					t.Fatal("invalid POST receipt did not retain HOLD semantics")
				}
			} else if err != nil || got.ID != "recurring-A" || start && got.StatusID != "2" || !start && got.StatusID != "1" {
				t.Fatal("authorized recurring write failed or caller mutated its snapshot")
			}
			p.mu.Lock()
			want := recurringAuthorizationPreflights(start)
			endpoint := "POST /api/v1/recurring_invoices"
			if start {
				endpoint += "/bulk"
			}
			want = append(want, endpoint)
			if calls != 1 || p.posts != 1 || !reflect.DeepEqual(p.paths, want) {
				t.Error("authorization/write retried or changed source-pinned request protocol")
			}
			p.mu.Unlock()
		}
	}
}
