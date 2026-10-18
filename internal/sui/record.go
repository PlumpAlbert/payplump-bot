package sui

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"math/big"
	"strconv"
	"strings"
	"unicode"
)

// ClientRecord is a complete, sensitive account snapshot, never a list summary.
// Raw and JSON serialization explicitly expose secrets for durable storage;
// ordinary formatting is redacted. Callers must protect serialized snapshots.
type ClientRecord struct{ wire clientWire }

func (ClientRecord) String() string        { return "[sensitive SUI record]" }
func (ClientRecord) GoString() string      { return "[sensitive SUI record]" }
func (r ClientRecord) ID() int64           { return r.wire.ID }
func (r ClientRecord) Name() string        { return r.wire.Name }
func (r ClientRecord) Enabled() bool       { return r.wire.Enable }
func (r ClientRecord) InboundIDs() []int64 { return append([]int64(nil), r.wire.Inbounds...) }

// CredentialsHash fingerprints the ENTIRE canonical config, including unknown
// nested keys. It is not a display-name lookup or a proof of local ownership.
func (r ClientRecord) CredentialsHash() [32]byte    { return sha256.Sum256(r.wire.Config) }
func (r ClientRecord) Raw() json.RawMessage         { b, _ := json.Marshal(r.wire); return b }
func (r ClientRecord) MarshalJSON() ([]byte, error) { return r.Raw(), nil }
func (r *ClientRecord) UnmarshalJSON(b []byte) error {
	v, err := ParseClientRecord(b)
	if err == nil {
		*r = v
	}
	return err
}

// ParseClientRecord snapshots input and rejects partial/extended top-level
// schemas. The pinned backend drops unknown top-level fields on save.
func ParseClientRecord(raw []byte) (ClientRecord, error) { return parseRecord(bytes.Clone(raw), false) }

type clientWire struct {
	ID         int64               `json:"id,omitempty"`
	Enable     bool                `json:"enable"`
	Name       string              `json:"name"`
	Config     json.RawMessage     `json:"config"`
	Inbounds   []int64             `json:"inbounds"`
	Links      []map[string]string `json:"links"`
	Volume     int64               `json:"volume"`
	Expiry     int64               `json:"expiry"`
	Down       int64               `json:"down"`
	Up         int64               `json:"up"`
	Desc       string              `json:"desc"`
	Group      string              `json:"group"`
	Remark     string              `json:"remark"`
	CreatedAt  int64               `json:"createdAt"`
	OnlineAt   int64               `json:"onlineAt"`
	DelayStart bool                `json:"delayStart"`
	AutoReset  bool                `json:"autoReset"`
	ResetDays  int64               `json:"resetDays"`
	NextReset  int64               `json:"nextReset"`
	TotalUp    int64               `json:"totalUp"`
	TotalDown  int64               `json:"totalDown"`
}

var recordKeys = strings.Fields("id enable name config inbounds links volume expiry down up desc group remark createdAt onlineAt delayStart autoReset resetDays nextReset totalUp totalDown")

func parseRecord(raw []byte, draft bool) (ClientRecord, error) {
	value, err := uniqueJSON(raw)
	if err != nil {
		return ClientRecord{}, ErrManagedResponse
	}
	obj, ok := value.(map[string]any)
	count := len(recordKeys)
	if draft {
		count--
	}
	if !ok || len(obj) != count {
		return ClientRecord{}, ErrManagedResponse
	}
	for _, key := range recordKeys {
		if draft && key == "id" {
			continue
		}
		if obj[key] == nil {
			return ClientRecord{}, ErrManagedResponse
		}
	}
	var w clientWire
	if json.Unmarshal(raw, &w) != nil || (!draft && w.ID <= 0) || (draft && w.ID != 0) || !validIdentity(w.Name) || w.Inbounds == nil || w.Links == nil {
		return ClientRecord{}, ErrManagedResponse
	}
	if !validInboundIDs(w.Inbounds, false) {
		return ClientRecord{}, ErrManagedResponse
	}
	configs, ok := obj["config"].(map[string]any)
	if !ok {
		return ClientRecord{}, ErrManagedResponse
	}
	for _, v := range configs {
		cfg, ok := v.(map[string]any)
		if !ok {
			return ClientRecord{}, ErrManagedResponse
		}
		for _, key := range []string{"name", "username"} {
			if identity, exists := cfg[key]; exists && identity != w.Name {
				return ClientRecord{}, ErrManagedResponse
			}
		}
	}
	if !safeConfigNumbers(configs) {
		return ClientRecord{}, ErrManagedResponse
	}
	w.Config, err = json.Marshal(configs)
	if err != nil {
		return ClientRecord{}, ErrManagedResponse
	}
	// Backend links are []map[string]string. Reject null entries/values, too.
	links, ok := obj["links"].([]any)
	if !ok {
		return ClientRecord{}, ErrManagedResponse
	}
	for _, link := range links {
		m, ok := link.(map[string]any)
		if !ok {
			return ClientRecord{}, ErrManagedResponse
		}
		for _, v := range m {
			if _, ok := v.(string); !ok {
				return ClientRecord{}, ErrManagedResponse
			}
		}
	}
	return ClientRecord{wire: w}, nil
}

