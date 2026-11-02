//go:build integration

package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/ninjaauthority"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

func authorityFixture(t *testing.T) (*Store, context.Context) {
	t.Helper()
	s, ctx := activationTestStore(t)
	paymentExec(t, s, ctx, `INSERT INTO telegram_bots(bot_id) VALUES(9123)`)
	return s, ctx
}
func authorityObservation(actor int64) ninjaauthority.Observation {
	return ninjaauthority.Observation{Actor: actor, CustomerID: "authority-customer-" + strconv.FormatInt(actor, 10), RecurringID: "authority-recurring-" + strconv.FormatInt(actor, 10), InvoiceID: "authority-invoice-" + strconv.FormatInt(actor, 10), NextInvoiceDate: "2040-02-20", StateKnown: true, Enabled: true, SourceHash: sha256.Sum256([]byte("synthetic source")), ScopeHash: strings.Repeat("a", 64), ObservedAt: time.Now().UTC(), InboundIDs: []int64{1, 2}}
}
func authorityDraft(t *testing.T, name string, enabled bool) sui.AuthorityCreateDraft {
	t.Helper()
	d, e := sui.NewAuthorityCreateDraft(name, enabled, []int64{1, 2})
	if e != nil {
		t.Fatal(e)
	}
	return d
}
func authorityRecord(t *testing.T, d sui.AuthorityCreateDraft, id int64) sui.ClientRecord {
	t.Helper()
	raw, e := json.Marshal(d)
	if e != nil {
		t.Fatal(e)
	}
	var fields map[string]json.RawMessage
	if e = json.Unmarshal(raw, &fields); e != nil {
		t.Fatal(e)
	}
	fields["id"] = json.RawMessage(strconv.FormatInt(id, 10))
	raw, e = json.Marshal(fields)
	if e != nil {
		t.Fatal(e)
	}
	r, e := sui.ParseClientRecord(raw)
	if e != nil {
		t.Fatal(e)
	}
	return r
}

