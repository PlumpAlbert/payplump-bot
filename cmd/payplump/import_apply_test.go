package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/importadmission"
	"github.com/PlumpAlbert/payplump-bot/internal/importverify"
	"github.com/PlumpAlbert/payplump-bot/internal/postgres"
)

// Invalid requests must fail before reading environment, constructing GET
// clients, or opening the database. The order is part of the command contract.
func TestImportApplyRejectsBeforeExternalCapabilities(t *testing.T) {
	f := newImportApplyFixture(t)
	getenvCalls, factoryCalls, openCalls := 0, 0, 0
	getenv := func(string) string { getenvCalls++; return "" }
	factory := func(verifyConfig) (importverify.Providers, error) {
		factoryCalls++
		return importverify.Providers{}, nil
	}
	open := func(context.Context, string, int32) (importApplyStore, error) { openCalls++; return nil, nil }
	malformed := filepath.Join(t.TempDir(), "malformed.json")
	if err := os.WriteFile(malformed, []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "manifest-link.json")
	if err := os.Symlink(f.manifestPath, link); err != nil {
		t.Fatal(err)
	}
	cases := [][]string{
		{}, {"--file", f.manifestPath}, {"--file", f.manifestPath, "--expected-hash", f.planHash},
		{"--file", malformed, "--expected-hash", f.planHash, "--apply", "--admission-revision", "1"},
		{"--file", link, "--expected-hash", f.planHash, "--apply", "--admission-revision", "1"},
		{"--file", f.manifestPath, "--expected-hash", f.planHash, "--apply", "--admission-revision", "01"},
		{"--file", f.manifestPath, "--expected-hash", "bad", "--apply", "--admission-revision", "1"},
		{"--file", f.manifestPath, "--expected-hash", f.planHash, "--apply", "--admission-revision", "1", "--unexpected"},
	}
	for _, args := range cases {
		if err := importApplyWith(args, &bytes.Buffer{}, getenv, factory, func() time.Time { return f.now }, open); err == nil {
			t.Errorf("accepted invalid invocation %q", args)
		}
	}
	if getenvCalls != 0 || factoryCalls != 0 || openCalls != 0 {
		t.Fatalf("invalid requests reached capabilities: getenv=%d factory=%d open=%d", getenvCalls, factoryCalls, openCalls)
	}
}

