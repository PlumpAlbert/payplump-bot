package activation

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
)

func newAuthorityFixture(t *testing.T) *workerFixture {
	t.Helper()
	f := newWorkerFixture(t)
	cfg := f.w.cfg
	cfg.NinjaAuthority = true
	cfg.SUIBaseURL, cfg.SUIToken, cfg.SUIVersion, cfg.InboundIDs = "", "", "", nil
	providers := f.w.providers
	providers.SUI = nil // Any panel read or write would panic, not silently succeed.
	w, err := New(f.s, providers, f.w.botID, f.w.admins, cfg, f.w.logger)
	if err != nil {
		t.Fatal(err)
	}
	f.w = w
	w.now = func() time.Time { return f.now }
	f.s.s.Mode, f.s.s.PolicyHash = NinjaAuthorityMode, cfg.PolicyHash()
	return f
}

func TestAuthorityActivationPaidFirstInvoiceCreatesAndStartsOnlyRecurring(t *testing.T) {
	f := newAuthorityFixture(t)
	for i, step := range StepsForMode(NinjaAuthorityMode) {
		f.claim()
		intent := f.s.s.Steps[step]
		if intent.ExternalID != "recurring-A" || intent.DispatchAttempt != i+1 || intent.RecurringAmount != "200" || intent.RecurringDatetime == "" || ValidateIntent(f.s.s, step, intent.Payload) != nil || f.s.guard {
			t.Fatal("missing durable original recurring request/receipt")
		}
	}
	f.claim()
	if f.s.finishCalls != 1 || f.s.holdCalls != 0 || f.s.confirmCalls != 2 || len(f.s.s.Steps) != 2 || len(f.s.recurringConfirmations) != 2 || !slices.Equal(f.api.steps, []Step{RecurringCreate, RecurringStart}) || len(f.s.notices) != 0 {
		t.Fatal("recurring provisioning forged the four-step/access audit or did not complete")
	}
	var draft invoiceninja.RecurringDraft
	if DecodeIntent(f.s.s.Steps[RecurringCreate].Payload, &draft) != nil || draft.PrivateNotes != Marker(f.s.s.Token) || draft.ClientID != f.s.s.Account.CustomerID || draft.NextSendDate != f.s.s.Proof.NextInvoiceDate {
		t.Fatal("recurring request lost the original owner/proof/marker")
	}
	if _, ok := f.s.s.Steps[SUICreate]; ok {
		t.Fatal("synthetic SUI create receipt")
	}
	if _, ok := f.s.s.Steps[SUIEnable]; ok {
		t.Fatal("synthetic SUI enable receipt")
	}
}

func TestAuthorityActivationStillRequiresFullFreshManualCashProof(t *testing.T) {
	cases := []struct {
		name   string
		change func(*workerFixture)
	}{
		{"unpaid", func(f *workerFixture) {
			f.p.invoice.Balance = "200"
			f.p.invoice.PaidToDate = "0"
			f.p.invoice.StatusID = "2"
		}},
		{"nonmanual", func(f *workerFixture) { f.p.cash[0].IsManual = false }},
		{"amount", func(f *workerFixture) { f.p.cash[0].Allocations[0].Amount = "199" }},
		{"currency", func(f *workerFixture) { f.p.cash[0].CurrencyID = "99" }},
		{"ownership", func(f *workerFixture) { f.p.customer.IDNumber = "999" }},
		{"ambiguous", func(f *workerFixture) { f.p.cash = append(f.p.cash, f.p.cash[0]) }},
		{"missing cash", func(f *workerFixture) { f.p.cash = nil }},
		{"refunded", func(f *workerFixture) { f.p.cash[0].Refunded = "1" }},
		{"source drift", func(f *workerFixture) { f.p.cash[0].Allocations[0].ID = "changed" }},
	}
	for _, tc := range cases {
		for progress := 0; progress <= 2; progress++ {
			t.Run(tc.name+string(rune('0'+progress)), func(t *testing.T) {
				f := newAuthorityFixture(t)
				f.progress(progress)
				tc.change(f)
				f.claim()
				f.assertHold(ReviewPayment)
				if f.api.writes != progress || f.s.prepareCalls != progress || f.s.finishCalls != 0 {
					t.Fatal("incomplete/changed payment authorized progress")
				}
			})
		}
	}
	t.Run("incomplete read", func(t *testing.T) {
		f := newAuthorityFixture(t)
		f.p.errorAt = 3
		f.claim()
		if f.api.writes != 0 || f.s.prepareCalls != 0 || f.s.holdCalls != 0 || len(f.s.retries) != 1 {
			t.Fatal("incomplete GET authorized a write or sticky negative")
		}
	})
}