// A source observation is enough for menus, including paused/archived state,
// but must never turn an unpaid source into a fabricated payment confirmation.
func TestAuthorityObservationAdoptsWithoutPaymentEvidence(t *testing.T) {
	s, ctx := authorityFixture(t)
	for i, tc := range []struct {
		known, enabled bool
		state          string
	}{
		{true, true, "active"},             // active recurring
		{true, false, "overdue"},           // paused recurring
		{true, false, "overdue"},           // archived customer/recurring, disabled by authority
		{false, false, "awaiting_payment"}, // unknown source state
	} {
		o := authorityObservation(int64(61000 + i))
		o.StateKnown, o.Enabled = tc.known, tc.enabled
		if e := s.SaveAuthorityObservation(ctx, 9123, o); e != nil {
			t.Fatal(e)
		}
		paymentSQL(t, s, ctx, `SELECT sub.state=$2 AND sub.desired_access=$3 AND sub.payment_date IS NULL AND sub.first_invoice_id IS NULL AND sub.current_invoice_id=$4 AND sub.recurring_id=$5 AND sub.next_invoice_date::text=$6 AND c.invoice_ninja_id=$7 AND c.sui_id IS NULL FROM clients c JOIN subscriptions sub ON sub.client_id=c.id WHERE c.telegram_id=$1`, o.Actor, tc.state, tc.known && tc.enabled, o.InvoiceID, o.RecurringID, o.NextInvoiceDate, o.CustomerID)
		paymentSQL(t, s, ctx, `SELECT observation_hash=$2 AND source_hash=$3 AND revision=1 AND observation->>'ScopeHash'=$4 FROM authority_observations WHERE bot_id=9123 AND actor_id=$1`, o.Actor, func() []byte { h := o.Hash(); return h[:] }(), o.SourceHash[:], o.ScopeHash)
		o.Enabled = false
		o.StateKnown = true
		o.ObservedAt = o.ObservedAt.Add(time.Second)
		if e := s.SaveAuthorityObservation(ctx, 9123, o); e != nil {
			t.Fatal(e)
		}
		paymentSQL(t, s, ctx, `SELECT a.revision=2 AND sub.state='overdue' AND sub.payment_date IS NULL AND c.configuration_revision>1 FROM authority_observations a JOIN clients c ON c.id=a.client_id JOIN subscriptions sub ON sub.client_id=c.id WHERE a.actor_id=$1`, o.Actor)
	}
	paymentSQL(t, s, ctx, `SELECT (SELECT count(*)=0 FROM payment_confirmations) AND (SELECT count(*)=0 FROM operations) AND (SELECT count(*)=0 FROM external_write_intents)`)
}
func TestAuthorityIdentityAndScopeConflictsRollback(t *testing.T) {
	s, ctx := authorityFixture(t)
	o := authorityObservation(62000)
	d := authorityDraft(t, "62000", false)
	o.Binding = ninjaauthority.Binding{SUIID: 3200, Name: d.Name(), CredentialsHash: d.CredentialsHash()}
	if e := s.SaveAuthorityObservation(ctx, 9123, o); e != nil {
		t.Fatal(e)
	}
	for _, mutate := range []func(*ninjaauthority.Observation){func(x *ninjaauthority.Observation) { x.CustomerID = "different" }, func(x *ninjaauthority.Observation) { x.Binding.SUIID++ }, func(x *ninjaauthority.Observation) { x.Binding.Name = "62001" }, func(x *ninjaauthority.Observation) { x.ScopeHash = strings.Repeat("b", 64) }, func(x *ninjaauthority.Observation) { x.Actor++ }} {
		changed := o
		mutate(&changed)
		if e := s.SaveAuthorityObservation(ctx, 9123, changed); !errors.Is(e, ninjaauthority.ErrConflict) {
			t.Fatalf("identity conflict not rejected: %v", e)
		}
	}
	paymentSQL(t, s, ctx, `SELECT count(*)=1 AND min(revision)=1 FROM authority_observations`)
	// Historical string IDs cannot be cast unsafely or used to invent a name.
	paymentExec(t, s, ctx, `INSERT INTO clients(telegram_id,chat_id,display_name,sui_id) VALUES(62002,62002,'','not-a-number')`)
	if _, e := s.LoadAuthorityBindings(ctx, 9123); !errors.Is(e, ninjaauthority.ErrConflict) {
		t.Fatalf("invalid legacy ID accepted: %v", e)
	}
	paymentExec(t, s, ctx, `UPDATE clients SET sui_id='3202' WHERE telegram_id=62002`)
	b, e := s.LoadAuthorityBindings(ctx, 9123)
	if e != nil || b[62002].SUIID != 3202 || b[62002].Name != "" {
		t.Fatal("unknown name was fabricated")
	}
}
func TestAuthorityOwnActivationMustFinishAndBindingIsReused(t *testing.T) {
	s, ctx := authorityFixture(t)
	const actor = 63000
	ids := seedActivationAccounts(t, s, ctx, 9123, actor, 1)
	seedActivationBatch(t, s, ctx, 9123, activationAllow(actor, 1), 1)
	var op int64
	if e := s.pool.QueryRow(ctx, `SELECT operation_id FROM activation_flows WHERE bot_id=9123`).Scan(&op); e != nil {
		t.Fatal(e)
	}
	o := authorityObservation(actor)
	o.CustomerID = "customer-63000"
	if e := s.SaveAuthorityObservation(ctx, 9123, o); !errors.Is(e, ninjaauthority.ErrConflict) {
		t.Fatalf("unfinished activation adopted: %v", e)
	}
	paymentSQL(t, s, ctx, `SELECT count(*)=0 FROM authority_observations`)
	d := authorityDraft(t, "pp-"+strings.Repeat("a", 40), true)
	hash := d.CredentialsHash()
	paymentExec(t, s, ctx, `INSERT INTO activation_sui_bindings(client_id,bot_id,sui_id,name,credentials_hash,operation_id) VALUES($1,9123,3300,$2,$3,$4)`, ids[0], d.Name(), hash[:], op)
	paymentExec(t, s, ctx, `UPDATE clients SET sui_id='3300' WHERE id=$1`, ids[0])
	paymentExec(t, s, ctx, `UPDATE operations SET status='completed',completed_at=clock_timestamp(),locked_until=NULL WHERE id=$1`, op)
	b, e := s.LoadAuthorityBindings(ctx, 9123)
	if e != nil {
		t.Fatal(e)
	}
	o.Binding = b[actor]
	if o.Binding.Name != d.Name() || o.Binding.SUIID != 3300 {
		t.Fatal("own binding was not reused")
	}
	if e = s.SaveAuthorityObservation(ctx, 9123, o); e != nil {
		t.Fatal(e)
	}
	paymentSQL(t, s, ctx, `SELECT a.name=$2 AND a.sui_id=3300 AND sub.first_invoice_id IS NOT NULL AND sub.payment_date='2039-01-31'::date FROM activation_sui_bindings a JOIN subscriptions sub ON sub.client_id=a.client_id WHERE a.client_id=$1`, ids[0], d.Name())
}
func TestAuthorityNoticesDeduplicateRearmAndRespectScanClock(t *testing.T) {
	s, ctx := authorityFixture(t)
	notice := func(reason, facts string) {
		t.Helper()
		if e := s.AuthorityNotice(ctx, 9123, "private-key", 0, reason, facts, []int64{9124}); e != nil {
			t.Fatal(e)
		}
	}
	notice("source_failure", "facts-one")
	notice("source_failure", "facts-one")
	paymentSQL(t, s, ctx, `SELECT (SELECT count(*)=1 FROM outbox) AND (SELECT generation=1 AND NOT resolved FROM authority_notices)`)
	notice("source_failure", "facts-two")
	paymentSQL(t, s, ctx, `SELECT (SELECT count(*)=2 FROM outbox) AND (SELECT generation=2 FROM authority_notices)`)
	var started time.Time
	if e := s.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&started); e != nil {
		t.Fatal(e)
	}
	notice("source_failure", "facts-two")
	if e := s.ReconcileAuthorityNotices(ctx, 9123, nil, started); e != nil {
		t.Fatal(e)
	}
	paymentSQL(t, s, ctx, `SELECT NOT resolved FROM authority_notices`)
	if e := s.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&started); e != nil {
		t.Fatal(e)
	}
	if e := s.ReconcileAuthorityNotices(ctx, 9123, nil, started); e != nil {
		t.Fatal(e)
	}
	notice("source_failure", "facts-two")
	paymentSQL(t, s, ctx, `SELECT (SELECT count(*)=3 FROM outbox) AND (SELECT generation=3 AND NOT resolved FROM authority_notices) AND NOT EXISTS(SELECT 1 FROM outbox WHERE body LIKE '%private-key%' OR body LIKE '%facts-%')`)
}
func TestAuthorityCreatePersistsFrozenIntentAndCAS(t *testing.T) {
	s, ctx := authorityFixture(t)
	o := authorityObservation(64000)
	d := authorityDraft(t, "64000", true)
	first, e := s.PrepareAuthorityCreate(ctx, 9123, o, d)
	if e != nil {
		t.Fatal(e)
	}
	fresh := authorityDraft(t, "64000", true)
	old, e := (&Store{pool: s.pool}).PrepareAuthorityCreate(ctx, 9123, o, fresh)
	if e != nil || old.Draft.CredentialsHash() != first.Draft.CredentialsHash() {
		t.Fatal("frozen credentials were replaced")
	}
	var won atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			e := s.AuthorizeAuthorityCreate(ctx, 9123, o.Actor, o.SourceHash)
			if e == nil {
				won.Add(1)
			} else if !errors.Is(e, ninjaauthority.ErrUnknown) {
				t.Errorf("CAS: %v", e)
			}
		})
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Fatal("authorization was not exactly once")
	}
	changed := o
	changed.SourceHash = sha256.Sum256([]byte("different"))
	if _, e = s.PrepareAuthorityCreate(ctx, 9123, changed, d); !errors.Is(e, ninjaauthority.ErrConflict) {
		t.Fatal("changed frozen source accepted")
	}
	paymentSQL(t, s, ctx, `SELECT dispatched AND NOT completed AND record_id IS NULL FROM authority_create_intents`)
	wrong := authorityRecord(t, authorityDraft(t, "64000", false), 3400)
	if e = s.CompleteAuthorityCreate(ctx, 9123, o.Actor, wrong); !errors.Is(e, ninjaauthority.ErrConflict) {
		t.Fatal("non-frozen record accepted")
	}
	r := authorityRecord(t, old.Draft, 3400)
	if e = s.CompleteAuthorityCreate(ctx, 9123, o.Actor, r); e != nil {
		t.Fatal(e)
	}
	if e = s.CompleteAuthorityCreate(ctx, 9123, o.Actor, r); e != nil {
		t.Fatal(e)
	}
	b, e := s.LoadAuthorityBindings(ctx, 9123)
	if e != nil || b[o.Actor].SUIID != r.ID() {
		t.Fatal("completed intent binding lost")
	}
	o.Binding = b[o.Actor]
	if e = s.SaveAuthorityObservation(ctx, 9123, o); e != nil {
		t.Fatal(e)
	}
	paymentSQL(t, s, ctx, `SELECT c.sui_id='3400' AND sub.payment_date IS NULL FROM clients c JOIN subscriptions sub ON sub.client_id=c.id WHERE c.telegram_id=64000`)
}
func TestAuthorityUnknownCreationRetainsFrozenOwnership(t *testing.T) {
	s, ctx := authorityFixture(t)
	o := authorityObservation(64001)
	d := authorityDraft(t, "64001", true)
	if _, err := s.PrepareAuthorityCreate(ctx, 9123, o, d); err != nil {
		t.Fatal(err)
	}
	if err := s.AuthorizeAuthorityCreate(ctx, 9123, o.Actor, o.SourceHash); err != nil {
		t.Fatal(err)
	}
	b, err := s.LoadAuthorityBindings(ctx, 9123)
	if err != nil {
		t.Fatal(err)
	}
	if b[o.Actor].SUIID != 0 || b[o.Actor].Name != d.Name() || b[o.Actor].CredentialsHash != d.CredentialsHash() || b[o.Actor].CustomerID != o.CustomerID {
		t.Fatal("unknown creation lost its ownership anchor")
	}
	foreign := authorityRecord(t, authorityDraft(t, "64001", true), 3401)
	o.Binding = ninjaauthority.Binding{CustomerID: o.CustomerID, SUIID: foreign.ID(), Name: foreign.Name(), CredentialsHash: foreign.CredentialsHash()}
	if err = s.SaveAuthorityObservation(ctx, 9123, o); !errors.Is(err, ninjaauthority.ErrConflict) {
		t.Fatal("same-name foreign record adopted after unknown POST")
	}
	o.Binding = b[o.Actor]
	if err = s.SaveAuthorityObservation(ctx, 9123, o); err != nil {
		t.Fatal(err)
	}
	r := authorityRecord(t, d, 3402)
	o.Binding.SUIID = r.ID()
	o.SUIEnabled = true
	if err = s.SaveAuthorityObservation(ctx, 9123, o); err != nil {
		t.Fatal(err)
	}
	paymentSQL(t, s, ctx, `SELECT dispatched AND NOT completed FROM authority_create_intents WHERE actor_id=64001`)
	if err = s.AuthorizeAuthorityCreate(ctx, 9123, o.Actor, o.SourceHash); !errors.Is(err, ninjaauthority.ErrUnknown) {
		t.Fatal("readback rearmed the dispatch")
	}
}
func TestAuthorityOldUnknownGuardAndCreateIntentAreNeverReleased(t *testing.T) {
	s, ctx := authorityFixture(t)
	o := authorityObservation(65000)
	d := authorityDraft(t, "65000", true)
	if _, e := s.PrepareAuthorityCreate(ctx, 9123, o, d); e != nil {
		t.Fatal(e)
	}
	reg, e := s.RegisterPrivateClient(ctx, TelegramIdentity{TelegramID: 65001, ChatID: 65001})
	if e != nil {
		t.Fatal(e)
	}
	var op int64
	if e = s.pool.QueryRow(ctx, `SELECT id FROM operations WHERE client_id=$1`, reg.Client.ID).Scan(&op); e != nil {
		t.Fatal(e)
	}
	h := sha256.Sum256([]byte("old unknown create"))
	paymentExec(t, s, ctx, `INSERT INTO external_write_intents(operation_id,step,origin,request_hash,dispatch_attempt) VALUES($1,'sui_create','activation',$2,1)`, op, h[:])
	paymentExec(t, s, ctx, `UPDATE activation_write_guard SET operation_id=$1,step='sui_create' WHERE id=1`, op)
	if _, e = s.PrepareAuthorityCreate(ctx, 9123, o, d); !errors.Is(e, ninjaauthority.ErrUnknown) {
		t.Fatal("old guard ignored in prepare")
	}
	if e = s.AuthorizeAuthorityCreate(ctx, 9123, o.Actor, o.SourceHash); !errors.Is(e, ninjaauthority.ErrUnknown) {
		t.Fatal("old guard ignored at dispatch")
	}
	paymentSQL(t, s, ctx, `SELECT operation_id=$1 AND step='sui_create' FROM activation_write_guard`, op)
	// Test fixture clears the guard, not production code: its unknown immutable
	// per-actor intent must independently block a second create.
	paymentExec(t, s, ctx, `UPDATE activation_write_guard SET operation_id=NULL,step=NULL WHERE id=1`)
	o = authorityObservation(65001)
	d = authorityDraft(t, "65001", true)
	if _, e = s.PrepareAuthorityCreate(ctx, 9123, o, d); !errors.Is(e, ninjaauthority.ErrUnknown) {
		t.Fatal("old unbound create was recreated")
	}
}
func TestAuthorityScanTryLockAndCanceledCallbackUnlock(t *testing.T) {
	s, ctx := authorityFixture(t)
	scanCtx, cancel := context.WithCancel(ctx)
	e := s.WithAuthorityScan(scanCtx, 9123, func(context.Context) error {
		called := false
		if e := s.WithAuthorityScan(ctx, 9123, func(context.Context) error { called = true; return nil }); e != nil {
			t.Fatal(e)
		}
		if called {
			t.Fatal("busy scan entered callback")
		}
		// The lock is bot scoped; callbacks still have usable pool connections.
		if e := s.WithAuthorityScan(ctx, 9124, func(context.Context) error { return s.Ping(ctx) }); e != nil {
			t.Fatal(e)
		}
		cancel()
		return scanCtx.Err()
	})
	if !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	called := false
	if e = s.WithAuthorityScan(ctx, 9123, func(context.Context) error { called = true; return nil }); e != nil || !called {
		t.Fatal("canceled scan retained pooled session lock")
	}
}

func TestAuthorityAdoptionResolvesOnlyItsOwnAutomaticImportIssues(t *testing.T) {
	s, ctx := authorityFixture(t)
	for i, actor := range []int64{66000, 66001} {
		if _, err := s.RecordAutomaticImportIssue(ctx, 9123, actor, strings.Repeat(strconv.Itoa(i+1), 64), "payment_unverified", strings.Repeat("c", 64), nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AuthorityNotice(ctx, 9123, "separate-authority-diagnostic", 66000, "state_drift", "private-facts", nil); err != nil {
		t.Fatal(err)
	}
	o := authorityObservation(66000)
	if err := s.SaveAuthorityObservation(ctx, 9123, o); err != nil {
		t.Fatal(err)
	}
	paymentSQL(t, s, ctx, `SELECT (SELECT resolved FROM automatic_import_issues WHERE actor_id=66000) AND (SELECT NOT resolved FROM automatic_import_issues WHERE actor_id=66001) AND (SELECT NOT resolved FROM authority_notices)`)
}
