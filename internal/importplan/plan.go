// Package importplan validates private, owner-supplied migration manifests. It
// deliberately does not grant application capabilities or call external APIs.
package importplan

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	MaxBytes   = 8 << 20
	MaxRecords = 10000
)

var canonicalPositive = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
var currencyCode = regexp.MustCompile(`^[A-Z]{3}$`)
var digest = regexp.MustCompile(`^[0-9a-f]{64}$`)
var safeVersion = regexp.MustCompile(`^[A-Za-z0-9._+/-]{1,64}$`)

type Record struct {
	TelegramID        string `json:"telegram_id"`
	InvoiceCustomerID string `json:"invoice_customer_id"`
	InvoiceNumber     string `json:"invoice_id_number"`
	CurrentInvoiceID  string `json:"current_invoice_id"`
	InvoiceStatus     string `json:"invoice_status"`
	RecurringID       string `json:"recurring_id"`
	SUIID             string `json:"sui_id"`
	SUIName           string `json:"sui_name"`
	ConfigHash        string `json:"config_hash"`
	PeriodStart       string `json:"period_start"`
	PeriodEnd         string `json:"period_end"`
	Currency          string `json:"currency"`
	Lifecycle         string `json:"lifecycle"`
	SourceVersion     string `json:"source_version"`
	PinnedProvenance  bool   `json:"pinned_provenance"`
	Excluded          bool   `json:"excluded"`
	CompleteProof     bool   `json:"complete_proof"`
	UnpaidInvoices    int    `json:"unpaid_invoices"`
	Amount            string `json:"amount"`
}
type Manifest struct {
	Records []Record `json:"records"`
}
type Candidate struct {
	Index int    `json:"index"`
	Facts Record `json:"facts"`
}
type Plan struct {
	Hash       string         `json:"hash"`
	Candidates []Candidate    `json:"-"`
	Reasons    map[string]int `json:"reasons"`
	Imported   int            `json:"imported"`
	Total      int            `json:"total"`
	manifest   []byte
}

func (p Plan) String() string {
	return fmt.Sprintf("import plan hash=%s total=%d candidates=%d", p.Hash, p.Total, p.Imported)
}
func (p Plan) GoString() string { return p.String() }
func (p Plan) LogValue() slog.Value {
	return slog.GroupValue(slog.String("plan_hash", p.Hash), slog.Int("total", p.Total), slog.Int("candidates", p.Imported))
}

type recordWire Record

// MarshalJSON redacts generic serialization; use PrivateJSON only for the private journal.
func (r Record) MarshalJSON() ([]byte, error) { return []byte(`"<private import record>"`), nil }
func (r Record) PrivateJSON() ([]byte, error) { return json.Marshal(recordWire(r)) }

func (r Record) String() string   { return "<private import record>" }
func (r Record) GoString() string { return r.String() }

// Decode enforces size, depth, duplicate-key, trailing-data, and exact schema
// rules before exposing a manifest. Error messages never include input values.
func DecodePrivateRecord(data []byte) (Record, error) {
	var wrapped bytes.Buffer
	wrapped.WriteString(`{"records":[`)
	wrapped.Write(data)
	wrapped.WriteString(`]}`)
	m, err := Decode(&wrapped)
	if err != nil || len(m.Records) != 1 {
		return Record{}, errors.New("private candidate facts are invalid")
	}
	return m.Records[0], nil
}

