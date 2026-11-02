package ninjaauthority

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

type Service struct {
	store    Store
	ninja    Ninja
	invoices Invoices
	panel    Panel
	cfg      Config
	logger   *slog.Logger
	now      func() time.Time
}

func New(store Store, ninja Ninja, invoices Invoices, panel Panel, cfg Config, logger *slog.Logger) (*Service, error) {
	if store == nil || ninja == nil || invoices == nil || panel == nil || cfg.BotID <= 0 || len(cfg.InboundIDs) == 0 || len(cfg.AdminIDs) == 0 || len(cfg.SourceScopeHash) != 64 {
		return nil, ErrConflict
	}
	for _, ids := range [][]int64{cfg.InboundIDs, cfg.AdminIDs, cfg.ExcludedIDs} {
		for _, id := range ids {
			if id <= 0 {
				return nil, ErrConflict
			}
		}
	}
	cfg.InboundIDs = slices.Clone(cfg.InboundIDs)
	slices.Sort(cfg.InboundIDs)
	cfg.InboundIDs = slices.Compact(cfg.InboundIDs)
	cfg.AdminIDs = slices.Clone(cfg.AdminIDs)
	slices.Sort(cfg.AdminIDs)
	cfg.AdminIDs = slices.Compact(cfg.AdminIDs)
	cfg.ExcludedIDs = slices.Clone(cfg.ExcludedIDs)
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{store: store, ninja: ninja, invoices: invoices, panel: panel, cfg: cfg, logger: logger, now: time.Now}, nil
}
func (s *Service) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		scan, cancel := context.WithTimeout(ctx, 3*time.Minute)
		if err := s.Scan(scan); err != nil && ctx.Err() == nil {
			s.logger.Warn("Ninja authority reconciliation deferred")
		}
		cancel()
		timer := time.NewTimer(5 * time.Minute)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	return nil
}
func (s *Service) Scan(ctx context.Context) error {
	return s.store.WithAuthorityScan(ctx, s.cfg.BotID, s.scan)
}
func (s *Service) scan(ctx context.Context) error {
	started := s.now().UTC()
	keys := []string{}
	notice := func(key string, actor int64, reason, facts string) error {
		key = s.cfg.SourceScopeHash + ":" + key
		keys = append(keys, key)
		sum := sha256.Sum256([]byte(facts))
		return s.store.AuthorityNotice(ctx, s.cfg.BotID, key, actor, reason, hex.EncodeToString(sum[:]), s.cfg.AdminIDs)
	}
	customers, err := s.ninja.ListCustomers(ctx)
	if err != nil {
		return notice("source:ninja", 0, "source_unavailable", "ninja_inventory")
	}
	inventory, err := s.panel.Inventory(ctx)
	if err != nil {
		return notice("source:sui", 0, "source_unavailable", "sui_inventory")
	}
	bindings, err := s.store.LoadAuthorityBindings(ctx, s.cfg.BotID)
	if err != nil {
		return err
	}
	customerCount := map[string]int{}
	customerIDs := map[string]bool{}
	customerActors := map[string]string{}
	customerOwners, nameOwners := map[string]int64{}, map[string]int64{}
	panelOwners := map[int64]int64{}
	for actor, binding := range bindings {
		if binding.CustomerID != "" {
			if owner, ok := customerOwners[binding.CustomerID]; ok && owner != actor {
				customerOwners[binding.CustomerID] = -1
			} else {
				customerOwners[binding.CustomerID] = actor
			}
		}
		if binding.Name != "" {
			if owner, ok := nameOwners[binding.Name]; ok && owner != actor {
				nameOwners[binding.Name] = -1
			} else {
				nameOwners[binding.Name] = actor
			}
		}
		if binding.SUIID > 0 {
			if owner, ok := panelOwners[binding.SUIID]; ok && owner != actor {
				panelOwners[binding.SUIID] = -1
			} else {
				panelOwners[binding.SUIID] = actor
			}
		}
	}
	for _, c := range customers {
		if c.ID == "" || customerIDs[c.ID] {
			return notice("source:ninja", 0, "ambiguous_identity", "duplicate_customer_id")
		}
		customerIDs[c.ID] = true
		customerActors[c.ID] = c.IDNumber
		customerCount[c.IDNumber]++
	}
	panelIDs := map[int64]sui.ClientRecord{}
	panelNames := map[string][]sui.ClientRecord{}
	for _, r := range inventory {
		if _, ok := panelIDs[r.ID()]; ok {
			return notice("source:sui", 0, "ambiguous_identity", "duplicate_panel_id")
		}
		panelIDs[r.ID()] = r
		panelNames[r.Name()] = append(panelNames[r.Name()], r)
	}
	matched := map[int64]bool{}
	for _, c := range customers {
		actor, e := strconv.ParseInt(c.IDNumber, 10, 64)
		if e != nil || actor <= 0 || strconv.FormatInt(actor, 10) != c.IDNumber {
			sum := sha256.Sum256([]byte(c.ID))
			if e = notice(fmt.Sprintf("customer:%x", sum[:8]), 0, "invalid_identity", "invalid_identity"); e != nil {
				return e
			}
			continue
		}
		if slices.Contains(s.cfg.ExcludedIDs, actor) || actor == s.cfg.BotID {
			for _, r := range panelNames[c.IDNumber] {
				matched[r.ID()] = true
			}
			if b := bindings[actor]; b.SUIID > 0 {
				matched[b.SUIID] = true
			}
			continue
		}
		key := "actor:" + c.IDNumber
		// A failed recurring/invoice read does not make this known Ninja
		// identity a panel-only account. Preserve that distinction in notices.
		for _, record := range panelNames[c.IDNumber] {
			matched[record.ID()] = true
		}
		if binding := bindings[actor]; binding.SUIID > 0 {
			matched[binding.SUIID] = true
		}
		if customerCount[c.IDNumber] != 1 {
			for _, r := range panelNames[c.IDNumber] {
				matched[r.ID()] = true
			}
			if e = notice(key, actor, "ambiguous_identity", "duplicate_actor"); e != nil {
				return e
			}
			continue
		}
		records, e := s.ninja.ListRecurring(ctx, c.ID)
		if e != nil {
			if e = notice(key, actor, "source_unavailable", "recurring_inventory"); e != nil {
				return e
			}
			continue
		}
		o, reason := ObserveCustomer(c, records)
		o.ScopeHash = s.cfg.SourceScopeHash
		o.ObservedAt = started
		if reason != "" {
			if e = notice(key+":state", actor, reason, reason); e != nil {
				return e
			}
		}
		old := bindings[actor]
		if owner, exists := customerOwners[c.ID]; exists && owner != actor {
			if e = notice(key, actor, "ambiguous_identity", "customer_owned_by_another_actor"); e != nil {
				return e
			}
			continue
		}
		if old.CustomerID != "" && old.CustomerID != c.ID {
			if e = notice(key, actor, "ambiguous_identity", "customer_binding_changed"); e != nil {
				return e
			}
			continue
		}
		name := c.IDNumber
		if old.Name != "" {
			name = old.Name
		}
		if owner, exists := nameOwners[name]; exists && owner != actor {
			if e = notice(key, actor, "ambiguous_identity", "name_owned_by_another_actor"); e != nil {
				return e
			}
			continue
		}
		matches := panelNames[name]
		if old.SUIID > 0 {
			if record, exists := panelIDs[old.SUIID]; exists && record.Name() != name {
				matched[old.SUIID] = true
				if e = notice(key, actor, "ambiguous_identity", "bound_name_changed"); e != nil {
					return e
				}
				continue
			}
		}
		if name != c.IDNumber && len(panelNames[c.IDNumber]) > 0 {
			for _, r := range panelNames[c.IDNumber] {
				matched[r.ID()] = true
			}
			for _, r := range matches {
				matched[r.ID()] = true
			}
			if e = notice(key, actor, "ambiguous_identity", "own_and_legacy_accounts"); e != nil {
				return e
			}
			continue
		}
		if len(matches) > 1 {
			for _, r := range matches {
				matched[r.ID()] = true
			}
			if e = notice(key, actor, "ambiguous_identity", "duplicate_sui_name"); e != nil {
				return e
			}
			continue
		}
		if len(matches) == 1 {
			r := matches[0]
			matched[r.ID()] = true
			if owner, exists := panelOwners[r.ID()]; exists && owner != actor {
				if e = notice(key, actor, "ambiguous_identity", "panel_owned_by_another_actor"); e != nil {
					return e
				}
				continue
			}
			if old.SUIID > 0 && r.ID() != old.SUIID {
				if e = notice(key, actor, "ambiguous_identity", "bound_id_changed"); e != nil {
					return e
				}
				continue
			}
			if old.CredentialsHash != ([32]byte{}) && old.CredentialsHash != r.CredentialsHash() {
				if e = notice(key, actor, "ambiguous_identity", "bound_credentials_changed"); e != nil {
					return e
				}
				continue
			}
			o.Binding = Binding{CustomerID: c.ID, SUIID: r.ID(), Name: r.Name(), CredentialsHash: r.CredentialsHash()}
			o.InboundIDs = r.InboundIDs()
			slices.Sort(o.InboundIDs)
			o.SUIEnabled = r.Enabled()
		} else if old.SUIID > 0 {
			// Missing is not permission to discard an established binding or
			// recreate a possibly unresolved historic account. Still observe Ninja.
			o.Binding = old
			o.Binding.CustomerID = c.ID
			if e = notice(key+":missing_sui", actor, "bound_sui_missing", "absent_from_complete_inventory"); e != nil {
				return e
			}
		} else {
			o.Binding = Binding{CustomerID: c.ID, Name: name, CredentialsHash: old.CredentialsHash}
		}
		// Invoices are observed for menu association, never used to manufacture a
		// paid period or to decide the state of an existing recurring subscription.
		invoices, e := s.invoices.ListInvoices(ctx, c.ID)
		if e != nil {
			if e = notice(key+":invoices", actor, "source_unavailable", "invoice_inventory"); e != nil {
				return e
			}
			continue
		}
		latest := ""
		ties := 0
		seen := map[string]bool{}
		valid := true
		for _, i := range invoices {
			if i.ID == "" || seen[i.ID] || i.ClientID != c.ID {
				valid = false
				break
			}
			seen[i.ID] = true
			if i.IsDeleted || i.ArchivedAt != 0 || i.StatusID == "1" || i.StatusID == "5" {
				continue
			}
			if o.RecurringID != "" && i.RecurringID != o.RecurringID {
				continue
			}
			if i.Date > latest {
				latest = i.Date
				o.InvoiceID = i.ID
				o.InvoiceNumber, o.InvoiceAmount, o.InvoiceStatus, o.InvoiceDate = i.Number, i.Amount.String(), i.StatusID, i.Date
				ties = 1
			} else if i.Date == latest {
				ties++
			}
		}
		if !valid {
			return notice("source:ninja", 0, "ambiguous_identity", "invalid_invoice_inventory")
		}
		if ties > 1 {
			o.InvoiceID = ""
			o.InvoiceNumber, o.InvoiceAmount, o.InvoiceStatus, o.InvoiceDate = "", "", "", ""
			if e = notice(key+":invoices", actor, "ambiguous_invoice", "latest_date_tie"); e != nil {
				return e
			}
		}
		if e = s.store.SaveAuthorityObservation(ctx, s.cfg.BotID, o); e != nil {
			if e = notice(key, actor, "local_binding_conflict", "save_observation"); e != nil {
				return e
			}
			continue
		}
		if len(matches) == 0 && old.SUIID == 0 && o.StateKnown && reason == "" {
			created, createErr := s.create(ctx, o)
			if createErr != nil {
				if e = notice(key+":create", actor, "creation_needs_review", "creation_unconfirmed"); e != nil {
					return e
				}
				continue
			}
			o.Binding = Binding{CustomerID: c.ID, SUIID: created.ID(), Name: created.Name(), CredentialsHash: created.CredentialsHash()}
			o.InboundIDs = created.InboundIDs()
			slices.Sort(o.InboundIDs)
			o.SUIEnabled = created.Enabled()
			if e = s.store.SaveAuthorityObservation(ctx, s.cfg.BotID, o); e != nil {
				return e
			}
			panelIDs[created.ID()] = created
			panelNames[created.Name()] = []sui.ClientRecord{created}
			matched[created.ID()] = true
		}
		if len(matches) == 1 && o.StateKnown && o.SUIEnabled != o.Enabled {
			if e = notice(key+":enabled", actor, "state_mismatch", fmt.Sprintf("ninja=%t,sui=%t", o.Enabled, o.SUIEnabled)); e != nil {
				return e
			}
		}
		if len(matches) == 1 && !slices.Equal(o.InboundIDs, s.cfg.InboundIDs) {
			if e = notice(key+":inbounds", actor, "inbound_mismatch", fmt.Sprint(o.InboundIDs, s.cfg.InboundIDs)); e != nil {
				return e
			}
		}
	}
	// A formerly known customer absent from a COMPLETE Ninja inventory loses
	// current authority; keep its identity/audit, but never serve stale access.
	for actor, b := range bindings {
		if b.CustomerID == "" || (customerIDs[b.CustomerID] && customerActors[b.CustomerID] == strconv.FormatInt(actor, 10)) || actor == s.cfg.BotID || slices.Contains(s.cfg.ExcludedIDs, actor) {
			continue
		}
		o := Observation{Actor: actor, CustomerID: b.CustomerID, ScopeHash: s.cfg.SourceScopeHash, Binding: b, ObservedAt: started, SourceHash: sha256.Sum256([]byte("absent:" + b.CustomerID))}
		if r, ok := panelIDs[b.SUIID]; ok {
			o.SUIEnabled = r.Enabled()
			o.InboundIDs = r.InboundIDs()
			slices.Sort(o.InboundIDs)
		}
		if err := s.store.SaveAuthorityObservation(ctx, s.cfg.BotID, o); err != nil {
			return err
		}
		if err := notice("actor:"+strconv.FormatInt(actor, 10)+":missing", actor, "ninja_missing", "absent_from_complete_inventory"); err != nil {
			return err
		}
	}
	// Include every existing account, even nonnumeric names. A persisted own
	// binding is not an orphan merely because its name has the pp-* format.
	for _, b := range bindings {
		if b.SUIID > 0 && customerIDs[b.CustomerID] {
			matched[b.SUIID] = true
		}
	}
	for _, r := range inventory {
		if !matched[r.ID()] {
			if err := notice(fmt.Sprintf("sui:%d", r.ID()), 0, "sui_only", fmt.Sprint(r.ID())); err != nil {
				return err
			}
		}
	}
	return s.store.ReconcileAuthorityNotices(ctx, s.cfg.BotID, keys, started)
}
func (s *Service) create(ctx context.Context, o Observation) (sui.ClientRecord, error) {
	draft, err := sui.NewAuthorityCreateDraft(o.Binding.Name, o.Enabled, s.cfg.InboundIDs)
	if err != nil {
		return sui.ClientRecord{}, err
	}
	intent, err := s.store.PrepareAuthorityCreate(ctx, s.cfg.BotID, o, draft)
	if err != nil {
		return sui.ClientRecord{}, err
	}
	found, exists, err := s.panel.FindCreated(ctx, intent.Draft)
	if err != nil {
		return sui.ClientRecord{}, err
	}
	if exists {
		if err = s.store.CompleteAuthorityCreate(ctx, s.cfg.BotID, o.Actor, found); err != nil {
			return sui.ClientRecord{}, err
		}
		return found, nil
	}
	if intent.Dispatched || intent.Completed {
		return sui.ClientRecord{}, ErrUnknown
	}
	created, err := s.panel.CreateAuthorized(ctx, intent.Draft, func(ctx context.Context) error {
		customers, err := s.ninja.ListCustomers(ctx)
		if err != nil {
			return err
		}
		var owner *invoiceninja.Customer
		seenIDs := map[string]bool{}
		for i := range customers {
			if customers[i].ID == "" || seenIDs[customers[i].ID] {
				return ErrConflict
			}
			seenIDs[customers[i].ID] = true
			if customers[i].IDNumber == strconv.FormatInt(o.Actor, 10) {
				if owner != nil {
					return ErrConflict
				}
				owner = &customers[i]
			}
		}
		if owner == nil || owner.ID != o.CustomerID {
			return ErrConflict
		}
		records, err := s.ninja.ListRecurring(ctx, o.CustomerID)
		if err != nil {
			return err
		}
		fresh, reason := ObserveCustomer(*owner, records)
		if reason != "" || !fresh.StateKnown || fresh.Enabled != o.Enabled || fresh.SourceHash != intent.SourceHash {
			return ErrConflict
		}
		return s.store.AuthorizeAuthorityCreate(ctx, s.cfg.BotID, o.Actor, intent.SourceHash)
	})
	if err != nil {
		return sui.ClientRecord{}, err
	}
	if err = s.store.CompleteAuthorityCreate(ctx, s.cfg.BotID, o.Actor, created); err != nil {
		return sui.ClientRecord{}, err
	}
	return created, nil
}