func TestImportApplyRequiresExplicitApplyAndCanonicalRevision(t *testing.T) {
	f := newImportApplyFixture(t)
	getenvCalls := 0
	getenv := func(string) string { getenvCalls++; return "" }
	for _, args := range [][]string{
		{"--file", f.manifestPath, "--expected-hash", f.planHash, "--admission-revision", "1"},
		{"--file", f.manifestPath, "--expected-hash", f.planHash, "--apply", "--admission-revision", "0"},
		{"--file", f.manifestPath, "--expected-hash", f.planHash, "--apply", "--admission-revision", "+1"},
		{"--file", f.manifestPath, "--expected-hash", f.planHash, "--apply", "--admission-revision", "01"},
	} {
		if err := importApplyWith(args, &bytes.Buffer{}, getenv, newVerifyProviders, func() time.Time { return f.now }, func(context.Context, string, int32) (importApplyStore, error) {
			t.Fatal("opened DB for invalid args")
			return nil, nil
		}); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
	if getenvCalls != 0 {
		t.Fatalf("read env for invalid args: %d", getenvCalls)
	}
}

// A success path exercises the actual source clients, importverify service and
// Prepare implementation; only the final persistence capability is substituted.
func TestImportApplyUsesFreshOpaquePlanAndDoesNotEnableByDefault(t *testing.T) {
	f := newImportApplyFixture(t)
	if age := time.Since(f.now); age < 0 || age > 10*time.Second {
		t.Fatalf("fixture clock is not near real time: age=%s now=%s", age, f.now)
	}
	var out bytes.Buffer
	opened := false
	store := &applyTestStore{t: t}
	args := []string{"--file", f.manifestPath, "--expected-hash", f.planHash, "--apply", "--admission-revision", "1"}
	err := importApplyWith(args, &out, func(k string) string { return f.env[k] }, newVerifyProviders, func() time.Time { return f.now }, func(_ context.Context, _ string, max int32) (importApplyStore, error) {
		if f.GETs.Load() == 0 {
			t.Fatal("opened persistence before complete source verification")
		}
		if max <= 0 {
			t.Fatal("invalid configured database pool bound")
		}
		opened = true
		return store, nil
	})
	if err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	if !opened || store.applies != 1 || store.syncs != 0 || !store.closed {
		t.Fatalf("unexpected persistence calls: opened=%v apply=%d sync=%d close=%v", opened, store.applies, store.syncs, store.closed)
	}
	for _, secret := range []string{"customer-42", "invoice-42", "synthetic-ninja-token", "synthetic-panel-token", f.server.URL, "private-subpath"} {
		if strings.Contains(out.String(), secret) {
			t.Errorf("output disclosed %q", secret)
		}
	}
}

type applyTestStore struct {
	t                 *testing.T
	syncs, applies    int
	closed            bool
	prepared          importadmission.PreparedPlan
	events            []string
	syncArgs          []any
	syncErr, applyErr error
}

func (s *applyTestStore) SyncImportAdmissionPolicy(_ context.Context, botID, revision int64, policyHash, scopeHash string, enabled bool) error {
	s.syncs++
	s.events = append(s.events, "sync")
	s.syncArgs = []any{botID, revision, policyHash, scopeHash, enabled}
	if s.syncErr != nil {
		return s.syncErr
	}
	if botID <= 0 || revision <= 0 || policyHash == "" || scopeHash == "" || !enabled {
		s.t.Fatal("invalid admission policy sync")
	}
	return nil
}
func (s *applyTestStore) ApplyVerifiedImport(_ context.Context, p importadmission.PreparedPlan) (postgres.ImportAdmissionResult, error) {
	s.applies++
	s.events = append(s.events, "apply")
	if s.applyErr != nil {
		return postgres.ImportAdmissionResult{}, s.applyErr
	}
	if err := p.Validate(); err != nil {
		s.t.Fatalf("caller supplied non-prepared plan: %v", err)
	}
	if p.BotID() == 0 || p.Revision() <= 0 || p.RowCount() != 1 || p.NotAfter().IsZero() {
		s.t.Fatal("incomplete fresh prepared plan")
	}
	s.prepared = p
	return postgres.ImportAdmissionResult{Admitted: 1}, nil
}
func (s *applyTestStore) Close() { s.closed = true; s.events = append(s.events, "close") }

func TestImportApplyEnableIsExplicitAndSyncsExactPreparedPolicyFirst(t *testing.T) {
	f := newImportApplyFixture(t)
	store := &applyTestStore{t: t}
	args := []string{"--file", f.manifestPath, "--expected-hash", f.planHash, "--admission-revision", "7", "--apply", "--enable-admission"}
	err := importApplyWith(args, &bytes.Buffer{}, func(k string) string { return f.env[k] }, newVerifyProviders, func() time.Time { return f.now }, func(_ context.Context, _ string, _ int32) (importApplyStore, error) { return store, nil })
	if err != nil {
		t.Fatalf("enabled apply: %v", err)
	}
	if len(store.events) != 3 || store.events[0] != "sync" || store.events[1] != "apply" || store.events[2] != "close" {
		t.Fatalf("operation order: %v", store.events)
	}
	if len(store.syncArgs) != 5 || store.syncArgs[0] != store.prepared.BotID() || store.syncArgs[1] != int64(7) || store.syncArgs[2] != store.prepared.PolicyHash() || store.syncArgs[3] != store.prepared.SourceScopeHash() || store.syncArgs[4] != true {
		t.Fatalf("sync did not use prepared policy: %#v", store.syncArgs)
	}
}

func TestImportApplyErrorsAreRedactedAndAlwaysClose(t *testing.T) {
	secret := "synthetic-secret-marker"
	f := newImportApplyFixture(t)
	args := []string{"--file", f.manifestPath, "--expected-hash", f.planHash, "--admission-revision", "1", "--apply", "--enable-admission"}
	store := &applyTestStore{t: t, syncErr: errors.New(secret)}
	out := &bytes.Buffer{}
	err := importApplyWith(args, out, func(k string) string { return f.env[k] }, newVerifyProviders, func() time.Time { return f.now }, func(context.Context, string, int32) (importApplyStore, error) { return store, nil })
	assertApplyFailureRedacted(t, err, out.String(), secret)
	if !store.closed || store.applies != 0 || strings.Join(store.events, ",") != "sync,close" {
		t.Fatalf("sync failure cleanup/order: %#v", store)
	}

	store = &applyTestStore{t: t, applyErr: errors.New(secret)}
	out.Reset()
	err = importApplyWith(args, out, func(k string) string { return f.env[k] }, newVerifyProviders, func() time.Time { return f.now }, func(context.Context, string, int32) (importApplyStore, error) { return store, nil })
	assertApplyFailureRedacted(t, err, out.String(), secret)
	if !store.closed || store.applies != 1 || strings.Join(store.events, ",") != "sync,apply,close" {
		t.Fatalf("apply failure cleanup/order: %#v", store)
	}

	out.Reset()
	err = importApplyWith(args, out, func(k string) string { return f.env[k] }, newVerifyProviders, func() time.Time { return f.now }, func(context.Context, string, int32) (importApplyStore, error) { return nil, errors.New(secret) })
	assertApplyFailureRedacted(t, err, out.String(), secret)
}

func TestImportApplyOutputFailureOccursAfterApplyAndCloses(t *testing.T) {
	f := newImportApplyFixture(t)
	store := &applyTestStore{t: t}
	args := []string{"--file", f.manifestPath, "--expected-hash", f.planHash, "--admission-revision", "1", "--apply"}
	err := importApplyWith(args, failApplyWriter{}, func(k string) string { return f.env[k] }, newVerifyProviders, func() time.Time { return f.now }, func(context.Context, string, int32) (importApplyStore, error) { return store, nil })
	if err == nil || err.Error() != "import apply result unavailable" {
		t.Fatalf("unexpected output error: %v", err)
	}
	if store.applies != 1 || !store.closed || strings.Join(store.events, ",") != "apply,close" {
		t.Fatalf("output failure lifecycle: %#v", store)
	}
}

type failApplyWriter struct{}

func (failApplyWriter) Write([]byte) (int, error) { return 0, errors.New("synthetic output failure") }
func assertApplyFailureRedacted(t *testing.T, err error, output, secret string) {
	t.Helper()
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(output, secret) {
		t.Fatalf("failure not redacted: err=%v output=%q", err, output)
	}
	for _, private := range []string{"customer-42", "invoice-42", "synthetic-ninja-token", "synthetic-panel-token"} {
		if strings.Contains(err.Error()+output, private) {
			t.Fatalf("private value leaked: %q", private)
		}
	}
}

func TestImportApplyBadConfigurationTimeAndSourceNeverOpenDatabase(t *testing.T) {
	for _, tc := range []struct {
		name, key, value string
		zeroClock        bool
		factoryErr       bool
		wantGets         bool
	}{
		{"timezone", "TZ", "/missing-zone", false, false, false},
		{"pool-zero", "DB_MAX_CONNS", "0", false, false, false}, {"pool-too-large", "DB_MAX_CONNS", "65", false, false, false}, {"pool-noncanonical", "DB_MAX_CONNS", "01", false, false, false},
		{"verify-config", "SUI_INBOUND_IDS", "0", false, false, false}, {"factory", "", "", false, true, false}, {"zero-clock", "", "", true, false, false},
		{"source-503", "", "", false, false, true}, {"unpaid-source", "", "", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newImportApplyFixture(t)
			if tc.key != "" {
				f.env[tc.key] = tc.value
			}
			if tc.name == "source-503" {
				f.fault.Store(1)
			}
			if tc.name == "unpaid-source" {
				f.fault.Store(2)
			}
			if tc.zeroClock {
				f.now = time.Time{}
			}
			if tc.wantGets && tc.name == "source-503" {
				f.fault.Store(1)
			}
			calls := 0
			getenv := func(k string) string { return f.env[k] }
			factory := verifyFactory(newVerifyProviders)
			if tc.factoryErr {
				factory = func(verifyConfig) (importverify.Providers, error) {
					calls++
					return importverify.Providers{}, errors.New("synthetic factory")
				}
			}
			err := importApplyWith([]string{"--file", f.manifestPath, "--expected-hash", f.planHash, "--admission-revision", "1", "--apply"}, &bytes.Buffer{}, getenv, factory, func() time.Time { return f.now }, func(context.Context, string, int32) (importApplyStore, error) {
				t.Fatal("opened DB after invalid config/time/source")
				return nil, nil
			})
			if err == nil {
				t.Fatal("invalid configuration/time/source accepted")
			}
			if f.GETs.Load() > 0 && !tc.wantGets {
				t.Fatalf("source GET happened too early: %d", f.GETs.Load())
			}
			if tc.wantGets && f.GETs.Load() == 0 {
				t.Fatal("source failure was not exercised")
			}
			if tc.factoryErr && calls != 1 {
				t.Fatalf("factory calls=%d", calls)
			}
		})
	}
}