func TestAuthorityActivationLegacyJournalsNeverReinterpreted(t *testing.T) {
	for progress := 0; progress <= 4; progress++ {
		t.Run(string(rune('0'+progress)), func(t *testing.T) {
			f := newWorkerFixture(t)
			f.progress(progress)
			f.w.cfg.NinjaAuthority = true
			f.w.providers.SUI = nil
			f.clearReads()
			f.claim()
			f.assertHold(ReviewPolicy)
			if f.api.reads != 0 || len(f.p.calls) != 0 || f.api.writes != progress || f.s.finishCalls != 0 {
				t.Fatal("legacy journal reinterpreted")
			}
		})
	}
	t.Run("unknown original SUI sender", func(t *testing.T) {
		f := newWorkerFixture(t)
		f.progress(1)
		intent := f.s.s.Steps[SUICreate]
		intent.ExternalID = ""
		f.s.s.Steps[SUICreate] = intent
		f.s.guard = true
		f.w.cfg.NinjaAuthority = true
		f.w.providers.SUI = nil
		f.clearReads()
		f.claim()
		f.assertHold(ReviewUnknownWrite)
		if !f.s.guard || f.api.reads != 0 || len(f.p.calls) != 0 || f.api.writes != 1 || f.s.prepareCalls != 1 {
			t.Fatal("unknown old SUI intent was replayed/adopted")
		}
	})
}

func TestAuthorityActivationUnknownRecurringResponseNeverReplays(t *testing.T) {
	for progress := 0; progress < 2; progress++ {
		f := newAuthorityFixture(t)
		f.progress(progress)
		f.api.postError = workerOutage
		f.claim()
		f.assertHold(ReviewUnknownWrite)
		if !f.s.guard || f.s.confirmCalls != progress {
			t.Fatal("unknown recurring result lost its reservation")
		}
		prepares := f.s.prepareCalls
		f.s.s.ReviewCode = 0
		f.clearReads()
		f.claim()
		if f.api.reads != 0 || len(f.p.calls) != 0 || f.s.prepareCalls != prepares || !f.s.guard {
			t.Fatal("unknown recurring result replayed")
		}
	}
}

func TestAuthorityActivationModeIsAnExplicitCapabilityAndJournalFence(t *testing.T) {
	f := newAuthorityFixture(t)
	providers := f.w.providers
	providers.SUI = f.api
	if _, err := New(f.s, providers, 42, f.w.admins, f.w.cfg, nil); err == nil {
		t.Fatal("authority worker accepted a panel writer")
	}
	legacy := f.w.cfg
	legacy.NinjaAuthority = false
	if _, err := New(f.s, f.w.providers, 42, f.w.admins, legacy, nil); err == nil {
		t.Fatal("legacy worker accepted no panel writer")
	}
	s := f.s.s
	for _, step := range []Step{SUICreate, SUIEnable} {
		if ValidateIntent(s, step, json.RawMessage(`{}`)) == nil {
			t.Fatal("authority mode accepted SUI intent")
		}
		s.Steps[step] = Intent{Payload: json.RawMessage(`{}`), DispatchAttempt: 1}
		if _, err := NextStep(s); err == nil {
			t.Fatal("authority order ignored an old SUI step")
		}
		delete(s.Steps, step)
	}
}

func TestAuthorityActivationEnvironmentModeIsNotAnEnvironmentFlag(t *testing.T) {
	enabledActivationEnv(t)
	for _, key := range []string{"SUI_BASE_URL", "SUI_API_TOKEN", "SUI_VERSION", "SUI_INBOUND_IDS"} {
		t.Setenv(key, "")
	}
	cfg, err := AuthorityConfigFromEnvironment(func(k string) string { return os.Getenv(k) }, true, true, false)
	if err != nil || !cfg.NinjaAuthority || cfg.Validate() != nil {
		t.Fatal("authority loader required/constructed SUI configuration")
	}
	if _, err := ConfigFromEnvironment(os.Getenv, true, true, false); err == nil {
		t.Fatal("legacy loader lost its SUI validation")
	}
}

func TestAuthorityActivationPriorRecurringMustRemainExact(t *testing.T) {
	for _, change := range []struct {
		name string
		edit func(*invoiceninja.RecurringRecord)
	}{
		{"owner", func(r *invoiceninja.RecurringRecord) { r.ClientID = "foreign" }},
		{"receipt amount", func(r *invoiceninja.RecurringRecord) { r.Amount = "201" }},
		{"receipt schedule", func(r *invoiceninja.RecurringRecord) { r.NextSendDatetime = "2040-02-29T13:00:00Z" }},
		{"deleted", func(r *invoiceninja.RecurringRecord) { r.IsDeleted = true }},
		{"archived", func(r *invoiceninja.RecurringRecord) { r.ArchivedAt = 1 }},
		{"marker", func(r *invoiceninja.RecurringRecord) { r.PrivateNotes = "foreign" }},
	} {
		for progress := 1; progress <= 2; progress++ {
			t.Run(change.name+string(rune('0'+progress)), func(t *testing.T) {
				f := newAuthorityFixture(t)
				f.progress(progress)
				change.edit(&f.api.recurring)
				f.claim()
				f.assertHold(ReviewRecurring)
				if f.api.writes != progress || f.s.prepareCalls != progress || f.s.finishCalls != 0 {
					t.Fatal("changed recurring source authorized a write/completion")
				}
			})
		}
	}
	t.Run("unknown history", func(t *testing.T) {
		f := newAuthorityFixture(t)
		f.api.foreign = []invoiceninja.RecurringRecord{{ID: "unknown", IsDeleted: true}}
		f.claim()
		f.assertHold(ReviewRecurring)
		if f.api.writes != 0 || f.s.prepareCalls != 0 {
			t.Fatal("unknown customer history authorized recurring creation")
		}
	})
}
