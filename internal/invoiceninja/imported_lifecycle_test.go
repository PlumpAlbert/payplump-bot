package invoiceninja

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func importedFixture(t *testing.T, status string) (ImportedRecurringProfile, RecurringRecord) {
	t.Helper()
	_, r := lifecycleFixture(t, status)
	r.PrivateNotes = "legacy customer note"
	r.Amount, r.Balance = r.LineItems[0].Cost, r.LineItems[0].Cost
	r.IsAmountDiscount = true
	r.LineItems[0].IsAmountDiscount = true
	r.LineItems[0].TaxName1, r.LineItems[0].TaxName2, r.LineItems[0].TaxName3 = "legacy VAT", "legacy local", ""
	r.LineItems[0].TaxRate1, r.LineItems[0].TaxRate2, r.LineItems[0].TaxRate3 = "0", "0", "0"
	r.LineItems[0].TaxID = "legacy-tax-id"
	r.LastSentDate = "2040-01-31 08:00:00"
	p, err := NewImportedRecurringProfile(r, "monthly")
	if err != nil {
		t.Fatal(err)
	}
	return p, r
}

func mutateJournalObject(t *testing.T, raw []byte, parent, field string, value json.RawMessage) []byte {
	t.Helper()
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(root[parent], &obj); err != nil {
		t.Fatal(err)
	}
	obj[field] = value
	root[parent], _ = json.Marshal(obj)
	out, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func mutateJournalLine(t *testing.T, raw []byte, parent, field string, value json.RawMessage) []byte {
	t.Helper()
	var root map[string]json.RawMessage
	_ = json.Unmarshal(raw, &root)
	var obj map[string]json.RawMessage
	_ = json.Unmarshal(root[parent], &obj)
	var lines []json.RawMessage
	_ = json.Unmarshal(obj["line_items"], &lines)
	var line map[string]json.RawMessage
	_ = json.Unmarshal(lines[0], &line)
	line[field] = value
	lines[0], _ = json.Marshal(line)
	obj["line_items"], _ = json.Marshal(lines)
	root[parent], _ = json.Marshal(obj)
	out, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestImportedJournalRequiresExactNestedV1Shapes(t *testing.T) {
	p, b := importedFixture(t, "2")
	profileJSON, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	pause, err := PrepareImportedPause(p, b)
	if err != nil {
		t.Fatal(err)
	}
	draftJSON, err := json.Marshal(pause)
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{
		"profile baseline unknown":     mutateJournalObject(t, profileJSON, "baseline", "unexpected", json.RawMessage(`true`)),
		"profile baseline case alias":  mutateJournalObject(t, profileJSON, "baseline", "ID", json.RawMessage(`"recurring-A"`)),
		"draft before unknown":         mutateJournalObject(t, draftJSON, "before", "unexpected", json.RawMessage(`true`)),
		"draft before line unknown":    mutateJournalLine(t, draftJSON, "before", "unexpected", json.RawMessage(`true`)),
		"draft before line case alias": mutateJournalLine(t, draftJSON, "before", "PRODUCT_KEY", json.RawMessage(`"monthly"`)),
	} {
		t.Run(name, func(t *testing.T) {
			if strings.HasPrefix(name, "profile") {
				var got ImportedRecurringProfile
				if json.Unmarshal(raw, &got) == nil {
					t.Fatal("accepted noncanonical profile baseline")
				}
				return
			}
			var got ImportedPauseDraft
			if json.Unmarshal(raw, &got) == nil {
				t.Fatal("accepted noncanonical draft before-image")
			}
		})
	}
	duplicateProfile := strings.Replace(string(profileJSON), `"id":"recurring-A"`, `"id":"recurring-A","id":"recurring-A"`, 1)
	var gotProfile ImportedRecurringProfile
	if json.Unmarshal([]byte(duplicateProfile), &gotProfile) == nil {
		t.Fatal("accepted duplicate baseline key")
	}
	duplicateDraft := string(draftJSON)
	idx := strings.LastIndex(duplicateDraft, `"id":"recurring-A"`)
	if idx < 0 {
		t.Fatal("canonical before id missing")
	}
	duplicateDraft = duplicateDraft[:idx] + `"id":"recurring-A","id":"recurring-A"` + duplicateDraft[idx+len(`"id":"recurring-A"`):]
	var gotDraft ImportedPauseDraft
	if json.Unmarshal([]byte(duplicateDraft), &gotDraft) == nil {
		t.Fatal("accepted duplicate before key")
	}
	var profileRoundTrip ImportedRecurringProfile
	if err := json.Unmarshal(profileJSON, &profileRoundTrip); err != nil || profileRoundTrip.Hash() != p.Hash() {
		t.Fatalf("canonical profile roundtrip: %v", err)
	}
	if err := json.Unmarshal(draftJSON, &gotDraft); err != nil || gotDraft.ProfileHash() != p.Hash() {
		t.Fatalf("canonical draft roundtrip: %v", err)
	}
}

func TestImportedProfileKeepsLegacyFlagsAndRejectsStaticDrift(t *testing.T) {
	p, r := importedFixture(t, "2")
	if _, err := PrepareImportedPause(p, r); err != nil {
		t.Fatalf("legacy zero-discount flags rejected: %v", err)
	}
	for name, change := range map[string]func(*RecurringRecord){
		"tax":                   func(x *RecurringRecord) { x.TaxRate1 = "1" },
		"surcharge":             func(x *RecurringRecord) { x.CustomSurcharge1 = "1" },
		"line tax":              func(x *RecurringRecord) { x.LineItems[0].TaxRate1 = "1" },
		"line notes":            func(x *RecurringRecord) { x.LineItems[0].Notes = "different" },
		"invoice discount mode": func(x *RecurringRecord) { x.IsAmountDiscount = false },
		"line discount mode":    func(x *RecurringRecord) { x.LineItems[0].IsAmountDiscount = false },
	} {
		t.Run(name, func(t *testing.T) {
			changed := r
			changed.LineItems = append([]InvoiceLine(nil), r.LineItems...)
			change(&changed)
			if _, err := PrepareImportedPause(p, changed); err == nil {
				t.Fatal("static drift accepted")
			}
		})
	}
	if p.String() == r.PrivateNotes || p.GoString() == r.PrivateNotes {
		t.Fatal("profile leaked")
	}
	d := recurringTestDraft(t)
	d.PrivateNotes = r.PrivateNotes
	if ValidateRecurringDraft(d) == nil {
		t.Fatal("owned draft accepted imported legacy notes")
	}
}

func TestImportedFreshGETRejectsProfileDriftBeforeAuthorization(t *testing.T) {
	p, b := importedFixture(t, "2")
	d, err := PrepareImportedPause(p, b)
	if err != nil {
		t.Fatal(err)
	}
	var callbacks, writes int
	srv := recurringTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/ping":
			recurringTestReply(t, w, map[string]any{})
		case "GET /api/v1/recurring_invoices/recurring-A":
			x := b
			x.TaxRate1 = "1"
			recurringTestReply(t, w, map[string]any{"data": x})
		case "POST /api/v1/recurring_invoices/bulk":
			writes++
		default:
			http.NotFound(w, r)
		}
	})
	c, _ := NewImportedRecurringClient(srv.URL, recurringTestToken, time.Second, 1, 1)
	_, err = c.PauseAuthorized(context.Background(), d, func(context.Context) error { callbacks++; return nil })
	if !errors.Is(err, ErrRecurringReceipt) || callbacks != 0 || writes != 0 {
		t.Fatalf("err=%v callbacks=%d writes=%d", err, callbacks, writes)
	}
}

