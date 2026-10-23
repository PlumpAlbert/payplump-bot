// Package importadmission prepares fresh, read-verified legacy facts for a
// future database-only admission. Preparation itself has no write capability.
package importadmission

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/importplan"
	"github.com/PlumpAlbert/payplump-bot/internal/importverify"
)

var digest = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (AdmissionRow) String() string         { return "<private import admission row>" }
func (r AdmissionRow) GoString() string     { return r.String() }
func (r AdmissionRow) LogValue() slog.Value { return slog.StringValue(r.String()) }
func (AdmissionRow) MarshalJSON() ([]byte, error) {
	return []byte(`"<private import admission row>"`), nil
}

type AdmissionRow struct {
	SourceIndex          int             `json:"source_index"`
	TelegramID           string          `json:"telegram_id"`
	CustomerID           string          `json:"customer_id"`
	RecurringID          string          `json:"recurring_id"`
	CurrentInvoiceID     string          `json:"current_invoice_id"`
	PaidAnchorInvoiceID  string          `json:"paid_anchor_invoice_id"`
	SUIID                int64           `json:"sui_id"`
	SUIName              string          `json:"sui_name"`
	CredentialHash       string          `json:"credential_hash"`
	SourceFactsHash      string          `json:"source_facts_hash"`
	Lifecycle            string          `json:"lifecycle"`
	CurrentInvoiceStatus string          `json:"current_invoice_status"`
	DebtAmount           string          `json:"debt_amount"`
	PaymentDate          string          `json:"payment_date"`
	NextInvoiceDate      string          `json:"next_invoice_date"`
	Currency             string          `json:"currency"`
	Amount               string          `json:"amount"`
	Template             json.RawMessage `json:"template"`
	Proof                json.RawMessage `json:"proof"`
	HistoricalInvoiceIDs []string        `json:"historical_invoice_ids"`
	InvoiceWatermark     string          `json:"invoice_watermark"`
	Evidence             json.RawMessage `json:"evidence"`
	StableHash           string          `json:"stable_hash"`
}

type PreparedPlan struct {
	botID                                 int64
	revision                              int64
	planHash, policyHash, sourceScopeHash string
	verifiedAt, notAfter                  time.Time
	plan                                  importplan.Plan
	rows                                  []AdmissionRow
}

func (PreparedPlan) String() string               { return "<prepared verified import>" }
func (p PreparedPlan) GoString() string           { return p.String() }
func (p PreparedPlan) LogValue() slog.Value       { return slog.StringValue(p.String()) }
func (PreparedPlan) MarshalJSON() ([]byte, error) { return []byte(`"<prepared verified import>"`), nil }
func (p PreparedPlan) BotID() int64               { return p.botID }
func (p PreparedPlan) Revision() int64            { return p.revision }
func (p PreparedPlan) PlanHash() string           { return p.planHash }
func (p PreparedPlan) PolicyHash() string         { return p.policyHash }
func (p PreparedPlan) SourceScopeHash() string    { return p.sourceScopeHash }
func (p PreparedPlan) VerifiedAt() time.Time      { return p.verifiedAt }
func (p PreparedPlan) NotAfter() time.Time        { return p.notAfter }
func (p PreparedPlan) RowCount() int              { return len(p.rows) }

// Plan returns the verified manifest metadata without exposing mutable backing data.
func (p PreparedPlan) Plan() importplan.Plan {
	out := p.plan
	out.Candidates = append([]importplan.Candidate(nil), p.plan.Candidates...)
	out.Reasons = make(map[string]int, len(p.plan.Reasons))
	for reason, count := range p.plan.Reasons {
		out.Reasons[reason] = count
	}
	return out
}

