package sui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
)

type managedPanel struct {
	mu          sync.Mutex
	t           *testing.T
	server      *httptest.Server
	records     map[int64]ClientRecord
	version     string
	inboundType string
	persist     bool
	success     bool
	postStatus  int
	posts       int
	fullQueries []string
	payloads    []ClientRecord
	actions     []string
	last        string
	summaryHook func([]any) []any
	fullHook    func([]json.RawMessage) []json.RawMessage
	afterSave   func(*managedPanel, ClientRecord, string)
	responseRaw string
}

func newManagedPanel(t *testing.T, records ...ClientRecord) (*managedPanel, *ManagedClient) {
	t.Helper()
	p := &managedPanel{t: t, records: map[int64]ClientRecord{}, version: "1.6.4", inboundType: "vless", persist: true, success: true}
	for _, r := range records {
		cloned, err := cloneRecord(r, false)
		if err != nil {
			t.Fatal("invalid invented record")
		}
		p.records[r.ID()] = cloned
	}
	p.server = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.server.Close)
	c, err := NewManagedClient(p.server.URL+"/installed%20panel", "invented-token", "1.6.4", 3*time.Second, 1000)
	if err != nil {
		t.Fatal("client construction failed")
	}
	return p, c
}
func (p *managedPanel) summaries() []any {
	ids := make([]int64, 0, len(p.records))
	for id := range p.records {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	list := []any{}
	for _, id := range ids {
		r := p.records[id]
		list = append(list, map[string]any{"id": r.ID(), "name": r.Name()})
	}
	return list
}
func (p *managedPanel) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.Header.Get("Token") != "invented-token" || r.Header.Get("Authorization") != "" || r.Header.Get("Accept") != "application/json" {
		p.t.Error("invalid managed authentication headers")
		w.WriteHeader(400)
		return
	}
	prefix := "/installed%20panel/apiv2/"
	if !strings.HasPrefix(r.URL.EscapedPath(), prefix) {
		p.t.Error("lost installed prefix")
		w.WriteHeader(404)
		return
	}
	route := strings.TrimPrefix(r.URL.EscapedPath(), prefix)
	previous := p.last
	p.last = route
	w.Header().Set("Content-Type", "application/json")
	reply := func(obj any) { _ = json.NewEncoder(w).Encode(map[string]any{"success": true, "obj": obj}) }
	if r.Method == http.MethodGet {
		switch route {
		case "status":
			if r.URL.RawQuery != "r=sys" {
				p.t.Error("wrong version query")
			}
			reply(map[string]any{"sys": map[string]any{"appVersion": p.version}})
		case "inbounds":
			if r.URL.RawQuery != "" {
				p.t.Error("wrong inbound query")
			}
			reply(map[string]any{"inbounds": []any{map[string]any{"id": 7, "type": p.inboundType}}})
		case "clients":
			if r.URL.RawQuery == "" {
				list := p.summaries()
				if p.summaryHook != nil {
					list = p.summaryHook(list)
				}
				reply(map[string]any{"clients": list})
				return
			}
			q := r.URL.Query()
			if len(q) != 1 || len(q["id"]) != 1 {
				p.t.Error("unexpected full query")
			}
			query := q.Get("id")
			p.fullQueries = append(p.fullQueries, query)
			ids := strings.Split(query, ",")
			if len(ids) > 50 {
				p.t.Error("oversized full batch")
			}
			list := []json.RawMessage{}
			for _, s := range ids {
				id, err := strconv.ParseInt(s, 10, 64)
				if err != nil || id <= 0 {
					p.t.Error("invalid full ID")
				}
				if rec, ok := p.records[id]; ok {
					list = append(list, rec.Raw())
				}
			}
			// A set response need not preserve request ordering.
			for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
				list[i], list[j] = list[j], list[i]
			}
			if p.fullHook != nil {
				list = p.fullHook(list)
			}
			reply(map[string]any{"clients": list})
		default:
			p.t.Error("unexpected managed GET")
			w.WriteHeader(404)
		}
		return
	}
	if r.Method != http.MethodPost || route != "save" {
		p.t.Error("unexpected mutation endpoint")
		w.WriteHeader(400)
		return
	}
	p.posts++
	if previous != "status" {
		p.t.Error("POST missing immediate version preflight")
	}
	if r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.ParseForm() != nil || len(r.PostForm) != 3 || r.PostForm.Get("object") != "clients" || len(r.PostForm["data"]) != 1 || len(r.PostForm["action"]) != 1 {
		p.t.Error("not the source-pinned save form")
		w.WriteHeader(400)
		return
	}
	action := r.PostForm.Get("action")
	if action != "new" && action != "edit" {
		p.t.Error("unsupported save action")
		w.WriteHeader(400)
		return
	}
	rec, err := parseRecord([]byte(r.PostForm.Get("data")), action == "new")
	if err != nil {
		p.t.Error("saved partial/invalid account")
		w.WriteHeader(400)
		return
	}
	p.payloads = append(p.payloads, rec)
	p.actions = append(p.actions, action)
	if p.postStatus != 0 {
		if p.postStatus >= 300 && p.postStatus < 400 {
			w.Header().Set("Location", p.server.URL+"/must-not-follow")
		}
		w.WriteHeader(p.postStatus)
		return
	}
	if p.persist {
		if action == "new" {
			rec.wire.ID = 999
			rec.wire.CreatedAt = 100
		}
		rec.wire.Links = append(nonlocalLinks(rec.wire.Links), map[string]string{"type": "local", "uri": "synthetic-regenerated-local"})
		p.records[rec.ID()] = rec
	}
	if p.afterSave != nil {
		p.afterSave(p, rec, action)
	}
	if p.responseRaw != "" {
		_, _ = w.Write([]byte(p.responseRaw))
		return
	}
	// Deliberately not a dedicated ID. This is the global summary receipt.
	_ = json.NewEncoder(w).Encode(map[string]any{"success": p.success, "msg": "invented-remote-secret", "obj": map[string]any{"clients": p.summaries()}})
}
func (p *managedPanel) countPosts() int { p.mu.Lock(); defer p.mu.Unlock(); return p.posts }

