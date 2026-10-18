package sui

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// Binding MUST come from a persisted local mapping, never a panel display-name
// search. The caller owns that trust boundary and must persist it before writes.
type Binding struct {
	ID              int64
	Name            string
	CredentialsHash [32]byte
}

// CreateDraft contains credentials and a frozen disabled intent. Persist its
// sensitive JSON, name and credentials BEFORE CreateDisabled. No retry key or
// server-side idempotency is provided; use FindCreated to reconcile uncertainty.
type CreateDraft struct{ record ClientRecord }

func (CreateDraft) String() string                 { return "[sensitive SUI create draft]" }
func (CreateDraft) GoString() string               { return "[sensitive SUI create draft]" }
func (d CreateDraft) Name() string                 { return d.record.Name() }
func (d CreateDraft) InboundIDs() []int64          { return d.record.InboundIDs() }
func (d CreateDraft) CredentialsHash() [32]byte    { return d.record.CredentialsHash() }
func (d CreateDraft) MarshalJSON() ([]byte, error) { return d.record.Raw(), nil }
func (d *CreateDraft) UnmarshalJSON(raw []byte) error {
	r, err := parseRecord(append([]byte(nil), raw...), true)
	if err != nil {
		return err
	}
	if err = validateCreate(r); err != nil {
		return err
	}
	d.record = r
	return nil
}

// EnableDraft freezes the BEFORE image and persisted binding. Persist this
// sensitive intent before Enable. An already-enabled image is NOT adoptable.
type EnableDraft struct {
	before  ClientRecord
	binding Binding
}

func (EnableDraft) String() string   { return "[sensitive SUI enable draft]" }
func (EnableDraft) GoString() string { return "[sensitive SUI enable draft]" }

// Binding returns a value copy of the persisted identity, not an adoption claim.
func (d EnableDraft) Binding() Binding { return d.binding }