// Rows returns detached copies suitable for a trusted persistence adapter.
func (p PreparedPlan) Rows() []AdmissionRow {
	out := make([]AdmissionRow, len(p.rows))
	for i, r := range p.rows {
		out[i] = r
		out[i].Template = append(json.RawMessage(nil), r.Template...)
		out[i].Proof = append(json.RawMessage(nil), r.Proof...)
		out[i].Evidence = append(json.RawMessage(nil), r.Evidence...)
		out[i].HistoricalInvoiceIDs = append([]string(nil), r.HistoricalInvoiceIDs...)
	}
	return out
}
func (p PreparedPlan) Validate() error {
	if p.plan.ValidateHash(p.planHash) != nil || p.botID <= 0 || p.revision <= 0 || !digest.MatchString(p.planHash) || !digest.MatchString(p.policyHash) || !digest.MatchString(p.sourceScopeHash) || p.verifiedAt.IsZero() || !p.notAfter.After(p.verifiedAt) || len(p.rows) == 0 || p.plan.Hash != p.planHash || p.plan.Total != len(p.rows) || len(p.plan.Candidates) != len(p.rows) {
		return errors.New("invalid prepared import")
	}
	seen := make(map[int]struct{}, len(p.rows))
	for i, r := range p.rows {
		if p.plan.Candidates[i].Index != r.SourceIndex || r.TelegramID == "" || r.CustomerID == "" || r.RecurringID == "" || r.CurrentInvoiceID == "" || r.PaidAnchorInvoiceID == "" || r.SUIID <= 0 || !digest.MatchString(r.SourceFactsHash) || !digest.MatchString(r.StableHash) || len(r.Evidence) == 0 || len(r.Proof) == 0 || len(r.Template) == 0 {
			return errors.New("invalid prepared import")
		}
		if _, ok := seen[r.SourceIndex]; ok {
			return errors.New("invalid prepared import")
		}
		seen[r.SourceIndex] = struct{}{}
		if p.plan.Candidates[i].Facts.TelegramID != r.TelegramID || p.plan.Candidates[i].Facts.InvoiceCustomerID != r.CustomerID || p.plan.Candidates[i].Facts.RecurringID != r.RecurringID || p.plan.Candidates[i].Facts.CurrentInvoiceID != r.CurrentInvoiceID || p.plan.Candidates[i].Facts.SUIID != fmt.Sprint(r.SUIID) || p.plan.Candidates[i].Facts.ConfigHash != r.CredentialHash || p.plan.Candidates[i].Facts.Amount != r.Amount || p.plan.Candidates[i].Facts.PeriodStart != r.PaymentDate || p.plan.Candidates[i].Facts.PeriodEnd != r.NextInvoiceDate || p.plan.Candidates[i].Facts.Currency != r.Currency || p.plan.Candidates[i].Facts.Lifecycle != r.Lifecycle {
			return errors.New("invalid prepared import")
		}
		if len(r.Template) > 65536 || len(r.Proof) > 65536 || len(r.Evidence) > 262144 || len(r.HistoricalInvoiceIDs) > 10000 || !jsonObject(r.Template) || !jsonObject(r.Proof) || !jsonObject(r.Evidence) {
			return errors.New("invalid prepared import")
		}
		var evidence importverify.PrivateEvidence
		if json.Unmarshal(r.Evidence, &evidence) != nil || evidence.BotID != p.botID || evidence.PlanHash != p.planHash || evidence.CandidateIndex != r.SourceIndex || evidence.PolicyHash != p.policyHash || evidence.SourceScope != p.sourceScopeHash || !evidence.VerifiedAt.Equal(p.verifiedAt) || !evidence.NotAfter.Equal(p.notAfter) || evidence.CustomerID != r.CustomerID || evidence.RecurringID != r.RecurringID || evidence.CurrentInvoiceID != r.CurrentInvoiceID || evidence.PaidAnchorInvoiceID != r.PaidAnchorInvoiceID || evidence.SUIID != r.SUIID || evidence.SUIName != r.SUIName || evidence.ConfigHash != r.CredentialHash || evidence.SourceFactsHash != r.SourceFactsHash || evidence.Lifecycle != r.Lifecycle || evidence.CurrentInvoiceStatus != r.CurrentInvoiceStatus || evidence.DebtAmount != r.DebtAmount || evidence.InvoiceWatermark != r.InvoiceWatermark || !slices.Equal(evidence.HistoricalInvoiceIDs, r.HistoricalInvoiceIDs) || string(evidence.LegacyRecurring) != string(r.Template) || string(evidence.Proof) != string(r.Proof) {
			return errors.New("invalid prepared import")
		}
	}
	return nil
}

func jsonObject(raw json.RawMessage) bool {
	var value map[string]json.RawMessage
	return json.Unmarshal(raw, &value) == nil && value != nil
}

func stableAdmissionHash(planHash string, botID, revision int64, index int, scope, policy, customer, recurring, current, anchor string, suiID int64, name, credential, lifecycle, amount, start, end string, template, proof json.RawMessage) string {
	stableData, _ := json.Marshal(struct {
		Plan                                                string
		BotID, Revision                                     int64
		Index                                               int
		Scope, Policy, Customer, Recurring, Current, Anchor string
		SUI                                                 int64
		Name, Credential, Lifecycle, Amount, Start, End     string
		Template, Proof                                     json.RawMessage
	}{planHash, botID, revision, index, scope, policy, customer, recurring, current, anchor, suiID, name, credential, lifecycle, amount, start, end, template, proof})
	sum := sha256.Sum256(stableData)
	return hex.EncodeToString(sum[:])
}