func TestManagedCreateDisabledAndSeparateRecovery(t *testing.T) {
	p, c := newManagedPanel(t)
	d, err := NewDisabledDraft([]int64{7})
	if err != nil {
		t.Fatal("draft failed")
	}
	created, err := c.CreateDisabled(context.Background(), d)
	if err != nil || created.ID() != 999 || created.Enabled() || created.CredentialsHash() != d.CredentialsHash() {
		t.Fatal("disabled durable creation failed")
	}
	if p.countPosts() != 1 || len(p.fullQueries) != 1 || p.fullQueries[0] != "999" {
		t.Fatal("creation relied on receipt rather than fresh full query")
	}
	if p.actions[0] != "new" || p.payloads[0].ID() != 0 || p.payloads[0].Enabled() {
		t.Fatal("wrong new payload")
	}
	if _, err = c.CreateDisabled(context.Background(), d); !errors.Is(err, ErrManagedConflict) || p.countPosts() != 1 {
		t.Fatal("create adopted/reposted an existing generated name")
	}
	found, ok, err := c.FindCreated(context.Background(), d)
	if err != nil || !ok || found.ID() != created.ID() || p.countPosts() != 1 {
		t.Fatal("separate exact recovery failed")
	}
}

func TestManagedEnableOnlyChangesEnableWithFreshSnapshot(t *testing.T) {
	r := managedTestRecord(t, 123)
	r.wire.Config = json.RawMessage(fmt.Sprintf(`{"vless":{"name":%q,"uuid":"invented-credential","flow":"xtls-rprx-vision","future":{"list":[1e2,true,null],"number":9007199254740991}}}`, r.Name()))
	r.wire.Links = []map[string]string{{"type": "external", "uri": "https://invalid.example/never-fetch", "extension": "retained"}, {"type": "sub", "uri": "https://invalid.example/subscription"}, {"type": "local", "uri": "synthetic-old"}}
	r.wire.Volume = 9007199254740993
	r.wire.Up = 1 // A quota with zero counters would request a backend reset.
	r.wire.Expiry = 2345678
	r, err := ParseClientRecord(r.Raw())
	if err != nil {
		t.Fatal("synthetic full record failed")
	}
	binding := Binding{ID: r.ID(), Name: r.Name(), CredentialsHash: r.CredentialsHash()}
	d, err := PrepareEnable(r, binding)
	if err != nil {
		t.Fatal("prepare failed")
	}
	current, _ := cloneRecord(r, false)
	current.wire.CreatedAt = 11
	current.wire.OnlineAt = 22
	current.wire.Up = 33
	current.wire.Down = 44
	current.wire.TotalUp = 55
	current.wire.TotalDown = 66
	current.wire.Links[2]["uri"] = "synthetic-fresh-local"
	p, c := newManagedPanel(t, current)
	p.afterSave = func(p *managedPanel, rec ClientRecord, _ string) {
		rec.wire.OnlineAt++
		rec.wire.Up++
		p.records[rec.ID()] = rec
	}
	enabled, err := c.Enable(context.Background(), d)
	if err != nil || !enabled.Enabled() || p.countPosts() != 1 {
		t.Fatal("durable enable failed")
	}
	payload := p.payloads[0]
	payload.wire.Enable = false
	if !bytes.Equal(payload.Raw(), current.Raw()) || p.actions[0] != "edit" {
		t.Fatal("edit changed fields other than enable or used stale server fields")
	}
	if len(p.fullQueries) != 2 || p.fullQueries[0] != "123" || p.fullQueries[1] != "123" {
		t.Fatal("enable missing full before/after fetch")
	}
	if _, err = c.Enable(context.Background(), d); !errors.Is(err, ErrManagedState) || p.countPosts() != 1 {
		t.Fatal("already-enabled state adopted or written again")
	}
}

