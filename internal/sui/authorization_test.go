package sui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func managedAuthorizationCase(t *testing.T, operation string) (*managedPanel, func(context.Context, func(context.Context) error) (ClientRecord, error)) {
	t.Helper()
	r := managedTestRecord(t, 123)
	p, c := newManagedPanel(t, r)
	if operation == "create" {
		d, err := NewDisabledDraft([]int64{7})
		if err != nil {
			t.Fatal("synthetic draft failed")
		}
		return p, func(ctx context.Context, authorize func(context.Context) error) (ClientRecord, error) {
			return c.CreateDisabledAuthorized(ctx, d, authorize)
		}
	}
	d, err := PrepareEnable(r, Binding{ID: r.ID(), Name: r.Name(), CredentialsHash: r.CredentialsHash()})
	if err != nil {
		t.Fatal("synthetic enable draft failed")
	}
	return p, func(ctx context.Context, authorize func(context.Context) error) (ClientRecord, error) {
		return c.EnableAuthorized(ctx, d, authorize)
	}
}

func TestManagedAuthorizedNilDeniesBeforeIO(t *testing.T) {
	for _, operation := range []string{"create", "enable"} {
		t.Run(operation, func(t *testing.T) {
			p, call := managedAuthorizationCase(t, operation)
			_, err := call(context.Background(), nil)
			if err != ErrManagedInvalid || errors.Is(err, ErrManagedUnknown) || p.countPosts() != 0 {
				t.Fatal("nil authorization was not an invalid pre-write intent")
			}
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.last != "" {
				t.Fatal("nil authorization reached GET preflights")
			}
		})
	}
}

func TestManagedAuthorizedRunsOnlyAfterAllPreflights(t *testing.T) {
	for _, operation := range []string{"create", "enable"} {
		for _, failure := range []string{"none", "inventory", "inbound", "version"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				p, call := managedAuthorizationCase(t, operation)
				switch failure {
				case "inventory":
					p.fullHook = func([]json.RawMessage) []json.RawMessage { return []json.RawMessage{} }
				case "inbound":
					p.inboundType = "wireguard"
				case "version":
					p.version = "unsupported"
				}
				denied := errors.New("synthetic authorization denied")
				calls := 0
				ctx := context.WithValue(context.Background(), struct{}{}, "synthetic")
				_, err := call(ctx, func(got context.Context) error {
					calls++
					p.mu.Lock()
					defer p.mu.Unlock()
					if got != ctx || p.last != "status" || len(p.fullQueries) != 1 || p.posts != 0 {
						t.Error("authorization was not the final pre-save gate")
					}
					return denied
				})
				want := 0
				if failure == "none" {
					want = 1
					if err != denied {
						t.Fatal("authorization error was changed")
					}
				}
				if err == nil || errors.Is(err, ErrManagedUnknown) || calls != want || p.countPosts() != 0 {
					t.Fatal("failed preflight/authorization posted, retried or became unknown")
				}
			})
		}
	}
}

func TestManagedAuthorizedCancelAfterCallbackDoesNotPOST(t *testing.T) {
	for _, operation := range []string{"create", "enable"} {
		t.Run(operation, func(t *testing.T) {
			p, call := managedAuthorizationCase(t, operation)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			_, err := call(ctx, func(context.Context) error { calls++; cancel(); return nil })
			if err != context.Canceled || errors.Is(err, ErrManagedUnknown) || calls != 1 || p.countPosts() != 0 {
				t.Fatal("cancellation at final gate attempted a POST or became unknown")
			}
		})
	}
}

func TestManagedAuthorizedSuccessStillRequiresFullReadback(t *testing.T) {
	for _, operation := range []string{"create", "enable"} {
		for _, brokenReadback := range []bool{false, true} {
			name := operation + "/success"
			if brokenReadback {
				name = operation + "/broken-readback"
			}
			t.Run(name, func(t *testing.T) {
				p, call := managedAuthorizationCase(t, operation)
				if brokenReadback {
					p.afterSave = func(p *managedPanel, _ ClientRecord, _ string) {
						p.fullHook = func([]json.RawMessage) []json.RawMessage { return []json.RawMessage{} }
					}
				}
				calls := 0
				r, err := call(context.Background(), func(context.Context) error { calls++; return nil })
				if brokenReadback {
					if !errors.Is(err, ErrManagedUnknown) {
						t.Fatal("receipt substituted for mandatory full readback")
					}
				} else if err != nil || r.ID() <= 0 || r.Enabled() != (operation == "enable") {
					t.Fatal("authorized durable write failed")
				}
				p.mu.Lock()
				defer p.mu.Unlock()
				if calls != 1 || p.posts != 1 || len(p.fullQueries) != 2 || p.last != "clients" {
					t.Fatal("authorization/write retried or full readback missing")
				}
			})
		}
	}
}

