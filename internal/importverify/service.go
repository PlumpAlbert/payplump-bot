// Package importverify performs bounded, read-only reconciliation of legacy
// import candidates. Reports are observations, never admission authority.
package importverify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/billing"
	"github.com/PlumpAlbert/payplump-bot/internal/importplan"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

const (
	ReasonExcluded      = "excluded"
	ReasonInvalidPolicy = "invalid_policy"
	ReasonSourceFailure = "source_failure"
	ReasonUnsupported   = "unsupported_profile"
	ReasonAmbiguous     = "ambiguous_source"
	ReasonUnpaid        = "unpaid_debt"
	ReasonPayment       = "payment_unverified"
	ReasonExpired       = "active_period_expired"
	ReasonChanged       = "source_changed"
	ReasonPausedHistory = "paused_history_not_supported"
	ReasonHistoryLimit  = "historical_anchor_limit"
)

type Policy struct {
	// Automatic discovery uses verified identities instead of a pilot allowlist.
	// Omitted false preserves the historical manual-policy hash encoding.
	Automatic             bool `json:",omitempty"`
	BotID                 int64
	SourceScopeHash       string
	PilotIDs, ExcludedIDs []string
	// Legacy serialized contract labels, not observed provider release claims.
	// They remain fixed to preserve existing import policy fingerprints.
	NinjaVersion, SUIVersion             string
	NativeCurrencyID, NativeCurrencyCode string
	ProductKey                           string
	PublicSubscriptionBase               string
	ExpectedInboundIDs                   []int64
	MaxRecords, MaxInventoryRecords      int
	MaxPages, MaxHistoricalAnchors       int
	EvidenceTTL                          time.Duration
}