func TestManagedCreateCredentialCollisionAllKnownFields(t *testing.T) {
	for _, field := range []string{"uuid", "password", "auth_str", "userkey"} {
		t.Run(field, func(t *testing.T) {
			d, _ := NewDisabledDraft([]int64{7})
			var cfg map[string]map[string]any
			_ = json.Unmarshal(d.record.wire.Config, &cfg)
			var value any
			switch field {
			case "uuid":
				value = cfg["vmess"][field]
			case "password":
				value = cfg["shadowsocks"][field]
			case "auth_str":
				value = cfg["hysteria"][field]
			case "userkey":
				value = cfg["snell"][field]
			}
			r := managedTestRecord(t, 123)
			r.wire.Config, _ = json.Marshal(map[string]any{"extension": map[string]any{field: value}})
			p, c := newManagedPanel(t, r)
			if _, err := c.CreateDisabled(context.Background(), d); !errors.Is(err, ErrManagedConflict) || p.countPosts() != 0 {
				t.Fatal("credential collision did not block POST")
			}
		})
	}
}

func TestManagedFullInventoryBatchingAndExactSets(t *testing.T) {
	records := make([]ClientRecord, 101)
	for i := range records {
		records[i] = managedTestRecord(t, int64(i+1))
	}
	p, c := newManagedPanel(t, records...)
	d, _ := NewDisabledDraft([]int64{7})
	_, found, err := c.FindCreated(context.Background(), d)
	if err != nil || found || len(p.fullQueries) != 3 {
		t.Fatal("complete inventory fetch failed")
	}
	for i, n := range []int{50, 50, 1} {
		if len(strings.Split(p.fullQueries[i], ",")) != n {
			t.Fatal("incorrect full chunk size")
		}
	}
	for _, mode := range []string{"missing", "duplicate", "foreign", "renamed", "unknown-top"} {
		t.Run(mode, func(t *testing.T) {
			a, b := managedTestRecord(t, 123), managedTestRecord(t, 124)
			p, c := newManagedPanel(t, a, b)
			p.fullHook = func(list []json.RawMessage) []json.RawMessage {
				switch mode {
				case "missing":
					return list[:1]
				case "duplicate":
					list[1] = list[0]
				case "foreign":
					list[0] = changeRaw(t, list[0], func(m map[string]json.RawMessage) { m["id"] = json.RawMessage(`9876`) })
				case "renamed":
					list[0] = changeRaw(t, list[0], func(m map[string]json.RawMessage) {
						m["name"] = json.RawMessage(`"foreign"`)
						m["config"] = json.RawMessage(`{}`)
					})
				case "unknown-top":
					list[0] = changeRaw(t, list[0], func(m map[string]json.RawMessage) { m["future"] = json.RawMessage(`0`) })
				}
				return list
			}
			if _, err := c.CreateDisabled(context.Background(), d); err == nil || p.countPosts() != 0 {
				t.Fatal("incomplete/foreign full inventory permitted POST")
			}
		})
	}
}

