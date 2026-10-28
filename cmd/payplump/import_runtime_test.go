package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type runtimeStoreFake struct {
	events []string
	fail   string
	args   []any
}

func (f *runtimeStoreFake) DisableImportedEffectPermissions(context.Context) error {
	f.events = append(f.events, "effects")
	if f.fail == "effects" {
		return errors.New("secret marker")
	}
	return nil
}
func (f *runtimeStoreFake) DisableImportedRuntimePermissions(context.Context) error {
	f.events = append(f.events, "runtime")
	if f.fail == "runtime" {
		return errors.New("secret marker")
	}
	return nil
}
func (f *runtimeStoreFake) SyncImportedEffectPermission(_ context.Context, bot, rev int64, policy, scope string, enabled bool) error {
	f.events = append(f.events, "effect-sync")
	f.args = []any{bot, rev, policy, scope, enabled}
	if f.fail == "effect-sync" {
		return errors.New("secret marker")
	}
	return nil
}
func (f *runtimeStoreFake) SyncImportedRuntimePermission(_ context.Context, bot, rev int64, policy, scope string, enabled bool) error {
	f.events = append(f.events, "sync")
	f.args = []any{bot, rev, policy, scope, enabled}
	if f.fail == "sync" {
		return errors.New("secret marker")
	}
	return nil
}

func TestLoadImportedRuntimeConfigDisabledIgnoresPolicyEnvironment(t *testing.T) {
	for _, flag := range []string{"", "false"} {
		t.Run("flag="+flag, func(t *testing.T) {
			reads := []string{}
			cfg, err := loadImportedRuntimeConfig(func(k string) string {
				reads = append(reads, k)
				if k == "IMPORTED_RUNTIME_ENABLED" {
					return flag
				}
				if k == "IMPORTED_EFFECTS_ENABLED" {
					return "false"
				}
				return "INVALID SECRET"
			}, false, false)
			if err != nil || cfg.Enabled || !reflect.DeepEqual(reads, []string{"IMPORTED_RUNTIME_ENABLED", "IMPORTED_EFFECTS_ENABLED"}) {
				t.Fatalf("cfg=%+v reads=%v err=%v", cfg, reads, err)
			}
		})
	}
}

func TestLoadImportedRuntimeEffectsFlagRequiresRuntimeAndRevision(t *testing.T) {
	for _, tc := range []struct {
		name, runtime, effects, effectsRevision string
		wantErr                                 bool
		wantReads                               []string
	}{
		{"both off ignores unused revision", "false", "false", "INVALID", false, []string{"IMPORTED_RUNTIME_ENABLED", "IMPORTED_EFFECTS_ENABLED"}},
		{"effects without runtime", "false", "true", "", true, []string{"IMPORTED_RUNTIME_ENABLED", "IMPORTED_EFFECTS_ENABLED"}},
		{"invalid effects uppercase", "true", "TRUE", "8", true, []string{"IMPORTED_RUNTIME_ENABLED", "IMPORTED_EFFECTS_ENABLED"}},
		{"invalid effects titlecase", "true", "False", "8", true, []string{"IMPORTED_RUNTIME_ENABLED", "IMPORTED_EFFECTS_ENABLED"}},
		{"invalid effects whitespace", "true", " true", "8", true, []string{"IMPORTED_RUNTIME_ENABLED", "IMPORTED_EFFECTS_ENABLED"}},
		{"invalid effects numeric", "true", "0", "8", true, []string{"IMPORTED_RUNTIME_ENABLED", "IMPORTED_EFFECTS_ENABLED"}},
		{"effects missing revision", "true", "true", "", true, []string{"IMPORTED_RUNTIME_ENABLED", "IMPORTED_EFFECTS_ENABLED", "IMPORTED_RUNTIME_REVISION", "IMPORTED_EFFECTS_REVISION"}},
		{"effects zero revision", "true", "true", "0", true, []string{"IMPORTED_RUNTIME_ENABLED", "IMPORTED_EFFECTS_ENABLED", "IMPORTED_RUNTIME_REVISION", "IMPORTED_EFFECTS_REVISION"}},
		{"effects leading zero", "true", "true", "01", true, []string{"IMPORTED_RUNTIME_ENABLED", "IMPORTED_EFFECTS_ENABLED", "IMPORTED_RUNTIME_REVISION", "IMPORTED_EFFECTS_REVISION"}},
		{"effects plus sign", "true", "true", "+1", true, []string{"IMPORTED_RUNTIME_ENABLED", "IMPORTED_EFFECTS_ENABLED", "IMPORTED_RUNTIME_REVISION", "IMPORTED_EFFECTS_REVISION"}},
		{"effects negative", "true", "true", "-1", true, []string{"IMPORTED_RUNTIME_ENABLED", "IMPORTED_EFFECTS_ENABLED", "IMPORTED_RUNTIME_REVISION", "IMPORTED_EFFECTS_REVISION"}},
		{"effects whitespace", "true", "true", " 1", true, []string{"IMPORTED_RUNTIME_ENABLED", "IMPORTED_EFFECTS_ENABLED", "IMPORTED_RUNTIME_REVISION", "IMPORTED_EFFECTS_REVISION"}},
		{"effects overflow", "true", "true", "9223372036854775808", true, []string{"IMPORTED_RUNTIME_ENABLED", "IMPORTED_EFFECTS_ENABLED", "IMPORTED_RUNTIME_REVISION", "IMPORTED_EFFECTS_REVISION"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := []string{}
			_, err := loadImportedRuntimeConfig(func(k string) string {
				reads = append(reads, k)
				switch k {
				case "IMPORTED_RUNTIME_ENABLED":
					return tc.runtime
				case "IMPORTED_RUNTIME_REVISION":
					return "7"
				case "IMPORTED_EFFECTS_ENABLED":
					return tc.effects
				case "IMPORTED_EFFECTS_REVISION":
					return tc.effectsRevision
				default:
					return "unused-secret"
				}
			}, true, true)
			if (err != nil) != tc.wantErr || (err != nil && strings.Contains(err.Error(), "unused-secret")) || !reflect.DeepEqual(reads, tc.wantReads) {
				t.Fatalf("reads=%v err=%v want reads=%v", reads, err, tc.wantReads)
			}
		})
	}
}