func TestImportApplyRejectsNonPrivateManifestModeAndEvidenceFlagBeforeEnv(t *testing.T) {
	f := newImportApplyFixture(t)
	if err := os.Chmod(f.manifestPath, 0644); err != nil {
		t.Fatal(err)
	}
	getenv, factory, open := 0, 0, 0
	args := [][]string{{"--file", f.manifestPath, "--expected-hash", f.planHash, "--admission-revision", "1", "--apply"}, {"--file", f.manifestPath, "--expected-hash", f.planHash, "--admission-revision", "1", "--apply", "--evidence-file", "x"}}
	for _, a := range args {
		if err := importApplyWith(a, &bytes.Buffer{}, func(string) string { getenv++; return "" }, func(verifyConfig) (importverify.Providers, error) { factory++; return importverify.Providers{}, nil }, func() time.Time { return f.now }, func(context.Context, string, int32) (importApplyStore, error) { open++; return nil, nil }); err == nil {
			t.Errorf("accepted %q", a)
		}
	}
	if getenv != 0 || factory != 0 || open != 0 {
		t.Fatalf("rejected request reached capabilities env=%d factory=%d open=%d", getenv, factory, open)
	}
}

func TestImportApplyMainDispatchMalformedDoesNotReachStartup(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := run(logger, []string{"import-apply", "--apply"}); err == nil || strings.Contains(err.Error(), "DATABASE_URL") || strings.Contains(err.Error(), "TELEGRAM") {
		t.Fatalf("bad CLI fell through to service startup: %v", err)
	}
}
