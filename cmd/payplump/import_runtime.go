package main

import (
	"context"
	"errors"
	"strings"
)

type importedRuntimeConfig struct {
	Enabled                     bool
	Revision                    int64
	EffectsEnabled              bool
	EffectsRevision             int64
	BotID                       int64
	PolicyHash, SourceScopeHash string
}

type importedRuntimeStartupStore interface {
	DisableImportedRuntimePermissions(context.Context) error
	DisableImportedEffectPermissions(context.Context) error
	SyncImportedRuntimePermission(context.Context, int64, int64, string, string, bool) error
}

func loadImportedRuntimeConfig(get verifyEnvironment, telegramEnabled, paymentsEnabled bool) (importedRuntimeConfig, error) {
	rawEnabled := get("IMPORTED_RUNTIME_ENABLED")
	rawEffectsEnabled := get("IMPORTED_EFFECTS_ENABLED")
	if rawEnabled != "" && rawEnabled != "false" && rawEnabled != "true" {
		return importedRuntimeConfig{}, errors.New("invalid imported runtime configuration")
	}
	if rawEffectsEnabled != "" && rawEffectsEnabled != "false" && rawEffectsEnabled != "true" {
		return importedRuntimeConfig{}, errors.New("invalid imported runtime configuration")
	}
	runtimeEnabled, effectsEnabled := rawEnabled == "true", rawEffectsEnabled == "true"
	if effectsEnabled && !runtimeEnabled {
		return importedRuntimeConfig{}, errors.New("invalid imported runtime configuration")
	}
	if !runtimeEnabled {
		return importedRuntimeConfig{}, nil
	}
	if !telegramEnabled || !paymentsEnabled {
		return importedRuntimeConfig{}, errors.New("invalid imported runtime configuration")
	}
	revision, err := canonicalID(get("IMPORTED_RUNTIME_REVISION"))
	if err != nil {
		return importedRuntimeConfig{}, errors.New("invalid imported runtime configuration")
	}
	var effectsRevision int64
	if effectsEnabled {
		effectsRevision, err = canonicalID(get("IMPORTED_EFFECTS_REVISION"))
		if err != nil {
			return importedRuntimeConfig{}, errors.New("invalid imported runtime configuration")
		}
	}
	verification, err := loadVerifyConfig(get)
	if err != nil {
		return importedRuntimeConfig{}, errors.New("invalid imported runtime configuration")
	}
	policyHash, err := verification.policy.Fingerprint()
	if err != nil {
		return importedRuntimeConfig{}, errors.New("invalid imported runtime configuration")
	}
	return importedRuntimeConfig{Enabled: true, Revision: revision, EffectsEnabled: effectsEnabled, EffectsRevision: effectsRevision, BotID: verification.policy.BotID, PolicyHash: policyHash, SourceScopeHash: verification.policy.SourceScopeHash}, nil
}

func fenceImportedRuntimeStartup(ctx context.Context, store importedRuntimeStartupStore, cfg importedRuntimeConfig) error {
	if cfg.EffectsEnabled && (!cfg.Enabled || cfg.EffectsRevision <= 0 || cfg.BotID <= 0 || strings.TrimSpace(cfg.PolicyHash) == "" || strings.TrimSpace(cfg.SourceScopeHash) == "") {
		return errors.New("invalid imported runtime configuration")
	}
	if !cfg.EffectsEnabled {
		if err := store.DisableImportedEffectPermissions(ctx); err != nil {
			return errors.New("imported runtime startup fence failed")
		}
	}
	if !cfg.Enabled {
		if err := store.DisableImportedRuntimePermissions(ctx); err != nil {
			return errors.New("imported runtime startup fence failed")
		}
	}
	return nil
}

type importedEffectStartupStore interface {
	SyncImportedEffectPermission(context.Context, int64, int64, string, string, bool) error
}

func installImportedEffectPermission(ctx context.Context, store importedEffectStartupStore, verifiedBotID int64, cfg importedRuntimeConfig) error {
	if !cfg.EffectsEnabled {
		return nil
	}
	if !cfg.Enabled || cfg.EffectsRevision <= 0 || verifiedBotID <= 0 || verifiedBotID != cfg.BotID || strings.TrimSpace(cfg.PolicyHash) == "" || strings.TrimSpace(cfg.SourceScopeHash) == "" {
		return errors.New("invalid imported effects configuration")
	}
	if err := store.SyncImportedEffectPermission(ctx, cfg.BotID, cfg.EffectsRevision, cfg.PolicyHash, cfg.SourceScopeHash, true); err != nil {
		return errors.New("imported effects permission update failed")
	}
	return nil
}

func installImportedRuntimePermission(ctx context.Context, store importedRuntimeStartupStore, verifiedBotID int64, cfg importedRuntimeConfig) error {
	if !cfg.Enabled {
		return nil
	}
	if verifiedBotID <= 0 || verifiedBotID != cfg.BotID {
		return errors.New("imported runtime bot identity mismatch")
	}
	if strings.TrimSpace(cfg.PolicyHash) == "" || strings.TrimSpace(cfg.SourceScopeHash) == "" {
		return errors.New("invalid imported runtime configuration")
	}
	if err := store.SyncImportedRuntimePermission(ctx, cfg.BotID, cfg.Revision, cfg.PolicyHash, cfg.SourceScopeHash, true); err != nil {
		return errors.New("imported runtime permission update failed")
	}
	return nil
}