func Decode(r io.Reader) (Manifest, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
	if err != nil || len(b) > MaxBytes {
		return Manifest{}, errors.New("manifest read failed or exceeds 8 MiB")
	}
	if !utf8.Valid(b) {
		return Manifest{}, errors.New("manifest JSON is invalid")
	}
	if err := checkJSON(b, 32); err != nil {
		return Manifest{}, errors.New("manifest JSON is invalid")
	}
	var shape any
	if err := json.Unmarshal(b, &shape); err != nil || !validSchema(shape) {
		return Manifest{}, errors.New("manifest schema is invalid")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, errors.New("manifest schema is invalid")
	}
	if m.Records == nil {
		return Manifest{}, errors.New("manifest records are required")
	}
	if len(m.Records) > MaxRecords {
		return Manifest{}, errors.New("manifest exceeds 10000 records")
	}
	return m, nil
}
func checkJSON(b []byte, maxDepth int) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > maxDepth {
			return errors.New("depth")
		}
		t, e := d.Token()
		if e != nil {
			return e
		}
		switch x := t.(type) {
		case json.Delim:
			switch x {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					k, e := d.Token()
					if e != nil {
						return e
					}
					key, ok := k.(string)
					if !ok {
						return errors.New("key")
					}
					folded := simpleFoldKey(key)
					if seen[folded] {
						return errors.New("duplicate")
					}
					seen[folded] = true
					if e = walk(depth + 1); e != nil {
						return e
					}
				}
				_, e = d.Token()
				return e
			case '[':
				for d.More() {
					if e := walk(depth + 1); e != nil {
						return e
					}
				}
				_, e = d.Token()
				return e
			}
		}
		return nil
	}
	if e := walk(0); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return errors.New("trailing")
	}
	return nil
}

func simpleFoldKey(s string) string {
	var b strings.Builder
	for _, r := range s {
		min := r
		for n := unicode.SimpleFold(r); n != r; n = unicode.SimpleFold(n) {
			if n < min {
				min = n
			}
		}
		b.WriteRune(min)
	}
	return b.String()
}