func TestImportedElapsedJournalsDecodeButWritesRemainDenied(t *testing.T) {
	today := time.Now().In(time.Local)
	yesterday := today.AddDate(0, 0, -1).Format(time.DateOnly)
	t.Run("schedule", func(t *testing.T) {
		p, b := importedFixture(t, "3")
		d := ImportedScheduleDraft{profile: p, before: b, date: yesterday}
		raw, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		var decoded ImportedScheduleDraft
		if err = json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("elapsed schedule journal decode: %v", err)
		}
		c, _ := NewImportedRecurringClient("http://127.0.0.1:1", recurringTestToken, time.Second, 1, 1)
		if _, err = c.RescheduleAuthorized(context.Background(), decoded, func(context.Context) error { t.Fatal("authorized expired schedule"); return nil }); !errors.Is(err, ErrRecurringExpired) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("resume", func(t *testing.T) {
		_, b := importedFixture(t, "3")
		b.NextSendDate = yesterday
		b.NextSendDatetime = time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
		p, err := NewImportedRecurringProfile(b, "monthly")
		if err != nil {
			t.Fatal(err)
		}
		receipt := RecurringReceipt{Amount: b.Amount, NextSendDatetime: b.NextSendDatetime}
		d := ImportedResumeDraft{profile: p, before: b, receipt: receipt}
		raw, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		var decoded ImportedResumeDraft
		if err = json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("elapsed resume journal decode: %v", err)
		}
		bad := strings.Replace(string(raw), `"NextSendDatetime":`, `"unknown":true,"NextSendDatetime":`, 1)
		if json.Unmarshal([]byte(bad), &decoded) == nil {
			t.Fatal("unknown receipt key accepted")
		}
		c, _ := NewImportedRecurringClient("http://127.0.0.1:1", recurringTestToken, time.Second, 1, 1)
		if _, err = c.ResumeAuthorized(context.Background(), decoded, func(context.Context) error { t.Fatal("authorized expired resume"); return nil }); !errors.Is(err, ErrRecurringExpired) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("date today but UTC future", func(t *testing.T) {
		_, b := importedFixture(t, "3")
		now := time.Now().In(time.Local)
		b.NextSendDate = now.Format(time.DateOnly)
		b.NextSendDatetime = now.UTC().Add(time.Hour).Format(time.RFC3339Nano)
		p, err := NewImportedRecurringProfile(b, "monthly")
		if err != nil {
			t.Fatal(err)
		}
		receipt := RecurringReceipt{Amount: b.Amount, NextSendDatetime: b.NextSendDatetime}
		if _, err = PrepareImportedResume(p, b, receipt); !errors.Is(err, ErrRecurringExpired) {
			t.Fatalf("Prepare accepted same-day date: %v", err)
		}
		d := ImportedResumeDraft{profile: p, before: b, receipt: receipt}
		c, _ := NewImportedRecurringClient("http://127.0.0.1:1", recurringTestToken, time.Second, 1, 1)
		c.base.now = func() time.Time { return now }
		if _, err = c.ResumeAuthorized(context.Background(), d, func(context.Context) error { t.Fatal("authorized same-day resume"); return nil }); !errors.Is(err, ErrRecurringExpired) {
			t.Fatalf("writer accepted same-day date: %v", err)
		}
	})
}

func TestImportedScheduleAndResumeHappyPaths(t *testing.T) {
	t.Run("schedule", func(t *testing.T) {
		p, b := importedFixture(t, "3")
		d, err := PrepareImportedSchedule(p, b, "2040-03-31", "2040-02-01")
		if err != nil {
			t.Fatal(err)
		}
		var gets, puts, calls int
		srv := recurringTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.Method + " " + r.URL.Path {
			case "GET /api/v1/ping":
				recurringTestReply(t, w, map[string]any{})
			case "GET /api/v1/recurring_invoices/recurring-A":
				gets++
				x := b
				if gets > 1 {
					x.NextSendDate = "2040-03-31"
					x.NextSendDatetime = "2040-03-31T08:00:00Z"
				}
				recurringTestReply(t, w, map[string]any{"data": x})
			case "PUT /api/v1/recurring_invoices/recurring-A":
				puts++
				var fields map[string]json.RawMessage
				_ = json.NewDecoder(r.Body).Decode(&fields)
				if len(fields) != 2 {
					t.Errorf("unexpected PUT fields: %v", fields)
				}
				x := b
				x.NextSendDate = "2040-03-31"
				x.NextSendDatetime = "2040-03-31T08:00:00Z"
				recurringTestReply(t, w, map[string]any{"data": x})
			default:
				http.NotFound(w, r)
			}
		})
		c, _ := NewImportedRecurringClient(srv.URL, recurringTestToken, time.Second, 1, 1)
		c.base.now = func() time.Time { return time.Date(2040, 1, 1, 12, 0, 0, 0, time.Local) }
		got, err := c.RescheduleAuthorized(context.Background(), d, func(context.Context) error { calls++; return nil })
		if err != nil || got.NextSendDate != "2040-03-31" || gets != 2 || puts != 1 || calls != 1 {
			t.Fatalf("got=%+v err=%v gets=%d puts=%d calls=%d", got, err, gets, puts, calls)
		}
	})
	t.Run("resume", func(t *testing.T) {
		p, b := importedFixture(t, "3")
		receipt := RecurringReceipt{Amount: b.Amount, NextSendDatetime: b.NextSendDatetime}
		d, err := PrepareImportedResume(p, b, receipt)
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
				x := b
				if gets > 1 {
					x.StatusID = "2"
				}
				recurringTestReply(t, w, map[string]any{"data": x})
			case "POST /api/v1/recurring_invoices/bulk":
				posts++
				x := b
				x.StatusID = "2"
				recurringTestReply(t, w, map[string]any{"data": []RecurringRecord{x}})
			default:
				http.NotFound(w, r)
			}
		})
		c, _ := NewImportedRecurringClient(srv.URL, recurringTestToken, time.Second, 1, 1)
		c.base.now = func() time.Time { return time.Date(2040, 1, 1, 12, 0, 0, 0, time.Local) }
		got, err := c.ResumeAuthorized(context.Background(), d, func(context.Context) error { calls++; return nil })
		if err != nil || got.StatusID != "2" || gets != 2 || posts != 1 || calls != 1 {
			t.Fatalf("got=%+v err=%v gets=%d posts=%d calls=%d", got, err, gets, posts, calls)
		}
	})
}

