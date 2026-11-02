package configdelivery

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/importverify"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/ninjaauthority"
)

type authoritySource struct {
	customer invoiceninja.Customer
	records  []invoiceninja.RecurringRecord
	calls    int
	hook     func(int)
	err      error
}

func (p *authoritySource) FindCustomers(_ context.Context, actor int64) ([]invoiceninja.Customer, error) {
	p.calls++
	if p.hook != nil {
		p.hook(p.calls)
	}
	return []invoiceninja.Customer{p.customer}, p.err
}
func (p *authoritySource) ListRecurring(context.Context, string) ([]invoiceninja.RecurringRecord, error) {
	return p.records, p.err
}

func authorityFixture(t *testing.T) (*deliveryFixture, *authoritySource) {
	t.Helper()
	f := newDeliveryFixture(t)
	f.sui.record, _ = canonicalImportedPanelRecord(t, f.sui.record, 101)
	source := &authoritySource{customer: invoiceninja.Customer{ID: "customer-A", IDNumber: "101"}, records: []invoiceninja.RecurringRecord{f.rec.record}}
	o, reason := ninjaauthority.ObserveCustomer(source.customer, source.records)
	if reason != "" {
		t.Fatal("invalid authority fixture")
	}
	o.ScopeHash, _ = importverify.SourceScope(f.cfg.BaseURL, f.cfg.Token, invoiceninja.SupportedSettlementVersion, f.cfg.SUIBaseURL, f.cfg.SUIToken, f.cfg.SUIVersion)
	o.ObservedAt = f.now.Add(-time.Minute)
	o.SUIEnabled = true
	o.Binding = ninjaauthority.Binding{CustomerID: o.CustomerID, SUIID: f.sui.record.ID(), Name: f.sui.record.Name(), CredentialsHash: f.sui.record.CredentialsHash()}
	o.InboundIDs = f.sui.record.InboundIDs()
	e := Entitlement{Authority: &AuthorityEntitlement{ClientID: 7, BotID: 42, Revision: 3, Observation: o}, Period: AuthorityPeriod, Revision: 2}
	e.Binding.ID, e.Binding.Name, e.Binding.CredentialsHash = o.Binding.SUIID, o.Binding.Name, o.Binding.CredentialsHash
	hash, err := e.Fingerprint()
	if err != nil {
		t.Fatal("invalid authority entitlement")
	}
	policy := f.cfg.Policy()
	r := Request{OutboxID: 80, BotID: 42, ClientID: 7, ActorID: 101, ChatID: 101, Origin: AuthorityOrigin, Period: AuthorityPeriod, ClientRevision: 2, AuthorityRevision: 3, AuthorityHash: o.Hash(), PolicyRevision: policy.Revision, PolicyHash: policy.Hash, EntitlementHash: hash, CreatedAt: f.now, ExpiresAt: f.now.Add(RequestTTL)}
	f.store.view = View{Request: r, Entitlement: e, Eligible: true}
	if !f.store.view.Matches() {
		t.Fatal("authority view mismatch")
	}
	f.s.providers.AuthorityCustomers = source
	return f, source
}

func TestAuthorityPaymentIndependentDelivery(t *testing.T) {
	f, source := authorityFixture(t)
	// No invoice, activation, payment proof or paid deadline exists in this view.
	source.records[0].NextSendDate = "" // freeze a source with no date at all
	o, _ := ninjaauthority.ObserveCustomer(source.customer, source.records)
	frozen := &f.store.view.Entitlement.Authority.Observation
	frozen.SourceHash = o.SourceHash
	frozen.NextInvoiceDate = o.NextInvoiceDate
	f.store.view.Request.AuthorityHash = frozen.Hash()
	f.store.view.Request.EntitlementHash, _ = f.store.view.Entitlement.Fingerprint()
	if _, err := f.deliver(); err != nil || f.store.decision.Kind != Secret || source.calls != 2 || f.p.calls != 0 || f.rec.reads != 0 || !strings.Contains(f.tg.content.Text, "https://subscription.invalid/") {
		t.Fatal("authority did not deliver independently of payment")
	}
	if !f.tg.content.NotAfter.Before(frozen.ObservedAt.Add(AuthorityFreshness)) {
		t.Fatal("freshness not bounded")
	}
}

