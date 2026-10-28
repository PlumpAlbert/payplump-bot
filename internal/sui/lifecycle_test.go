package sui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestLifecycleDraftsFreezeAndRedact(t *testing.T) {
	r := managedTestRecord(t, 321)
	r.wire.Enable = true
	r.wire.Volume = 100
	r.wire.Up = 5
	r.wire.Config = json.RawMessage(fmt.Sprintf(`{"vless":{"name":%q,"uuid":"synthetic-secret","future":{"x":[1,true]}}}`, r.Name()))
	r, _ = ParseClientRecord(r.Raw())
	b := Binding{ID: r.ID(), Name: r.Name(), CredentialsHash: r.CredentialsHash()}
	d, e := PrepareDisable(r, b)
	if e != nil {
		t.Fatal(e)
	}
	encoded, e := json.Marshal(d)
	if e != nil {
		t.Fatal(e)
	}
	var restored DisableDraft
	if e = json.Unmarshal(encoded, &restored); e != nil || restored.Binding() != b {
		t.Fatal("disable journal roundtrip", e)
	}
	if strings.Contains(fmt.Sprintf("%v %#v", d, d), "synthetic-secret") {
		t.Fatal("draft leaked secret")
	}
	off := r
	off.wire.Enable = false
	c, e := PrepareCloseSessions(off, b)
	if e != nil {
		t.Fatal(e)
	}
	cb, _ := json.Marshal(c)
	var cr CloseSessionsDraft
	if json.Unmarshal(cb, &cr) != nil || cr.Binding() != b {
		t.Fatal("close journal roundtrip")
	}
	if _, e = PrepareDisable(off, b); !errors.Is(e, ErrManagedState) {
		t.Fatal("already-disabled before accepted")
	}
	if _, e = PrepareCloseSessions(r, b); !errors.Is(e, ErrManagedState) {
		t.Fatal("enabled close accepted")
	}
	if managedMessageSuccess([]byte(`{"success":true,"success":false}`)) == nil || managedMessageSuccess([]byte(`{"success":false}`)) == nil || managedMessageSuccess([]byte(`{"success":true,"msg":"secret"}`)) != nil {
		t.Fatal("close envelope validation")
	}
}

