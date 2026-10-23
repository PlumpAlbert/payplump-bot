package invoiceninja

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func lifecycleFixture(t *testing.T, status string) (RecurringDraft, RecurringRecord) {
	t.Helper()
	d := recurringTestDraft(t)
	r := recurringTestRecord(t)
	r.StatusID = status
	r.ExchangeRate = "1"
	r.NextSendDate = "2040-02-29"
	r.LastSentDate = "2040-01-31 08:00:00"
	raw, _ := json.Marshal(r)
	var round RecurringRecord
	if e := json.Unmarshal(raw, &round); e != nil {
		t.Fatalf("fixture roundtrip: %v (%s)", e, raw)
	}
	if e := lifecycleTemplate(r, d); e != nil {
		t.Fatal(e)
	}
	return d, r
}
func TestValidateLifecycleRecurringForDraftAllowsHistoricalLastSent(t *testing.T) {
	d, record := lifecycleFixture(t, "2")
	d.NextSendDate = record.NextSendDate
	if err := ValidateLifecycleRecurringForDraft(record, d, "2040-02-29", "2040-02-28"); err != nil {
		t.Fatalf("valid completed-period recurring rejected: %v", err)
	}
	for _, tc := range []struct {
		target, today string
	}{
		{"2040-03-01", "2040-02-28"},
		{"2040-02-29", "2040-02-29"},
		{"2040-02-29", "2040-03-01"},
	} {
		if err := ValidateLifecycleRecurringForDraft(record, d, tc.target, tc.today); err == nil {
			t.Fatalf("accepted mismatching/nonfuture lifecycle date target=%s today=%s", tc.target, tc.today)
		}
	}
}

func TestPauseAuthorizedLifecycle(t *testing.T) {
	d, b := lifecycleFixture(t, "2")
	draft, e := PreparePause(b, d)
	if !lifecycleSame(b, b, d) {
		t.Fatal("fixture baseline mismatch")
	}
	check := b
	check.StatusID = "3"
	if !lifecycleSame(b, check, d) {
		t.Fatal("status changed other facts")
	}
	checkRaw, _ := json.Marshal(check)
	var checkRound RecurringRecord
	if er := json.Unmarshal(checkRaw, &checkRound); er != nil {
		t.Fatalf("check roundtrip %v", er)
	}
	if !lifecycleSame(b, checkRound, d) {
		t.Fatal("roundtrip changed facts")
	}
	if e != nil {
		t.Fatal(e)
	}
	var posts, gets int
	srv := recurringTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/ping":
			recurringTestReply(t, w, map[string]any{})
		case "GET /api/v1/recurring_invoices/recurring-A":
			gets++
			a := b
			if gets > 1 {
				a.StatusID = "3"
			}
			recurringTestReply(t, w, map[string]any{"data": a, "unknown_metadata": true})
		case "POST /api/v1/recurring_invoices/bulk":
			posts++
			body, _ := io.ReadAll(r.Body)
			if string(body) != "{\"ids\":[\"recurring-A\"],\"action\":\"stop\"}" {
				t.Errorf("payload %s", body)
			}
			a := b
			a.StatusID = "3"
			recurringTestReply(t, w, map[string]any{"data": []RecurringRecord{a}})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	c := recurringTestClient(t, srv.URL, 1, 1)
	calls := 0
	got, e := c.PauseAuthorized(context.Background(), draft, func(context.Context) error {
		calls++
		if gets != 1 || posts != 0 {
			t.Errorf("callback timing gets=%d posts=%d", gets, posts)
		}
		return nil
	})
	if e != nil || got.StatusID != "3" || calls != 1 || gets != 2 || posts != 1 {
		t.Fatalf("got=%+v err=%v calls=%d gets=%d posts=%d", got, e, calls, gets, posts)
	}
}
func TestRescheduleAuthorizedUsesSparsePutAndNeverReplays(t *testing.T) {
	d, b := lifecycleFixture(t, "3")
	draft, e := PrepareSchedule(b, d, "2040-03-31", "2040-02-01")
	if e != nil {
		t.Fatal(e)
	}
	var gets, puts, calls int
	srv := recurringTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/ping":
			recurringTestReply(t, w, map[string]any{})
		case "GET /api/v1/recurring_invoices/recurring-A":
			gets++
			a := b
			if gets > 1 {
				a.NextSendDate = "2040-03-31"
				a.NextSendDatetime = "2040-03-31T08:00:00Z"
			}
			recurringTestReply(t, w, map[string]any{"data": a, "extra": "must not be written"})
		case "PUT /api/v1/recurring_invoices/recurring-A":
			puts++
			if r.Header.Get("Content-Type") != "application/json" {
				t.Error("not JSON")
			}
			raw, _ := io.ReadAll(r.Body)
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(raw, &fields)
			if len(fields) != 2 || string(fields["next_send_date"]) != "\"2040-03-31\"" || string(fields["exchange_rate"]) != "1" {
				t.Errorf("unexpected sparse payload %s", raw)
			}
			a := b
			a.NextSendDate = "2040-03-31"
			a.NextSendDatetime = "2040-03-31T08:00:00Z"
			recurringTestReply(t, w, map[string]any{"data": a})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	c := recurringTestClient(t, srv.URL, 1, 1)
	got, e := c.RescheduleAuthorized(context.Background(), draft, func(context.Context) error {
		calls++
		if gets != 1 || puts != 0 {
			t.Errorf("callback timing")
		}
		return nil
	})
	if e != nil || got.NextSendDate != "2040-03-31" || puts != 1 || gets != 2 || calls != 1 {
		t.Fatalf("got=%+v err=%v gets=%d puts=%d calls=%d", got, e, gets, puts, calls)
	}
}
func TestResumeAuthorizedUsesOriginalReceiptAndStartsOnly(t *testing.T) {
	d, b := lifecycleFixture(t, "3")
	receipt := RecurringReceipt{Amount: b.Amount, NextSendDatetime: b.NextSendDatetime}
	draft, err := PrepareResume(b, d, receipt, "2040-02-01")
	if err != nil {
		t.Fatal(err)
	}
	var gets, posts, calls int
	srv := recurringTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/ping":
			recurringTestReply(t, w, map[string]any{})
		case "GET /api/v1/recurring_invoices/recurring-A":
			gets++
			a := b
			if gets > 1 {
				a.StatusID = "2"
			}
			recurringTestReply(t, w, map[string]any{"data": a})
		case "POST /api/v1/recurring_invoices/bulk":
			posts++
			raw, _ := io.ReadAll(r.Body)
			if string(raw) != `{"ids":["recurring-A"],"action":"start"}` {
				t.Errorf("payload %s", raw)
			}
			a := b
			a.StatusID = "2"
			recurringTestReply(t, w, map[string]any{"data": []RecurringRecord{a}})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	c := recurringTestClient(t, srv.URL, 1, 1)
	got, err := c.ResumeAuthorized(context.Background(), draft, func(context.Context) error { calls++; return nil })
	if err != nil || got.StatusID != "2" || gets != 2 || posts != 1 || calls != 1 {
		t.Fatalf("got=%+v err=%v gets=%d posts=%d calls=%d", got, err, gets, posts, calls)
	}
	if _, err = PrepareResume(func() RecurringRecord { x := b; x.StatusID = "2"; return x }(), d, receipt, "2040-02-01"); err == nil {
		t.Fatal("already active record accepted")
	}
}