func TestManagedAuthorizedSnapshotsBeforeCallback(t *testing.T) {
	for _, operation := range []string{"create", "enable"} {
		t.Run(operation, func(t *testing.T) {
			d, _ := NewDisabledDraft([]int64{7})
			r := d.record
			r.wire.ID = 123
			var call func(func(context.Context) error) (ClientRecord, error)
			var mutate func()
			var p *managedPanel
			if operation == "create" {
				var c *ManagedClient
				p, c = newManagedPanel(t)
				call = func(auth func(context.Context) error) (ClientRecord, error) {
					return c.CreateDisabledAuthorized(context.Background(), d, auth)
				}
				mutate = func() { d.record.wire.Inbounds[0] = 8; d.record.wire.Config[0] = '[' }
			} else {
				var c *ManagedClient
				p, c = newManagedPanel(t, r)
				e, err := PrepareEnable(r, Binding{ID: r.ID(), Name: r.Name(), CredentialsHash: r.CredentialsHash()})
				if err != nil {
					t.Fatal("synthetic enable preparation failed")
				}
				call = func(auth func(context.Context) error) (ClientRecord, error) {
					return c.EnableAuthorized(context.Background(), e, auth)
				}
				mutate = func() { e.before.wire.Inbounds[0] = 8; e.before.wire.Config[0] = '[' }
			}
			if _, err := call(func(context.Context) error { mutate(); return nil }); err != nil || p.countPosts() != 1 {
				t.Fatal("callback changed the snapshotted intent")
			}
		})
	}
}

func TestValidateRecordForDraftAndBindingCopy(t *testing.T) {
	d, _ := NewDisabledDraft([]int64{7})
	r, err := cloneRecord(d.record, true)
	if err != nil {
		t.Fatal("synthetic clone failed")
	}
	r.wire.ID = 123
	r.wire.CreatedAt, r.wire.OnlineAt = 10, 20
	r.wire.Up, r.wire.Down, r.wire.TotalUp, r.wire.TotalDown = 1, 2, 3, 4
	r.wire.Links = []map[string]string{{"type": "local", "uri": "synthetic-regenerated"}}
	before, draftBefore := r.Raw(), d.record.Raw()
	if ValidateRecordForDraft(r, d, false) != nil || !bytes.Equal(before, r.Raw()) || !bytes.Equal(draftBefore, d.record.Raw()) {
		t.Fatal("server-managed allowances rejected or inputs mutated")
	}
	r.wire.ID++ // This helper deliberately cannot bind/adopt a persisted ID.
	r.wire.Enable = true
	if ValidateRecordForDraft(r, d, true) != nil || ValidateRecordForDraft(r, d, false) == nil {
		t.Fatal("expected enable flag ignored")
	}
	for _, mutate := range []func(*ClientRecord){
		func(r *ClientRecord) { r.wire.ID = 0 },
		func(r *ClientRecord) { r.wire.Name = "foreign" },
		func(r *ClientRecord) { r.wire.Config = json.RawMessage(`{}`) },
		func(r *ClientRecord) {
			r.wire.Config = append(r.wire.Config[:len(r.wire.Config)-1], []byte(`,"extension":{"key":"synthetic"}}`)...)
		},
		func(r *ClientRecord) { r.wire.Inbounds[0] = 8 },
		func(r *ClientRecord) { r.wire.Volume = 1 },
		func(r *ClientRecord) { r.wire.Expiry = 1 },
		func(r *ClientRecord) { r.wire.Remark = "foreign" },
		func(r *ClientRecord) {
			r.wire.Links = append(r.wire.Links, map[string]string{"type": "external", "uri": "synthetic"})
		},
		func(r *ClientRecord) { r.wire.Links = nil },
	} {
		changed, err := cloneRecord(r, false)
		if err != nil {
			t.Fatal("synthetic clone failed")
		}
		mutate(&changed)
		if ValidateRecordForDraft(changed, d, true) == nil {
			t.Fatal("record mismatch accepted")
		}
	}
	if ValidateRecordForDraft(r, CreateDraft{}, true) == nil {
		t.Fatal("invalid disabled intent accepted")
	}
	invalid, _ := cloneRecord(d.record, true)
	invalid.wire.Enable = true
	if ValidateRecordForDraft(r, CreateDraft{record: invalid}, true) == nil {
		t.Fatal("enabled create intent accepted")
	}
	r.wire.Enable = false
	binding := Binding{ID: r.ID(), Name: r.Name(), CredentialsHash: r.CredentialsHash()}
	e, err := PrepareEnable(r, binding)
	if err != nil || e.Binding() != binding {
		t.Fatal("binding getter changed persisted identity")
	}
	copy := e.Binding()
	copy.ID++
	copy.CredentialsHash[0] ^= 1
	if e.Binding() != binding {
		t.Fatal("binding getter exposes mutable identity")
	}
}
