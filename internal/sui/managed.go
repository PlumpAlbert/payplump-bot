package sui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
)

var (
	ErrManagedInvalid  = errors.New("invalid SUI managed intent")
	ErrManagedResponse = errors.New("invalid SUI managed response")
	ErrManagedConflict = errors.New("SUI managed account conflict")
	ErrManagedState    = errors.New("unsupported SUI managed state")
	ErrManagedVersion  = errors.New("unsupported SUI managed version claim")
	// ErrManagedUnknown means a POST was attempted. The caller MUST hold the
	// operation and reconcile, never blindly resubmit, even after success=true.
	ErrManagedUnknown = errors.New("SUI write outcome unknown; hold and reconcile")
)

// ManagedAPI is an opt-in adapter, not runtime activation or an entitlement
// policy. Missing mapped records are errors, never permission to recreate.
type ManagedAPI interface {
	GetRecord(context.Context, int64) (ClientRecord, error)
	FindCreated(context.Context, CreateDraft) (ClientRecord, bool, error)
	CreateDisabled(context.Context, CreateDraft) (ClientRecord, error)
	Enable(context.Context, EnableDraft) (ClientRecord, error)
}

// ManagedClient targets backend db7f9545e7e2907a5d46a2b6bfc4a0b64c2de348
// (v1.6.4). Version checking is only a version CLAIM, not build attestation.
//
// The panel has no CAS or idempotency. Callers must enforce an EXCLUSIVE writer
// across all processes, administrators and panel UI; this mutex serializes only
// this instance. Preflight/readback cannot close an external writer race.
// Durable readback proves neither SUI core application nor connection health.
// No disable, session closing, URL fetching, retries or runtime wiring exists.
type ManagedClient struct {
	http            *integrationhttp.Client
	writer          *integrationhttp.Writer
	declaredVersion string
	maxAccounts     int
	mu              sync.Mutex
}

// ValidateManagedClientConfiguration checks managed-writer settings without
// creating the read-only or write capability.
func ValidateManagedClientConfiguration(base, token, declaredVersion string, timeout time.Duration, maxAccounts int) error {
	if (declaredVersion != "1.6.4" && declaredVersion != "v1.6.4") || maxAccounts < 1 || maxAccounts > 1000 {
		return errInvalidConfiguration
	}
	return ValidateClientConfiguration(base, token, timeout)
}

// NewManagedClient requires an explicit manual declaration of 1.6.4 (or
// v1.6.4). The GET status claim must exactly match that spelling before EVERY
// POST. HTTP supports private NetBird networks; HTTPS uses normal verification.
func NewManagedClient(base, token, declaredVersion string, timeout time.Duration, maxAccounts int) (*ManagedClient, error) {
	if err := ValidateManagedClientConfiguration(base, token, declaredVersion, timeout, maxAccounts); err != nil {
		return nil, err
	}
	// Reuse the read-only constructor's URL/token validation, NOT its partial
	// account DTOs. No mutation methods are added to Client.
	reader, err := New(base, token, timeout)
	if err != nil {
		return nil, err
	}
	writer, err := integrationhttp.NewWriter(base, http.Header{"Token": {token}}, timeout)
	if err != nil {
		return nil, errInvalidConfiguration
	}
	return &ManagedClient{http: reader.http, writer: writer, declaredVersion: declaredVersion, maxAccounts: maxAccounts}, nil
}

type managedGetter func(context.Context, string, url.Values) (map[string]any, error)

func (c *ManagedClient) get(ctx context.Context, endpoint string, query url.Values) (map[string]any, error) {
	return getManaged(ctx, c.http, endpoint, query)
}

func getManaged(ctx context.Context, reader *integrationhttp.Client, endpoint string, query url.Values) (map[string]any, error) {
	var raw json.RawMessage
	if _, err := reader.Get(ctx, []string{"apiv2", endpoint}, query, &raw); err != nil {
		return nil, err
	}
	return managedEnvelope(raw)
}
func managedEnvelope(raw []byte) (map[string]any, error) {
	v, err := uniqueJSON(raw)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, ErrManagedResponse
	}
	success, ok := m["success"].(bool)
	if !ok {
		return nil, ErrManagedResponse
	}
	if !success {
		return nil, errUnsuccessful
	}
	obj, ok := m["obj"].(map[string]any)
	if !ok {
		return nil, ErrManagedResponse
	}
	return obj, nil
}
func (c *ManagedClient) checkVersion(ctx context.Context) error {
	return checkManagedVersion(ctx, c.get, c.declaredVersion)
}