func clonePlan(plan importplan.Plan) importplan.Plan {
	out := plan
	out.Candidates = append([]importplan.Candidate(nil), plan.Candidates...)
	out.Reasons = make(map[string]int, len(plan.Reasons))
	for reason, count := range plan.Reasons {
		out.Reasons[reason] = count
	}
	return out
}

// Prepare performs fresh GET-only verification itself. No report or evidence
// artifact is accepted as authority. Every plan record must verify atomically.
func Prepare(ctx context.Context, service *importverify.Service, plan importplan.Plan, expectedHash string, revision int64, now time.Time) (PreparedPlan, error) {
	if ctx == nil || service == nil || revision <= 0 || now.IsZero() {
		return PreparedPlan{}, errors.New("invalid import admission request")
	}
	if err := plan.ValidateHash(expectedHash); err != nil || plan.Total == 0 || len(plan.Candidates) != plan.Total {
		return PreparedPlan{}, errors.New("import plan is not wholly admissible")
	}
	result, evidence, err := service.Verify(ctx, plan, expectedHash, now)
	if err != nil || result.Verified != plan.Total || result.Manual != 0 || len(evidence) != plan.Total {
		return PreparedPlan{}, errors.New("fresh import verification did not verify the complete plan")
	}
	p := PreparedPlan{planHash: plan.Hash, revision: revision, plan: clonePlan(plan), rows: make([]AdmissionRow, 0, len(evidence))}
	for i, e := range evidence {
		c := plan.Candidates[i]
		if c.Index != e.CandidateIndex || e.PlanHash != plan.Hash || e.BotID <= 0 || !digest.MatchString(e.PolicyHash) || !digest.MatchString(e.SourceScope) || !digest.MatchString(e.SourceFactsHash) || !e.NotAfter.After(now) {
			return PreparedPlan{}, errors.New("fresh import verification evidence is inconsistent")
		}
		if i == 0 {
			p.botID = e.BotID
			p.policyHash = e.PolicyHash
			p.sourceScopeHash = e.SourceScope
			p.verifiedAt = e.VerifiedAt
			p.notAfter = e.NotAfter
		} else if p.botID != e.BotID || p.policyHash != e.PolicyHash || p.sourceScopeHash != e.SourceScope || !p.verifiedAt.Equal(e.VerifiedAt) || !p.notAfter.Equal(e.NotAfter) {
			return PreparedPlan{}, errors.New("fresh import verification policy is inconsistent")
		}
		if e.CustomerID != c.Facts.InvoiceCustomerID || e.RecurringID != c.Facts.RecurringID || e.SUIID <= 0 || e.SUIName != c.Facts.SUIName || e.Lifecycle != c.Facts.Lifecycle || e.CurrentInvoiceID == "" || e.PaidAnchorInvoiceID == "" {
			return PreparedPlan{}, errors.New("fresh import verification facts do not match plan")
		}
		proof := append(json.RawMessage(nil), e.Proof...)
		template := append(json.RawMessage(nil), e.LegacyRecurring...)
		raw, _ := e.PrivateJSON()
		stableHash := stableAdmissionHash(plan.Hash, e.BotID, revision, c.Index, e.SourceScope, e.PolicyHash, e.CustomerID, e.RecurringID, e.CurrentInvoiceID, e.PaidAnchorInvoiceID, e.SUIID, e.SUIName, e.ConfigHash, e.Lifecycle, c.Facts.Amount, c.Facts.PeriodStart, c.Facts.PeriodEnd, template, proof)
		p.rows = append(p.rows, AdmissionRow{SourceIndex: c.Index, TelegramID: c.Facts.TelegramID, CustomerID: e.CustomerID, RecurringID: e.RecurringID, CurrentInvoiceID: e.CurrentInvoiceID, PaidAnchorInvoiceID: e.PaidAnchorInvoiceID, SUIID: e.SUIID, SUIName: e.SUIName, CredentialHash: e.ConfigHash, SourceFactsHash: e.SourceFactsHash, Lifecycle: e.Lifecycle, CurrentInvoiceStatus: e.CurrentInvoiceStatus, DebtAmount: e.DebtAmount, PaymentDate: c.Facts.PeriodStart, NextInvoiceDate: c.Facts.PeriodEnd, Currency: c.Facts.Currency, Amount: c.Facts.Amount, Template: template, Proof: proof, HistoricalInvoiceIDs: append([]string(nil), e.HistoricalInvoiceIDs...), InvoiceWatermark: e.InvoiceWatermark, Evidence: raw, StableHash: stableHash})
	}
	if err := p.Validate(); err != nil {
		return PreparedPlan{}, fmt.Errorf("prepared import invalid: %w", err)
	}
	return p, nil
}
