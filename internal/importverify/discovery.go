package importverify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
}

func (Discovery) String() string { return "[private import discovery]" }

func (s *Service) Discover(ctx context.Context, now time.Time, known map[int64]bool) ([]Discovery, error) {
	if !s.policy.Automatic {
		return nil, errors.New("automatic discovery policy required")
	}
	inventory, ok := s.providers.Customers.(CustomerInventory)
	if !ok {
		return nil, errors.New("complete customer inventory unavailable")
	}
	customers, e := inventory.ListCustomers(ctx)
	if e != nil || len(customers) > s.policy.MaxInventoryRecords {
		return nil, errors.New("complete customer inventory unavailable")
	}
	identities, e := s.providers.Identities.ListIdentities(ctx, s.policy.MaxInventoryRecords)
	if e != nil || len(identities) > s.policy.MaxInventoryRecords {
		return nil, errors.New("complete SUI inventory unavailable")
	}
	byActor := map[string]int{}
	seenCustomer := map[string]bool{}
	for _, c := range customers {
		if c.ID == "" || seenCustomer[c.ID] {
			return nil, errors.New("ambiguous complete customer inventory")
		}
		seenCustomer[c.ID] = true
		byActor[c.IDNumber]++
	}
	byName := map[string][]sui.Identity{}
	seenSUI := map[int64]bool{}
	for _, id := range identities {
		if id.ID() <= 0 || id.Name() == "" || seenSUI[id.ID()] {
			return nil, errors.New("ambiguous complete SUI inventory")
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
		if parseErr != nil || !canonicalPositive(c.IDNumber) || actor <= 0 {
			d.Actor = 0
			d.Reason = ReasonUnsupported
		} else if !c.Active() || byActor[c.IDNumber] != 1 || len(byName[c.IDNumber]) != 1 {
			d.Reason = ReasonAmbiguous
		} else {
			d.Facts, d.Reason = s.discoverRecord(ctx, c, byName[c.IDNumber][0], now)
		}
		raw, _ := json.Marshal(c)
		facts, _ := d.Facts.PrivateJSON()
		sum := sha256.Sum256(append(raw, facts...))
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
		sum := sha256.Sum256(raw)
		out = append(out, Discovery{CustomerID: "sui:" + name, Actor: actor, Reason: ReasonAmbiguous, FactsHash: hex.EncodeToString(sum[:])})
	}
	return out, nil
}

func (s *Service) discoverRecord(ctx context.Context, c invoiceninja.Customer, id sui.Identity, now time.Time) (importplan.Record, string) {
	r := importplan.Record{TelegramID: c.IDNumber, InvoiceNumber: c.IDNumber, InvoiceCustomerID: c.ID, SUIID: strconv.FormatInt(id.ID(), 10), SUIName: id.Name(), Currency: s.policy.NativeCurrencyCode, SourceVersion: s.policy.NinjaVersion}
	recurrings, e := s.providers.Customers.ListRecurring(ctx, c.ID)
	if e != nil {
		return r, ReasonSourceFailure
	}
	if len(recurrings) > s.policy.MaxInventoryRecords {
		return r, ReasonUnsupported
	}
	var live []invoiceninja.RecurringRecord
	seen := map[string]bool{}
	for _, v := range recurrings {
		if v.ID == "" || seen[v.ID] || v.ClientID != c.ID {
			return r, ReasonAmbiguous
		}
		seen[v.ID] = true
		if !v.IsDeleted && v.ArchivedAt == 0 {
			live = append(live, v)
		}
	}
	if len(live) != 1 {
		return r, ReasonAmbiguous
	}
	recurring := live[0]
	r.RecurringID = recurring.ID
	switch {
	case recurring.StatusID == "2" && id.Enabled():
		r.Lifecycle = "active"
		r.InvoiceStatus = "paid"
	case recurring.StatusID == "3" && !id.Enabled():
		r.Lifecycle = "paused"
		r.InvoiceStatus = "sent"
	default:
		return r, ReasonUnsupported
	}
	record, e := s.providers.Records.GetRecord(ctx, id.ID())
	if e != nil {
		return r, ReasonSourceFailure
	}
	hash := record.CredentialsHash()
	r.ConfigHash = hex.EncodeToString(hash[:])
	invoices, e := s.providers.Invoices.ListInvoices(ctx, c.ID)
	if e != nil {
		return r, ReasonSourceFailure
	}
	if len(invoices) > s.policy.MaxInventoryRecords {
		return r, ReasonUnsupported
	}
	var current *invoiceninja.CycleInvoice
	latest := ""
	ties := 0
	seen = map[string]bool{}
	for i := range invoices {
		iv := &invoices[i]
		if iv.ID == "" || seen[iv.ID] || iv.ClientID != c.ID {
			return r, ReasonAmbiguous
		}
		seen[iv.ID] = true
		if iv.StatusID == "2" || iv.StatusID == "3" {
			r.UnpaidInvoices++
		}
		if iv.IsDeleted || iv.ArchivedAt != 0 || iv.RecurringID != recurring.ID || iv.StatusID == "1" || iv.StatusID == "5" {
			continue
		}
		if !canonicalDate(iv.Date) {
			return r, ReasonUnsupported
		}
		if iv.Date > latest {
			latest = iv.Date
			current = iv
			ties = 1
		} else if iv.Date == latest {
			ties++
		}
	}
	if current == nil || ties != 1 {
		return r, ReasonAmbiguous
	}
	r.CurrentInvoiceID = current.ID
	r.Amount, e = billing.FormatAmount(current.Amount)
	if e != nil || r.Amount == "0" {
		return r, ReasonUnsupported
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
		return r, ReasonPayment
	}
	matches := 0
	actor, _ := strconv.ParseInt(c.IDNumber, 10, 64)
	for _, iv := range anchors {
		decision, err := payments.Observe(ctx, s.providers.Payments, payments.Snapshot{ClientID: 1, TelegramID: actor, ChatID: actor, BotID: s.policy.BotID, CustomerID: c.ID, InvoiceID: iv.ID, RecurringID: recurring.ID}, s.policy.NativeCurrencyID, localCalendarDate(now))
		if err != nil {
			return r, ReasonSourceFailure
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
		return r, ReasonPayment
	}
	r.CompleteProof = true
	r.PinnedProvenance = true
	// These claims are never trusted here: Prepare will verify the entire profile
	// twice against fresh independent source reads before a database admission.
	return r, ""
}