func TestManagedSummaryDuplicatesAndCapBlockWrites(t *testing.T) {
	for _, mode := range []string{"duplicate-id", "duplicate-name", "over-cap", "full-cap"} {
		t.Run(mode, func(t *testing.T) {
			a, b := managedTestRecord(t, 123), managedTestRecord(t, 124)
			p, c := newManagedPanel(t, a, b)
			switch mode {
			case "duplicate-id":
				p.summaryHook = func(list []any) []any { list[1].(map[string]any)["id"] = int64(123); return list }
			case "duplicate-name":
				p.summaryHook = func(list []any) []any { list[1].(map[string]any)["name"] = a.Name(); return list }
			case "over-cap":
				c.maxAccounts = 1
			case "full-cap":
				c.maxAccounts = 2
			}
			d, _ := NewDisabledDraft([]int64{7})
			if _, err := c.CreateDisabled(context.Background(), d); err == nil || p.countPosts() != 0 {
				t.Fatal("invalid/capped inventory permitted POST")
			}
		})
	}
}

func TestManagedNoPOSTOnBadBindingStaleStateOrInbounds(t *testing.T) {
	for _, mode := range []string{"binding-id", "binding-name", "binding-hash", "missing", "name", "config", "quota", "expiry", "nonlocal", "inbounds", "enabled", "delay", "reset", "unsupported-inbound", "missing-inbound"} {
		t.Run(mode, func(t *testing.T) {
			r := managedTestRecord(t, 123)
			binding := Binding{ID: r.ID(), Name: r.Name(), CredentialsHash: r.CredentialsHash()}
			d, _ := PrepareEnable(r, binding)
			current, _ := cloneRecord(r, false)
			switch mode {
			case "binding-id":
				d.binding.ID++
			case "binding-name":
				d.binding.Name = "foreign"
			case "binding-hash":
				d.binding.CredentialsHash[0] ^= 1
			case "name":
				current.wire.Name = "foreign"
				current.wire.Config = json.RawMessage(`{}`)
			case "config":
				current.wire.Config = json.RawMessage(`{"vless":{"uuid":"foreign-credential"}}`)
			case "quota":
				current.wire.Volume = 1
			case "expiry":
				current.wire.Expiry = 1
			case "nonlocal":
				current.wire.Links = []map[string]string{{"type": "external", "uri": "synthetic-foreign"}}
			case "inbounds":
				current.wire.Inbounds = []int64{8}
			case "enabled":
				current.wire.Enable = true
			case "delay":
				current.wire.DelayStart = true
			case "reset":
				current.wire.NextReset = 1
			}
			p, c := newManagedPanel(t, current)
			if mode == "missing" {
				delete(p.records, 123)
			}
			if mode == "unsupported-inbound" {
				p.inboundType = "wireguard"
			}
			if mode == "missing-inbound" {
				d.before.wire.Inbounds = []int64{8}
				p.records[123] = d.before
			}
			if _, err := c.Enable(context.Background(), d); err == nil || p.countPosts() != 0 {
				t.Fatal("unsafe enable permitted POST")
			}
		})
	}
}