func TestLoadImportedRuntimeConfigEnabledUsesValidatedPolicyWithoutHTTP(t *testing.T) {
	f := newImportApplyFixture(t)
	env := make(map[string]string, len(f.env)+2)
	for k, v := range f.env {
		env[k] = v
	}
	env["IMPORTED_RUNTIME_ENABLED"], env["IMPORTED_RUNTIME_REVISION"] = "true", "7"
	get := func(k string) string { return env[k] }
	wantPolicy, err := loadVerifyConfig(get)
	if err != nil {
		t.Fatal(err)
	}
	wantHash, err := wantPolicy.policy.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := loadImportedRuntimeConfig(get, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg != (importedRuntimeConfig{Enabled: true, Revision: 7, BotID: 9, PolicyHash: wantHash, SourceScopeHash: wantPolicy.policy.SourceScopeHash}) {
		t.Fatalf("unexpected runtime config: %+v", cfg)
	}
	if strings.Contains(fmt.Sprintf("%+v", cfg), "synthetic") {
		t.Fatal("runtime config exposes source credentials")
	}
	if got := f.GETs.Load(); got != 0 {
		t.Fatalf("loader made %d source GETs", got)
	}
	env["IMPORTED_EFFECTS_ENABLED"], env["IMPORTED_EFFECTS_REVISION"] = "true", "8"
	effectsCfg, err := loadImportedRuntimeConfig(get, true, true)
	if err != nil || !effectsCfg.EffectsEnabled || effectsCfg.EffectsRevision != 8 || effectsCfg.PolicyHash != cfg.PolicyHash || effectsCfg.SourceScopeHash != cfg.SourceScopeHash {
		t.Fatalf("effects config=%+v err=%v", effectsCfg, err)
	}
	store := &runtimeStoreFake{}
	if err = fenceImportedRuntimeStartup(context.Background(), store, cfg); err != nil {
		t.Fatal(err)
	}
	if err = installImportedRuntimePermission(context.Background(), store, cfg.BotID, cfg); err != nil {
		t.Fatal(err)
	}
	if got := f.GETs.Load(); got != 0 {
		t.Fatalf("startup permission wiring made %d source GETs", got)
	}
	if !reflect.DeepEqual(store.events, []string{"effects", "sync"}) || !reflect.DeepEqual(store.args, []any{int64(9), int64(7), wantHash, wantPolicy.policy.SourceScopeHash, true}) {
		t.Fatalf("events=%v args=%v", store.events, store.args)
	}
}

func TestLoadImportedRuntimeConfigRejectsRevisionAndDependencyBeforePolicyRead(t *testing.T) {
	for _, revision := range []string{"", "0", "01", "+1", "-1", " 1", "9223372036854775808"} {
		t.Run("revision="+revision, func(t *testing.T) {
			reads := []string{}
			_, err := loadImportedRuntimeConfig(func(k string) string {
				reads = append(reads, k)
				if k == "IMPORTED_RUNTIME_ENABLED" {
					return "true"
				}
				if k == "IMPORTED_EFFECTS_ENABLED" {
					return "false"
				}
				if k == "IMPORTED_RUNTIME_REVISION" {
					return revision
				}
				return "SECRET"
			}, true, true)
			if err == nil || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("expected redacted error, got %v", err)
			}
			if !reflect.DeepEqual(reads, []string{"IMPORTED_RUNTIME_ENABLED", "IMPORTED_EFFECTS_ENABLED", "IMPORTED_RUNTIME_REVISION"}) {
				t.Fatalf("unexpected reads before revision rejection: %v", reads)
			}
		})
	}
	for _, deps := range []struct {
		name               string
		telegram, payments bool
	}{{"telegram-disabled", false, true}, {"payments-disabled", true, false}} {
		t.Run(deps.name, func(t *testing.T) {
			reads := []string{}
			_, err := loadImportedRuntimeConfig(func(k string) string {
				reads = append(reads, k)
				if k == "IMPORTED_RUNTIME_ENABLED" {
					return "true"
				}
				if k == "IMPORTED_EFFECTS_ENABLED" {
					return "false"
				}
				if k == "IMPORTED_RUNTIME_REVISION" {
					return "7"
				}
				return "SECRET"
			}, deps.telegram, deps.payments)
			if err == nil || strings.Contains(err.Error(), "SECRET") || !reflect.DeepEqual(reads, []string{"IMPORTED_RUNTIME_ENABLED", "IMPORTED_EFFECTS_ENABLED"}) {
				t.Fatalf("reads=%v err=%v", reads, err)
			}
		})
	}
}

