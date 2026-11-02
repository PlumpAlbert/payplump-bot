package sui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

func authorityPanel(t *testing.T, records ...ClientRecord) (*managedPanel, *AuthorityClient) {
	t.Helper()
	p, _ := newManagedPanel(t, records...)
	c, err := NewAuthorityClient(p.server.URL+"/installed%20panel", "invented-token", time.Second, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return p, c
}
func TestAuthorityDraftSerialization(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		d, err := NewAuthorityCreateDraft("123456", enabled, []int64{7})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		var restored AuthorityCreateDraft
		if err = json.Unmarshal(raw, &restored); err != nil {
			t.Fatal(err)
		}
		if restored.Name() != "123456" || restored.Enabled() != enabled || restored.CredentialsHash() != d.CredentialsHash() || !bytes.Equal(restored.record.Raw(), d.record.Raw()) {
			t.Fatal("lost complete intent")
		}
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(raw, &fields)
		if len(fields) != len(recordKeys)-1 {
			t.Fatal("partial record serialization")
		}
		ids := restored.InboundIDs()
		ids[0] = 8
		if restored.InboundIDs()[0] != 7 {
			t.Fatal("mutable inbound accessor")
		}
		for _, mutate := range []func(map[string]json.RawMessage){
			func(m map[string]json.RawMessage) { m["id"] = json.RawMessage(`0`) },
			func(m map[string]json.RawMessage) { m["id"] = json.RawMessage(`9`) },
			func(m map[string]json.RawMessage) { m["enable"] = json.RawMessage(`"true"`) },
			func(m map[string]json.RawMessage) { m["enable"] = json.RawMessage(`null`) },
			func(m map[string]json.RawMessage) { delete(m, "enable") },
			func(m map[string]json.RawMessage) { m["mode"] = json.RawMessage(`"edit"`) },
			func(m map[string]json.RawMessage) { m["volume"] = json.RawMessage(`1`) },
			func(m map[string]json.RawMessage) { m["autoReset"] = json.RawMessage(`true`) },
			func(m map[string]json.RawMessage) { m["config"] = json.RawMessage(`{}`) },
			func(m map[string]json.RawMessage) { m["name"] = json.RawMessage(`"0123"`) },
		} {
			var m map[string]json.RawMessage
			_ = json.Unmarshal(raw, &m)
			mutate(m)
			bad, _ := json.Marshal(m)
			if json.Unmarshal(bad, &restored) == nil {
				t.Fatal("tampered draft accepted")
			}
			if !bytes.Equal(restored.record.Raw(), d.record.Raw()) {
				t.Fatal("failed decode changed draft")
			}
		}
		duplicate := append(append([]byte(nil), raw[:len(raw)-1]...), []byte(`,"enable":true}`)...)
		if json.Unmarshal(duplicate, &restored) == nil {
			t.Fatal("duplicate accepted")
		}
	}
	for _, name := range []string{"0", "-1", "+1", "01", " 1", "1 ", "9223372036854775808", "foreign", "pp-ABCDEF0123456789012345678901234567890123"} {
		if _, err := NewAuthorityCreateDraft(name, true, []int64{7}); err == nil {
			t.Fatal("unsafe name accepted")
		}
	}
	own, _ := NewDisabledDraft([]int64{7})
	if _, err := NewAuthorityCreateDraft(own.Name(), false, []int64{7}); err != nil {
		t.Fatal("persisted own name rejected")
	}
	if _, err := json.Marshal(AuthorityCreateDraft{}); err == nil {
		t.Fatal("invalid draft marshaled")
	}
}

