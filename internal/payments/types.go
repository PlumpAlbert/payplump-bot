// Package payments verifies manual Invoice Ninja payment evidence through GETs.
// It records observations, never grants access or mutates external services.
package payments

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/PlumpAlbert/payplump-bot/internal/billing"
	"github.com/PlumpAlbert/payplump-bot/internal/importorigin"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
)

const (
	ReviewAssociation  = 2001
	ReviewInvoice      = 2002
	ReviewAllocations  = 2003
	ReviewCurrency     = 2004
	ReviewDate         = 2005
	ReviewContract     = 2006
	ReviewChangedProof = 2007
	ReviewExcluded     = 2008
)

type Claim struct {
	queue.Lease
	BotID, Generation int64
}

// Snapshot is private and comparable; finalize rechecks it under account locks.
type Snapshot struct {
	ClientID, TelegramID, ChatID, BotID                              int64
	CustomerID, InvoiceID, FirstInvoiceID, RecurringID, SUIID, State string
	DesiredAccess                                                    bool
	PaymentDate, NextInvoiceDate                                     string
	ImportedOrigin                                                   importorigin.Ref
	ImportedBaselineProofHash                                        [32]byte
}
type Kind string

const (
	Pending     Kind = "pending"
	Verified    Kind = "verified"
	NeedsReview Kind = "review"
)

type Allocation struct {
	PaymentID    string `json:"payment_id"`
	AllocationID string `json:"allocation_id"`
	Amount       string `json:"amount"`
	Date         string `json:"date"`
}
type Evidence struct {
	InvoiceID       string       `json:"invoice_id"`
	CustomerID      string       `json:"customer_id"`
	Amount          string       `json:"amount"`
	CurrencyID      string       `json:"currency_id"`
	PaymentDate     string       `json:"payment_date"`
	NextInvoiceDate string       `json:"next_invoice_date"`
	Allocations     []Allocation `json:"allocations"`
}
type Proof struct {
	Evidence
	InvoiceNumber string
	Hash          [32]byte
	JSON          json.RawMessage
}

func (p Proof) Validate() error {
	if !identifier(p.InvoiceID) || !identifier(p.CustomerID) || !printable(p.InvoiceNumber, 128) || p.InvoiceNumber == "" || !currency(p.CurrencyID) || len(p.JSON) == 0 || len(p.JSON) > 64*1024 || sha256.Sum256(p.JSON) != p.Hash {
		return errors.New("invalid payment proof")
	}
	amount, err := billing.FormatAmount(json.Number(p.Amount))
	if err != nil || amount == "0" || amount != p.Amount {
		return errors.New("invalid payment proof amount")
	}
	next, err := billing.NextBillingDate(p.PaymentDate)
	if err != nil || next != p.NextInvoiceDate {
		return errors.New("invalid payment proof dates")
	}
	var decoded Evidence
	if json.Unmarshal(p.JSON, &decoded) != nil {
		return errors.New("invalid payment proof evidence")
	}
	canonical, err := json.Marshal(p.Evidence)
	if err != nil || string(canonical) != string(p.JSON) || jsonbSizeBound(p.JSON) > 64*1024 {
		return errors.New("payment proof evidence mismatch")
	}
	if len(p.Allocations) == 0 || len(p.Allocations) > 1000 {
		return errors.New("invalid payment proof allocations")
	}
	seen := map[string]bool{}
	sum := new(big.Rat)
	latest := ""
	for _, a := range p.Allocations {
		if !identifier(a.PaymentID) || !identifier(a.AllocationID) || seen[a.AllocationID] || !validDate(a.Date) {
			return errors.New("invalid payment proof allocation")
		}
		seen[a.AllocationID] = true
		value, err := billing.FormatAmount(json.Number(a.Amount))
		if err != nil || value == "0" || value != a.Amount {
			return errors.New("invalid payment proof allocation amount")
		}
		n, _ := new(big.Rat).SetString(value)
		sum.Add(sum, n)
		if a.Date > latest {
			latest = a.Date
		}
	}
	expected, _ := new(big.Rat).SetString(amount)
	if sum.Cmp(expected) != 0 || latest != p.PaymentDate {
		return errors.New("payment proof coverage mismatch")
	}
	return nil
}

type Decision struct {
	Kind  Kind
	Code  int
	Proof *Proof
}

func (d Decision) Validate() error {
	switch d.Kind {
	case Pending:
		if d.Proof == nil && d.Code == 0 {
			return nil
		}
	case NeedsReview:
		if d.Proof == nil && d.Code >= 2001 && d.Code <= 2008 {
			return nil
		}
	case Verified:
		if d.Proof != nil && d.Code == 0 {
			return d.Proof.Validate()
		}
	}
	return errors.New("invalid payment decision")
}

type Notices struct{ Confirmed, Review []queue.Draft }

type Store interface {
	RecordPaymentWake(context.Context, int64, [32]byte) (bool, error)
	SeedPaymentChecks(context.Context, int64) (int, error)
	ClaimPaymentChecks(context.Context, int64, int) ([]Claim, error)
	LoadPaymentSnapshot(context.Context, Claim) (Snapshot, error)
	DeferPaymentCheck(context.Context, Claim, time.Duration, int) error
	FinishPaymentCheck(context.Context, Claim, Snapshot, Decision, Notices, time.Duration) error
}

// PostgreSQL jsonb::text adds spaces after structural commas/colons. Evidence
// contains only strings/arrays/objects (no expanding JSON numbers), so this is
// an upper bound that aligns accepted proofs with the database's byte limit.
func jsonbSizeBound(raw []byte) int {
	n := len(raw)
	quoted, escaped := false, false
	for _, b := range raw {
		if quoted {
			if escaped {
				escaped = false
			} else if b == '\\' {
				escaped = true
			} else if b == '"' {
				quoted = false
			}
			continue
		}
		if b == '"' {
			quoted = true
		} else if b == ',' || b == ':' {
			n++
		}
	}
	return n
}

func identifier(s string) bool {
	return s != "" && len(s) <= 256 && s != "." && s != ".." && utf8.ValidString(s) && strings.TrimSpace(s) == s && !strings.ContainsAny(s, "/\\") && strings.IndexFunc(s, unicode.IsControl) < 0
}
func printable(s string, max int) bool {
	return utf8.ValidString(s) && strings.TrimSpace(s) == s && utf8.RuneCountInString(s) <= max && strings.IndexFunc(s, unicode.IsControl) < 0
}
func currency(s string) bool {
	if s == "" || len(s) > 10 || s[0] < '1' || s[0] > '9' {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
func validDate(s string) bool {
	d, err := time.Parse(time.DateOnly, s)
	return err == nil && d.Year() > 0 && d.Format(time.DateOnly) == s
}