func TestLoadImportedRuntimeConfigRejectsInvalidSourceEnvironmentWithoutHTTP(t *testing.T) {
	for _, tc := range []struct{ name, key, value string }{{"timezone", "TZ", "Invalid/SecretMarker"}, {"currency", "PAYMENTS_CURRENCY_ID", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newImportApplyFixture(t)
			env := make(map[string]string, len(f.env)+2)
			for k, v := range f.env {
				env[k] = v
			}
			env[tc.key] = tc.value
			env["IMPORTED_RUNTIME_ENABLED"], env["IMPORTED_RUNTIME_REVISION"] = "true", "7"
			_, err := loadImportedRuntimeConfig(func(k string) string { return env[k] }, true, true)
			if err == nil || strings.Contains(err.Error(), "SecretMarker") || (tc.value != "" && strings.Contains(err.Error(), tc.value)) {
				t.Fatalf("expected redacted source-policy error, got %v", err)
			}
			if got := f.GETs.Load(); got != 0 {
				t.Fatalf("invalid loader made %d source GETs", got)
			}
		})
	}
}

func TestLoadImportedRuntimeConfigRejectsNoncanonicalEnablement(t *testing.T) {
	for _, value := range []string{"False", " true", "TRUE", "0"} {
		t.Run(value, func(t *testing.T) {
			_, err := loadImportedRuntimeConfig(func(k string) string {
				if k == "IMPORTED_RUNTIME_ENABLED" {
					return value
				}
				return ""
			}, true, true)
			if err == nil || strings.Contains(err.Error(), value) {
				t.Fatalf("expected redacted error, got %v", err)
			}
		})
	}
}

func TestFenceImportedRuntimeStartupOrderAndFailureStops(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     importedRuntimeConfig
		failure string
		want    []string
		wantErr bool
	}{{"disabled", importedRuntimeConfig{}, "", []string{"effects", "runtime"}, false}, {"enabled", importedRuntimeConfig{Enabled: true}, "", []string{"effects"}, false}, {"effects enabled", importedRuntimeConfig{Enabled: true, EffectsEnabled: true, EffectsRevision: 8, BotID: 9, PolicyHash: "p", SourceScopeHash: "s"}, "", nil, false}, {"invalid effects", importedRuntimeConfig{EffectsEnabled: true}, "", nil, true}, {"effect failure", importedRuntimeConfig{}, "effects", []string{"effects"}, true}, {"runtime failure", importedRuntimeConfig{}, "runtime", []string{"effects", "runtime"}, true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := &runtimeStoreFake{fail: tc.failure}
			err := fenceImportedRuntimeStartup(context.Background(), f, tc.cfg)
			if (err != nil) != tc.wantErr || !reflect.DeepEqual(f.events, tc.want) {
				t.Fatalf("events=%v err=%v", f.events, err)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatal("error leaked")
			}
		})
	}
}