func TestAuthorityCreateAndExactRecovery(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		p, c := authorityPanel(t, managedTestRecord(t, 123))
		d, _ := NewAuthorityCreateDraft("456", enabled, []int64{7})
		calls := 0
		r, err := c.CreateAuthorized(context.Background(), d, func(context.Context) error {
			calls++
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.last != "inbounds" || len(p.fullQueries) != 1 || p.posts != 0 {
				t.Error("not final preflight gate")
			}
			return nil
		})
		if err != nil || r.Enabled() != enabled || r.CredentialsHash() != d.CredentialsHash() || calls != 1 {
			t.Fatal("creation failed", err)
		}
		if p.countPosts() != 1 || len(p.actions) != 1 || p.actions[0] != "new" || p.payloads[0].Enabled() != enabled || len(p.fullQueries) != 2 {
			t.Fatal("not sole new with full readback")
		}
		recovered, found, err := c.FindCreated(context.Background(), d)
		if err != nil || !found || recovered.ID() != r.ID() {
			t.Fatal("exact recovery failed")
		}
		inventory, err := c.Inventory(context.Background())
		if err != nil || len(inventory) != 2 {
			t.Fatal("inventory incomplete")
		}
		p.mu.Lock()
		r.wire.Enable = !enabled
		p.records[r.ID()] = r
		p.mu.Unlock()
		if _, found, err = c.FindCreated(context.Background(), d); err == nil || found {
			t.Fatal("wrong enabled recovered")
		}
		if _, err = c.CreateAuthorized(context.Background(), d, func(context.Context) error { t.Error("existing name reached authorization"); return nil }); err != ErrManagedConflict || p.countPosts() != 1 {
			t.Fatal("existing name reposted")
		}
	}
	// The capability must not accidentally grow edit/enable/save methods.
	typ := reflect.TypeOf((*AuthorityClient)(nil))
	if typ.NumMethod() != 3 {
		t.Fatal("unexpected exported capability")
	}
	for i := 0; i < typ.NumMethod(); i++ {
		switch typ.Method(i).Name {
		case "Inventory", "FindCreated", "CreateAuthorized":
		default:
			t.Fatal("mutation capability exposed")
		}
	}
}

func TestAuthorityPreflightAndAuthorizationDenial(t *testing.T) {
	for _, mode := range []string{"nil", "source", "cancel", "collision", "inbound", "incomplete"} {
		t.Run(mode, func(t *testing.T) {
			d, _ := NewAuthorityCreateDraft("456", true, []int64{7})
			r := managedTestRecord(t, 123)
			if mode == "collision" {
				r.wire.Config = append(json.RawMessage(nil), d.record.wire.Config...)
				r.wire.Name = "789"
				var cfg map[string]map[string]any
				_ = json.Unmarshal(r.wire.Config, &cfg)
				for _, f := range cfg {
					for _, k := range []string{"name", "username"} {
						if _, ok := f[k]; ok {
							f[k] = "789"
						}
					}
				}
				r.wire.Config, _ = json.Marshal(cfg)
			}
			p, c := authorityPanel(t, r)
			if mode == "inbound" {
				p.inboundType = "wireguard"
			}
			if mode == "incomplete" {
				p.fullHook = func([]json.RawMessage) []json.RawMessage { return nil }
			}
			denied := errors.New("source fence rejected")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			auth := func(context.Context) error {
				calls++
				if mode == "cancel" {
					cancel()
					return nil
				}
				return denied
			}
			if mode == "nil" {
				auth = nil
			}
			_, err := c.CreateAuthorized(ctx, d, auth)
			if err == nil || errors.Is(err, ErrManagedUnknown) || p.countPosts() != 0 {
				t.Fatal("denied preflight mutated")
			}
			if mode == "source" && (err != denied || calls != 1) {
				t.Fatal("source error changed")
			}
			if mode != "source" && mode != "cancel" && calls != 0 {
				t.Fatal("authorization before preflight")
			}
		})
	}
}

func TestAuthorityUnknownReadbackNoRetry(t *testing.T) {
	for _, mode := range []string{"receipt", "enabled", "missing-full", "not-durable", "http"} {
		t.Run(mode, func(t *testing.T) {
			p, c := authorityPanel(t)
			d, _ := NewAuthorityCreateDraft("456", true, []int64{7})
			switch mode {
			case "receipt":
				p.responseRaw = `{"success":true,"obj":null}`
			case "enabled":
				p.afterSave = func(p *managedPanel, r ClientRecord, _ string) { r.wire.Enable = false; p.records[r.ID()] = r }
			case "missing-full":
				p.afterSave = func(p *managedPanel, _ ClientRecord, _ string) {
					p.fullHook = func([]json.RawMessage) []json.RawMessage { return nil }
				}
			case "not-durable":
				p.persist = false
			case "http":
				p.postStatus = 500
			}
			_, err := c.CreateAuthorized(context.Background(), d, func(context.Context) error { return nil })
			if err != ErrManagedUnknown || p.countPosts() != 1 || p.actions[0] != "new" {
				t.Fatal("uncertain write accepted or retried")
			}
			if mode == "receipt" {
				if len(p.fullQueries) != 1 {
					t.Fatal("unknown receipt skipped full readback")
				}
				if _, found, err := c.FindCreated(context.Background(), d); err != nil || !found {
					t.Fatal("separate recovery failed")
				}
			}
		})
	}
}
