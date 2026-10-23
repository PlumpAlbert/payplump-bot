package sui

import (
	"bytes"
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
func TestBindingJSONRequiresExactByteArray(t *testing.T) {
	var hash [32]byte
	for i := range hash {
		hash[i] = byte(i)
	}
	valid, err := json.Marshal(Binding{ID: 7, Name: "synthetic", CredentialsHash: hash})
	if err != nil {
		t.Fatal(err)
	}
	invalidArray := func(index int, value string) string {
		var values [32]string
		for i := range values {
			values[i] = "0"
		}
		values[index] = value
		return "[" + strings.Join(values[:], ",") + "]"
	}
	oneArray := func(size int) string {
		return "[" + strings.TrimSuffix(strings.Repeat("1,", size), ",") + "]"
	}
	cases := []struct {
		name string
		hash string
	}{
		{"short", oneArray(31)},
		{"long", oneArray(33)},
		{"null-hash", "null"},
		{"null-element", invalidArray(3, "null")},
		{"fraction", invalidArray(3, "1.5")},
		{"fractional-spelling", invalidArray(3, "1.0")},
		{"exponent", invalidArray(3, "1e0")},
		{"overflow", invalidArray(3, "256")},
		{"negative", invalidArray(3, "-1")},
		{"zero-fingerprint", "[" + strings.TrimSuffix(strings.Repeat("0,", 32), ",") + "]"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			raw := []byte(`{"ID":7,"Name":"synthetic","CredentialsHash":` + test.hash + `}`)
			var decoded Binding
			if json.Unmarshal(raw, &decoded) == nil {
				t.Fatal("accepted malformed credential fingerprint")
			}
		})
	}
	var decoded Binding
	if err := json.Unmarshal(valid, &decoded); err != nil || decoded.ID != 7 || decoded.Name != "synthetic" || decoded.CredentialsHash != hash {
		t.Fatal("valid 32-byte fingerprint rejected", err)
	}
	encoded, err := json.Marshal(decoded)
	if err != nil || !bytes.Equal(valid, encoded) {
		t.Fatal("valid fingerprint changed during roundtrip", err)
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