func validIdentity(s string) bool {
	return s != "" && strings.TrimSpace(s) == s && strings.IndexFunc(s, unicode.IsControl) < 0
}
func validInboundIDs(ids []int64, require bool) bool {
	if ids == nil || (require && len(ids) == 0) {
		return false
	}
	seen := map[int64]bool{}
	for _, id := range ids {
		if id <= 0 || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

// uniqueJSON rejects duplicate keys at EVERY depth, including envelopes and
// unknown config objects, before normal unmarshalling can erase evidence.
func uniqueJSON(raw []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	v, err := uniqueValue(d, 0)
	if err != nil {
		return nil, ErrManagedResponse
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, ErrManagedResponse
	}
	return v, nil
}
func uniqueValue(d *json.Decoder, depth int) (any, error) {
	if depth > 128 {
		return nil, ErrManagedResponse
	}
	t, err := d.Token()
	if err != nil {
		return nil, ErrManagedResponse
	}
	switch t {
	case json.Delim('{'):
		m := map[string]any{}
		for d.More() {
			k, err := d.Token()
			if err != nil {
				return nil, ErrManagedResponse
			}
			key, ok := k.(string)
			if !ok {
				return nil, ErrManagedResponse
			}
			if _, exists := m[key]; exists {
				return nil, ErrManagedResponse
			}
			v, err := uniqueValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			m[key] = v
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return nil, ErrManagedResponse
		}
		return m, nil
	case json.Delim('['):
		a := []any{}
		for d.More() {
			v, err := uniqueValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return nil, ErrManagedResponse
		}
		return a, nil
	default:
		if _, ok := t.(json.Delim); ok {
			return nil, ErrManagedResponse
		}
		return t, nil
	}
}

// setConfigIdentity decodes through float64 in the pinned backend. Canonicalize
// integer spellings, refusing fractional or inexact (>2^53-1) config numbers.
func safeConfigNumbers(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, v := range x {
			if n, ok := v.(json.Number); ok {
				normalized, good := safeNumber(n)
				if !good {
					return false
				}
				x[k] = normalized
			} else if !safeConfigNumbers(v) {
				return false
			}
		}
	case []any:
		for i, v := range x {
			if n, ok := v.(json.Number); ok {
				normalized, good := safeNumber(n)
				if !good {
					return false
				}
				x[i] = normalized
			} else if !safeConfigNumbers(v) {
				return false
			}
		}
	}
	return true
}
func safeNumber(n json.Number) (json.Number, bool) {
	// Bound the spelling before big.Rat exponent expansion.
	if len(n) > 64 {
		return "", false
	}
	if at := strings.IndexAny(string(n), "eE"); at >= 0 {
		exponent, err := strconv.ParseInt(string(n)[at+1:], 10, 32)
		if err != nil || exponent < -400 || exponent > 400 {
			return "", false
		}
	}
	f, err := n.Float64()
	if err != nil || f > 9007199254740991 || f < -9007199254740991 {
		return "", false
	}
	r, ok := new(big.Rat).SetString(string(n))
	if !ok || !r.IsInt() || !r.Num().IsInt64() {
		return "", false
	}
	i := r.Num().Int64()
	if i > 9007199254740991 || i < -9007199254740991 {
		return "", false
	}
	return json.Number(r.Num().String()), true
}

// ValidateRecordForDraft checks a full snapshot against a valid disabled intent,
// allowing only server-managed counters/timestamps and local link regeneration.
// It does not authorize adoption: the caller must bind the exact persisted ID.
func ValidateRecordForDraft(record ClientRecord, draft CreateDraft, enabled bool) error {
	actual, err := cloneRecord(record, false)
	if err != nil {
		return err
	}
	expected, err := cloneRecord(draft.record, true)
	if err != nil {
		return ErrManagedInvalid
	}
	if err = validateCreate(expected); err != nil {
		return err
	}
	expected.wire.Enable = enabled
	if actual.ID() <= 0 || actual.Name() != expected.Name() || actual.CredentialsHash() != expected.CredentialsHash() || !sameStatic(expected, actual, true) {
		return ErrManagedConflict
	}
	return nil
}

func cloneRecord(r ClientRecord, draft bool) (ClientRecord, error) {
	return parseRecord(r.Raw(), draft)
}
func sameStatic(a, b ClientRecord, ignoreID bool) bool {
	x, y := a.wire, b.wire
	if ignoreID {
		x.ID, y.ID = 0, 0
	}
	x.CreatedAt, x.OnlineAt, x.Up, x.Down, x.TotalUp, x.TotalDown = 0, 0, 0, 0, 0, 0
	y.CreatedAt, y.OnlineAt, y.Up, y.Down, y.TotalUp, y.TotalDown = 0, 0, 0, 0, 0, 0
	x.Links, y.Links = nonlocalLinks(x.Links), nonlocalLinks(y.Links)
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return bytes.Equal(xb, yb)
}
func nonlocalLinks(links []map[string]string) []map[string]string {
	out := []map[string]string{}
	for _, l := range links {
		if l["type"] != "local" {
			out = append(out, l)
		}
	}
	return out
}
func safeEnablePolicy(r ClientRecord) bool {
	w := r.wire
	// The backend interprets up=down=0 as a traffic-reset request. A stats
	// tick can accrue usage after our GET even for a disabled account. Without
	// a field-level enable endpoint/CAS, zero-counter quota edits are unsafe.
	return !w.DelayStart && !w.AutoReset && w.ResetDays == 0 && w.NextReset == 0 && (w.Volume == 0 || w.Up != 0 || w.Down != 0)
}