func validSchema(v any) bool {
	root, ok := v.(map[string]any)
	if !ok || len(root) != 1 {
		return false
	}
	records, ok := root["records"].([]any)
	if !ok {
		return false
	}
	allowed := map[string]bool{}
	t := reflect.TypeOf(Record{})
	for i := 0; i < t.NumField(); i++ {
		allowed[t.Field(i).Tag.Get("json")] = true
	}
	for _, item := range records {
		r, ok := item.(map[string]any)
		if !ok || len(r) != len(allowed) {
			return false
		}
		for key, value := range r {
			if !allowed[key] || value == nil {
				return false
			}
		}
	}
	return true
}
func Analyze(m Manifest) Plan {
	p := Plan{Total: len(m.Records), Reasons: map[string]int{}}
	wire := struct {
		Records []recordWire `json:"records"`
	}{Records: make([]recordWire, len(m.Records))}
	for i, r := range m.Records {
		wire.Records[i] = recordWire(r)
	}
	canon, _ := json.Marshal(wire)
	p.manifest = append([]byte(nil), canon...)
	sum := sha256.Sum256(canon)
	p.Hash = hex.EncodeToString(sum[:])
	byTG := map[string]int{}
	byInvoice := map[string]int{}
	byRecurring := map[string]int{}
	byInvoiceRecord := map[string]int{}
	bySUI := map[string]int{}
	bySUIName := map[string]int{}
	for _, r := range m.Records {
		byTG[r.TelegramID]++
		byInvoice[r.InvoiceCustomerID]++
		byRecurring[r.RecurringID]++
		byInvoiceRecord[r.CurrentInvoiceID]++
		bySUI[r.SUIID]++
		bySUIName[r.SUIName]++
	}
	for i, r := range m.Records {
		reason := ""
		switch {
		case r.Excluded:
			reason = "excluded"
		case !canonicalID(r.TelegramID) || !canonicalID(r.InvoiceNumber) || r.TelegramID != r.InvoiceNumber:
			reason = "invalid_identity"
		case !bounded(r.InvoiceCustomerID, 256) || !bounded(r.CurrentInvoiceID, 256) || !bounded(r.RecurringID, 256) || !bounded(r.SUIID, 256) || !bounded(r.SUIName, 256) || !safeVersion.MatchString(r.SourceVersion):
			reason = "invalid_source_facts"
		case byTG[r.TelegramID] != 1 || byInvoice[r.InvoiceCustomerID] != 1 || byRecurring[r.RecurringID] != 1 || byInvoiceRecord[r.CurrentInvoiceID] != 1 || bySUI[r.SUIID] != 1 || bySUIName[r.SUIName] != 1:
			reason = "ambiguous_pair"
		case r.InvoiceCustomerID == "" || r.RecurringID == "" || r.SUIID == "" || r.SUIName == "" || !digest.MatchString(r.ConfigHash):
			reason = "incomplete_binding"
		case r.InvoiceStatus != "paid" && r.InvoiceStatus != "sent" && r.InvoiceStatus != "partial":
			reason = "unsupported_invoice_status"
		case !r.CompleteProof:
			reason = "incomplete_payment_proof"
		case r.UnpaidInvoices < 0:
			reason = "invalid_invoice_count"
		case r.UnpaidInvoices > 1:
			reason = "multiple_unpaid_invoices"
		case !r.PinnedProvenance || strings.TrimSpace(r.SourceVersion) == "":
			reason = "unverified_provenance"
		case r.Lifecycle != "active" && r.Lifecycle != "paused":
			reason = "unsupported_lifecycle"
		case !currencyCode.MatchString(r.Currency):
			reason = "invalid_currency"
		case !validPeriod(r.PeriodStart, r.PeriodEnd):
			reason = "invalid_period"
		case !validAmount(r.Amount):
			reason = "invalid_amount"
		}
		if reason != "" {
			p.Reasons[reason]++
			continue
		}
		p.Candidates = append(p.Candidates, Candidate{Index: i, Facts: r})
	}
	p.Imported = len(p.Candidates)
	return p
}
func canonicalID(s string) bool {
	n, err := strconv.ParseInt(s, 10, 64)
	return err == nil && n > 0 && canonicalPositive.MatchString(s)
}
func bounded(s string, max int) bool {
	if s == "" || len(s) > max || strings.TrimSpace(s) != s || strings.Contains(s, "://") {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validPeriod(a, b string) bool {
	start, e := time.Parse("2006-01-02", a)
	if e != nil {
		return false
	}
	end, e := time.Parse("2006-01-02", b)
	return e == nil && end.After(start)
}
func validAmount(s string) bool {
	if s == "" || strings.HasPrefix(s, "+") || strings.HasPrefix(s, "-") {
		return false
	}
	parts := strings.Split(s, ".")
	if len(parts) > 2 || len(parts[0]) == 0 || len(parts[0]) > 12 {
		return false
	}
	if len(parts[0]) > 1 && parts[0][0] == '0' {
		return false
	}
	nonzero := false
	for _, p := range parts {
		if p == "" {
			return false
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return false
			}
			if c != '0' {
				nonzero = true
			}
		}
	}
	return nonzero && (len(parts) == 1 || len(parts[1]) <= 4)
}
func SafeSummary(p Plan) []byte {
	reasons := make(map[string]int, len(p.Reasons))
	for k, v := range p.Reasons {
		reasons[k] = v
	}
	indexes := make([]int, 0, len(p.Candidates))
	for _, candidate := range p.Candidates {
		indexes = append(indexes, candidate.Index)
	}
	b, _ := json.Marshal(struct {
		Hash       string         `json:"plan_hash"`
		Total      int            `json:"total"`
		Candidates int            `json:"candidates"`
		Indexes    []int          `json:"candidate_indexes"`
		Reasons    map[string]int `json:"manual_reasons"`
	}{p.Hash, p.Total, p.Imported, indexes, reasons})
	return b
}
func (p Plan) ValidateHash(expected string) error {
	if !digest.MatchString(expected) || !digest.MatchString(p.Hash) || expected != p.Hash || len(p.manifest) == 0 {
		return errors.New("expected plan hash does not match")
	}
	var m Manifest
	if json.Unmarshal(p.manifest, &m) != nil {
		return errors.New("plan integrity check failed")
	}
	fresh := Analyze(m)
	if fresh.Hash != p.Hash || fresh.Total != p.Total || fresh.Imported != p.Imported || !reflect.DeepEqual(fresh.Candidates, p.Candidates) || !reflect.DeepEqual(fresh.Reasons, p.Reasons) {
		return errors.New("plan integrity check failed")
	}
	return nil
}
