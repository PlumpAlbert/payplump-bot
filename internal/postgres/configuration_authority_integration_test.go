//go:build integration

package postgres

import (
	"crypto/sha256"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/configdelivery"
	"github.com/PlumpAlbert/payplump-bot/internal/ninjaauthority"
)

func TestAuthorityConfigurationDatabaseProtocol(t *testing.T) {
	s, ctx := activationTestStore(t)
	p := configdelivery.Policy{Enabled: true, Revision: 1, Hash: sha256.Sum256([]byte("synthetic authority configuration policy"))}
	if err := s.InstallConfigurationPolicy(ctx, 123, p); err != nil {
		t.Fatal(err)
	}
	o := ninjaauthority.Observation{Actor: configActor, CustomerID: "authority-customer", RecurringID: "authority-recurring", StateKnown: true, Enabled: true, SourceHash: [32]byte{1}, ScopeHash: "synthetic-scope", SUIEnabled: true, InboundIDs: []int64{7}, ObservedAt: time.Now().UTC()}
	o.Binding = ninjaauthority.Binding{CustomerID: o.CustomerID, SUIID: 77, Name: "33000", CredentialsHash: [32]byte{2}}
	save := func() {
		t.Helper()
		if err := s.SaveAuthorityObservation(ctx, 123, o); err != nil {
			t.Fatal(err)
		}
	}
	save()
	configAssert(t, s, ctx, `SELECT NOT EXISTS(SELECT 1 FROM payment_confirmations) AND NOT EXISTS(SELECT 1 FROM activation_flows) AND NOT EXISTS(SELECT 1 FROM verified_import_origins) AND EXISTS(SELECT 1 FROM subscriptions WHERE payment_date IS NULL)`)
	// This uses the real private-menu compare/commit and protected enqueue path.
	u, prepared := configPrepared(t, s, ctx, 900, configActor, p)
	if err := s.ApplyMenuUpdate(ctx, 123, menuTestUsername, u, prepared); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyMenuUpdate(ctx, 123, menuTestUsername, u, prepared); err != nil {
		t.Fatal(err)
	}
	configAssert(t, s, ctx, `SELECT count(*)=1 FROM configuration_requests WHERE origin_kind='authority' AND period_kind='authority' AND invoice_id IS NULL AND proof_hash IS NULL AND activation_operation_id IS NULL`)
	m := configClaim(t, s, ctx)
	v := configView(t, s, ctx, m)
	if !v.Matches() || v.Request.Origin != configdelivery.AuthorityOrigin || v.Request.AuthorityHash != o.Hash() || v.Request.AuthorityRevision != 1 {
		t.Fatal("authority request did not freeze observation")
	}
	// Same apparent source/binding still has a new revision: no ABA upgrade.
	save()
	permit, err := s.AuthorizeConfiguration(ctx, m, v, configdelivery.Decision{Kind: configdelivery.Secret})
	if err != nil || permit.Kind != configdelivery.Denial {
		t.Fatal("changed observation revision authorized")
	}
	configFinish(t, s, ctx, m)
	m = configRequest(t, s, ctx, 901, p)
	v = configView(t, s, ctx, m)
	if !v.Matches() || v.Request.AuthorityRevision != 2 {
		t.Fatal("new request did not select new observation")
	}
	permit, err = s.AuthorizeConfiguration(ctx, m, v, configdelivery.Decision{Kind: configdelivery.Secret})
	if err != nil || permit.Kind != configdelivery.Secret || permit.NotAfter.After(o.ObservedAt.Add(configdelivery.AuthorityFreshness)) {
		t.Fatal("authority secret reservation failed")
	}
	if err := s.FailConfigurationOutbox(ctx, m.Lease, 0, 0, false); err != nil {
		t.Fatal(err)
	}
	// An unknown transmission retains the same non-expiring chat guard across
	// new requests and observation revisions, just as the payment modes do.
	save()
	m = configRequest(t, s, ctx, 902, p)
	v = configView(t, s, ctx, m)
	if !v.ChatBlocked {
		t.Fatal("unknown authority send released chat guard")
	}
	permit, err = s.AuthorizeConfiguration(ctx, m, v, configdelivery.Decision{Kind: configdelivery.Secret})
	if err != nil || permit.Kind != configdelivery.Denial {
		t.Fatal("new authority request bypassed unknown guard")
	}
	configFinish(t, s, ctx, m)
	configAssert(t, s, ctx, `SELECT count(*)=0 FROM payment_confirmations`)
}

func TestAuthorityDoesNotUpgradeOwnedPendingRequest(t *testing.T) {
	s, ctx, _, p := configFixture(t)
	if err := s.InstallConfigurationPolicy(ctx, 123, p); err != nil {
		t.Fatal(err)
	}
	m := configRequest(t, s, ctx, 910, p)
	old := configView(t, s, ctx, m)
	if !old.Matches() {
		t.Fatal("owned fixture mismatch")
	}
	// Adopt the existing binding without rewriting its immutable audit origin.
	a := old.Entitlement.Activation.Account
	o := ninjaauthority.Observation{Actor: configActor, CustomerID: a.CustomerID, RecurringID: a.RecurringID, InvoiceID: a.InvoiceID, StateKnown: true, Enabled: true, NextInvoiceDate: a.NextInvoiceDate, SourceHash: [32]byte{1}, ScopeHash: "synthetic-scope", ObservedAt: time.Now().UTC(), SUIEnabled: true, InboundIDs: []int64{7}}
	b := old.Entitlement.Binding
	o.Binding = ninjaauthority.Binding{CustomerID: a.CustomerID, SUIID: b.ID, Name: b.Name, CredentialsHash: b.CredentialsHash}
	if err := s.SaveAuthorityObservation(ctx, 123, o); err != nil {
		t.Fatal(err)
	}
	v := configView(t, s, ctx, m)
	if v.Eligible || v.Matches() || v.Request.Origin == configdelivery.AuthorityOrigin {
		t.Fatal("pending owned request upgraded to authority")
	}
	permit, err := s.AuthorizeConfiguration(ctx, m, old, configdelivery.Decision{Kind: configdelivery.Secret})
	if err != nil || permit.Kind != configdelivery.Denial {
		t.Fatal("owned request upgraded during final authorization")
	}
	configFinish(t, s, ctx, m)
}