// The fake implements only the pinned GET and form POST surface; no production SUI client is used.
func TestLifecycleDisableAndCloseExactWireAndFinalAuthorization(t *testing.T) {
	r := managedTestRecord(t, 456)
	r.wire.Enable = true
	r.wire.Volume = 500
	r.wire.Up = 3
	r.wire.Down = 4
	r.wire.TotalUp = 12
	r.wire.TotalDown = 13
	r.wire.Links = []map[string]string{{"type": "sub", "uri": "https://invalid.example/sub", "x": "keep"}, {"type": "local", "uri": "stale"}}
	r.wire.Config = json.RawMessage(fmt.Sprintf(`{"vless":{"name":%q,"uuid":"invented-secret","future":{"n":1}}}`, r.Name()))
	r, _ = ParseClientRecord(r.Raw())
	bind := Binding{ID: r.ID(), Name: r.Name(), CredentialsHash: r.CredentialsHash()}
	disable, _ := PrepareDisable(r, bind)
	state := r
	calls := []string{}
	posts := 0
	var payload ClientRecord
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		calls = append(calls, q.Method+" "+q.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case q.Method == "GET" && strings.HasSuffix(q.URL.Path, "/clients"):
			fmt.Fprintf(w, `{"success":true,"obj":{"clients":[%s]}}`, state.Raw())
		case q.Method == "POST" && strings.HasSuffix(q.URL.Path, "/save"):
			posts++
			_ = q.ParseForm()
			form = q.PostForm
			var e error
			payload, e = ParseClientRecord([]byte(q.PostForm.Get("data")))
			if e != nil {
				t.Error(e)
			}
			state = payload
			fmt.Fprint(w, `{"success":true,"obj":{"clients":[]}}`)
		case q.Method == "POST" && strings.HasSuffix(q.URL.Path, "/closeSessions"):
			posts++
			_ = q.ParseForm()
			form = q.PostForm
			fmt.Fprint(w, `{"success":true,"msg":"ok"}`)
		default:
			t.Errorf("unexpected request %s", q.URL)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c, e := NewManagedClient(srv.URL, "token", "", time.Second, 100)
	if e != nil {
		t.Fatal(e)
	}
	authorized := 0
	got, e := c.DisableAuthorized(context.Background(), disable, func(context.Context) error { authorized++; return nil })
	if e != nil || got.Enabled() || authorized != 1 || posts != 1 {
		t.Fatalf("disable: %v", e)
	}
	if form.Get("object") != "clients" || form.Get("action") != "edit" || len(form) != 3 {
		t.Fatal("wrong edit form")
	}
	if !sameStatic(payload, state, false) || state.Enabled() {
		t.Fatal("disable changed frozen static fields")
	}
	if len(calls) != 4 || !strings.HasSuffix(calls[len(calls)-2], "/save") {
		t.Fatal("unexpected disable flow")
	}
	closeDraft, e := PrepareCloseSessions(state, bind)
	if e != nil {
		t.Fatal(e)
	}
	authorized = 0
	e = c.CloseSessionsAuthorized(context.Background(), closeDraft, func(context.Context) error {
		authorized++
		if !strings.HasSuffix(calls[len(calls)-1], "/clients") {
			t.Error("callback not after final full GET")
		}
		return nil
	})
	if e != nil || authorized != 1 || posts != 2 || len(calls) != 8 {
		t.Fatalf("close: %v", e)
	}
	if len(form) != 1 || form.Get("u") != bind.Name || form.Get("u") == fmt.Sprint(bind.ID) {
		t.Fatal("close must use exact name in u form")
	}
}

func TestLifecycleNoCallbackNoWrite(t *testing.T) {
	r := managedTestRecord(t, 789)
	r.wire.Enable = true
	r.wire.Up = 1
	r, _ = ParseClientRecord(r.Raw())
	b := Binding{ID: r.ID(), Name: r.Name(), CredentialsHash: r.CredentialsHash()}
	d, _ := PrepareDisable(r, b)
	p, c := newManagedPanel(t, r)
	if _, e := c.DisableAuthorized(context.Background(), d, nil); !errors.Is(e, ErrManagedInvalid) || p.countPosts() != 0 {
		t.Fatal("nil authorization allowed write")
	}
	errDenied := errors.New("denied")
	if _, e := c.DisableAuthorized(context.Background(), d, func(context.Context) error { return errDenied }); !errors.Is(e, errDenied) || p.countPosts() != 0 {
		t.Fatal("denial did not suppress write")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := c.DisableAuthorized(ctx, d, func(context.Context) error { return nil }); e == nil || p.countPosts() != 0 {
		t.Fatal("canceled authorization wrote")
	}
}

func lifecycleRecordWithName(t *testing.T, id int64, name string, enabled bool) ClientRecord {
	t.Helper()
	r := managedTestRecord(t, id)
	var config map[string]map[string]any
	if err := json.Unmarshal(r.wire.Config, &config); err != nil {
		t.Fatal("invalid synthetic config")
	}
	for _, values := range config {
		if _, ok := values["name"]; ok {
			values["name"] = name
		}
		if _, ok := values["username"]; ok {
			values["username"] = name
		}
	}
	r.wire.Name = name
	r.wire.Enable = enabled
	var err error
	r.wire.Config, err = json.Marshal(config)
	if err != nil {
		t.Fatal("could not encode synthetic config")
	}
	parsed, err := ParseClientRecord(r.Raw())
	if err != nil {
		t.Fatal("could not build named synthetic record", err)
	}
	return parsed
}

func TestLifecycleDraftsRoundTripBindingNameBoundaries(t *testing.T) {
	names := []struct {
		label string
		value string
	}{
		{"minimum", "a"},
		{"maximum-ascii", strings.Repeat("x", 256)},
		{"maximum-utf8-bytes", strings.Repeat("é", 128)},
	}
	for i, boundary := range names {
		name := boundary.value
		t.Run(boundary.label, func(t *testing.T) {
			id := int64(900 + 3*i)
			disabled := lifecycleRecordWithName(t, id, name, false)
			enableBinding := Binding{ID: id, Name: name, CredentialsHash: disabled.CredentialsHash()}
			enable, err := PrepareEnable(disabled, enableBinding)
			if err != nil {
				t.Fatal("valid boundary name refused by enable preparation", err)
			}
			raw, err := json.Marshal(enable)
			if err != nil {
				t.Fatal(err)
			}
			var restoredEnable EnableDraft
			if err = json.Unmarshal(raw, &restoredEnable); err != nil || restoredEnable.Binding() != enableBinding || !sameStatic(restoredEnable.before, disabled, false) {
				t.Fatal("enable boundary name failed JSON roundtrip", err)
			}

			enabled := lifecycleRecordWithName(t, id+1, name, true)
			disableBinding := Binding{ID: enabled.ID(), Name: name, CredentialsHash: enabled.CredentialsHash()}
			disable, err := PrepareDisable(enabled, disableBinding)
			if err != nil {
				t.Fatal("valid boundary name refused by disable preparation", err)
			}
			raw, err = json.Marshal(disable)
			if err != nil {
				t.Fatal(err)
			}
			var restoredDisable DisableDraft
			if err = json.Unmarshal(raw, &restoredDisable); err != nil || restoredDisable.Binding() != disableBinding || !sameStatic(restoredDisable.before, enabled, false) {
				t.Fatal("disable boundary name failed JSON roundtrip", err)
			}

			closeRecord := lifecycleRecordWithName(t, id+2, name, false)
			closeBinding := Binding{ID: closeRecord.ID(), Name: name, CredentialsHash: closeRecord.CredentialsHash()}
			closeDraft, err := PrepareCloseSessions(closeRecord, closeBinding)
			if err != nil {
				t.Fatal("valid boundary name refused by close-sessions preparation", err)
			}
			raw, err = json.Marshal(closeDraft)
			if err != nil {
				t.Fatal(err)
			}
			var restoredClose CloseSessionsDraft
			if err = json.Unmarshal(raw, &restoredClose); err != nil || restoredClose.Binding() != closeBinding || !sameStatic(restoredClose.before, closeRecord, false) {
				t.Fatal("close-sessions boundary name failed JSON roundtrip", err)
			}
		})
	}
}

func TestLifecycleDraftPreparationRejectsUnsupportedBindingName(t *testing.T) {
	name := "legacy client"
	disabled := lifecycleRecordWithName(t, 950, name, false)
	enabled := lifecycleRecordWithName(t, 951, name, true)
	for _, test := range []struct {
		name string
		r    ClientRecord
		prep func(ClientRecord, Binding) error
	}{
		{"enable", disabled, func(r ClientRecord, b Binding) error { _, err := PrepareEnable(r, b); return err }},
		{"disable", enabled, func(r ClientRecord, b Binding) error { _, err := PrepareDisable(r, b); return err }},
		{"close-sessions", disabled, func(r ClientRecord, b Binding) error { _, err := PrepareCloseSessions(r, b); return err }},
	} {
		t.Run(test.name, func(t *testing.T) {
			binding := Binding{ID: test.r.ID(), Name: test.r.Name(), CredentialsHash: test.r.CredentialsHash()}
			if err := test.prep(test.r, binding); !errors.Is(err, ErrManagedConflict) {
				t.Fatal("unsupported binding name was not rejected before preparing intent", err)
			}
		})
	}
}

func TestLifecycleDraftDecodersShareStrictEnvelopeChecks(t *testing.T) {
	r := managedTestRecord(t, 960)
	enable, err := PrepareEnable(r, Binding{ID: r.ID(), Name: r.Name(), CredentialsHash: r.CredentialsHash()})
	if err != nil {
		t.Fatal(err)
	}
	r.wire.Enable = true
	disable, err := PrepareDisable(r, Binding{ID: r.ID(), Name: r.Name(), CredentialsHash: r.CredentialsHash()})
	if err != nil {
		t.Fatal(err)
	}
	r.wire.Enable = false
	closeDraft, err := PrepareCloseSessions(r, Binding{ID: r.ID(), Name: r.Name(), CredentialsHash: r.CredentialsHash()})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		raw  []byte
		read func([]byte) error
	}{
		{"enable", mustMarshalDraft(t, enable), func(raw []byte) error { var d EnableDraft; return json.Unmarshal(raw, &d) }},
		{"disable", mustMarshalDraft(t, disable), func(raw []byte) error { var d DisableDraft; return json.Unmarshal(raw, &d) }},
		{"close-sessions", mustMarshalDraft(t, closeDraft), func(raw []byte) error { var d CloseSessionsDraft; return json.Unmarshal(raw, &d) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			badInputs := [][]byte{
				changeRaw(t, test.raw, func(fields map[string]json.RawMessage) { delete(fields, "binding") }),
				changeRaw(t, test.raw, func(fields map[string]json.RawMessage) { fields["future"] = json.RawMessage(`0`) }),
				[]byte(strings.Replace(string(test.raw), `,"binding":`, `,"before":null,"binding":`, 1)),
			}
			tooLarge := append([]byte{'{'}, bytes.Repeat([]byte{' '}, maxBindingDraftSize-len(test.raw)+1)...)
			tooLarge = append(tooLarge, test.raw[1:]...)
			badInputs = append(badInputs, tooLarge)
			for i, raw := range badInputs {
				if err := test.read(raw); err == nil {
					t.Fatalf("accepted malformed shared envelope case %d", i)
				}
			}
		})
	}
}

func mustMarshalDraft(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestLifecycleFinalFreshRecordRecheckSurvivesVersionProbeRemoval(t *testing.T) {
	for _, operation := range []string{"disable", "close"} {
		t.Run(operation, func(t *testing.T) {
			r := managedTestRecord(t, 123)
			r.wire.Enable = operation == "disable"
			binding := Binding{ID: r.ID(), Name: r.Name(), CredentialsHash: r.CredentialsHash()}
			p, c := newManagedPanel(t, r)
			reads := 0
			p.fullHook = func(records []json.RawMessage) []json.RawMessage {
				reads++
				if reads == 2 {
					changed := r
					changed.wire.Remark = "external static change"
					return []json.RawMessage{changed.Raw()}
				}
				return records
			}
			authorized := 0
			authorize := func(context.Context) error { authorized++; return nil }
			var err error
			if operation == "disable" {
				draft, e := PrepareDisable(r, binding)
				if e != nil {
					t.Fatal(e)
				}
				_, err = c.DisableAuthorized(context.Background(), draft, authorize)
			} else {
				draft, e := PrepareCloseSessions(r, binding)
				if e != nil {
					t.Fatal(e)
				}
				err = c.CloseSessionsAuthorized(context.Background(), draft, authorize)
			}
			if !errors.Is(err, ErrManagedConflict) || authorized != 0 || p.countPosts() != 0 || reads != 2 {
				t.Fatal("final record drift did not stop authorization and write", err)
			}
		})
	}
}