func TestImportedPauseAuthorizedAndReceipt(t *testing.T) {
	p, b := importedFixture(t, "2")
	d, err := PrepareImportedPause(p, b)
	if err != nil {
		t.Fatal(err)
	}
	var gets, writes, calls int
	srv := recurringTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/ping":
			recurringTestReply(t, w, map[string]any{})
		case "GET /api/v1/recurring_invoices/recurring-A":
			gets++
			x := b
			if gets == 2 {
				x.StatusID = "3"
			}
			recurringTestReply(t, w, map[string]any{"data": x})
		case "POST /api/v1/recurring_invoices/bulk":
			writes++
			x := b
			x.StatusID = "3"
			recurringTestReply(t, w, map[string]any{"data": []RecurringRecord{x}})
		default:
			http.NotFound(w, r)
		}
	})
	c, _ := NewImportedRecurringClient(srv.URL, recurringTestToken, time.Second, 1, 1)
	got, err := c.PauseAuthorized(context.Background(), d, func(context.Context) error {
		calls++
		if gets != 1 || writes != 0 {
			t.Errorf("authorization timing")
		}
		return nil
	})
	if err != nil || got.StatusID != "3" || gets != 2 || writes != 1 || calls != 1 {
		t.Fatalf("got=%+v err=%v gets=%d writes=%d calls=%d", got, err, gets, writes, calls)
	}
}
