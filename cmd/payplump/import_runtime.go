package main

import (
	"context"
	"errors"
	"strings"
)

type importedRuntimeConfig struct {
	Enabled                     bool
	Revision                    int64
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
	if rawEnabled == "" || rawEnabled == "false" {
		return importedRuntimeConfig{}, nil
	}
	if rawEnabled != "true" {
		return importedRuntimeConfig{}, errors.New("invalid imported runtime configuration")
	}
	if !telegramEnabled || !paymentsEnabled {
		return importedRuntimeConfig{}, errors.New("invalid imported runtime configuration")
	}
	revision, err := canonicalID(get("IMPORTED_RUNTIME_REVISION"))
	if err != nil {
		return importedRuntimeConfig{}, errors.New("invalid imported runtime configuration")
	}
	verification, err := loadVerifyConfig(get)
	if err != nil {
		return importedRuntimeConfig{}, errors.New("invalid imported runtime configuration")
	}
	policyHash, err := verification.policy.Fingerprint()
	if err != nil {
		return importedRuntimeConfig{}, errors.New("invalid imported runtime configuration")
	}
	return importedRuntimeConfig{Enabled: true, Revision: revision, BotID: verification.policy.BotID, PolicyHash: policyHash, SourceScopeHash: verification.policy.SourceScopeHash}, nil
}

func fenceImportedRuntimeStartup(ctx context.Context, store importedRuntimeStartupStore, cfg importedRuntimeConfig) error {
	if err := store.DisableImportedEffectPermissions(ctx); err != nil {
		return errors.New("imported runtime startup fence failed")
	}
	if !cfg.Enabled {
		if err := store.DisableImportedRuntimePermissions(ctx); err != nil {
			return errors.New("imported runtime startup fence failed")
		}
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
