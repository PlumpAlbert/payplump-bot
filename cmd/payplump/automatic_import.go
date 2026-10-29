package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/importadmission"
	"github.com/PlumpAlbert/payplump-bot/internal/importplan"
	"github.com/PlumpAlbert/payplump-bot/internal/importverify"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/lifecycle"
	"github.com/PlumpAlbert/payplump-bot/internal/postgres"
)

func serviceImportedRuntimeConfig(cfg lifecycle.EnforcementConfig) (importedRuntimeConfig, error) {
	// These labels identify the persisted import contract, not installed API
	// releases. Keep them stable so a server upgrade does not orphan provenance.
	scope, e := importverify.SourceScope(cfg.BaseURL, cfg.Token, "5.13.47", cfg.SUIBaseURL, cfg.SUIToken, "1.6.4")
	if e != nil {
		return importedRuntimeConfig{}, errors.New("invalid normal service import source configuration")
	}
	return importedRuntimeConfig{Enabled: true, EffectsEnabled: true, SourceScopeHash: scope}, nil
}

func serviceVerificationConfig(ctx context.Context, get func(string) string, bot int64, cfg activation.Config) (verifyConfig, error) {
	c := verifyConfig{ninjaBase: cfg.BaseURL, ninjaToken: cfg.Token, suiBase: cfg.SUIBaseURL, suiToken: cfg.SUIToken, timeout: cfg.HTTPTimeout, pageSize: cfg.PageSize, maxPages: cfg.MaxPages}
	currencyCode := get("PAYMENTS_CURRENCY_CODE")
	if len(currencyCode) != 3 || strings.Trim(currencyCode, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
		return c, errors.New("PAYMENTS_CURRENCY_CODE must identify the native billing currency as three uppercase ASCII letters")
	}
	api, e := invoiceninja.NewCustomerInventoryReader(c.ninjaBase, c.ninjaToken, c.timeout, c.pageSize, c.maxPages)
	if e != nil {
		return c, errors.New("invalid normal service tariff reader")
	}
	productKey, e := startupProductKey(ctx, api, cfg.ProductID)
	if e != nil {
		return c, fmt.Errorf("Invoice Ninja product lookup (INVOICE_NINJA_PRODUCT_ID): %w", e)
	}
	scope, e := importverify.SourceScope(c.ninjaBase, c.ninjaToken, "5.13.47", c.suiBase, c.suiToken, "1.6.4")
	if e != nil {
		return c, e
	}
	ids := slices.Clone(cfg.InboundIDs)
	slices.Sort(ids)
	excluded := make(map[string]bool)
	for _, id := range cfg.ExcludedTelegramIDs {
		excluded[strconv.FormatInt(id, 10)] = true
	}
	// Preserve the manual import contract: administrator identities are not
	// automatically adopted as legacy billing clients. OWN registration remains
	// independent and may still use an administrator's private account.
	for _, raw := range strings.Split(get("TELEGRAM_ADMIN_IDS"), ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil || id <= 0 {
			return c, errors.New("invalid TELEGRAM_ADMIN_IDS for import exclusions")
		}
		excluded[strconv.FormatInt(id, 10)] = true
	}
	exclusions := make([]string, 0, len(excluded))
	for id := range excluded {
		exclusions = append(exclusions, id)
	}
	sort.Strings(exclusions)
	c.policy = importverify.Policy{Automatic: true, BotID: bot, SourceScopeHash: scope, ExcludedIDs: exclusions, NinjaVersion: "5.13.47", SUIVersion: "1.6.4", NativeCurrencyID: cfg.CurrencyID, NativeCurrencyCode: currencyCode, ProductKey: productKey, PublicSubscriptionBase: get("SUI_SUBSCRIPTION_BASE_URL"), ExpectedInboundIDs: ids, MaxRecords: 20, MaxInventoryRecords: 1000, MaxPages: cfg.MaxPages, MaxHistoricalAnchors: 10, EvidenceTTL: 5 * time.Minute}
	if e = c.policy.Validate(); e != nil {
		return c, errors.New("invalid shared tariff/currency/inbound configuration for automatic import")
	}
	return c, nil
}

type automaticImportStore interface {
	AutomaticImportScanStart(context.Context) (time.Time, error)
	AutomaticImportKnownActors(context.Context) (map[int64]bool, error)
	ApplyVerifiedImport(context.Context, importadmission.PreparedPlan) (postgres.ImportAdmissionResult, error)
	RecordAutomaticImportIssue(context.Context, int64, int64, string, string, string, []int64) (bool, error)
	ResolveAutomaticImportIssue(context.Context, int64, string, time.Time) error
	ReconcileAutomaticImportIssues(context.Context, int64, []string, time.Time) error
}

type automaticImporter struct {
	store    automaticImportStore
	verifier *importverify.Service
	policy   importverify.Policy
	revision int64
	admins   []int64
	logger   *slog.Logger
	now      func() time.Time
	mu       sync.Mutex
}