func checkManagedVersion(ctx context.Context, get managedGetter, declaredVersion string) error {
	obj, err := get(ctx, "status", url.Values{"r": {"sys"}})
	if err != nil {
		return err
	}
	sys, ok := obj["sys"].(map[string]any)
	if !ok || sys["appVersion"] != declaredVersion {
		return ErrManagedVersion
	}
	return nil
}

type managedSummary struct {
	id   int64
	name string
}

func (c *ManagedClient) summaries(ctx context.Context) ([]managedSummary, error) {
	obj, err := c.get(ctx, "clients", nil)
	if err != nil {
		return nil, err
	}
	list, ok := obj["clients"].([]any)
	if !ok || len(list) > c.maxAccounts {
		return nil, ErrManagedResponse
	}
	out := make([]managedSummary, 0, len(list))
	ids := map[int64]bool{}
	names := map[string]bool{}
	for _, v := range list {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, ErrManagedResponse
		}
		number, ok := m["id"].(json.Number)
		if !ok {
			return nil, ErrManagedResponse
		}
		id, err := number.Int64()
		if err != nil || id <= 0 || ids[id] {
			return nil, ErrManagedResponse
		}
		name, ok := m["name"].(string)
		if !ok || !validIdentity(name) || names[name] {
			return nil, ErrManagedResponse
		}
		ids[id], names[name] = true, true
		out = append(out, managedSummary{id, name})
	}
	return out, nil
}
func (c *ManagedClient) full(ctx context.Context, ids []int64) ([]ClientRecord, error) {
	return fullManagedRecords(ctx, c.get, ids)
}

func fullManagedRecords(ctx context.Context, get managedGetter, ids []int64) ([]ClientRecord, error) {
	if len(ids) == 0 || len(ids) > 50 {
		return nil, ErrManagedInvalid
	}
	requested := map[int64]bool{}
	query := make([]string, len(ids))
	for i, id := range ids {
		if id <= 0 || requested[id] {
			return nil, ErrManagedInvalid
		}
		requested[id] = true
		query[i] = strconv.FormatInt(id, 10)
	}
	obj, err := get(ctx, "clients", url.Values{"id": {strings.Join(query, ",")}})
	if err != nil {
		return nil, err
	}
	list, ok := obj["clients"].([]any)
	if !ok || len(list) != len(ids) {
		return nil, ErrManagedResponse
	}
	out := make([]ClientRecord, 0, len(ids))
	names := map[string]bool{}
	for _, v := range list {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, ErrManagedResponse
		}
		r, err := ParseClientRecord(raw)
		if err != nil {
			return nil, err
		}
		if !requested[r.ID()] || names[r.Name()] {
			return nil, ErrManagedResponse
		}
		delete(requested, r.ID())
		names[r.Name()] = true
		out = append(out, r)
	}
	if len(requested) != 0 {
		return nil, ErrManagedResponse
	}
	return out, nil
}

// inventory never saves summaries. It fetches EVERY listed ID in bounded
// batches and verifies exact ID/name sets; incomplete batches fail closed.
func (c *ManagedClient) inventory(ctx context.Context) ([]ClientRecord, error) {
	list, err := c.summaries(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ClientRecord, 0, len(list))
	for start := 0; start < len(list); start += 50 {
		end := start + 50
		if end > len(list) {
			end = len(list)
		}
		ids := make([]int64, 0, end-start)
		expected := map[int64]string{}
		for _, s := range list[start:end] {
			ids = append(ids, s.id)
			expected[s.id] = s.name
		}
		batch, err := c.full(ctx, ids)
		if err != nil {
			return nil, err
		}
		for _, r := range batch {
			if expected[r.ID()] != r.Name() {
				return nil, ErrManagedResponse
			}
			out = append(out, r)
		}
	}
	return out, nil
}
func (c *ManagedClient) GetRecord(ctx context.Context, id int64) (ClientRecord, error) {
	records, err := c.full(ctx, []int64{id})
	if err != nil {
		return ClientRecord{}, err
	}
	return records[0], nil
}