func TestAuthorityDeliveryDoesNotImposeLegacyUnlimitedProfile(t *testing.T) {
	f, _ := authorityFixture(t)
	f.sui.record = deliveryMutateRecord(t, f.sui.record, func(m map[string]json.RawMessage) { m["volume"] = json.RawMessage(`1000000`) })
	if _, err := f.deliver(); err != nil || f.store.decision.Kind != Secret {
		t.Fatal("legacy unlimited-only policy masked authority identity")
	}
}
func TestAuthorityDenials(t *testing.T) {
	cases := []struct {
		name   string
		change func(*deliveryFixture, *authoritySource)
		reason int
	}{
		{"paused", func(f *deliveryFixture, p *authoritySource) { p.records[0].StatusID = "3" }, DeniedRecurring},
		{"inactive", func(f *deliveryFixture, p *authoritySource) { p.customer.IsDeleted = true }, DeniedRecurring},
		{"source_drift_second_read", func(f *deliveryFixture, p *authoritySource) {
			p.hook = func(n int) {
				if n == 2 {
					p.records[0].PrivateNotes = "changed private data"
				}
			}
		}, DeniedRecurring},
		{"wrong_customer", func(f *deliveryFixture, p *authoritySource) { p.customer.ID = "other" }, DeniedRecurring},
		{"wrong_actor", func(f *deliveryFixture, p *authoritySource) { p.customer.IDNumber = "102" }, DeniedRecurring},
		{"missing_provider", func(f *deliveryFixture, p *authoritySource) { f.s.providers.AuthorityCustomers = nil }, DeniedUnavailable},
		{"provider_error", func(f *deliveryFixture, p *authoritySource) { p.err = errors.New("private provider failure") }, DeniedUnavailable},
		{"disabled_sui", func(f *deliveryFixture, p *authoritySource) {
			f.sui.record = deliveryMutateRecord(t, f.sui.record, func(m map[string]json.RawMessage) { m["enable"] = json.RawMessage(`false`) })
		}, DeniedSUI},
		{"renamed_sui", func(f *deliveryFixture, p *authoritySource) {
			f.sui.record, _ = canonicalImportedPanelRecord(t, f.sui.record, 102)
		}, DeniedSUI},
		{"credential_drift", func(f *deliveryFixture, p *authoritySource) {
			f.sui.record = deliveryMutateRecord(t, f.sui.record, func(m map[string]json.RawMessage) {
				var cfg map[string]map[string]any
				if json.Unmarshal(m["config"], &cfg) != nil {
					t.Fatal("invalid config")
				}
				cfg["trojan"]["password"] = "changed-password"
				m["config"] = deliveryJSON(t, cfg)
			})
		}, DeniedSUI},
		{"inbound_drift", func(f *deliveryFixture, p *authoritySource) {
			f.sui.record = deliveryMutateRecord(t, f.sui.record, func(m map[string]json.RawMessage) { m["inbounds"] = json.RawMessage(`[7]`) })
		}, DeniedSUI},
		{"scope", func(f *deliveryFixture, p *authoritySource) { f.s.cfg.Token = "wrong-source" }, DeniedPolicy},
		{"stale", func(f *deliveryFixture, p *authoritySource) { f.now = f.now.Add(AuthorityFreshness) }, DeniedExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, p := authorityFixture(t)
			tc.change(f, p)
			f.assertDenial(tc.reason, false)
			if f.p.calls != 0 || strings.Contains(f.tg.content.Text, "https://") {
				t.Fatal("denial leaked URL or read payment")
			}
		})
	}
}

func TestAuthorityFrozenIdentityAndNoProof(t *testing.T) {
	for _, change := range []func(*View){
		func(v *View) { v.Request.AuthorityRevision++ },
		func(v *View) { v.Request.AuthorityHash[0] ^= 1 },
		func(v *View) { v.Request.PolicyHash[0] ^= 1 },
		func(v *View) { v.Request.BotID++ },
		func(v *View) { v.Request.ActorID++ },
		func(v *View) { v.Request.ProofHash = [32]byte{1} },
		func(v *View) { v.Entitlement.Authority.Observation.Enabled = false },
		func(v *View) { v.Entitlement.Authority.Observation.SUIEnabled = false },
		func(v *View) { v.Entitlement.Authority.Observation.Binding.Name = "other" },
	} {
		f, _ := authorityFixture(t)
		change(&f.store.view)
		if f.s.evaluate(context.Background(), f.message, f.store.view).Kind != Denial {
			t.Fatal("changed authority accepted")
		}
	}
}

func TestAuthorityFingerprintAndFreshness(t *testing.T) {
	for _, change := range []func(*AuthorityEntitlement){
		func(a *AuthorityEntitlement) { a.Revision++ },
		func(a *AuthorityEntitlement) { a.BotID++ },
		func(a *AuthorityEntitlement) { a.ClientID++ },
		func(a *AuthorityEntitlement) { a.Observation.SourceHash[0] ^= 1 },
		func(a *AuthorityEntitlement) { a.Observation.ScopeHash = "other-scope" },
		func(a *AuthorityEntitlement) { a.Observation.ObservedAt = a.Observation.ObservedAt.Add(time.Second) },
		func(a *AuthorityEntitlement) { a.Observation.InvoiceID = "other-invoice" },
	} {
		f, _ := authorityFixture(t)
		before, _ := f.store.view.Entitlement.Fingerprint()
		change(f.store.view.Entitlement.Authority)
		after, err := f.store.view.Entitlement.Fingerprint()
		if err != nil || before == after {
			t.Fatal("authority fact not fingerprinted")
		}
	}
	f, _ := authorityFixture(t)
	o := &f.store.view.Entitlement.Authority.Observation
	o.ObservedAt = f.now.Add(-AuthorityFreshness)
	f.store.view.Request.AuthorityHash = o.Hash()
	f.store.view.Request.EntitlementHash, _ = f.store.view.Entitlement.Fingerprint()
	f.assertDenial(DeniedExpired, false)
}
