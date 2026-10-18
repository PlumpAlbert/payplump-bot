package broadcast

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
)

// Policy is the immutable, per-bot authorization fence installed in storage.
type Policy struct {
	Enabled       bool
	Revision      int64
	Hash          [32]byte
	AdminIDs      []int64
	MaxRecipients int
}

type Progress struct{ Enqueued, Skipped, Completed int }

var (
	ErrStalePolicy    = errors.New("stale broadcast policy")
	ErrInvalidCommand = errors.New("invalid broadcast command")
	ErrExpired        = errors.New("broadcast draft expired")
)

func (Policy) String() string               { return "[private broadcast policy]" }
func (Policy) GoString() string             { return "[private broadcast policy]" }
func (Policy) LogValue() slog.Value         { return slog.StringValue("[private broadcast policy]") }
func (Policy) MarshalJSON() ([]byte, error) { return []byte(`"[private broadcast policy]"`), nil }
func (p Policy) Equal(q Policy) bool {
	if p.Enabled != q.Enabled || p.Revision != q.Revision || p.Hash != q.Hash || p.MaxRecipients != q.MaxRecipients || len(p.AdminIDs) != len(q.AdminIDs) {
		return false
	}
	for i := range p.AdminIDs {
		if p.AdminIDs[i] != q.AdminIDs[i] {
			return false
		}
	}
	return true
}
func (p Policy) Validate() error {
	if !p.Enabled {
		if p.Revision != 0 || p.Hash != ([32]byte{}) || len(p.AdminIDs) != 0 || p.MaxRecipients != 0 {
			return fmt.Errorf("invalid disabled policy")
		}
		return nil
	}
	if p.Revision < 1 || len(p.AdminIDs) < 1 || len(p.AdminIDs) > 1000 || p.MaxRecipients < 1 || p.MaxRecipients > 10000 {
		return fmt.Errorf("invalid broadcast policy")
	}
	ids := append([]int64(nil), p.AdminIDs...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for i, id := range ids {
		if id <= 0 || (i > 0 && ids[i-1] == id) {
			return fmt.Errorf("invalid broadcast policy")
		}
	}
	if p.Hash != policyHash(ids, p.MaxRecipients) {
		return fmt.Errorf("invalid broadcast policy hash")
	}
	return nil
}
func policyHash(ids []int64, max int) [32]byte {
	ids = append([]int64(nil), ids...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	projection := struct {
		Version       int     `json:"version"`
		AdminIDs      []int64 `json:"admin_ids"`
		MaxRecipients int     `json:"max_recipients"`
	}{Version: 1, AdminIDs: ids, MaxRecipients: max}
	b, _ := json.Marshal(projection)
	return sha256.Sum256(b)
}
