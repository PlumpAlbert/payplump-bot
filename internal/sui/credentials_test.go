package sui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestManagedDraftCSPRNGSchemaAndPersistence(t *testing.T) {
	seenNames := map[string]bool{}
	seenCredentials := map[[32]byte]bool{}
	for i := 0; i < 32; i++ {
		ids := []int64{7, 9}
		d, err := NewDisabledDraft(ids)
		if err != nil {
			t.Fatal("generation failed")
		}
		ids[0] = 999
		if d.record.wire.Inbounds[0] != 7 || validateCreate(d.record) != nil {
			t.Fatal("invalid generated draft")
		}
		if seenNames[d.Name()] || seenCredentials[d.CredentialsHash()] {
			t.Fatal("generated credential/name reused")
		}
		seenNames[d.Name()] = true
		seenCredentials[d.CredentialsHash()] = true
		raw, err := json.Marshal(d)
		if err != nil {
			t.Fatal("serialization failed")
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil || len(m) != 20 || m["id"] != nil {
			t.Fatal("new record must omit id and contain all other fields")
		}
		if string(m["enable"]) != "false" || string(m["expiry"]) != "0" || string(m["volume"]) != "0" {
			t.Fatal("new record grants access or limits")
		}
		var cfg map[string]map[string]any
		_ = json.Unmarshal(m["config"], &cfg)
		if len(cfg) != 15 || cfg["vmess"]["alterId"] != float64(0) || cfg["vless"]["flow"] != "xtls-rprx-vision" {
			t.Fatal("frontend protocol schema mismatch")
		}
		uuid, _ := cfg["vmess"]["uuid"].(string)
		if !validUUID4(uuid) || uuid != cfg["vless"]["uuid"] || uuid != cfg["tuic"]["uuid"] {
			t.Fatal("UUIDv4 sharing mismatch")
		}
		password, _ := cfg["mixed"]["password"].(string)
		for _, p := range []string{"socks", "http", "anytls", "trojan", "naive", "hysteria2", "tuic"} {
			if cfg[p]["password"] != password || len(password) < 32 {
				t.Fatal("password sharing/length mismatch")
			}
		}
		if cfg["hysteria"]["auth_str"] != password || cfg["shadowtls"]["password"] != cfg["shadowsocks"]["password"] {
			t.Fatal("frontend shared credential mismatch")
		}
		var restored CreateDraft
		if json.Unmarshal(raw, &restored) != nil || restored.Name() != d.Name() || restored.CredentialsHash() != d.CredentialsHash() {
			t.Fatal("create intent persistence failed")
		}
		again, _ := json.Marshal(restored)
		if !bytes.Equal(raw, again) {
			t.Fatal("create intent changed during persistence")
		}
		for _, format := range []string{"%v", "%+v", "%#v"} {
			if strings.Contains(fmt.Sprintf(format, d), password) {
				t.Fatal("draft formatting leaked secret")
			}
		}
	}
	for _, ids := range [][]int64{nil, {}, {0}, {-1}, {7, 7}} {
		if _, err := NewDisabledDraft(ids); err == nil {
			t.Fatal("accepted invalid inbound IDs")
		}
	}
}

func TestManagedPrepareEnableBindingsAndPolicies(t *testing.T) {
	r := managedTestRecord(t, 123)
	binding := Binding{ID: r.ID(), Name: r.Name(), CredentialsHash: r.CredentialsHash()}
	d, err := PrepareEnable(r, binding)
	if err != nil {
		t.Fatal("valid binding rejected")
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal("serialization failed")
	}
	var restored EnableDraft
	if json.Unmarshal(raw, &restored) != nil || !sameStatic(restored.before, r, false) || restored.binding != binding {
		t.Fatal("enable intent persistence failed")
	}
	for _, change := range []func(*Binding){
		func(b *Binding) { b.ID = 0 }, func(b *Binding) { b.ID++ }, func(b *Binding) { b.Name = "foreign" }, func(b *Binding) { b.CredentialsHash[0] ^= 1 },
	} {
		b := binding
		change(&b)
		if _, err := PrepareEnable(r, b); err == nil {
			t.Fatal("accepted foreign binding")
		}
	}
	for _, change := range []func(*clientWire){
		func(w *clientWire) { w.Enable = true }, func(w *clientWire) { w.DelayStart = true }, func(w *clientWire) { w.AutoReset = true }, func(w *clientWire) { w.ResetDays = 1 }, func(w *clientWire) { w.NextReset = 1 },
	} {
		next, _ := cloneRecord(r, false)
		change(&next.wire)
		if _, err := PrepareEnable(next, binding); err == nil {
			t.Fatal("accepted unsupported state")
		}
	}
	// Quotas and expiry are frozen, not silently zeroed.
	r.wire.Volume = 12345
	r.wire.Expiry = 23456
	if _, err := PrepareEnable(r, binding); err == nil {
		t.Fatal("zero counters could reset quota usage")
	}
	r.wire.Up = 1
	if _, err := PrepareEnable(r, binding); err != nil {
		t.Fatal("fixed quota/expiry rejected")
	}
	if _, err := PrepareEnable(ClientRecord{}, binding); err == nil {
		t.Fatal("accepted partial zero record")
	}
}

func TestManagedRejectTamperedCreateIntent(t *testing.T) {
	d, _ := NewDisabledDraft([]int64{7})
	raw, _ := json.Marshal(d)
	for _, mutate := range []func(map[string]json.RawMessage){
		func(m map[string]json.RawMessage) { m["enable"] = json.RawMessage(`true`) },
		func(m map[string]json.RawMessage) { m["id"] = json.RawMessage(`123`) },
		func(m map[string]json.RawMessage) { m["volume"] = json.RawMessage(`123`) },
		func(m map[string]json.RawMessage) { m["autoReset"] = json.RawMessage(`true`) },
		func(m map[string]json.RawMessage) { m["config"] = json.RawMessage(`{}`) },
	} {
		var restored CreateDraft
		if json.Unmarshal(changeRaw(t, raw, mutate), &restored) == nil {
			t.Fatal("accepted tampered create intent")
		}
	}
}