func (c *ManagedClient) verifyInbounds(ctx context.Context, ids []int64) error {
	obj, err := c.get(ctx, "inbounds", nil)
	if err != nil {
		return err
	}
	list, ok := obj["inbounds"].([]any)
	if !ok {
		return ErrManagedResponse
	}
	known := map[int64]string{}
	for _, v := range list {
		m, ok := v.(map[string]any)
		if !ok {
			return ErrManagedResponse
		}
		n, ok := m["id"].(json.Number)
		if !ok {
			return ErrManagedResponse
		}
		id, err := n.Int64()
		if err != nil || id <= 0 {
			return ErrManagedResponse
		}
		if _, exists := known[id]; exists {
			return ErrManagedResponse
		}
		t, ok := m["type"].(string)
		if !ok || t == "" {
			return ErrManagedResponse
		}
		known[id] = t
	}
	for _, id := range ids {
		if !supportedManagedInbound(known[id]) {
			return ErrManagedState
		}
	}
	return nil
}
func supportedManagedInbound(t string) bool {
	switch t {
	case "mixed", "socks", "http", "shadowsocks", "shadowtls", "vmess", "vless", "anytls", "trojan", "naive", "hysteria", "snell", "tuic", "hysteria2":
		return true
	}
	return false
}
func collision(candidate ClientRecord, inventory []ClientRecord, skipID int64) error {
	credentials, err := credentialSet(candidate)
	if err != nil {
		return err
	}
	for _, r := range inventory {
		if skipID > 0 && r.ID() == skipID {
			continue
		}
		other, err := credentialSet(r)
		if err != nil {
			return err
		}
		for hash := range other {
			if credentials[hash] {
				return ErrManagedConflict
			}
		}
	}
	return nil
}
func findCreated(expected ClientRecord, inventory []ClientRecord) (ClientRecord, bool, error) {
	for _, r := range inventory {
		if r.Name() != expected.Name() {
			continue
		}
		if !sameStatic(expected, r, true) || r.CredentialsHash() != expected.CredentialsHash() {
			return ClientRecord{}, false, ErrManagedConflict
		}
		if err := collision(expected, inventory, r.ID()); err != nil {
			return ClientRecord{}, false, err
		}
		return r, true, nil
	}
	if err := collision(expected, inventory, 0); err != nil {
		return ClientRecord{}, false, err
	}
	return ClientRecord{}, false, nil
}
func (c *ManagedClient) FindCreated(ctx context.Context, draft CreateDraft) (ClientRecord, bool, error) {
	r, err := cloneRecord(draft.record, true)
	if err != nil {
		return ClientRecord{}, false, ErrManagedInvalid
	}
	if err = validateCreate(r); err != nil {
		return ClientRecord{}, false, err
	}
	inventory, err := c.inventory(ctx)
	if err != nil {
		return ClientRecord{}, false, err
	}
	return findCreated(r, inventory)
}

func (c *ManagedClient) CreateDisabled(ctx context.Context, draft CreateDraft) (ClientRecord, error) {
	return c.createDisabled(ctx, draft, nil)
}

// CreateDisabledAuthorized requires final authorization after all GET preflights.
// Rejection is returned unchanged without a POST; authorization is never retried.
func (c *ManagedClient) CreateDisabledAuthorized(ctx context.Context, draft CreateDraft, authorize func(context.Context) error) (ClientRecord, error) {
	if authorize == nil {
		return ClientRecord{}, ErrManagedInvalid
	}
	return c.createDisabled(ctx, draft, authorize)
}

