package main

import (
	"context"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/lifecycle"
)

func providerConfigs(base, ninjaToken, suiBase, suiToken string) (lifecycle.EnforcementConfig, lifecycle.RenewalConfig) {
	ids := []int64{42}
	enforcement := lifecycle.EnforcementConfig{
		Enabled: true, ExclusiveWriter: true, BaseURL: base, Token: ninjaToken,
		SUIBaseURL: suiBase, SUIToken: suiToken, SUIVersion: "1.6.4",
		AllowedTelegramIDs: ids, Workers: 1, MaxAccounts: 1000,
		PollInterval: time.Second, HTTPTimeout: 5 * time.Second,
	}
	renewal := lifecycle.RenewalConfig{
		Enabled: true, ExclusiveWriter: true, BaseURL: base, Token: ninjaToken,
		InvoiceNinjaVersion: "v5.13.47", SUIBaseURL: suiBase, SUIToken: suiToken, SUIVersion: "1.6.4",
		AllowedTelegramIDs: ids, Workers: 1, MaxAccounts: 1000,
		PollInterval: time.Second, HTTPTimeout: 5 * time.Second,
	}
	return enforcement, renewal
}

func runtimeConfigForFixture(t *testing.T, f *importApplyFixture) importedRuntimeConfig {
	t.Helper()
	f.env["IMPORTED_RUNTIME_ENABLED"], f.env["IMPORTED_RUNTIME_REVISION"] = "true", "1"
	cfg, err := loadImportedRuntimeConfig(func(k string) string { return f.env[k] }, true, true)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestNewImportedBillingProvidersDisabledAndIndependent(t *testing.T) {
	f := newImportApplyFixture(t)
	invalidEnforcement, invalidRenewal := lifecycle.EnforcementConfig{Enabled: true}, lifecycle.RenewalConfig{Enabled: true}
	providers, err := newImportedBillingProviders(importedRuntimeConfig{}, invalidEnforcement, invalidRenewal)
	if err != nil || providers.Enforcement != nil || providers.Renewal != nil || f.GETs.Load() != 0 {
		t.Fatalf("disabled runtime used native configuration/provider: providers=%+v gets=%d err=%v", providers, f.GETs.Load(), err)
	}

	runtime := runtimeConfigForFixture(t, f)
	enforcement, renewal := providerConfigs(f.env["INVOICE_NINJA_BASE_URL"], f.env["INVOICE_NINJA_API_TOKEN"], f.env["SUI_BASE_URL"], f.env["SUI_API_TOKEN"])
	for _, tc := range []struct {
		name        string
		enforcement lifecycle.EnforcementConfig
		renewal     lifecycle.RenewalConfig
		wantE       bool
		wantR       bool
	}{
		{name: "none", enforcement: lifecycle.EnforcementConfig{}, renewal: lifecycle.RenewalConfig{}},
		{name: "enforcement", enforcement: enforcement, renewal: lifecycle.RenewalConfig{}, wantE: true},
		{name: "renewal", enforcement: lifecycle.EnforcementConfig{}, renewal: renewal, wantR: true},
		{name: "both", enforcement: enforcement, renewal: renewal, wantE: true, wantR: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := f.GETs.Load()
			got, err := newImportedBillingProviders(runtime, tc.enforcement, tc.renewal)
			if err != nil || (got.Enforcement != nil) != tc.wantE || (got.Renewal != nil) != tc.wantR {
				t.Fatalf("providers=%+v err=%v", got, err)
			}
			if f.GETs.Load() != before {
				t.Fatal("provider construction performed source HTTP")
			}
		})
	}
}

func TestNewImportedBillingProvidersConstructsScopedNativeClientsWithoutHTTP(t *testing.T) {
	f := newImportApplyFixture(t)
	runtime := runtimeConfigForFixture(t, f)
	enforcement, renewal := providerConfigs(f.env["INVOICE_NINJA_BASE_URL"], f.env["INVOICE_NINJA_API_TOKEN"], f.env["SUI_BASE_URL"], f.env["SUI_API_TOKEN"])
	providers, err := newImportedBillingProviders(runtime, enforcement, renewal)
	if err != nil {
		t.Fatal(err)
	}
	if f.GETs.Load() != 0 {
		t.Fatal("provider construction performed source HTTP")
	}
	if _, err = providers.Enforcement.Get(context.Background(), "recurring-42"); err != nil {
		t.Fatalf("enforcement scoped GET: %v", err)
	}
	if f.GETs.Load() != 1 {
		t.Fatalf("expected one native GET, got %d", f.GETs.Load())
	}
	if _, err = providers.Renewal.Get(context.Background(), "recurring-42"); err != nil {
		t.Fatalf("renewal scoped GET: %v", err)
	}
	if f.GETs.Load() != 2 {
		t.Fatalf("expected two native GETs, got %d", f.GETs.Load())
	}
}

func TestNewImportedBillingProvidersRejectsInvalidConfigAndScopeBeforeHTTP(t *testing.T) {
	f := newImportApplyFixture(t)
	runtime := runtimeConfigForFixture(t, f)
	base, token := f.env["INVOICE_NINJA_BASE_URL"], f.env["INVOICE_NINJA_API_TOKEN"]
	panel, panelToken := f.env["SUI_BASE_URL"], f.env["SUI_API_TOKEN"]
	validE, validR := providerConfigs(base, token, panel, panelToken)
	cases := []struct {
		name string
		e    lifecycle.EnforcementConfig
		r    lifecycle.RenewalConfig
	}{
		{name: "token", e: func() lifecycle.EnforcementConfig { c := validE; c.Token = "wrong-secret-marker"; return c }()},
		{name: "base", e: func() lifecycle.EnforcementConfig { c := validE; c.BaseURL += "/other"; return c }()},
		{name: "port", e: func() lifecycle.EnforcementConfig {
			c := validE
			u, _ := url.Parse(c.BaseURL)
			u.Host = net.JoinHostPort(u.Hostname(), "1")
			c.BaseURL = u.String()
			return c
		}()},
		{name: "panel-version", r: func() lifecycle.RenewalConfig { c := validR; c.SUIVersion = "1.6.3"; return c }()},
		{name: "scope-hash", r: func() lifecycle.RenewalConfig { c := validR; c.SUIToken = "different-secret-marker"; return c }()},
		{name: "exclusive-writer", e: func() lifecycle.EnforcementConfig { c := validE; c.ExclusiveWriter = false; return c }()},
		{name: "pilot-required", r: func() lifecycle.RenewalConfig { c := validR; c.AllowedTelegramIDs = nil; return c }()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, r := tc.e, tc.r
			if !e.Enabled && !r.Enabled {
				e = validE
			}
			before := f.GETs.Load()
			_, err := newImportedBillingProviders(runtime, e, r)
			if err == nil || strings.Contains(err.Error(), "secret-marker") || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), base) {
				t.Fatalf("expected redacted provider error, got %v", err)
			}
			if f.GETs.Load() != before {
				t.Fatal("invalid source configuration performed HTTP")
			}
		})
	}
}