func TestManagedReadbackFailuresAreUnknownNoRetry(t *testing.T) {
	for _, operation := range []string{"create", "enable"} {
		for _, mode := range []string{"not-durable", "missing", "mismatch", "foreign", "nonlocal-link", "missing-full", "false-200", "invalid-receipt", "duplicate-receipt", "http-error", "redirect"} {
			t.Run(operation+"/"+mode, func(t *testing.T) {
				r := managedTestRecord(t, 123)
				p, c := newManagedPanel(t)
				if operation == "enable" {
					p.records[123] = r
				}
				switch mode {
				case "not-durable":
					p.persist = false
				case "missing":
					p.afterSave = func(p *managedPanel, r ClientRecord, _ string) { delete(p.records, r.ID()) }
				case "mismatch":
					p.afterSave = func(p *managedPanel, r ClientRecord, _ string) { r.wire.Volume++; p.records[r.ID()] = r }
				case "foreign":
					p.afterSave = func(p *managedPanel, r ClientRecord, _ string) {
						r.wire.Config = json.RawMessage(`{"vless":{"uuid":"foreign"}}`)
						p.records[r.ID()] = r
					}
				case "nonlocal-link":
					p.afterSave = func(p *managedPanel, r ClientRecord, _ string) {
						r.wire.Links = append(r.wire.Links, map[string]string{"type": "external", "uri": "synthetic-foreign"})
						p.records[r.ID()] = r
					}
				case "missing-full":
					p.afterSave = func(p *managedPanel, _ ClientRecord, _ string) {
						p.fullHook = func([]json.RawMessage) []json.RawMessage { return []json.RawMessage{} }
					}
				case "false-200":
					p.success = false
				case "invalid-receipt":
					p.responseRaw = `{"success":true,"obj":null}`
				case "duplicate-receipt":
					p.responseRaw = `{"success":true,"success":true,"obj":{}}`
				case "http-error":
					p.postStatus = 500
				case "redirect":
					p.postStatus = 307
				}
				var err error
				if operation == "create" {
					d, _ := NewDisabledDraft([]int64{7})
					_, err = c.CreateDisabled(context.Background(), d)
				} else {
					d, _ := PrepareEnable(r, Binding{ID: r.ID(), Name: r.Name(), CredentialsHash: r.CredentialsHash()})
					_, err = c.Enable(context.Background(), d)
				}
				if !errors.Is(err, ErrManagedUnknown) || strings.Contains(err.Error(), "invented-remote-secret") || p.countPosts() != 1 {
					t.Fatal("write uncertainty was accepted, leaked or retried")
				}
			})
		}
	}
}

func TestManagedFindCreatedExactOnly(t *testing.T) {
	d, _ := NewDisabledDraft([]int64{7})
	for _, mode := range []string{"absent", "exact", "enabled", "foreign-config", "foreign-quota", "foreign-inbound", "foreign-link", "duplicate-name"} {
		t.Run(mode, func(t *testing.T) {
			r, _ := cloneRecord(d.record, true)
			r.wire.ID = 123
			r.wire.CreatedAt = 100
			r.wire.Up = 10
			r.wire.Links = []map[string]string{{"type": "local", "uri": "synthetic"}}
			switch mode {
			case "enabled":
				r.wire.Enable = true
			case "foreign-config":
				r.wire.Config = json.RawMessage(`{"vless":{"uuid":"foreign"}}`)
			case "foreign-quota":
				r.wire.Volume = 1
			case "foreign-inbound":
				r.wire.Inbounds = []int64{8}
			case "foreign-link":
				r.wire.Links = []map[string]string{{"type": "external", "uri": "synthetic"}}
			}
			p, c := newManagedPanel(t, r)
			if mode == "absent" {
				delete(p.records, 123)
			}
			if mode == "duplicate-name" {
				p.summaryHook = func(list []any) []any { return append(list, map[string]any{"id": 124, "name": r.Name()}) }
			}
			_, found, err := c.FindCreated(context.Background(), d)
			if mode == "exact" {
				if err != nil || !found {
					t.Fatal("exact recovery failed")
				}
			} else if mode == "absent" {
				if err != nil || found {
					t.Fatal("missing recovery was positive")
				}
			} else if err == nil || found {
				t.Fatal("fuzzy/foreign recovery accepted")
			}
			if p.countPosts() != 0 {
				t.Fatal("recovery issued a write")
			}
		})
	}
}

