package configdelivery

import (
	"crypto/sha256"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/importorigin"
	"github.com/PlumpAlbert/payplump-bot/internal/importverify"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

func prepareImportedBaseline(t *testing.T, f *deliveryFixture) {
	t.Helper()
	current := f.store.view.Entitlement.Activation.Account
	current.FirstInvoiceID = ""
	current.ImportedOrigin = importorigin.Ref{ClientID: current.ClientID, BotID: current.BotID, AdmissionRevision: 4, RuntimeRevision: 6, ConfigurationRevision: 2, StableHash: [32]byte{1}, PolicyHash: [32]byte{2}, SourceScopeHash: [32]byte{3}}
	f.p.invoice.RecurringID = current.RecurringID
	rec := f.rec.record
	rec.PrivateNotes = ""
	rec.LineItems[0].Notes = ""
	rec.NextSendDate = "2040-02-29"
	rec.NextSendDatetime = "2040-02-29T12:00:00Z"
	f.rec.record = rec
	proofResult := payments.Evaluate(current, f.p.customer, f.p.invoice, f.p.cash, f.cfg.CurrencyID, "2040-01-31")
	if proofResult.Kind != payments.Verified {
		t.Fatal("invalid imported fixture payment")
	}
	current.PaymentDate, current.NextInvoiceDate = proofResult.Proof.PaymentDate, proofResult.Proof.NextInvoiceDate
	current.ImportedBaselineProofHash = proofResult.Proof.Hash
	// The pinned imported schedule is a stable future send within the verified date.
	profile, err := invoiceninja.NewImportedRecurringProfile(rec, "monthly")
	if err != nil {
		t.Fatal("invalid imported recurring profile")
	}
	b := &ImportedBaseline{Origin: current.ImportedOrigin, AdmissionPermissionRevision: 9, InvoiceID: current.InvoiceID, PaidAnchorInvoiceID: current.InvoiceID, Lifecycle: "active", ProofHash: proofResult.Proof.Hash,
		PublicBaseHash: sha256.Sum256([]byte(f.cfg.SubscriptionBaseURL)), Profile: profile, InboundIDs: []int64{7, 9}, NextSendUTC: time.Date(2040, 2, 29, 12, 0, 0, 0, time.UTC)}
	scope, err := importverify.SourceScope(f.cfg.BaseURL, f.cfg.Token, invoiceninja.SupportedSettlementVersion, f.cfg.SUIBaseURL, f.cfg.SUIToken, f.cfg.SUIVersion)
	if err != nil {
		t.Fatal("cannot construct source scope")
	}
	b.Origin.SourceScopeHash, _ = importorigin.ParseHashHex(scope)
	current.ImportedOrigin = b.Origin
	panelRecord, binding := canonicalImportedPanelRecord(t, f.sui.record, current.TelegramID)
	f.sui.record = panelRecord
	e := Entitlement{Imported: b, Revision: 2, Binding: binding, Period: ImportedBaselinePeriod, Current: current, CurrentProof: *proofResult.Proof}
	hash, err := e.Fingerprint()
	if err != nil {
		t.Fatal("invalid imported entitlement")
	}
	policy := f.cfg.Policy()
	r := Request{OutboxID: f.message.ID, BotID: current.BotID, ClientID: current.ClientID, ActorID: current.TelegramID, ChatID: current.ChatID,
		Period: ImportedBaselinePeriod, Origin: ImportedOrigin, ImportedOriginClientID: b.Origin.ClientID, ImportedAdmissionPermissionRevision: b.AdmissionPermissionRevision, ImportedRuntimeRevision: b.Origin.RuntimeRevision,
		ClientRevision: e.Revision, PolicyRevision: policy.Revision, PolicyHash: policy.Hash, ProofHash: b.ProofHash, EntitlementHash: hash, InvoiceID: b.InvoiceID, CreatedAt: f.now, ExpiresAt: f.now.Add(RequestTTL)}
	f.store.view = View{Request: r, Entitlement: e, Eligible: true}
	f.rec.record = rec
	if !f.store.view.Matches() {
		t.Fatal("imported view does not match")
	}
}

func canonicalImportedPanelRecord(t *testing.T, original sui.ClientRecord, telegramID int64) (sui.ClientRecord, sui.Binding) {
	t.Helper()
	name := strconv.FormatInt(telegramID, 10)
	var fields map[string]json.RawMessage
	if json.Unmarshal(original.Raw(), &fields) != nil {
		t.Fatal("cannot decode synthetic SUI record")
	}
	fields["name"] = deliveryJSON(t, name)
	var configs map[string]map[string]any
	if json.Unmarshal(fields["config"], &configs) != nil {
		t.Fatal("cannot decode synthetic SUI credentials")
	}
	for _, config := range configs {
		for _, key := range []string{"name", "username"} {
			if _, exists := config[key]; exists {
				config[key] = name
			}
		}
	}
	fields["config"] = deliveryJSON(t, configs)
	record, err := sui.ParseClientRecord(deliveryJSON(t, fields))
	if err != nil {
		t.Fatal("cannot parse canonical imported SUI record")
	}
	return record, sui.Binding{ID: record.ID(), Name: name, CredentialsHash: record.CredentialsHash()}
}

func TestImportedBaselineProtectedDelivery(t *testing.T) {
	f := newDeliveryFixture(t)
	prepareImportedBaseline(t, f)
	id, err := f.deliver()
	if err != nil || id != 7001 || f.store.decision != (Decision{Kind: Secret}) || f.tg.posts != 1 || f.tg.gets != 1 {
		t.Fatal("verified active imported baseline did not reach one protected send")
	}
}

func TestImportedBaselineFingerprintPinsActualOriginAndConfiguration(t *testing.T) {
	f := newDeliveryFixture(t)
	prepareImportedBaseline(t, f)
	e := f.store.view.Entitlement
	before, err := e.Fingerprint()
	if err != nil {
		t.Fatal("cannot fingerprint imported baseline")
	}
	e.Imported.Origin.SourceScopeHash[0] ^= 1
	e.Current.ImportedOrigin.SourceScopeHash[0] ^= 1
	after, err := e.Fingerprint()
	if err != nil || before == after {
		t.Fatal("actual redacted origin source-scope mutation was not fingerprinted")
	}
	fresh := newDeliveryFixture(t)
	prepareImportedBaseline(t, fresh)
	e = fresh.store.view.Entitlement
	before, err = e.Fingerprint()
	if err != nil {
		t.Fatal("cannot fingerprint imported baseline")
	}
	e.Imported.PublicBaseHash[0] ^= 1
	after, err = e.Fingerprint()
	if err != nil || before == after {
		t.Fatal("public-base hash mutation was not fingerprinted")
	}
}

func TestImportedBaselineSourceAndPublicBaseDriftDeniedBeforeGET(t *testing.T) {
	for _, mutate := range []func(*deliveryFixture){
		func(f *deliveryFixture) { f.cfg.BaseURL = "https://billing-other.invalid/api/v1" },
		func(f *deliveryFixture) { f.cfg.Token = "different-token" },
		func(f *deliveryFixture) { f.cfg.SubscriptionBaseURL = "https://other.invalid/sub/" },
		func(f *deliveryFixture) { f.cfg.CurrencyID = "2"; f.cfg.Revision++ },
	} {
		f := newDeliveryFixture(t)
		prepareImportedBaseline(t, f)
		mutate(f)
		f.s.cfg = f.cfg
		policy := f.cfg.Policy()
		f.store.view.Request.PolicyRevision, f.store.view.Request.PolicyHash = policy.Revision, policy.Hash
		_, err := f.deliver()
		if err != nil || f.store.decision != (Decision{Kind: Denial, Reason: DeniedPolicy}) || f.sui.reads != 0 || f.rec.reads != 0 || f.p.calls != 0 {
			t.Fatal("configuration drift was not denied before provider GETs")
		}
	}
}
