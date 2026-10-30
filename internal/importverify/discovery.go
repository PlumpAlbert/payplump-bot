package importverify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strconv"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/billing"
	"github.com/PlumpAlbert/payplump-bot/internal/importplan"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

type CustomerInventory interface {
	ListCustomers(context.Context) ([]invoiceninja.Customer, error)
}

// Discovery is an observation, not admission authority. A nonempty Reason
// never carries a usable plan. Prepare must independently reverify each result.
type Discovery struct {
	CustomerID string
	Actor      int64
	Facts      importplan.Record
	Reason     string
	FactsHash  string
	Diagnostic Diagnostic
}

func (Discovery) String() string { return "[private import discovery]" }

func (s *Service) Discover(ctx context.Context, now time.Time, known map[int64]bool) ([]Discovery, error) {
	if !s.policy.Automatic {
		return nil, Diagnostic{Stage: "discovery", Check: "automatic_policy_required"}
	}
	inventory, ok := s.providers.Customers.(CustomerInventory)
	if !ok {
		return nil, Diagnostic{Stage: "customer_inventory", Check: "inventory_capability_missing"}
	}
	customers, e := inventory.ListCustomers(ctx)
	if e != nil {
		return nil, FailureDiagnostic("customer_inventory", "read", e)
	}
	if len(customers) > s.policy.MaxInventoryRecords {
		return nil, Diagnostic{Stage: "customer_inventory", Check: "inventory_limit", Count: len(customers)}
	}
	identities, e := s.providers.Identities.ListIdentities(ctx, s.policy.MaxInventoryRecords)
	if e != nil {
		return nil, FailureDiagnostic("sui_inventory", "read", e)
	}
	if len(identities) > s.policy.MaxInventoryRecords {
		return nil, Diagnostic{Stage: "sui_inventory", Check: "inventory_limit", Count: len(identities)}
	}
	byActor := map[string]int{}
	seenCustomer := map[string]bool{}
	for _, c := range customers {
		if c.ID == "" || seenCustomer[c.ID] {
			return nil, Diagnostic{Stage: "customer_inventory", Check: "invalid_or_duplicate_id", Count: len(customers)}
		}
		seenCustomer[c.ID] = true
		byActor[c.IDNumber]++
	}
	byName := map[string][]sui.Identity{}
	seenSUI := map[int64]bool{}
	for _, id := range identities {
		if id.ID() <= 0 || id.Name() == "" || seenSUI[id.ID()] {
			return nil, Diagnostic{Stage: "sui_inventory", Check: "invalid_or_duplicate_identity", Count: len(identities)}
		}
		seenSUI[id.ID()] = true
		byName[id.Name()] = append(byName[id.Name()], id)
	}
	out := make([]Discovery, 0, len(customers))
	for _, c := range customers {
		actor, parseErr := strconv.ParseInt(c.IDNumber, 10, 64)
		if known[actor] || c.IDNumber == strconv.FormatInt(s.policy.BotID, 10) || slices.Contains(s.policy.ExcludedIDs, c.IDNumber) {
			continue
		}
		d := Discovery{CustomerID: c.ID, Actor: actor}
		d.Diagnostic = Diagnostic{Stage: "pairing"}
		switch {
		case parseErr != nil || !canonicalPositive(c.IDNumber) || actor <= 0:
			d.Actor = 0
			d.Reason = ReasonUnsupported
			d.Diagnostic.Check = "invalid_actor"
		case !c.Active():
			d.Reason = ReasonAmbiguous
			d.Diagnostic.Check = "customer_inactive"
		case byActor[c.IDNumber] != 1:
			d.Reason = ReasonAmbiguous
			d.Diagnostic.Check, d.Diagnostic.Count = "duplicate_ninja_actor", byActor[c.IDNumber]
		case len(byName[c.IDNumber]) == 0:
			d.Reason = ReasonAmbiguous
			d.Diagnostic.Check = "sui_match_missing"
		case len(byName[c.IDNumber]) > 1:
			d.Reason = ReasonAmbiguous
			d.Diagnostic.Check, d.Diagnostic.Count = "sui_match_multiple", len(byName[c.IDNumber])
		default:
			d.Facts, d.Reason, d.Diagnostic = s.discoverRecord(ctx, c, byName[c.IDNumber][0], now)
		}
		raw, _ := json.Marshal(c)
		facts, _ := d.Facts.PrivateJSON()
		diagnostic, _ := json.Marshal(d.Diagnostic)
		sum := sha256.Sum256(append(append(raw, facts...), diagnostic...))
		d.FactsHash = hex.EncodeToString(sum[:])
		out = append(out, d)
	}
	// Canonical SUI accounts with no Ninja counterpart are candidates too: a new
	// /start must not create an unrelated first-invoice flow for that identity.
	for name, ids := range byName {
		if byActor[name] != 0 || !canonicalPositive(name) || name == strconv.FormatInt(s.policy.BotID, 10) || slices.Contains(s.policy.ExcludedIDs, name) {
			continue
		}
		actor, _ := strconv.ParseInt(name, 10, 64)
		if known[actor] {
			continue
		}
		raw, _ := json.Marshal(ids)
		d := Diagnostic{Stage: "pairing", Check: "ninja_match_missing", Count: len(ids)}
		diagnostic, _ := json.Marshal(d)
		sum := sha256.Sum256(append(raw, diagnostic...))
		out = append(out, Discovery{CustomerID: "sui:" + name, Actor: actor, Reason: ReasonAmbiguous, FactsHash: hex.EncodeToString(sum[:]), Diagnostic: d})
	}
	return out, nil
}