func (c *ManagedClient) createDisabled(ctx context.Context, draft CreateDraft, authorize func(context.Context) error) (ClientRecord, error) {
	// Snapshot all caller-owned bytes/slices before I/O or waiting on the lock.
	r, err := cloneRecord(draft.record, true)
	if err != nil {
		return ClientRecord{}, ErrManagedInvalid
	}
	if err = validateCreate(r); err != nil {
		return ClientRecord{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	inventory, err := c.inventory(ctx)
	if err != nil {
		return ClientRecord{}, err
	}
	if len(inventory) >= c.maxAccounts {
		return ClientRecord{}, ErrManagedState
	}
	for _, current := range inventory {
		if current.Name() == r.Name() {
			return ClientRecord{}, ErrManagedConflict
		}
	}
	if err = collision(r, inventory, 0); err != nil {
		return ClientRecord{}, err
	}
	if err = c.verifyInbounds(ctx, r.wire.Inbounds); err != nil {
		return ClientRecord{}, err
	}
	if err = c.checkVersion(ctx); err != nil {
		return ClientRecord{}, err
	}
	if authorize != nil {
		if err = authorize(ctx); err != nil {
			return ClientRecord{}, err
		}
		if err = ctx.Err(); err != nil {
			return ClientRecord{}, err
		}
	}
	postErr := c.save(ctx, "new", r)
	// The receipt is global obj.clients, not a new ID. The source's commit
	// errors are not propagated reliably, so even success needs NEW GETs.
	after, readErr := c.inventory(ctx)
	if postErr != nil || readErr != nil {
		return ClientRecord{}, ErrManagedUnknown
	}
	created, found, err := findCreated(r, after)
	if err != nil || !found {
		return ClientRecord{}, ErrManagedUnknown
	}
	return created, nil
}

func (c *ManagedClient) Enable(ctx context.Context, draft EnableDraft) (ClientRecord, error) {
	return c.enable(ctx, draft, nil)
}

// EnableAuthorized requires final authorization after all GET preflights.
// It retains Enable's mandatory full readback and unknown-write hold policy.
func (c *ManagedClient) EnableAuthorized(ctx context.Context, draft EnableDraft, authorize func(context.Context) error) (ClientRecord, error) {
	if authorize == nil {
		return ClientRecord{}, ErrManagedInvalid
	}
	return c.enable(ctx, draft, authorize)
}

func (c *ManagedClient) enable(ctx context.Context, draft EnableDraft, authorize func(context.Context) error) (ClientRecord, error) {
	prepared, err := PrepareEnable(draft.before, draft.binding)
	if err != nil {
		return ClientRecord{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	inventory, err := c.inventory(ctx)
	if err != nil {
		return ClientRecord{}, err
	}
	var current ClientRecord
	found := false
	for _, r := range inventory {
		if r.ID() == prepared.binding.ID {
			current = r
			found = true
			break
		}
	}
	if !found {
		return ClientRecord{}, ErrManagedState
	}
	if !matchesBinding(current, prepared.binding) {
		return ClientRecord{}, ErrManagedConflict
	}
	if current.Enabled() || !safeEnablePolicy(current) {
		return ClientRecord{}, ErrManagedState
	}
	if !sameStatic(prepared.before, current, false) {
		return ClientRecord{}, ErrManagedConflict
	}
	if err = collision(current, inventory, current.ID()); err != nil {
		return ClientRecord{}, err
	}
	if err = c.verifyInbounds(ctx, current.wire.Inbounds); err != nil {
		return ClientRecord{}, err
	}
	if err = c.checkVersion(ctx); err != nil {
		return ClientRecord{}, err
	}
	// Only enable changes. Preserve fresh counters, all nested config keys and
	// all links in the payload; only local link regeneration is tolerated.
	current.wire.Enable = true
	if authorize != nil {
		if err = authorize(ctx); err != nil {
			return ClientRecord{}, err
		}
		if err = ctx.Err(); err != nil {
			return ClientRecord{}, err
		}
	}
	postErr := c.save(ctx, "edit", current)
	after, readErr := c.inventory(ctx)
	if postErr != nil || readErr != nil {
		return ClientRecord{}, ErrManagedUnknown
	}
	for _, r := range after {
		if r.ID() == current.ID() {
			if !matchesBinding(r, prepared.binding) || !sameStatic(current, r, false) || collision(r, after, r.ID()) != nil {
				return ClientRecord{}, ErrManagedUnknown
			}
			return r, nil
		}
	}
	return ClientRecord{}, ErrManagedUnknown
}
func (c *ManagedClient) save(ctx context.Context, action string, r ClientRecord) error {
	var raw json.RawMessage
	_, err := c.writer.PostForm(ctx, []string{"apiv2", "save"}, nil, url.Values{"object": {"clients"}, "action": {action}, "data": {string(r.Raw())}}, &raw)
	if err != nil {
		return err
	}
	_, err = managedEnvelope(raw)
	return err
}

var _ ManagedAPI = (*ManagedClient)(nil)
