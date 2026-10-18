package sui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func managedTestRecord(t *testing.T, id int64) ClientRecord {
	t.Helper()
	d, err := NewDisabledDraft([]int64{7})
	if err != nil {
		t.Fatal("draft generation failed")
	}
	r := d.record
	r.wire.ID = id
	return r
}
func changeRaw(t *testing.T, raw []byte, change func(map[string]json.RawMessage)) []byte {
	t.Helper()
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		t.Fatal("invalid test fixture")
	}
	change(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal("invalid test fixture")
	}
	return b
}

func TestManagedRecordStrictSchema(t *testing.T) {
	r := managedTestRecord(t, 123)
	for _, key := range recordKeys {
		t.Run("missing-"+key, func(t *testing.T) {
			raw := changeRaw(t, r.Raw(), func(m map[string]json.RawMessage) { delete(m, key) })
			if _, err := ParseClientRecord(raw); err == nil {
				t.Fatal("accepted missing full field")
			}
		})
		t.Run("null-"+key, func(t *testing.T) {
			raw := changeRaw(t, r.Raw(), func(m map[string]json.RawMessage) { m[key] = json.RawMessage(`null`) })
			if _, err := ParseClientRecord(raw); err == nil {
				t.Fatal("accepted null full field")
			}
		})
	}
	cases := []struct {
		name string
		raw  []byte
	}{
		{"unknown-top", changeRaw(t, r.Raw(), func(m map[string]json.RawMessage) { m["future"] = json.RawMessage(`0`) })},
		{"unknown-instead-known", changeRaw(t, r.Raw(), func(m map[string]json.RawMessage) { delete(m, "group"); m["future"] = json.RawMessage(`0`) })},
		{"duplicate-top", append([]byte(`{"id":123,`), r.Raw()[1:]...)},
		{"duplicate-config", changeRaw(t, r.Raw(), func(m map[string]json.RawMessage) {
			m["config"] = json.RawMessage(`{"vless":{"uuid":"invented","extra":{"x":1,"x":2}}}`)
		})},
		{"duplicate-link", changeRaw(t, r.Raw(), func(m map[string]json.RawMessage) {
			m["links"] = json.RawMessage(`[{"type":"external","uri":"a","uri":"b"}]`)
		})},
		{"identity", changeRaw(t, r.Raw(), func(m map[string]json.RawMessage) { m["config"] = json.RawMessage(`{"vless":{"name":"foreign"}}`) })},
		{"username-identity", changeRaw(t, r.Raw(), func(m map[string]json.RawMessage) { m["config"] = json.RawMessage(`{"mixed":{"username":"foreign"}}`) })},
		{"both-identities", changeRaw(t, r.Raw(), func(m map[string]json.RawMessage) {
			m["config"] = json.RawMessage(fmt.Sprintf(`{"mixed":{"name":%q,"username":"foreign"}}`, r.Name()))
		})},
		{"config-array", changeRaw(t, r.Raw(), func(m map[string]json.RawMessage) { m["config"] = json.RawMessage(`[]`) })},
		{"config-nonobject", changeRaw(t, r.Raw(), func(m map[string]json.RawMessage) { m["config"] = json.RawMessage(`{"extension":[]}`) })},
		{"link-null", changeRaw(t, r.Raw(), func(m map[string]json.RawMessage) { m["links"] = json.RawMessage(`[null]`) })},
		{"link-null-value", changeRaw(t, r.Raw(), func(m map[string]json.RawMessage) { m["links"] = json.RawMessage(`[{"uri":null}]`) })},
		{"duplicate-inbounds", changeRaw(t, r.Raw(), func(m map[string]json.RawMessage) { m["inbounds"] = json.RawMessage(`[7,7]`) })},
		{"negative-inbound", changeRaw(t, r.Raw(), func(m map[string]json.RawMessage) { m["inbounds"] = json.RawMessage(`[-7]`) })},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseClientRecord(test.raw); err == nil {
				t.Fatal("accepted unsafe full record")
			}
		})
	}
}