func (d EnableDraft) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Before  ClientRecord `json:"before"`
		Binding Binding      `json:"binding"`
	}{d.before, d.binding})
}
func (d *EnableDraft) UnmarshalJSON(raw []byte) error {
	v, err := uniqueJSON(raw)
	if err != nil {
		return err
	}
	m, ok := v.(map[string]any)
	if !ok || len(m) != 2 || m["before"] == nil || m["binding"] == nil {
		return ErrManagedInvalid
	}
	var data struct {
		Before  ClientRecord `json:"before"`
		Binding Binding      `json:"binding"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return ErrManagedInvalid
	}
	prepared, err := PrepareEnable(data.Before, data.Binding)
	if err != nil {
		return err
	}
	*d = prepared
	return nil
}

// NewDisabledDraft mirrors frontend randomConfigs at e4525297b002c1c3be234cc1c9695ef84be741a0,
// using stronger CSPRNG credentials. shadowsocks16 is a config variant, so the
// frontend has 15 configs covering 14 inbound protocol types.
func NewDisabledDraft(inboundIDs []int64) (CreateDraft, error) {
	ids := append([]int64(nil), inboundIDs...)
	if !validInboundIDs(ids, true) {
		return CreateDraft{}, ErrManagedInvalid
	}
	nameBytes, err := randomBytes(20)
	if err != nil {
		return CreateDraft{}, err
	}
	name := "pp-" + hex.EncodeToString(nameBytes)
	password, err := randomHex(20)
	if err != nil {
		return CreateDraft{}, err
	}
	ss16, err := randomBytes(16)
	if err != nil {
		return CreateDraft{}, err
	}
	ss32, err := randomBytes(32)
	if err != nil {
		return CreateDraft{}, err
	}
	uuidBytes, err := randomBytes(16)
	if err != nil {
		return CreateDraft{}, err
	}
	uuidBytes[6] = (uuidBytes[6] & 15) | 64
	uuidBytes[8] = (uuidBytes[8] & 63) | 128
	u := hex.EncodeToString(uuidBytes)
	uuid := u[:8] + "-" + u[8:12] + "-" + u[12:16] + "-" + u[16:20] + "-" + u[20:]
	snell, err := randomHex(16)
	if err != nil {
		return CreateDraft{}, err
	}
	cfg := map[string]map[string]any{}
	for _, p := range []string{"mixed", "socks", "http", "naive"} {
		cfg[p] = map[string]any{"username": name, "password": password}
	}
	for _, p := range []string{"anytls", "trojan", "hysteria2"} {
		cfg[p] = map[string]any{"name": name, "password": password}
	}
	cfg["shadowsocks"] = map[string]any{"name": name, "password": base64.StdEncoding.EncodeToString(ss32)}
	cfg["shadowtls"] = map[string]any{"name": name, "password": base64.StdEncoding.EncodeToString(ss32)}
	cfg["shadowsocks16"] = map[string]any{"name": name, "password": base64.StdEncoding.EncodeToString(ss16)}
	cfg["vmess"] = map[string]any{"name": name, "uuid": uuid, "alterId": 0}
	cfg["vless"] = map[string]any{"name": name, "uuid": uuid, "flow": "xtls-rprx-vision"}
	cfg["tuic"] = map[string]any{"name": name, "uuid": uuid, "password": password}
	cfg["hysteria"] = map[string]any{"name": name, "auth_str": password}
	cfg["snell"] = map[string]any{"name": name, "userkey": snell}
	config, _ := json.Marshal(cfg)
	r := ClientRecord{wire: clientWire{Name: name, Config: config, Inbounds: ids, Links: []map[string]string{}}}
	return CreateDraft{record: r}, nil
}
func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, ErrManagedInvalid
	}
	return b, nil
}
func randomHex(n int) (string, error) { b, e := randomBytes(n); return hex.EncodeToString(b), e }

// PrepareEnable does not perform I/O. Reset/delay policies are unsupported;
// quota and expiry remain frozen. A quota with zero traffic counters is unsafe
// because the pinned backend treats those counters as an explicit reset.
func PrepareEnable(record ClientRecord, binding Binding) (EnableDraft, error) {
	r, err := cloneRecord(record, false)
	if err != nil {
		return EnableDraft{}, ErrManagedInvalid
	}
	if !matchesBinding(r, binding) {
		return EnableDraft{}, ErrManagedConflict
	}
	if r.Enabled() || !safeEnablePolicy(r) || !validInboundIDs(r.wire.Inbounds, true) {
		return EnableDraft{}, ErrManagedState
	}
	return EnableDraft{before: r, binding: binding}, nil
}
func matchesBinding(r ClientRecord, b Binding) bool {
	return b.ID > 0 && r.ID() == b.ID && r.Name() == b.Name && r.CredentialsHash() == b.CredentialsHash
}
func validateCreate(r ClientRecord) error {
	w := r.wire
	if len(w.Name) != 43 || !strings.HasPrefix(w.Name, "pp-") {
		return ErrManagedInvalid
	}
	if b, err := hex.DecodeString(w.Name[3:]); err != nil || len(b) != 20 || strings.ToLower(w.Name) != w.Name {
		return ErrManagedInvalid
	}
	if w.ID != 0 || w.Enable || w.Volume != 0 || w.Expiry != 0 || w.Up != 0 || w.Down != 0 || w.CreatedAt != 0 || w.OnlineAt != 0 || w.TotalUp != 0 || w.TotalDown != 0 || w.Desc != "" || w.Group != "" || w.Remark != "" || len(w.Links) != 0 || !safeEnablePolicy(r) || !validInboundIDs(w.Inbounds, true) {
		return ErrManagedInvalid
	}
	// Validate the exact frontend schema and intentional intra-client sharing.
	var cfg map[string]map[string]any
	if json.Unmarshal(w.Config, &cfg) != nil || len(cfg) != 15 {
		return ErrManagedInvalid
	}
	password, ok := cfg["mixed"]["password"].(string)
	if !ok || len(password) < 32 {
		return ErrManagedInvalid
	}
	uuid, ok := cfg["vmess"]["uuid"].(string)
	if !ok || !validUUID4(uuid) {
		return ErrManagedInvalid
	}
	ss32, ok := cfg["shadowsocks"]["password"].(string)
	if !ok || !base64Size(ss32, 32) {
		return ErrManagedInvalid
	}
	ss16, ok := cfg["shadowsocks16"]["password"].(string)
	if !ok || !base64Size(ss16, 16) {
		return ErrManagedInvalid
	}
	snell, ok := cfg["snell"]["userkey"].(string)
	if !ok || len(snell) != 32 {
		return ErrManagedInvalid
	}
	expected := map[string]map[string]any{}
	for _, p := range []string{"mixed", "socks", "http", "naive"} {
		expected[p] = map[string]any{"username": w.Name, "password": password}
	}
	for _, p := range []string{"anytls", "trojan", "hysteria2"} {
		expected[p] = map[string]any{"name": w.Name, "password": password}
	}
	for _, p := range []string{"shadowsocks", "shadowtls"} {
		expected[p] = map[string]any{"name": w.Name, "password": ss32}
	}
	expected["shadowsocks16"] = map[string]any{"name": w.Name, "password": ss16}
	expected["vmess"] = map[string]any{"name": w.Name, "uuid": uuid, "alterId": float64(0)}
	expected["vless"] = map[string]any{"name": w.Name, "uuid": uuid, "flow": "xtls-rprx-vision"}
	expected["tuic"] = map[string]any{"name": w.Name, "uuid": uuid, "password": password}
	expected["hysteria"] = map[string]any{"name": w.Name, "auth_str": password}
	expected["snell"] = map[string]any{"name": w.Name, "userkey": snell}
	actualJSON, _ := json.Marshal(cfg)
	expectedJSON, _ := json.Marshal(expected)
	if string(actualJSON) != string(expectedJSON) {
		return ErrManagedInvalid
	}
	return nil
}
func validUUID4(s string) bool {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' || s[14] != '4' || !strings.ContainsRune("89ab", rune(s[19])) {
		return false
	}
	b, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	return err == nil && len(b) == 16 && strings.ToLower(s) == s
}
func base64Size(s string, n int) bool {
	b, e := base64.StdEncoding.DecodeString(s)
	return e == nil && len(b) == n && base64.StdEncoding.EncodeToString(b) == s
}

// Only hashes enter collision sets; values never enter errors or logging.
func credentialSet(r ClientRecord) (map[[32]byte]bool, error) {
	v, err := uniqueJSON(r.wire.Config)
	if err != nil {
		return nil, err
	}
	out := map[[32]byte]bool{}
	var visit func(any) error
	visit = func(v any) error {
		switch x := v.(type) {
		case map[string]any:
			for k, v := range x {
				switch k {
				case "uuid", "password", "auth_str", "userkey":
					s, ok := v.(string)
					if !ok || s == "" {
						return ErrManagedResponse
					}
					out[sha256.Sum256([]byte(s))] = true
					if k == "uuid" {
						// The protocol indexes parsed UUID bytes, not their JSON
						// spelling. gofrs/uuid accepts compact, braced and URN
						// forms as well as case-insensitive canonical strings.
						out[sha256.Sum256([]byte(strings.ToLower(s)))] = true
						if normalized, ok := canonicalCredentialUUID(s); ok {
							out[sha256.Sum256([]byte(normalized))] = true
						}
					}
				}
				if err := visit(v); err != nil {
					return err
				}
			}
		case []any:
			for _, v := range x {
				if err := visit(v); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := visit(v); err != nil {
		return nil, err
	}
	return out, nil
}

// canonicalCredentialUUID mirrors the accepted textual identities at the pin
// without adding a runtime dependency. Unknown strings retain their exact hash;
// this function does not rewrite credentials or claim that they are valid.
func canonicalCredentialUUID(value string) (string, bool) {
	s := strings.ToLower(value)
	if strings.HasPrefix(s, "urn:uuid:") {
		s = s[len("urn:uuid:"):]
	}
	if len(s) == 38 && s[0] == '{' && s[37] == '}' {
		s = s[1:37]
	}
	if len(s) == 36 {
		if s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
			return "", false
		}
		s = strings.ReplaceAll(s, "-", "")
	}
	if len(s) != 32 {
		return "", false
	}
	decoded, err := hex.DecodeString(s)
	if err != nil || len(decoded) != 16 {
		return "", false
	}
	s = hex.EncodeToString(decoded)
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:], true
}

// Keep fmt's interfaces explicit: adding exported fields must not expose secrets.
var _ fmt.Stringer = ClientRecord{}
var _ fmt.Stringer = CreateDraft{}
var _ fmt.Stringer = EnableDraft{}
