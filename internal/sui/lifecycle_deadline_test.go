package sui

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestSUILifecycleCancellationAfterPermissionIsUnknown(t *testing.T) {
	for _, closeOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "disable", true: "close"}[closeOnly], func(t *testing.T) {
			r := managedTestRecord(t, 701)
			r.wire.Enable = !closeOnly
			r.wire.Up = 1
			b := Binding{ID: r.ID(), Name: r.Name(), CredentialsHash: r.CredentialsHash()}
			panel, c := newManagedPanel(t, r)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			permission := func(context.Context) error { cancel(); return nil }
			var err error
			if closeOnly {
				draft, e := PrepareCloseSessions(r, b)
				if e != nil {
					t.Fatal(e)
				}
				err = c.CloseSessionsAuthorized(ctx, draft, permission)
			} else {
				draft, e := PrepareDisable(r, b)
				if e != nil {
					t.Fatal(e)
				}
				_, err = c.DisableAuthorized(ctx, draft, permission)
			}
			if !errors.Is(err, ErrManagedUnknown) || !errors.Is(err, context.Canceled) || panel.countPosts() != 0 {
				t.Fatal("granted cancellation lost one-shot hold or transmitted")
			}
		})
	}
}
func TestBindingJournalRejectsAliasesAndUnknownFacts(t *testing.T) {
	r := managedTestRecord(t, 702)
	b := Binding{ID: r.ID(), Name: r.Name(), CredentialsHash: r.CredentialsHash()}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var valid Binding
	if json.Unmarshal(raw, &valid) != nil || valid != b {
		t.Fatal("canonical binding rejected")
	}
	for _, extra := range []string{`,"id":702}`, `,"name":"synthetic-other"}`, `,"future":1}`} {
		bad := string(raw[:len(raw)-1]) + extra
		var out Binding
		if json.Unmarshal([]byte(bad), &out) == nil {
			t.Fatal("ambiguous binding identity accepted")
		}
	}
	r.wire.Enable = true
	draft, err := PrepareDisable(r, b)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(draft)
	bad := strings.Replace(string(encoded), `"ID":702`, `"ID":1,"id":702`, 1)
	var out DisableDraft
	if json.Unmarshal([]byte(bad), &out) == nil {
		t.Fatal("draft erased contradictory binding identity")
	}
}