func TestManagedRefusesVersionAndInvalidConstruction(t *testing.T) {
	p, c := newManagedPanel(t)
	d, _ := NewDisabledDraft([]int64{7})
	for _, version := range []string{"", "1.6.3", "1.6.4-dev", "v1.6.4", "1.7.0"} {
		p.version = version
		if _, err := c.CreateDisabled(context.Background(), d); !errors.Is(err, ErrManagedVersion) || p.countPosts() != 0 {
			t.Fatal("unmatched version claim permitted POST")
		}
	}
	p.version = "v1.6.4"
	c.declaredVersion = "v1.6.4"
	if _, err := c.CreateDisabled(context.Background(), d); err != nil {
		t.Fatal("explicitly declared version prefix rejected")
	}
	for _, version := range []string{"", "1.6.3", "v1.6.4-dev", " 1.6.4"} {
		if _, err := NewManagedClient(p.server.URL, "invented-token", version, time.Second, 1); err == nil {
			t.Fatal("accepted unpinned manual version")
		}
	}
	for _, cap := range []int{-1, 0, 1001} {
		if _, err := NewManagedClient(p.server.URL, "invented-token", "1.6.4", time.Second, cap); err == nil {
			t.Fatal("accepted invalid cap")
		}
	}
	for _, suffix := range []string{"/apiv2", "?secret=invented", "#fragment"} {
		if _, err := NewManagedClient(p.server.URL+suffix, "invented-token", "1.6.4", time.Second, 1); err == nil {
			t.Fatal("accepted invalid base")
		}
	}
}

func TestManagedReadEnvelopeAndResponseBound(t *testing.T) {
	for _, body := range []string{
		`{"success":false,"msg":"invented-remote-secret","obj":{"clients":[]}}`,
		`{"obj":{"clients":[]}}`,
		`{"success":true,"obj":null}`,
		`{"success":true,"obj":{"clients":[],"clients":[]}}`,
		`{"success":true,"obj":{"clients":null}}`,
		`{"success":true,"obj":{"clients":[]}} {"extra":true}`,
		strings.Repeat(" ", integrationhttp.MaxResponseBytes+1),
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Error("invalid read response permitted POST")
			}
			_, _ = w.Write([]byte(body))
		}))
		c, err := NewManagedClient(server.URL, "invented-token", "1.6.4", time.Second, 1)
		if err != nil {
			server.Close()
			t.Fatal("construction failed")
		}
		d, _ := NewDisabledDraft([]int64{7})
		_, err = c.CreateDisabled(context.Background(), d)
		server.Close()
		if err == nil || strings.Contains(err.Error(), "invented-remote-secret") {
			t.Fatal("unsafe response accepted or leaked")
		}
	}
}

func TestManagedGetRecordSingleFullAndRejectRedirect(t *testing.T) {
	r := managedTestRecord(t, 123)
	p, c := newManagedPanel(t, r)
	got, err := c.GetRecord(context.Background(), 123)
	if err != nil || got.ID() != 123 || len(p.fullQueries) != 1 || p.fullQueries[0] != "123" {
		t.Fatal("GetRecord did not use exact full query")
	}
	if _, err := c.GetRecord(context.Background(), 124); err == nil {
		t.Fatal("missing mapped record accepted")
	}
	if _, err := c.GetRecord(context.Background(), 0); err == nil {
		t.Fatal("invalid mapped ID accepted")
	}
	var mu sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.Header().Set("Location", "/elsewhere")
		w.WriteHeader(302)
	}))
	defer server.Close()
	redirectClient, _ := NewManagedClient(server.URL, "invented-token", "1.6.4", time.Second, 1)
	if _, err := redirectClient.GetRecord(context.Background(), 123); err == nil {
		t.Fatal("redirect accepted")
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 1 {
		t.Fatal("redirect followed")
	}
}