func TestManagedConfigNumbers(t *testing.T) {
	r := managedTestRecord(t, 123)
	for _, number := range []string{"1.5", "9007199254740992", "9007199254740993", "-9007199254740992", "-9007199254740993", "1e100", "1e-100", "0e999999999", "0e-999999999"} {
		t.Run(number, func(t *testing.T) {
			raw := changeRaw(t, r.Raw(), func(m map[string]json.RawMessage) {
				m["config"] = json.RawMessage(`{"extension":{"nested":[{"n":` + number + `}]}}`)
			})
			if _, err := ParseClientRecord(raw); err == nil {
				t.Fatal("accepted unsafe config number")
			}
		})
	}
	for _, number := range []string{"0", "1.0", "1e2", "-9007199254740991", "9007199254740991"} {
		t.Run("safe-"+number, func(t *testing.T) {
			raw := changeRaw(t, r.Raw(), func(m map[string]json.RawMessage) {
				m["config"] = json.RawMessage(`{"extension":{"nested":[` + number + `]}}`)
			})
			if _, err := ParseClientRecord(raw); err != nil {
				t.Fatal("rejected exact config integer")
			}
		})
	}
	// Top-level int64 fields do NOT pass through setConfigIdentity/float64.
	raw := changeRaw(t, r.Raw(), func(m map[string]json.RawMessage) { m["volume"] = json.RawMessage(`9007199254740993`) })
	parsed, err := ParseClientRecord(raw)
	if err != nil || parsed.wire.Volume != 9007199254740993 {
		t.Fatal("rounded top-level integer")
	}
}

func TestManagedRecordPreservationAndOwnership(t *testing.T) {
	r := managedTestRecord(t, 123)
	raw := changeRaw(t, r.Raw(), func(m map[string]json.RawMessage) {
		m["config"] = json.RawMessage(fmt.Sprintf(`{"vless":{"name":%q,"uuid":"invented-uuid","future":{"array":[1e2,true,null,"invented-secret"],"object":{"key":"value"}}}}`, r.Name()))
		m["links"] = json.RawMessage(`[{"type":"external","uri":"https://invalid.example/synthetic","future":"keep"},{"type":"sub","uri":"https://invalid.example/never-fetch"},{"type":"local","uri":"synthetic-local"}]`)
	})
	parsed, err := ParseClientRecord(raw)
	if err != nil {
		t.Fatal("valid extension rejected")
	}
	before := parsed.Raw()
	hash := parsed.CredentialsHash()
	for i := range raw {
		raw[i] = 'x'
	}
	returned := parsed.Raw()
	for i := range returned {
		returned[i] = 'x'
	}
	ids := parsed.InboundIDs()
	ids[0] = 999
	if !bytes.Equal(before, parsed.Raw()) || parsed.CredentialsHash() != hash || parsed.InboundIDs()[0] != 7 {
		t.Fatal("record aliases caller storage")
	}
	var restored ClientRecord
	if json.Unmarshal(before, &restored) != nil || !sameStatic(parsed, restored, false) {
		t.Fatal("record persistence failed")
	}
	changed := changeRaw(t, before, func(m map[string]json.RawMessage) {
		m["config"] = json.RawMessage(fmt.Sprintf(`{"vless":{"name":%q,"uuid":"invented-uuid","future":{"array":[100,true,null,"changed"],"object":{"key":"value"}}}}`, parsed.Name()))
	})
	next, err := ParseClientRecord(changed)
	if err != nil || next.CredentialsHash() == hash {
		t.Fatal("fingerprint omitted unknown config keys")
	}
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		if strings.Contains(fmt.Sprintf(format, parsed), "invented-secret") || strings.Contains(fmt.Sprintf(format, &parsed), "invented-secret") {
			t.Fatal("formatting disclosed a secret")
		}
	}
}