func TestLifecycleRefusalsAndUnknownAreOneShot(t *testing.T) {
	d, b := lifecycleFixture(t, "2")
	changed := b
	changed.Amount = "999"
	changed.Balance = "999"
	wrongClient := b
	wrongClient.ClientID = "client-B"
	wrongMarker := b
	wrongMarker.PrivateNotes = "payplump:zzzzzzzzzzzzzzzzzzzzzzzz:recurring_create"
	wrongLines := b
	wrongLines.LineItems = append([]InvoiceLine(nil), b.LineItems...)
	wrongLines.LineItems[0].Cost = "1"
	wrongStatus := b
	wrongStatus.StatusID = "3"
	wrongSent := b
	wrongSent.LastSentDate = "2040-01-30 08:00:00"
	wrongDate := b
	wrongDate.NextSendDate = "2040-03-01"
	p, e := PreparePause(b, d)
	if e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		name        string
		before      RecurringRecord
		callback    error
		wantUnknown bool
	}{{"changed amount", changed, nil, false}, {"wrong client", wrongClient, nil, false}, {"wrong marker", wrongMarker, nil, false}, {"changed lines", wrongLines, nil, false}, {"wrong status", wrongStatus, nil, false}, {"changed last sent", wrongSent, nil, false}, {"changed schedule", wrongDate, nil, false}, {"permission error", b, errors.New("denied"), false}, {"negative POST", b, nil, true}} {
		t.Run(tc.name, func(t *testing.T) {
			var mutations, calls, gets int
			s := recurringTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET /api/v1/ping":
					recurringTestReply(t, w, map[string]any{})
				case "GET /api/v1/recurring_invoices/recurring-A":
					gets++
					recurringTestReply(t, w, map[string]any{"data": tc.before})
				case "POST /api/v1/recurring_invoices/bulk":
					mutations++
					http.Error(w, "negative", 500)
				default:
					http.NotFound(w, r)
				}
			})
			c := recurringTestClient(t, s.URL, 1, 1)
			_, err := c.PauseAuthorized(context.Background(), p, func(context.Context) error { calls++; return tc.callback })
			if tc.callback != nil {
				if err != tc.callback || mutations != 0 || calls != 1 {
					t.Fatalf("err=%v mutations=%d calls=%d", err, mutations, calls)
				}
			} else if tc.wantUnknown {
				if !errors.Is(err, ErrRecurringUnknown) || mutations != 1 || calls != 1 {
					t.Fatalf("err=%v mutations=%d calls=%d", err, mutations, calls)
				}
			} else if !errors.Is(err, ErrRecurringReceipt) || mutations != 0 || calls != 0 {
				t.Fatalf("err=%v mutations=%d calls=%d", err, mutations, calls)
			}
		})
	}
}
func TestLifecycleDraftStrictAndRedacted(t *testing.T) {
	d, b := lifecycleFixture(t, "2")
	p, e := PreparePause(b, d)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := json.Marshal(p)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(raw), b.PrivateNotes) {
		t.Fatal("journal JSON omitted trusted before-image")
	}
	if strings.Contains(p.String(), b.PrivateNotes) || strings.Contains(p.GoString(), b.PrivateNotes) {
		t.Fatal("format leaked private marker")
	}
	var out PauseDraft
	if e = json.Unmarshal(append(raw[:len(raw)-1], []byte(`,"extra":1}`)...), &out); e == nil {
		t.Fatal("unknown field accepted")
	}
	if e = json.Unmarshal([]byte(`{"before":{},"before":{},"template":{}}`), &out); e == nil {
		t.Fatal("duplicate field accepted")
	}
}