func (w *automaticImporter) key(candidate string) string {
	sum := sha256.Sum256([]byte(w.policy.SourceScopeHash + ":" + candidate))
	return hex.EncodeToString(sum[:])
}
func (w *automaticImporter) report(ctx context.Context, actor int64, key, reason, facts string, diagnostic ...importverify.Diagnostic) error {
	return w.recordIssue(ctx, actor, key, reason, facts, w.admins, diagnostic...)
}
func (w *automaticImporter) recordIssue(ctx context.Context, actor int64, key, reason, facts string, admins []int64, diagnostic ...importverify.Diagnostic) error {
	// Prepare/admission can replace discovery's diagnosis; rearm on that change too.
	if len(diagnostic) > 0 {
		raw, _ := json.Marshal(diagnostic[0])
		sum := sha256.Sum256(append([]byte(facts), raw...))
		facts = hex.EncodeToString(sum[:])
	}
	changed, e := w.store.RecordAutomaticImportIssue(ctx, w.policy.BotID, actor, key, reason, facts, admins)
	if e != nil {
		return e
	}
	if changed {
		attrs := []any{"candidate", key, "reason", reason}
		if len(diagnostic) > 0 {
			d := diagnostic[0]
			attrs = append(attrs, "stage", d.Stage, "check", d.Check, "category", d.Category, "http_status", d.HTTPStatus, "count", d.Count)
		}
		w.logger.Error("automatic import candidate not admitted", attrs...)
	}
	return nil
}
func (w *automaticImporter) Scan(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	global := w.key("complete-inventory")
	started, e := w.store.AutomaticImportScanStart(ctx)
	if e != nil {
		return w.report(ctx, 0, global, importverify.ReasonSourceFailure, global, importverify.FailureDiagnostic("db_scan_start", "read", e))
	}
	known, e := w.store.AutomaticImportKnownActors(ctx)
	if e != nil {
		return w.report(ctx, 0, global, importverify.ReasonSourceFailure, global, importverify.FailureDiagnostic("db_known_actors", "read", e))
	}
	discoveries, e := w.verifier.Discover(ctx, w.now().In(time.Local), known)
	if e != nil {
		diagnostic := importverify.FailureDiagnostic("discovery", "read", e)
		var observation importverify.Diagnostic
		if errors.As(e, &observation) {
			diagnostic = observation
		}
		return w.report(ctx, 0, global, importverify.ReasonSourceFailure, global, diagnostic)
	}
	active := make([]string, 0, len(discoveries)+1)
	sourceFailure := false
	var sourceDiagnostic importverify.Diagnostic
	for _, candidate := range discoveries {
		if e = ctx.Err(); e != nil {
			return e
		}
		key := w.key(candidate.CustomerID)
		active = append(active, key)
		reason := candidate.Reason
		diagnostic := candidate.Diagnostic
		if reason == "" {
			plan := importplan.Analyze(importplan.Manifest{Records: []importplan.Record{candidate.Facts}})
			if plan.Imported != 1 {
				reason = importverify.ReasonUnsupported
				diagnostic = importverify.Diagnostic{Stage: "analysis", Check: "unsupported_plan"}
			} else {
				prepared, verifyErr := importadmission.Prepare(ctx, w.verifier, plan, plan.Hash, w.revision, w.now().In(time.Local))
				if verifyErr != nil {
					reason = importverify.ReasonChanged
					diagnostic = importverify.FailureDiagnostic("verification", "prepare", verifyErr)
					var rejection importadmission.VerificationFailure
					if errors.As(verifyErr, &rejection) {
						for _, code := range []string{importverify.ReasonSourceFailure, importverify.ReasonAmbiguous, importverify.ReasonUnsupported, importverify.ReasonUnpaid, importverify.ReasonPayment, importverify.ReasonExpired, importverify.ReasonChanged, importverify.ReasonPausedHistory, importverify.ReasonHistoryLimit, importverify.ReasonExcluded} {
							if rejection.Reasons[code] > 0 {
								reason = code
								break
							}
						}
						if len(rejection.Diagnostics) > 0 {
							diagnostic = rejection.Diagnostics[0]
						}
					}
				} else if _, applyErr := w.store.ApplyVerifiedImport(ctx, prepared); applyErr != nil {
					reason = "admission_conflict"
					diagnostic = importverify.FailureDiagnostic("admission", "apply", applyErr)
				}
			}
		}
		if reason != "" {
			admins := w.admins
			if reason == importverify.ReasonSourceFailure {
				// One unavailable source must not send one identical API-outage
				// message per account. Persist each hold, report the outage once.
				// Choose a stable representative, independent of candidate order.
				if !sourceFailure || diagnostic.Error() < sourceDiagnostic.Error() {
					sourceDiagnostic = diagnostic
				}
				sourceFailure = true
				admins = nil
			}
			if e = w.recordIssue(ctx, candidate.Actor, key, reason, candidate.FactsHash, admins, diagnostic); e != nil {
				return e
			}
			continue
		}
		if e = w.store.ResolveAutomaticImportIssue(ctx, w.policy.BotID, key, started); e != nil {
			return e
		}
	}
	if sourceFailure {
		active = append(active, global)
		if e = w.report(ctx, 0, global, importverify.ReasonSourceFailure, global, sourceDiagnostic); e != nil {
			return e
		}
	} else if e = w.store.ResolveAutomaticImportIssue(ctx, w.policy.BotID, global, started); e != nil {
		return e
	}
	return w.store.ReconcileAutomaticImportIssues(ctx, w.policy.BotID, active, started)
}

func (w *automaticImporter) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		scan, cancel := context.WithTimeout(ctx, 3*time.Minute)
		e := w.Scan(scan)
		cancel()
		if e != nil && ctx.Err() == nil {
			w.logger.Warn("automatic import scan deferred")
		}
		timer := time.NewTimer(5 * time.Minute)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	return nil
}