func (s *Service) discoverRecord(ctx context.Context, c invoiceninja.Customer, id sui.Identity, now time.Time) (importplan.Record, string, Diagnostic) {
	r := importplan.Record{TelegramID: c.IDNumber, InvoiceNumber: c.IDNumber, InvoiceCustomerID: c.ID, SUIID: strconv.FormatInt(id.ID(), 10), SUIName: id.Name(), Currency: s.policy.NativeCurrencyCode, SourceVersion: s.policy.NinjaVersion}
	recurrings, e := s.providers.Customers.ListRecurring(ctx, c.ID)
	if e != nil {
		return r, ReasonSourceFailure, FailureDiagnostic("recurring", "read", e)
	}
	if len(recurrings) > s.policy.MaxInventoryRecords {
		return r, ReasonUnsupported, Diagnostic{Stage: "recurring", Check: "inventory_limit", Count: len(recurrings)}
	}
	var live []invoiceninja.RecurringRecord
	seen := map[string]bool{}
	for _, v := range recurrings {
		if v.ID == "" {
			return r, ReasonAmbiguous, Diagnostic{Stage: "recurring", Check: "missing_id"}
		}
		if seen[v.ID] {
			return r, ReasonAmbiguous, Diagnostic{Stage: "recurring", Check: "duplicate_id", Count: len(recurrings)}
		}
		if v.ClientID != c.ID {
			return r, ReasonAmbiguous, Diagnostic{Stage: "recurring", Check: "foreign_customer"}
		}
		seen[v.ID] = true
		if !v.IsDeleted && v.ArchivedAt == 0 {
			live = append(live, v)
		}
	}
	if len(live) != 1 {
		return r, ReasonAmbiguous, Diagnostic{Stage: "recurring", Check: "live_recurring_count", Count: len(live)}
	}
	recurring := live[0]
	r.RecurringID = recurring.ID
	switch {
	case (recurring.StatusID == "2" || recurring.StatusID == "3") && id.Enabled():
		r.Lifecycle = "active"
		r.InvoiceStatus = "paid"
	case recurring.StatusID == "3" && !id.Enabled():
		r.Lifecycle = "paused"
		r.InvoiceStatus = "sent"
	case recurring.StatusID == "2":
		return r, ReasonUnsupported, Diagnostic{Stage: "recurring", Check: "active_recurring_sui_disabled"}
	case recurring.StatusID == "1":
		return r, ReasonUnsupported, Diagnostic{Stage: "recurring", Check: "recurring_draft"}
	case recurring.StatusID == "4":
		return r, ReasonUnsupported, Diagnostic{Stage: "recurring", Check: "recurring_completed"}
	default:
		return r, ReasonUnsupported, Diagnostic{Stage: "recurring", Check: "recurring_status_unsupported"}
	}
	record, e := s.providers.Records.GetRecord(ctx, id.ID())
	if e != nil {
		return r, ReasonSourceFailure, FailureDiagnostic("sui_record", "read", e)
	}
	hash := record.CredentialsHash()
	r.ConfigHash = hex.EncodeToString(hash[:])
	invoices, e := s.providers.Invoices.ListInvoices(ctx, c.ID)
	if e != nil {
		return r, ReasonSourceFailure, FailureDiagnostic("invoices", "read", e)
	}
	if len(invoices) > s.policy.MaxInventoryRecords {
		return r, ReasonUnsupported, Diagnostic{Stage: "invoices", Check: "inventory_limit", Count: len(invoices)}
	}
	var current *invoiceninja.CycleInvoice
	latest := ""
	ties := 0
	seen = map[string]bool{}
	for i := range invoices {
		iv := &invoices[i]
		if iv.ID == "" {
			return r, ReasonAmbiguous, Diagnostic{Stage: "invoices", Check: "missing_id"}
		}
		if seen[iv.ID] {
			return r, ReasonAmbiguous, Diagnostic{Stage: "invoices", Check: "duplicate_id", Count: len(invoices)}
		}
		if iv.ClientID != c.ID {
			return r, ReasonAmbiguous, Diagnostic{Stage: "invoices", Check: "foreign_customer"}
		}
		seen[iv.ID] = true
		if iv.StatusID == "2" || iv.StatusID == "3" {
			r.UnpaidInvoices++
		}
		if iv.IsDeleted || iv.ArchivedAt != 0 || iv.RecurringID != recurring.ID || iv.StatusID == "1" || iv.StatusID == "5" {
			continue
		}
		if !canonicalDate(iv.Date) {
			return r, ReasonUnsupported, Diagnostic{Stage: "invoices", Check: "invalid_date"}
		}
		if iv.Date > latest {
			latest = iv.Date
			current = iv
			ties = 1
		} else if iv.Date == latest {
			ties++
		}
	}
	if current == nil {
		return r, ReasonAmbiguous, Diagnostic{Stage: "invoices", Check: "current_invoice_missing"}
	}
	if ties != 1 {
		return r, ReasonAmbiguous, Diagnostic{Stage: "invoices", Check: "latest_invoice_tie", Count: ties}
	}
	r.CurrentInvoiceID = current.ID
	r.Amount, e = billing.FormatAmount(current.Amount)
	if e != nil || r.Amount == "0" {
		return r, ReasonUnsupported, Diagnostic{Stage: "invoices", Check: "invalid_amount"}
	}
	var anchors []invoiceninja.CycleInvoice
	if r.Lifecycle == "active" {
		anchors = append(anchors, *current)
	} else {
		for _, iv := range invoices {
			if iv.ID != current.ID && !iv.IsDeleted && iv.ArchivedAt == 0 && iv.RecurringID == recurring.ID && iv.StatusID == "4" && iv.Date < current.Date {
				anchors = append(anchors, iv)
			}
		}
	}
	if len(anchors) == 0 || len(anchors) > s.policy.MaxHistoricalAnchors && r.Lifecycle == "paused" {
		return r, ReasonPayment, Diagnostic{Stage: "payments", Check: "anchor_count", Count: len(anchors)}
	}
	matches := 0
	actor, _ := strconv.ParseInt(c.IDNumber, 10, 64)
	for _, iv := range anchors {
		decision, err := payments.Observe(ctx, s.providers.Payments, payments.Snapshot{ClientID: 1, TelegramID: actor, ChatID: actor, BotID: s.policy.BotID, CustomerID: c.ID, InvoiceID: iv.ID, RecurringID: recurring.ID}, s.policy.NativeCurrencyID, localCalendarDate(now))
		if err != nil {
			return r, ReasonSourceFailure, FailureDiagnostic("payments", "observe", err)
		}
		if decision.Kind != payments.Verified || decision.Proof == nil || !amountStringEqual(decision.Proof.Amount, r.Amount) {
			continue
		}
		proof := decision.Proof
		if r.Lifecycle == "paused" && proof.NextInvoiceDate != current.Date {
			continue
		}
		r.PeriodStart, r.PeriodEnd = proof.PaymentDate, proof.NextInvoiceDate
		matches++
	}
	if matches != 1 {
		return r, ReasonPayment, Diagnostic{Stage: "payments", Check: "verified_match_count", Count: matches}
	}
	r.CompleteProof = true
	r.PinnedProvenance = true
	// These claims are never trusted here: Prepare will verify the entire profile
	// twice against fresh independent source reads before a database admission.
	return r, "", Diagnostic{}
}