func TestInstallImportedEffectPermissionIsExplicitAndBound(t *testing.T) {
	ctx := context.Background()
	fixture := newImportApplyFixture(t)
	env := make(map[string]string, len(fixture.env)+4)
	for k, v := range fixture.env {
		env[k] = v
	}
	env["IMPORTED_RUNTIME_ENABLED"], env["IMPORTED_RUNTIME_REVISION"] = "true", "7"
	get := func(k string) string { return env[k] }
	readCfg, err := loadImportedRuntimeConfig(get, true, true)
	if err != nil {
		t.Fatal(err)
	}
	env["IMPORTED_EFFECTS_ENABLED"], env["IMPORTED_EFFECTS_REVISION"] = "true", "8"
	cfg, err := loadImportedRuntimeConfig(get, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PolicyHash != readCfg.PolicyHash || cfg.SourceScopeHash != readCfg.SourceScopeHash {
		t.Fatal("effects flag or revision changed the verified policy fingerprints")
	}

	f := &runtimeStoreFake{}
	if err := installImportedEffectPermission(ctx, f, cfg.BotID-1, cfg); err == nil || len(f.events) != 0 {
		t.Fatalf("mismatch events=%v err=%v", f.events, err)
	}
	if err := installImportedEffectPermission(ctx, f, cfg.BotID, cfg); err != nil || !reflect.DeepEqual(f.events, []string{"effect-sync"}) || !reflect.DeepEqual(f.args, []any{cfg.BotID, cfg.EffectsRevision, cfg.PolicyHash, cfg.SourceScopeHash, true}) {
		t.Fatalf("events=%v args=%v err=%v", f.events, f.args, err)
	}

	f.events, f.args, f.fail = nil, nil, "effect-sync"
	if err := installImportedEffectPermission(ctx, f, cfg.BotID, cfg); err == nil || strings.Contains(err.Error(), "secret marker") || !reflect.DeepEqual(f.events, []string{"effect-sync"}) {
		t.Fatalf("sync failure events=%v err=%v", f.events, err)
	}

	f.events, f.args, f.fail = nil, nil, ""
	invalid := []struct {
		name string
		edit func(*importedRuntimeConfig)
	}{
		{"runtime disabled", func(c *importedRuntimeConfig) { c.Enabled = false }},
		{"revision zero", func(c *importedRuntimeConfig) { c.EffectsRevision = 0 }},
		{"bot zero", func(c *importedRuntimeConfig) { c.BotID = 0 }},
		{"policy hash empty", func(c *importedRuntimeConfig) { c.PolicyHash = " " }},
		{"scope hash empty", func(c *importedRuntimeConfig) { c.SourceScopeHash = " " }},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			bad := cfg
			tc.edit(&bad)
			if err := installImportedEffectPermission(ctx, f, cfg.BotID, bad); err == nil || len(f.events) != 0 {
				t.Fatalf("events=%v err=%v", f.events, err)
			}
		})
	}
	cfg.EffectsEnabled = false
	if err := installImportedEffectPermission(ctx, f, 0, cfg); err != nil || len(f.events) != 0 {
		t.Fatalf("disabled events=%v err=%v", f.events, err)
	}
	if got := fixture.GETs.Load(); got != 0 {
		t.Fatalf("loader or permission install made %d source GETs", got)
	}
}

func TestInstallImportedRuntimePermissionRequiresMatchingBotAndUsesRealPolicyHashes(t *testing.T) {
	ctx := context.Background()
	fixture := newImportApplyFixture(t)
	env := make(map[string]string, len(fixture.env)+2)
	for k, v := range fixture.env {
		env[k] = v
	}
	env["IMPORTED_RUNTIME_ENABLED"], env["IMPORTED_RUNTIME_REVISION"] = "true", "7"
	cfg, err := loadImportedRuntimeConfig(func(k string) string { return env[k] }, true, true)
	if err != nil {
		t.Fatal(err)
	}
	f := &runtimeStoreFake{}
	if err := installImportedRuntimePermission(ctx, f, cfg.BotID-1, cfg); err == nil || len(f.events) != 0 {
		t.Fatalf("mismatch events=%v err=%v", f.events, err)
	}
	if err := installImportedRuntimePermission(ctx, f, cfg.BotID, cfg); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.events, []string{"sync"}) || !reflect.DeepEqual(f.args, []any{cfg.BotID, cfg.Revision, cfg.PolicyHash, cfg.SourceScopeHash, true}) {
		t.Fatalf("events=%v args=%v", f.events, f.args)
	}
	f.events = nil
	f.fail = "sync"
	if err := installImportedRuntimePermission(ctx, f, cfg.BotID, cfg); err == nil || strings.Contains(err.Error(), "secret marker") || !reflect.DeepEqual(f.events, []string{"sync"}) {
		t.Fatalf("sync failure events=%v err=%v", f.events, err)
	}
	f.events = nil
	if err := installImportedRuntimePermission(ctx, f, 0, importedRuntimeConfig{}); err != nil || len(f.events) != 0 {
		t.Fatalf("disabled events=%v err=%v", f.events, err)
	}
	if got := fixture.GETs.Load(); got != 0 {
		t.Fatalf("permission install made %d source GETs", got)
	}
}