// Fingerprint returns the stable hash of a validated policy's existing JSON encoding.
func (p Policy) Fingerprint() (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(p)
	if err != nil {
		return "", errors.New("could not encode import verification policy")
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (p Policy) Validate() error {
	if p.BotID <= 0 || p.PublicSubscriptionBase == "" || !validDigest(p.SourceScopeHash) || p.MaxInventoryRecords < 1 || p.MaxInventoryRecords > 1000 || p.NinjaVersion != invoiceninja.SupportedRecurringVersion || (p.SUIVersion != "1.6.4" && p.SUIVersion != "v1.6.4") || p.NativeCurrencyID == "" || p.NativeCurrencyCode == "" || p.ProductKey == "" || p.MaxRecords < 1 || p.MaxRecords > importplan.MaxRecords || p.MaxPages < 1 || p.MaxPages > 100 || p.MaxHistoricalAnchors < 1 || p.MaxHistoricalAnchors > 20 || p.EvidenceTTL <= 0 || p.EvidenceTTL > 15*time.Minute {
		return errors.New("invalid import verification policy")
	}
	if !canonicalPositive(p.NativeCurrencyID) || len(p.NativeCurrencyCode) != 3 || p.NativeCurrencyCode[0] < 'A' || p.NativeCurrencyCode[0] > 'Z' || p.NativeCurrencyCode[1] < 'A' || p.NativeCurrencyCode[1] > 'Z' || p.NativeCurrencyCode[2] < 'A' || p.NativeCurrencyCode[2] > 'Z' {
		return errors.New("invalid import verification policy")
	}
	base, err := url.Parse(p.PublicSubscriptionBase)
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return errors.New("invalid import verification policy")
	}
	if len(p.ExpectedInboundIDs) == 0 {
		return errors.New("invalid import verification policy")
	}
	for i, id := range p.ExpectedInboundIDs {
		if id <= 0 || (i > 0 && p.ExpectedInboundIDs[i-1] >= id) {
			return errors.New("invalid import verification policy")
		}
	}
	for _, ids := range [][]string{p.PilotIDs, p.ExcludedIDs} {
		if !slices.IsSorted(ids) {
			return errors.New("invalid import verification policy")
		}
		seen := map[string]bool{}
		for _, id := range ids {
			if !canonicalPositive(id) || seen[id] {
				return errors.New("invalid import verification policy")
			}
			seen[id] = true
		}
	}
	return nil
}

type CustomerReader interface {
	FindCustomers(context.Context, int64) ([]invoiceninja.Customer, error)
	ListRecurring(context.Context, string) ([]invoiceninja.RecurringRecord, error)
}
type RecurringReader interface {
	Get(context.Context, string) (invoiceninja.RecurringRecord, error)
}
type InvoiceReader interface {
	ListInvoices(context.Context, string) ([]invoiceninja.CycleInvoice, error)
	GetInvoice(context.Context, string) (invoiceninja.CycleInvoice, error)
}
type PaymentReader interface{ payments.Reader }
type IdentityReader interface {
	ListIdentities(context.Context, int) ([]sui.Identity, error)
}
type RecordReader interface {
	GetRecord(context.Context, int64) (sui.ClientRecord, error)
}
type SettingsReader interface {
	GetSettings(context.Context) (sui.Settings, error)
}
type Providers struct {
	Customers  CustomerReader
	Recurring  RecurringReader
	Invoices   InvoiceReader
	Payments   PaymentReader
	Identities IdentityReader
	Records    RecordReader
	Settings   SettingsReader
}
type Result struct {
	Verified     int            `json:"verified"`
	Manual       int            `json:"manual"`
	Total        int            `json:"total"`
	PlanHash     string         `json:"plan_hash"`
	EvidenceHash string         `json:"evidence_hash"`
	Reasons      map[string]int `json:"reasons"`
	Diagnostics  []Diagnostic   `json:"diagnostics,omitempty"`
}

func (r Result) String() string {
	return fmt.Sprintf("import verification verified=%d manual=%d total=%d", r.Verified, r.Manual, r.Total)
}
func (r Result) GoString() string { return r.String() }
func (r Result) LogValue() slog.Value {
	return slog.GroupValue(slog.Int("verified", r.Verified), slog.Int("manual", r.Manual), slog.Int("total", r.Total), slog.String("plan_hash", r.PlanHash))
}

type PrivateEvidence struct {
	SourceScope          string          `json:"source_scope"`
	PolicyHash           string          `json:"policy_hash"`
	BotID                int64           `json:"bot_id"`
	PlanHash             string          `json:"plan_hash"`
	CandidateIndex       int             `json:"candidate_index"`
	VerifiedAt           time.Time       `json:"verified_at"`
	NotAfter             time.Time       `json:"not_after"`
	CustomerID           string          `json:"customer_id"`
	RecurringID          string          `json:"recurring_id"`
	CurrentInvoiceID     string          `json:"current_invoice_id"`
	CurrentInvoice       json.RawMessage `json:"current_invoice"`
	SUISettings          sui.Settings    `json:"sui_settings"`
	Lifecycle            string          `json:"lifecycle"`
	CurrentInvoiceStatus string          `json:"current_invoice_status"`
	DebtAmount           string          `json:"debt_amount,omitempty"`
	SUIID                int64           `json:"sui_id"`
	ProofHash            string          `json:"proof_hash"`
	SourceFactsHash      string          `json:"source_facts_hash"`
	LegacyRecurring      json.RawMessage `json:"legacy_recurring"`
	LegacyRecurringHash  string          `json:"legacy_recurring_hash"`
	HistoricalInvoiceIDs []string        `json:"historical_invoice_ids"`
	InvoiceWatermark     string          `json:"invoice_watermark"`
	PaidAnchorInvoiceID  string          `json:"paid_anchor_invoice_id"`
	SUIName              string          `json:"sui_name"`
	PublicBaseHash       string          `json:"public_base_hash"`
	ConfigHash           string          `json:"config_hash"`
	InboundIDs           []int64         `json:"inbound_ids"`
	Proof                json.RawMessage `json:"proof"`
}

func (PrivateEvidence) String() string         { return "[private import verification evidence]" }
func (e PrivateEvidence) GoString() string     { return e.String() }
func (e PrivateEvidence) LogValue() slog.Value { return slog.StringValue(e.String()) }
func (PrivateEvidence) MarshalJSON() ([]byte, error) {
	return []byte(`"[private import verification evidence]"`), nil
}
func (e PrivateEvidence) PrivateJSON() ([]byte, error) {
	type plain PrivateEvidence
	return json.Marshal(plain(e))
}

func localCalendarDate(now time.Time) string { return now.Format(time.DateOnly) }

func hashEvidence(evidence []PrivateEvidence) (string, error) {
	privateDocuments := make([]json.RawMessage, 0, len(evidence))
	for _, item := range evidence {
		document, err := item.PrivateJSON()
		if err != nil {
			return "", err
		}
		privateDocuments = append(privateDocuments, document)
	}
	data, err := json.Marshal(privateDocuments)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value && value != "0000000000000000000000000000000000000000000000000000000000000000"
}
func canonicalPositive(value string) bool {
	n, err := strconv.ParseInt(value, 10, 64)
	return err == nil && n > 0 && strconv.FormatInt(n, 10) == value
}

type Service struct {
	policy    Policy
	providers Providers
}

func New(policy Policy, providers Providers) (*Service, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	if providers.Customers == nil || providers.Recurring == nil || providers.Invoices == nil || providers.Payments == nil || providers.Identities == nil || providers.Records == nil || providers.Settings == nil {
		return nil, errors.New("import verification requires GET-only providers")
	}
	policy.PilotIDs = slices.Clone(policy.PilotIDs)
	policy.ExcludedIDs = slices.Clone(policy.ExcludedIDs)
	policy.ExpectedInboundIDs = slices.Clone(policy.ExpectedInboundIDs)
	return &Service{policy: policy, providers: providers}, nil
}

// Verify rejects an invalid digest/policy/exclusion before any provider call.
func (s *Service) Verify(ctx context.Context, plan importplan.Plan, expectedHash string, now time.Time) (Result, []PrivateEvidence, error) {
	if ctx == nil || s == nil || plan.ValidateHash(expectedHash) != nil || plan.Hash != expectedHash {
		return Result{}, nil, errors.New("invalid import verification plan")
	}
	result := Result{Total: plan.Total, PlanHash: plan.Hash, Reasons: map[string]int{}}
	for code, count := range plan.Reasons {
		result.Reasons[code] = count
	}
	if plan.Total > s.policy.MaxRecords || len(plan.Candidates) > s.policy.MaxRecords {
		return result, nil, errors.New("import verification record limit exceeded")
	}
	if now.IsZero() {
		return result, nil, errors.New("invalid verification time")
	}
	calendarToday := localCalendarDate(now)
	verifiedAt := now.UTC()
	reportEvidence := make([]PrivateEvidence, 0)
	policyHash, err := s.policy.Fingerprint()
	if err != nil {
		return result, nil, err
	}
	for _, candidate := range plan.Candidates {
		if err := ctx.Err(); err != nil {
			return Result{}, nil, err
		}
		r := candidate.Facts
		if r.Excluded || r.TelegramID == strconv.FormatInt(s.policy.BotID, 10) || slices.Contains(s.policy.ExcludedIDs, r.TelegramID) || (!s.policy.Automatic && !slices.Contains(s.policy.PilotIDs, r.TelegramID)) {
			result.Reasons[ReasonExcluded]++
			result.Diagnostics = append(result.Diagnostics, Diagnostic{Stage: "verification", Check: "excluded"})
			continue
		}
		if !canonicalPositive(r.TelegramID) || !canonicalPositive(r.SUIID) {
			result.Reasons[ReasonUnsupported]++
			result.Diagnostics = append(result.Diagnostics, Diagnostic{Stage: "verification", Check: "invalid_identity"})
			continue
		}
		if r.Lifecycle != "active" && r.Lifecycle != "paused" {
			result.Reasons[ReasonUnsupported]++
			result.Diagnostics = append(result.Diagnostics, Diagnostic{Stage: "verification", Check: "unsupported_lifecycle"})
			continue
		}
		reason, evidence, diagnostic := s.verifyCandidate(ctx, candidate, calendarToday, verifiedAt, policyHash, plan.Hash)
		if err := ctx.Err(); err != nil {
			return Result{}, nil, err
		}
		if reason == "" {
			secondReason, second, secondDiagnostic := s.verifyCandidate(ctx, candidate, calendarToday, verifiedAt, policyHash, plan.Hash)
			if err := ctx.Err(); err != nil {
				return Result{}, nil, err
			}
			if secondReason != "" || evidence.ProofHash != second.ProofHash || evidence.SourceFactsHash != second.SourceFactsHash || evidence.CustomerID != second.CustomerID || evidence.RecurringID != second.RecurringID || evidence.CurrentInvoiceID != second.CurrentInvoiceID || evidence.PaidAnchorInvoiceID != second.PaidAnchorInvoiceID || evidence.SUIID != second.SUIID {
				reason = ReasonChanged
				diagnostic = secondDiagnostic
				if secondReason == "" {
					diagnostic = Diagnostic{Stage: "verification", Check: "second_read_changed"}
				}
			}
		}
		if reason != "" {
			result.Reasons[reason]++
			result.Diagnostics = append(result.Diagnostics, diagnostic)
			continue
		}
		result.Verified++
		reportEvidence = append(reportEvidence, evidence)
	}
	result.Manual = result.Total - result.Verified
	evidenceHash, hashErr := hashEvidence(reportEvidence)
	if hashErr != nil {
		return Result{}, nil, errors.New("private evidence encoding failed")
	}
	result.EvidenceHash = evidenceHash
	return result, reportEvidence, nil
}

func (s *Service) verifyCandidate(ctx context.Context, c importplan.Candidate, calendarToday string, verifiedAt time.Time, policyHash, planHash string) (string, PrivateEvidence, Diagnostic) {
	r := c.Facts
	tg := mustInt(r.TelegramID)
	customers, err := s.providers.Customers.FindCustomers(ctx, tg)
	if err != nil {
		return ReasonSourceFailure, PrivateEvidence{}, FailureDiagnostic("customers", "read", err)
	}
	if len(customers) != 1 || customers[0].ID != r.InvoiceCustomerID || !customers[0].Active() || customers[0].IDNumber != r.TelegramID {
		return ReasonAmbiguous, PrivateEvidence{}, Diagnostic{Stage: "customers", Check: "customer_match", Count: len(customers)}
	}
	customer := customers[0]

	recurrings, err := s.providers.Customers.ListRecurring(ctx, customer.ID)
	if err != nil {
		return ReasonSourceFailure, PrivateEvidence{}, FailureDiagnostic("recurring", "read", err)
	}
	if len(recurrings) > s.policy.MaxInventoryRecords {
		return ReasonUnsupported, PrivateEvidence{}, Diagnostic{Stage: "recurring", Check: "inventory_limit", Count: len(recurrings)}
	}
	sort.Slice(recurrings, func(i, j int) bool { return recurrings[i].ID < recurrings[j].ID })
	live := make([]invoiceninja.RecurringRecord, 0, 1)
	seenRecurring := map[string]bool{}
	for _, x := range recurrings {
		if x.ID == "" || seenRecurring[x.ID] || x.ClientID != customer.ID {
			return ReasonAmbiguous, PrivateEvidence{}, Diagnostic{Stage: "recurring", Check: "invalid_duplicate_or_foreign_record"}
		}
		seenRecurring[x.ID] = true
		if !x.IsDeleted && x.ArchivedAt == 0 {
			live = append(live, x)
		}
	}
	expectedRecurringStatus := "2"
	if r.Lifecycle == "paused" {
		expectedRecurringStatus = "3"
	}
	if len(live) != 1 || live[0].ID != r.RecurringID || live[0].StatusID != expectedRecurringStatus {
		return ReasonAmbiguous, PrivateEvidence{}, Diagnostic{Stage: "recurring", Check: "live_recurring_match", Count: len(live)}
	}
	recurring, err := s.providers.Recurring.Get(ctx, r.RecurringID)
	if err != nil {
		return ReasonSourceFailure, PrivateEvidence{}, FailureDiagnostic("recurring_record", "read", err)
	}
	if recurring.ID != live[0].ID || recurring.ClientID != customer.ID {
		return ReasonChanged, PrivateEvidence{}, Diagnostic{Stage: "recurring_record", Check: "identity_changed"}
	}
	inventoryJSON, _ := json.Marshal(live[0])
	directJSON, _ := json.Marshal(recurring)
	if string(inventoryJSON) != string(directJSON) {
		return ReasonChanged, PrivateEvidence{}, Diagnostic{Stage: "recurring_record", Check: "inventory_changed"}
	}
	if check := legacyRecurringCheck(recurring, s.policy.ProductKey, r.Amount, expectedRecurringStatus); check != "" {
		return ReasonUnsupported, PrivateEvidence{}, Diagnostic{Stage: "recurring_record", Check: check}
	}
	if r.Lifecycle == "active" && recurring.NextSendDate != r.PeriodEnd {
		return ReasonUnsupported, PrivateEvidence{}, Diagnostic{Stage: "recurring_record", Check: "schedule_date"}
	}
	if recurring.NextSendDatetime != "" {
		scheduled, parseErr := invoiceninja.ParseRecurringUTC(recurring.NextSendDatetime)
		if parseErr != nil || (r.Lifecycle == "active" && !scheduled.After(verifiedAt)) {
			return ReasonUnsupported, PrivateEvidence{}, Diagnostic{Stage: "recurring_record", Check: "schedule_datetime"}
		}
	} else if r.Lifecycle == "active" {
		return ReasonUnsupported, PrivateEvidence{}, Diagnostic{Stage: "recurring_record", Check: "schedule_missing"}
	}
	recurringJSON, _ := json.Marshal(recurring)
	recurringHash := sha256.Sum256(recurringJSON)

	identities, err := s.providers.Identities.ListIdentities(ctx, s.policy.MaxInventoryRecords)
	if err != nil {
		return ReasonSourceFailure, PrivateEvidence{}, FailureDiagnostic("sui_inventory", "read", err)
	}
	if len(identities) > s.policy.MaxInventoryRecords {
		return ReasonUnsupported, PrivateEvidence{}, Diagnostic{Stage: "sui_inventory", Check: "inventory_limit", Count: len(identities)}
	}
	sort.Slice(identities, func(i, j int) bool {
		if identities[i].ID() == identities[j].ID() {
			return identities[i].Name() < identities[j].Name()
		}
		return identities[i].ID() < identities[j].ID()
	})
	var identity *sui.Identity
	seenIdentityIDs, seenIdentityNames := map[int64]bool{}, map[string]bool{}
	for i := range identities {
		id, name := identities[i].ID(), identities[i].Name()
		if id <= 0 || name == "" || seenIdentityIDs[id] || seenIdentityNames[name] {
			return ReasonAmbiguous, PrivateEvidence{}, Diagnostic{Stage: "sui_inventory", Check: "invalid_or_duplicate_identity"}
		}
		seenIdentityIDs[id], seenIdentityNames[name] = true, true
		if id == mustInt(r.SUIID) || name == r.SUIName {
			if identity != nil {
				return ReasonAmbiguous, PrivateEvidence{}, Diagnostic{Stage: "sui_inventory", Check: "identity_match_ambiguous"}
			}
			identity = &identities[i]
		}
	}
	if identity == nil || identity.ID() != mustInt(r.SUIID) || identity.Name() != r.SUIName || r.SUIName != r.TelegramID {
		return ReasonAmbiguous, PrivateEvidence{}, Diagnostic{Stage: "sui_inventory", Check: "identity_match"}
	}
	settings, err := s.providers.Settings.GetSettings(ctx)
	if err != nil {
		return ReasonSourceFailure, PrivateEvidence{}, FailureDiagnostic("sui_settings", "read", err)
	}
	if settings.SubURI != "" && settings.SubURI != s.policy.PublicSubscriptionBase {
		return ReasonUnsupported, PrivateEvidence{}, Diagnostic{Stage: "sui_settings", Check: "subscription_base"}
	}
	// Use the owner-configured public URL when the panel has no override.
	// Keep settings itself unchanged so evidence still records the real source.
	publicBaseHash := sha256.Sum256([]byte(s.policy.PublicSubscriptionBase))
	record, err := s.providers.Records.GetRecord(ctx, identity.ID())
	if err != nil {
		return ReasonSourceFailure, PrivateEvidence{}, FailureDiagnostic("sui_record", "read", err)
	}
	if record.ID() != identity.ID() || record.Name() != identity.Name() || record.Enabled() != identity.Enabled() || record.Name() != r.SUIName || record.Enabled() != (r.Lifecycle == "active") {
		return ReasonUnsupported, PrivateEvidence{}, Diagnostic{Stage: "sui_record", Check: "identity_or_lifecycle"}
	}
	if !slices.Equal(sorted(record.InboundIDs()), s.policy.ExpectedInboundIDs) {
		return ReasonUnsupported, PrivateEvidence{}, Diagnostic{Stage: "sui_record", Check: "inbounds"}
	}
	credentialHash := record.CredentialsHash()
	if hex.EncodeToString(credentialHash[:]) != r.ConfigHash {
		return ReasonUnsupported, PrivateEvidence{}, Diagnostic{Stage: "sui_record", Check: "credentials"}
	}
	if !unlimitedRecord(record) {
		return ReasonUnsupported, PrivateEvidence{}, Diagnostic{Stage: "sui_record", Check: "quotas"}
	}

	invoices, err := s.providers.Invoices.ListInvoices(ctx, customer.ID)
	if err != nil {
		return ReasonSourceFailure, PrivateEvidence{}, FailureDiagnostic("invoices", "read", err)
	}
	if len(invoices) > s.policy.MaxInventoryRecords {
		return ReasonUnsupported, PrivateEvidence{}, Diagnostic{Stage: "invoices", Check: "inventory_limit", Count: len(invoices)}
	}
	sort.Slice(invoices, func(i, j int) bool { return invoices[i].ID < invoices[j].ID })
	unpaid := 0
	var current *invoiceninja.CycleInvoice
	latestIssuedDate, latestIssuedID, latestIssuedCount := "", "", 0
	seenInvoices := map[string]bool{}
	invoiceIDs := make([]string, 0, len(invoices))
	var historical []invoiceninja.CycleInvoice
	for i := range invoices {
		iv := &invoices[i]
		if iv.ID == "" || seenInvoices[iv.ID] || iv.ClientID != customer.ID {
			return ReasonAmbiguous, PrivateEvidence{}, Diagnostic{Stage: "invoices", Check: "invalid_duplicate_or_foreign_record"}
		}
		seenInvoices[iv.ID] = true
		invoiceIDs = append(invoiceIDs, iv.ID)
		if iv.StatusID == "2" || iv.StatusID == "3" {
			unpaid++
		}
		if !iv.IsDeleted && iv.ArchivedAt == 0 && iv.RecurringID == r.RecurringID {
			switch iv.StatusID {
			case "1", "5": // Draft and cancelled records are not current issued invoices.
			case "2", "3", "4":
				if !canonicalDate(iv.Date) {
					return ReasonUnsupported, PrivateEvidence{}, Diagnostic{Stage: "invoices", Check: "issued_date"}
				}
				if iv.Date > latestIssuedDate {
					latestIssuedDate, latestIssuedID, latestIssuedCount = iv.Date, iv.ID, 1
				} else if iv.Date == latestIssuedDate {
					latestIssuedCount++
				}
				if r.Lifecycle == "paused" && iv.StatusID == "4" && iv.ID != r.CurrentInvoiceID && iv.Date < r.PeriodEnd {
					historical = append(historical, *iv)
				}
			default:
				return ReasonUnsupported, PrivateEvidence{}, Diagnostic{Stage: "invoices", Check: "issued_status"}
			}
		}
		if iv.ID == r.CurrentInvoiceID && iv.RecurringID == r.RecurringID && !iv.IsDeleted && iv.ArchivedAt == 0 {
			if current != nil {
				return ReasonAmbiguous, PrivateEvidence{}, Diagnostic{Stage: "invoices", Check: "current_invoice_duplicate"}
			}
			current = iv
		}
	}
	slices.Sort(invoiceIDs)
	if latestIssuedCount != 1 || latestIssuedID != r.CurrentInvoiceID {
		return ReasonAmbiguous, PrivateEvidence{}, Diagnostic{Stage: "invoices", Check: "latest_invoice_match", Count: latestIssuedCount}
	}
	if current == nil || r.Currency != s.policy.NativeCurrencyCode {
		return ReasonUnsupported, PrivateEvidence{}, Diagnostic{Stage: "invoices", Check: "current_invoice_or_currency"}
	}
	if r.Lifecycle == "active" {
		if unpaid != r.UnpaidInvoices || unpaid != 0 {
			return ReasonUnpaid, PrivateEvidence{}, Diagnostic{Stage: "invoices", Check: "active_unpaid_count", Count: unpaid}
		}
		if current.StatusID != "4" || !isZero(current.Balance) || !amountStringEqual(current.Amount.String(), r.Amount) {
			return ReasonUnsupported, PrivateEvidence{}, Diagnostic{Stage: "invoice", Check: "active_paid_amount"}
		}
		if r.PeriodEnd <= calendarToday {
			return ReasonExpired, PrivateEvidence{}, Diagnostic{Stage: "verification", Check: "active_period_expired"}
		}
	} else {
		if unpaid != r.UnpaidInvoices || unpaid != 1 || r.UnpaidInvoices != 1 || r.InvoiceStatus != "sent" || current.StatusID != "2" || current.Date != r.PeriodEnd || r.PeriodEnd > calendarToday || current.Date == "" {
			return ReasonUnpaid, PrivateEvidence{}, Diagnostic{Stage: "invoices", Check: "paused_debt", Count: unpaid}
		}
		if len(historical) > s.policy.MaxHistoricalAnchors {
			return ReasonHistoryLimit, PrivateEvidence{}, Diagnostic{Stage: "invoices", Check: "historical_anchor_limit", Count: len(historical)}
		}
		if !canonicalDate(r.PeriodStart) || !canonicalDate(r.PeriodEnd) || r.PeriodEnd <= r.PeriodStart || current.Date <= r.PeriodStart {
			return ReasonPausedHistory, PrivateEvidence{}, Diagnostic{Stage: "invoices", Check: "paused_period"}
		}
		if !amountStringEqual(current.Amount.String(), r.Amount) || !amountStringEqual(current.Balance.String(), r.Amount) || !isZero(current.PaidToDate) || !amountStringEqual(current.ExchangeRate.String(), "1") {
			return ReasonUnpaid, PrivateEvidence{}, Diagnostic{Stage: "invoice", Check: "paused_debt_amount"}
		}
	}
	fresh, err := s.providers.Invoices.GetInvoice(ctx, current.ID)
	if err != nil {
		return ReasonSourceFailure, PrivateEvidence{}, FailureDiagnostic("invoice", "read", err)
	}
	if !sameInvoice(fresh, *current) {
		return ReasonChanged, PrivateEvidence{}, Diagnostic{Stage: "invoice", Check: "inventory_changed"}
	}
	// Observe is read-only; ID 1 is structural only and is never persisted here.
	observeInvoice := current
	if r.Lifecycle == "paused" {
		observeInvoice = nil
	}
	var proof *payments.Proof
	if observeInvoice != nil {
		snapshot := payments.Snapshot{ClientID: 1, TelegramID: tg, ChatID: tg, BotID: s.policy.BotID, CustomerID: customer.ID, InvoiceID: current.ID, RecurringID: recurring.ID}
		decision, err := payments.Observe(ctx, s.providers.Payments, snapshot, s.policy.NativeCurrencyID, calendarToday)
		if err != nil {
			return ReasonSourceFailure, PrivateEvidence{}, FailureDiagnostic("payments", "observe", err)
		}
		if decision.Kind != payments.Verified || decision.Proof == nil || !amountStringEqual(decision.Proof.Amount, r.Amount) || decision.Proof.PaymentDate != r.PeriodStart || decision.Proof.NextInvoiceDate != r.PeriodEnd {
			return ReasonPayment, PrivateEvidence{}, Diagnostic{Stage: "payments", Check: "active_proof"}
		}
		proof = decision.Proof
	} else {
		if len(historical) == 0 {
			return ReasonPayment, PrivateEvidence{}, Diagnostic{Stage: "payments", Check: "historical_anchor_missing"}
		}
		matches := 0
		for _, anchor := range historical {
			anchorFresh, getErr := s.providers.Invoices.GetInvoice(ctx, anchor.ID)
			if getErr != nil {
				return ReasonSourceFailure, PrivateEvidence{}, FailureDiagnostic("historical_invoice", "read", getErr)
			}
			if !sameInvoice(anchorFresh, anchor) {
				return ReasonChanged, PrivateEvidence{}, Diagnostic{Stage: "historical_invoice", Check: "inventory_changed"}
			}
			if !amountStringEqual(anchor.Amount.String(), r.Amount) || !isZero(anchor.Balance) || !amountStringEqual(anchor.PaidToDate.String(), r.Amount) || !amountStringEqual(anchor.ExchangeRate.String(), "1") {
				continue
			}
			snapshot := payments.Snapshot{ClientID: 1, TelegramID: tg, ChatID: tg, BotID: s.policy.BotID, CustomerID: customer.ID, InvoiceID: anchor.ID, RecurringID: recurring.ID}
			decision, observeErr := payments.Observe(ctx, s.providers.Payments, snapshot, s.policy.NativeCurrencyID, calendarToday)
			if observeErr != nil {
				return ReasonSourceFailure, PrivateEvidence{}, FailureDiagnostic("historical_payments", "observe", observeErr)
			}
			if decision.Kind != payments.Verified || decision.Proof == nil || !amountStringEqual(decision.Proof.Amount, r.Amount) {
				return ReasonPayment, PrivateEvidence{}, Diagnostic{Stage: "historical_payments", Check: "anchor_proof"}
			}
			if decision.Proof.PaymentDate != r.PeriodStart || decision.Proof.NextInvoiceDate != r.PeriodEnd {
				continue
			}
			matches++
			proof = decision.Proof
		}
		if matches > 1 {
			return ReasonAmbiguous, PrivateEvidence{}, Diagnostic{Stage: "historical_payments", Check: "anchor_match_ambiguous", Count: matches}
		}
		if matches != 1 || proof == nil {
			return ReasonPayment, PrivateEvidence{}, Diagnostic{Stage: "historical_payments", Check: "anchor_match_missing", Count: matches}
		}
	}
	identityFacts := make([]struct {
		ID      int64  `json:"id"`
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}, 0, len(identities))
	for _, item := range identities {
		identityFacts = append(identityFacts, struct {
			ID      int64  `json:"id"`
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
		}{item.ID(), item.Name(), item.Enabled()})
	}
	customerJSON, _ := json.Marshal(struct {
		ID, IDNumber string
		Deleted      bool
		ArchivedAt   int64
	}{customer.ID, customer.IDNumber, customer.IsDeleted, customer.ArchivedAt})
	facts, _ := json.Marshal(struct {
		Customer        json.RawMessage
		Recurrings      []invoiceninja.RecurringRecord
		DirectRecurring json.RawMessage
		Invoices        []invoiceninja.CycleInvoice
		Identities      any
		Settings        sui.Settings
		PaymentProof    json.RawMessage
		SUIID           int64
		SUIName         string
		ConfigHash      string
		Enabled         bool
		InboundIDs      []int64
	}{customerJSON, recurrings, recurringJSON, invoices, identityFacts, settings, proof.JSON, record.ID(), record.Name(), hex.EncodeToString(credentialHash[:]), record.Enabled(), sorted(record.InboundIDs())})
	factsHash := sha256.Sum256(facts)
	invoiceWatermark := ""
	if len(invoices) > 0 {
		invoiceWatermark = invoiceIDs[len(invoiceIDs)-1]
	}
	currentInvoiceJSON, err := json.Marshal(fresh)
	if err != nil {
		return ReasonSourceFailure, PrivateEvidence{}, FailureDiagnostic("evidence", "invoice_encoding", err)
	}
	debtAmount := ""
	if r.Lifecycle == "paused" {
		debtAmount = current.Amount.String()
	}
	return "", PrivateEvidence{SourceScope: s.policy.SourceScopeHash, PolicyHash: policyHash, BotID: s.policy.BotID, PlanHash: planHash, CandidateIndex: c.Index, VerifiedAt: verifiedAt, NotAfter: verifiedAt.Add(s.policy.EvidenceTTL), CustomerID: customer.ID, RecurringID: recurring.ID, CurrentInvoiceID: current.ID, CurrentInvoice: currentInvoiceJSON, SUISettings: settings, Lifecycle: r.Lifecycle, CurrentInvoiceStatus: current.StatusID, DebtAmount: debtAmount, SUIID: record.ID(), ProofHash: hex.EncodeToString(proof.Hash[:]), SourceFactsHash: hex.EncodeToString(factsHash[:]), LegacyRecurring: recurringJSON, LegacyRecurringHash: hex.EncodeToString(recurringHash[:]), HistoricalInvoiceIDs: invoiceIDs, InvoiceWatermark: invoiceWatermark, PaidAnchorInvoiceID: proof.InvoiceID, SUIName: record.Name(), PublicBaseHash: hex.EncodeToString(publicBaseHash[:]), ConfigHash: hex.EncodeToString(credentialHash[:]), InboundIDs: sorted(record.InboundIDs()), Proof: proof.JSON}, Diagnostic{}
}
func unlimitedRecord(record sui.ClientRecord) bool {
	var limits struct {
		Volume     int64 `json:"volume"`
		Expiry     int64 `json:"expiry"`
		DelayStart bool  `json:"delayStart"`
		AutoReset  bool  `json:"autoReset"`
		ResetDays  int64 `json:"resetDays"`
		NextReset  int64 `json:"nextReset"`
	}
	return json.Unmarshal(record.Raw(), &limits) == nil && limits.Volume == 0 && limits.Expiry == 0 && !limits.DelayStart && !limits.AutoReset && limits.ResetDays == 0 && limits.NextReset == 0
}

// The executor owns acceptance. These checks only explain a rejected profile;
// they must never authorize a profile that NewImportedRecurringProfile rejects.
func legacyRecurringCheck(r invoiceninja.RecurringRecord, productKey, amount, expectedStatus string) string {
	_, err := invoiceninja.NewImportedRecurringProfile(r, productKey)
	if err == nil && r.StatusID == expectedStatus && amountStringEqual(r.Amount.String(), amount) {
		return ""
	}
	if r.IsDeleted || r.ArchivedAt != 0 || r.StatusID != expectedStatus {
		return "legacy_status"
	}
	if r.FrequencyID != "5" {
		return "legacy_frequency"
	}
	if r.AutoBillEnabled || r.AutoBill != "off" {
		return "legacy_auto_bill"
	}
	if r.RemainingCycles != -1 {
		return "legacy_remaining_cycles"
	}
	if !amountStringEqual(r.Amount.String(), amount) || isZero(r.Amount) || strings.HasPrefix(r.Amount.String(), "-") {
		return "legacy_amount"
	}
	if !amountEqual(r.Balance, r.Amount) {
		return "legacy_balance"
	}
	if !amountStringEqual(r.ExchangeRate.String(), "1") {
		return "legacy_exchange_rate"
	}
	if len(r.LineItems) != 1 {
		return "legacy_line_count"
	}
	line := r.LineItems[0]
	if line.ProductKey != productKey || line.TypeID != "1" {
		return "legacy_product"
	}
	if !amountStringEqual(line.Quantity.String(), "1") {
		return "legacy_quantity"
	}
	if !amountEqual(line.Cost, r.Amount) {
		return "legacy_line_cost"
	}
	if !isZero(r.Discount) || !isZero(line.Discount) {
		return "legacy_discount"
	}
	if !isZero(r.TaxRate1) || !isZero(r.TaxRate2) || !isZero(r.TaxRate3) || !isZero(line.TaxRate1) || !isZero(line.TaxRate2) || !isZero(line.TaxRate3) {
		return "legacy_tax"
	}
	if !isZero(r.CustomSurcharge1) || !isZero(r.CustomSurcharge2) || !isZero(r.CustomSurcharge3) || !isZero(r.CustomSurcharge4) {
		return "legacy_surcharge"
	}
	if !isZero(r.Partial) {
		return "legacy_partial"
	}
	if !isZero(r.PaidToDate) {
		return "legacy_paid_to_date"
	}
	return "legacy_profile_invalid"
}
func normalizedAmount(value json.Number) (string, bool) {
	formatted, err := billing.FormatAmount(value)
	return formatted, err == nil
}
func amountStringEqual(a, b string) bool {
	x, ok := normalizedAmount(json.Number(a))
	if !ok {
		return false
	}
	y, ok := normalizedAmount(json.Number(b))
	return ok && x == y
}
func amountEqual(a, b json.Number) bool { return amountStringEqual(a.String(), b.String()) }
func isZero(value json.Number) bool {
	formatted, ok := normalizedAmount(value)
	return ok && formatted == "0"
}

func canonicalDate(value string) bool {
	parsed, err := time.Parse(time.DateOnly, value)
	return err == nil && parsed.Format(time.DateOnly) == value
}

func sameInvoice(a, b invoiceninja.CycleInvoice) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
func mustInt(s string) int64   { v, _ := strconv.ParseInt(s, 10, 64); return v }
func sorted(v []int64) []int64 { x := slices.Clone(v); slices.Sort(x); return x }
