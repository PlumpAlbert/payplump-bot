// Package importorigin contains the small, comparable identity reference for
// database-verified imported accounts. It deliberately carries no source data.
package importorigin

import (
	"encoding/hex"
	"errors"
	"log/slog"
)

type Ref struct {
	ClientID, BotID, AdmissionRevision, RuntimeRevision int64
	ConfigurationRevision                               int64
	StableHash, PolicyHash, SourceScopeHash             [32]byte
}

func (r Ref) Valid() bool {
	return r.ClientID > 0 && r.BotID > 0 && r.AdmissionRevision > 0 && r.RuntimeRevision > 0 && r.ConfigurationRevision >= 0 &&
		r.StableHash != ([32]byte{}) && r.PolicyHash != ([32]byte{}) && r.SourceScopeHash != ([32]byte{})
}

func (Ref) String() string               { return "<private imported origin reference>" }
func (r Ref) GoString() string           { return r.String() }
func (r Ref) LogValue() slog.Value       { return slog.StringValue(r.String()) }
func (Ref) MarshalJSON() ([]byte, error) { return []byte(`"<private imported origin reference>"`), nil }
func (r *Ref) UnmarshalJSON([]byte) error {
	if r == nil {
		return errors.New("invalid imported origin reference")
	}
	return errors.New("imported origin reference cannot be decoded from JSON")
}

func ParseHashHex(value string) ([32]byte, bool) {
	var out [32]byte
	if len(value) != 64 {
		return out, false
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || hex.EncodeToString(decoded) != value {
		return out, false
	}
	return ParseHash(decoded)
}

func ParseHash(value []byte) ([32]byte, bool) {
	var out [32]byte
	if len(value) != len(out) {
		return out, false
	}
	copy(out[:], value)
	return out, out != ([32]byte{})
}

func (r Ref) Hashes() (stable, policy, scope string) {
	return hex.EncodeToString(r.StableHash[:]), hex.EncodeToString(r.PolicyHash[:]), hex.EncodeToString(r.SourceScopeHash[:])
}
