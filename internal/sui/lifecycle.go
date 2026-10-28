package sui

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
)

// DisableDraft freezes the full enabled before-image and its durable owner binding.
type DisableDraft struct {
	before  ClientRecord
	binding Binding
}

func (DisableDraft) String() string         { return "[sensitive SUI disable draft]" }
func (d DisableDraft) GoString() string     { return d.String() }
func (d DisableDraft) LogValue() slog.Value { return slog.StringValue(d.String()) }
func (d DisableDraft) Binding() Binding     { return d.binding }
func (d DisableDraft) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Before  ClientRecord `json:"before"`
		Binding Binding      `json:"binding"`
	}{d.before, d.binding})
}
func (d *DisableDraft) UnmarshalJSON(raw []byte) error {
	before, binding, err := decodeBindingDraft(raw)
	if err != nil {
		return err
	}
	prepared, err := PrepareDisable(before, binding)
	if err != nil {
		return err
	}
	*d = prepared
	return nil
}
func PrepareDisable(record ClientRecord, binding Binding) (DisableDraft, error) {
	r, e := cloneRecord(record, false)
	if e != nil {
		return DisableDraft{}, ErrManagedInvalid
	}
	if !matchesBinding(r, binding) {
		return DisableDraft{}, ErrManagedConflict
	}
	if !r.Enabled() || !safeEnablePolicy(r) || !validInboundIDs(r.wire.Inbounds, true) {
		return DisableDraft{}, ErrManagedState
	}
	return DisableDraft{r, binding}, nil
}

// CloseSessionsDraft freezes the bound disabled before-image. This command does
// not establish durable session closure or network connectivity.
type CloseSessionsDraft struct {
	before  ClientRecord
	binding Binding
}

func (CloseSessionsDraft) String() string         { return "[sensitive SUI close-sessions draft]" }
func (d CloseSessionsDraft) GoString() string     { return d.String() }
func (d CloseSessionsDraft) LogValue() slog.Value { return slog.StringValue(d.String()) }
func (d CloseSessionsDraft) Binding() Binding     { return d.binding }
func (d CloseSessionsDraft) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Before  ClientRecord `json:"before"`
		Binding Binding      `json:"binding"`
	}{d.before, d.binding})
}
func (d *CloseSessionsDraft) UnmarshalJSON(raw []byte) error {
	before, binding, err := decodeBindingDraft(raw)
	if err != nil {
		return err
	}
	prepared, err := PrepareCloseSessions(before, binding)
	if err != nil {
		return err
	}
	*d = prepared
	return nil
}
func PrepareCloseSessions(record ClientRecord, binding Binding) (CloseSessionsDraft, error) {
	r, e := cloneRecord(record, false)
	if e != nil {
		return CloseSessionsDraft{}, ErrManagedInvalid
	}
	if !matchesBinding(r, binding) {
		return CloseSessionsDraft{}, ErrManagedConflict
	}
	if r.Enabled() || !validInboundIDs(r.wire.Inbounds, true) {
		return CloseSessionsDraft{}, ErrManagedState
	}
	return CloseSessionsDraft{r, binding}, nil
}

func (c *ManagedClient) DisableAuthorized(ctx context.Context, d DisableDraft, authorize func(context.Context) error) (ClientRecord, error) {
	if authorize == nil {
		return ClientRecord{}, ErrManagedInvalid
	}
	prepared, e := PrepareDisable(d.before, d.binding)
	if e != nil {
		return ClientRecord{}, e
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	current, e := c.GetRecord(ctx, prepared.binding.ID)
	if e != nil {
		return ClientRecord{}, e
	}
	if !matchesBinding(current, prepared.binding) || !sameStatic(prepared.before, current, false) {
		return ClientRecord{}, ErrManagedConflict
	}
	if !current.Enabled() || !safeEnablePolicy(current) {
		return ClientRecord{}, ErrManagedState
	}
	current, e = c.GetRecord(ctx, prepared.binding.ID)
	if e != nil {
		return ClientRecord{}, e
	}
	if !matchesBinding(current, prepared.binding) || !sameStatic(prepared.before, current, false) {
		return ClientRecord{}, ErrManagedConflict
	}
	if !current.Enabled() || !safeEnablePolicy(current) {
		return ClientRecord{}, ErrManagedState
	}
	current.wire.Enable = false
	if e = authorize(ctx); e != nil {
		return ClientRecord{}, e
	}
	if e = ctx.Err(); e != nil {
		return ClientRecord{}, errors.Join(ErrManagedUnknown, e)
	}
	postErr := c.save(ctx, "edit", current)
	after, readErr := c.GetRecord(ctx, prepared.binding.ID)
	if postErr != nil || readErr != nil {
		return ClientRecord{}, ErrManagedUnknown
	}
	if !matchesBinding(after, prepared.binding) || after.Enabled() || !sameStatic(current, after, false) {
		return ClientRecord{}, ErrManagedUnknown
	}
	return after, nil
}

func (c *ManagedClient) CloseSessionsAuthorized(ctx context.Context, d CloseSessionsDraft, authorize func(context.Context) error) error {
	if authorize == nil {
		return ErrManagedInvalid
	}
	prepared, e := PrepareCloseSessions(d.before, d.binding)
	if e != nil {
		return e
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	current, e := c.GetRecord(ctx, prepared.binding.ID)
	if e != nil {
		return e
	}
	if !matchesBinding(current, prepared.binding) || !sameStatic(prepared.before, current, false) {
		return ErrManagedConflict
	}
	if current.Enabled() {
		return ErrManagedState
	}
	// This is deliberately the last GET before caller authorization.
	current, e = c.GetRecord(ctx, prepared.binding.ID)
	if e != nil {
		return e
	}
	if !matchesBinding(current, prepared.binding) || !sameStatic(prepared.before, current, false) {
		return ErrManagedConflict
	}
	if current.Enabled() {
		return ErrManagedState
	}
	if e = authorize(ctx); e != nil {
		return e
	}
	if e = ctx.Err(); e != nil {
		return errors.Join(ErrManagedUnknown, e)
	}
	var raw json.RawMessage
	_, e = c.writer.PostForm(ctx, []string{"apiv2", "closeSessions"}, nil, url.Values{"u": {prepared.binding.Name}}, &raw)
	if e == nil {
		e = managedMessageSuccess(raw)
	}
	after, readErr := c.GetRecord(ctx, prepared.binding.ID)
	if e != nil || readErr != nil {
		return ErrManagedUnknown
	}
	if !matchesBinding(after, prepared.binding) || after.Enabled() || !sameStatic(current, after, false) {
		return ErrManagedUnknown
	}
	return nil
}
func managedMessageSuccess(raw []byte) error {
	if len(raw) > 4<<20 {
		return ErrManagedResponse
	}
	v, e := uniqueJSON(raw)
	if e != nil {
		return ErrManagedResponse
	}
	m, ok := v.(map[string]any)
	if !ok {
		return ErrManagedResponse
	}
	success, ok := m["success"].(bool)
	if !ok {
		return ErrManagedResponse
	}
	if !success {
		return errors.New("SUI close-sessions command rejected")
	}
	return nil
}
