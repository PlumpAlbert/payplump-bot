package configdelivery

import (
	"context"
	"slices"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/importverify"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/ninjaauthority"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
)

const AuthorityFreshness = 10 * time.Minute

// AuthorityCustomers is optional. Its absence denies only authority delivery.
type AuthorityCustomers interface {
	FindCustomers(context.Context, int64) ([]invoiceninja.Customer, error)
	ListRecurring(context.Context, string) ([]invoiceninja.RecurringRecord, error)
}

func (e Entitlement) validateAuthority() error {
	a := e.Authority
	o := a.Observation
	if a.ClientID <= 0 || a.BotID <= 0 || a.Revision <= 0 || e.Revision < 0 || e.Period != AuthorityPeriod || e.Imported != nil || e.ActivationID != 0 || !emptyActivationSnapshot(e.Activation) || e.Current != (payments.Snapshot{}) || !emptyProof(e.CurrentProof) || e.Renewal != (RenewalAudit{}) ||
		o.Actor <= 0 || o.CustomerID == "" || o.RecurringID == "" || !o.StateKnown || !o.Enabled || !o.SUIEnabled || o.SourceHash == ([32]byte{}) || o.ScopeHash == "" || o.ObservedAt.IsZero() ||
		o.Binding.CustomerID != o.CustomerID || o.Binding.SUIID <= 0 || o.Binding.Name == "" || o.Binding.CredentialsHash == ([32]byte{}) || !validPositiveIDs(o.InboundIDs) || len(o.InboundIDs) == 0 ||
		e.Binding.ID != o.Binding.SUIID || e.Binding.Name != o.Binding.Name || e.Binding.CredentialsHash != o.Binding.CredentialsHash {
		return ErrDenied
	}
	return nil
}

func (s *Service) evaluateAuthority(ctx context.Context, v View) Decision {
	o := v.Entitlement.Authority.Observation
	scope, err := importverify.SourceScope(s.cfg.BaseURL, s.cfg.Token, invoiceninja.SupportedSettlementVersion, s.cfg.SUIBaseURL, s.cfg.SUIToken, s.cfg.SUIVersion)
	if err != nil || scope != o.ScopeHash {
		return deny(DeniedPolicy)
	}
	if s.providers.AuthorityCustomers == nil {
		return deny(DeniedUnavailable)
	}
	fresh := func() bool {
		now := s.now()
		return !now.Before(o.ObservedAt) && !now.Before(v.Request.CreatedAt) && now.Before(v.Request.ExpiresAt) && now.Before(o.ObservedAt.Add(AuthorityFreshness))
	}
	if !fresh() {
		return deny(DeniedExpired)
	}
	// Repeat both source GETs and the exact panel binding check. Neither a
	// payment observation nor a provider write belongs to this authorization.
	for range 2 {
		customers, err := s.providers.AuthorityCustomers.FindCustomers(ctx, o.Actor)
		if err != nil {
			return deny(DeniedUnavailable)
		}
		if len(customers) != 1 {
			return deny(DeniedEntitlement)
		}
		records, err := s.providers.AuthorityCustomers.ListRecurring(ctx, o.CustomerID)
		if err != nil {
			return deny(DeniedUnavailable)
		}
		current, reason := ninjaauthority.ObserveCustomer(customers[0], records)
		if reason != "" || current.Actor != o.Actor || current.CustomerID != o.CustomerID || current.RecurringID != o.RecurringID || current.SourceHash != o.SourceHash || !current.StateKnown || !current.Enabled {
			return deny(DeniedRecurring)
		}
		record, err := s.providers.SUI.GetRecord(ctx, o.Binding.SUIID)
		if err != nil {
			return deny(DeniedUnavailable)
		}
		actualInbounds := record.InboundIDs()
		slices.Sort(actualInbounds)
		if record.ID() != o.Binding.SUIID || record.Name() != o.Binding.Name || record.CredentialsHash() != o.Binding.CredentialsHash || !record.Enabled() || !slices.Equal(actualInbounds, o.InboundIDs) {
			return deny(DeniedSUI)
		}
	}
	if !fresh() {
		return deny(DeniedExpired)
	}
	return Decision{Kind: Secret}
}
