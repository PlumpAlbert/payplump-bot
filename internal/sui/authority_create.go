package sui

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// AuthorityCreateDraft is a complete sensitive create intent. Persist its JSON
// before writing. A pp- name is accepted only to recover the caller's own
// persisted binding; syntax validation is not ownership authorization.
type AuthorityCreateDraft struct{ record ClientRecord }

func (AuthorityCreateDraft) String() string              { return "[sensitive SUI authority create draft]" }
func (AuthorityCreateDraft) GoString() string            { return "[sensitive SUI authority create draft]" }
func (d AuthorityCreateDraft) Name() string              { return d.record.Name() }
func (d AuthorityCreateDraft) Enabled() bool             { return d.record.Enabled() }
func (d AuthorityCreateDraft) CredentialsHash() [32]byte { return d.record.CredentialsHash() }
func (d AuthorityCreateDraft) InboundIDs() []int64       { return d.record.InboundIDs() }
func (d AuthorityCreateDraft) MarshalJSON() ([]byte, error) {
	r, err := cloneRecord(d.record, true)
	if err != nil {
		return nil, ErrManagedInvalid
	}
	if err = validateAuthorityCreate(r); err != nil {
		return nil, err
	}
	return r.Raw(), nil
}
func (d *AuthorityCreateDraft) UnmarshalJSON(raw []byte) error {
	if len(raw) > maxBindingDraftSize {
		return ErrManagedInvalid
	}
	r, err := parseRecord(append([]byte(nil), raw...), true)
	if err != nil {
		return err
	}
	if err = validateAuthorityCreate(r); err != nil {
		return err
	}
	d.record = r
	return nil
}

func validAuthorityCreateName(name string) bool {
	if id, err := strconv.ParseInt(name, 10, 64); err == nil && id > 0 && strconv.FormatInt(id, 10) == name {
		return true
	}
	if len(name) != 43 || !strings.HasPrefix(name, "pp-") || strings.ToLower(name) != name {
		return false
	}
	b, err := hex.DecodeString(name[3:])
	return err == nil && len(b) == 20
}
func validateAuthorityCreate(r ClientRecord) error {
	if !validAuthorityCreateName(r.Name()) {
		return ErrManagedInvalid
	}
	return validateCreateSchema(r)
}

// NewAuthorityCreateDraft generates strong protocol credentials and binds every
// protocol identity to name, with the requested initial enabled state.
func NewAuthorityCreateDraft(name string, enabled bool, inboundIDs []int64) (AuthorityCreateDraft, error) {
	if !validAuthorityCreateName(name) {
		return AuthorityCreateDraft{}, ErrManagedInvalid
	}
	d, err := NewDisabledDraft(inboundIDs)
	if err != nil {
		return AuthorityCreateDraft{}, err
	}
	r := d.record
	var cfg map[string]map[string]any
	if json.Unmarshal(r.wire.Config, &cfg) != nil {
		return AuthorityCreateDraft{}, ErrManagedInvalid
	}
	for _, fields := range cfg {
		for _, key := range []string{"name", "username"} {
			if _, exists := fields[key]; exists {
				fields[key] = name
			}
		}
	}
	r.wire.Config, err = json.Marshal(cfg)
	if err != nil {
		return AuthorityCreateDraft{}, ErrManagedInvalid
	}
	r.wire.Name, r.wire.Enable = name, enabled
	if err = validateAuthorityCreate(r); err != nil {
		return AuthorityCreateDraft{}, err
	}
	return AuthorityCreateDraft{record: r}, nil
}

// AuthorityClient exposes only full inventory, exact reconciliation and a single
// authorized create. Callers own durable authorization, source fencing and
// exclusive cross-process/panel concurrency. Unknown writes must not be retried.
type AuthorityClient struct{ managed *ManagedClient }

func NewAuthorityClient(base, token string, timeout time.Duration, maxAccounts int) (*AuthorityClient, error) {
	c, err := NewManagedClient(base, token, "", timeout, maxAccounts)
	if err != nil {
		return nil, err
	}
	return &AuthorityClient{managed: c}, nil
}
func (c *AuthorityClient) Inventory(ctx context.Context) ([]ClientRecord, error) {
	return c.managed.inventory(ctx)
}
func (c *AuthorityClient) FindCreated(ctx context.Context, draft AuthorityCreateDraft) (ClientRecord, bool, error) {
	r, err := cloneRecord(draft.record, true)
	if err != nil {
		return ClientRecord{}, false, ErrManagedInvalid
	}
	if err = validateAuthorityCreate(r); err != nil {
		return ClientRecord{}, false, err
	}
	inventory, err := c.managed.inventory(ctx)
	if err != nil {
		return ClientRecord{}, false, err
	}
	return findCreated(r, inventory)
}
func (c *AuthorityClient) CreateAuthorized(ctx context.Context, draft AuthorityCreateDraft, authorize func(context.Context) error) (ClientRecord, error) {
	if authorize == nil {
		return ClientRecord{}, ErrManagedInvalid
	}
	r, err := cloneRecord(draft.record, true)
	if err != nil {
		return ClientRecord{}, ErrManagedInvalid
	}
	if err = validateAuthorityCreate(r); err != nil {
		return ClientRecord{}, err
	}
	m := c.managed
	m.mu.Lock()
	defer m.mu.Unlock()
	inventory, err := m.inventory(ctx)
	if err != nil {
		return ClientRecord{}, err
	}
	if len(inventory) >= m.maxAccounts {
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
	if err = m.verifyInbounds(ctx, r.InboundIDs()); err != nil {
		return ClientRecord{}, err
	}
	if err = authorize(ctx); err != nil {
		return ClientRecord{}, err
	}
	if err = ctx.Err(); err != nil {
		return ClientRecord{}, err
	}
	postErr := m.save(ctx, "new", r)
	after, readErr := m.inventory(ctx)
	if postErr != nil || readErr != nil {
		return ClientRecord{}, ErrManagedUnknown
	}
	created, found, err := findCreated(r, after)
	if err != nil || !found {
		return ClientRecord{}, ErrManagedUnknown
	}
	return created, nil
}
