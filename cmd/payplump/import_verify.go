package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/importplan"
	"github.com/PlumpAlbert/payplump-bot/internal/importverify"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

type verifyEnvironment func(string) string
type verifyFactory func(verifyConfig) (importverify.Providers, error)
type verifyClock func() time.Time

type verifyConfig struct {
	policy                                   importverify.Policy
	ninjaBase, ninjaToken, suiBase, suiToken string
	timeout                                  time.Duration
	pageSize, maxPages                       int
}

// String, GoString, LogValue and MarshalJSON deliberately redact this policy
// artifact: it includes provider credentials and private operator identifiers.
func (verifyConfig) String() string                 { return "[private import verification configuration]" }
func (c verifyConfig) GoString() string             { return c.String() }
func (c verifyConfig) LogValue() slog.Value         { return slog.StringValue(c.String()) }
func (c verifyConfig) MarshalJSON() ([]byte, error) { return json.Marshal(c.String()) }

func importVerifyCommand(args []string, out io.Writer) error {
	return importVerifyWith(args, out, os.Getenv, newVerifyProviders, time.Now)
}

func importVerifyWith(args []string, out io.Writer, getenv verifyEnvironment, factory verifyFactory, clock verifyClock) error {
	fs := flag.NewFlagSet("import-verify", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("file", "", "private manifest")
	expected := fs.String("expected-hash", "", "plan hash")
	readOnly := fs.Bool("read-only", false, "required read-only confirmation")
	evidencePath := fs.String("evidence-file", "", "new private evidence file")
	if fs.Parse(args) != nil || fs.NArg() != 0 || *path == "" || *expected == "" || !*readOnly {
		return errors.New("usage: payplump import-verify --file PATH --expected-hash HASH --read-only [--evidence-file NEW_PRIVATE_PATH]")
	}
	manifest, err := loadPrivateManifest(*path)
	if err != nil {
		return errors.New("private manifest unavailable")
	}
	plan := importplan.Analyze(manifest)
	if plan.ValidateHash(*expected) != nil || plan.Hash != *expected {
		return errors.New("invalid import verification plan")
	}
	cfg, err := loadVerifyConfig(getenv)
	if err != nil {
		return errors.New("invalid import verification configuration")
	}
	providers, err := factory(cfg)
	if err != nil {
		return errors.New("import verification sources unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	now := clock()
	if now.IsZero() {
		return errors.New("invalid import verification time")
	}
	service, err := importverify.New(cfg.policy, providers)
	if err != nil {
		return errors.New("invalid import verification policy")
	}
	location, err := loadVerifyLocation(getenv("TZ"))
	if err != nil {
		return errors.New("invalid import verification configuration")
	}
	result, evidence, err := service.Verify(ctx, plan, *expected, now.In(location))
	if err != nil {
		return errors.New("import verification failed")
	}
	if *evidencePath != "" {
		if err := writePrivateEvidence(*evidencePath, result, evidence); err != nil {
			return errors.New("private evidence could not be written")
		}
	}
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(true)
	if err := enc.Encode(result); err != nil {
		return errors.New("verification report could not be written")
	}
	return nil
}

func loadVerifyConfig(getenv verifyEnvironment) (verifyConfig, error) {
	get := func(k string) string { return getenv(k) }
	c := verifyConfig{ninjaBase: get("INVOICE_NINJA_BASE_URL"), ninjaToken: get("INVOICE_NINJA_API_TOKEN"), suiBase: get("SUI_BASE_URL"), suiToken: get("SUI_API_TOKEN"), pageSize: 50, maxPages: 20}
	if get("INVOICE_NINJA_TELEGRAM_ID_FIELD") != "id_number" {
		return verifyConfig{}, errors.New("invalid configuration")
	}
	if strings.TrimPrefix(get("INVOICE_NINJA_VERSION"), "v") != "5.13.47" || strings.TrimPrefix(get("SUI_VERSION"), "v") != "1.6.4" {
		return verifyConfig{}, errors.New("invalid configuration")
	}
	c.timeout = 3 * time.Second
	if raw := get("INTEGRATION_HTTP_TIMEOUT"); raw != "" {
		d, e := time.ParseDuration(raw)
		if e != nil || d < time.Second || d > 10*time.Second {
			return verifyConfig{}, errors.New("invalid configuration")
		}
		c.timeout = d
	}
	var err error
	if c.pageSize, err = boundedInt(get("INTEGRATION_PAGE_SIZE"), 50, 1, 100); err != nil {
		return verifyConfig{}, err
	}
	if c.maxPages, err = boundedInt(get("INTEGRATION_MAX_PAGES"), 20, 1, 100); err != nil {
		return verifyConfig{}, err
	}
	bot, err := canonicalID(get("IMPORT_VERIFY_BOT_ID"))
	if err != nil {
		return verifyConfig{}, err
	}
	pilots, err := parseIDs(get("IMPORT_VERIFY_ALLOWED_TELEGRAM_IDS"), 1, 20, true)
	if err != nil {
		return verifyConfig{}, err
	}
	excluded := map[string]bool{}
	for _, key := range []string{"IMPORT_VERIFY_EXCLUDED_TELEGRAM_IDS", "REGISTRATION_EXCLUDED_TELEGRAM_IDS", "TELEGRAM_ADMIN_IDS"} {
		values := get(key)
		if values == "" {
			continue
		}
		ids, e := parseIDs(values, 1, 1000, false)
		if e != nil {
			return verifyConfig{}, e
		}
		for _, id := range ids {
			excluded[id] = true
		}
	}
	excluded[strconv.FormatInt(bot, 10)] = true
	exclusions := make([]string, 0, len(excluded))
	for id := range excluded {
		exclusions = append(exclusions, id)
	}
	sort.Strings(exclusions)
	currencyID, e := canonicalID(get("PAYMENTS_CURRENCY_ID"))
	if e != nil {
		return verifyConfig{}, e
	}
	currencyCode := get("IMPORT_VERIFY_CURRENCY_CODE")
	if len(currencyCode) != 3 || currencyCode != strings.ToUpper(currencyCode) || currencyCode[0] < 'A' || currencyCode[0] > 'Z' || currencyCode[1] < 'A' || currencyCode[1] > 'Z' || currencyCode[2] < 'A' || currencyCode[2] > 'Z' {
		return verifyConfig{}, errors.New("invalid configuration")
	}
	inbounds, e := parseIntIDs(get("SUI_INBOUND_IDS"))
	if e != nil {
		return verifyConfig{}, e
	}
	product := get("IMPORT_VERIFY_PRODUCT_KEY")
	if product == "" || strings.TrimSpace(product) != product {
		return verifyConfig{}, errors.New("invalid configuration")
	}
	maxRecords, e := boundedInt(get("IMPORT_VERIFY_MAX_RECORDS"), 20, 1, 20)
	if e != nil {
		return verifyConfig{}, e
	}
	maxInventory, e := boundedInt(get("IMPORT_VERIFY_MAX_INVENTORY_RECORDS"), 1000, 1, 1000)
	if e != nil {
		return verifyConfig{}, e
	}
	anchors, e := boundedInt(get("IMPORT_VERIFY_MAX_HISTORICAL_ANCHORS"), 10, 1, 20)
	if e != nil {
		return verifyConfig{}, e
	}
	ttl := 5 * time.Minute
	if raw := get("IMPORT_VERIFY_EVIDENCE_TTL"); raw != "" {
		ttl, e = time.ParseDuration(raw)
		if e != nil || ttl < time.Minute || ttl > 15*time.Minute {
			return verifyConfig{}, errors.New("invalid configuration")
		}
	}
	scope, e := importverify.SourceScope(c.ninjaBase, c.ninjaToken, "5.13.47", c.suiBase, c.suiToken, "1.6.4")
	if e != nil || invoiceninja.ValidateClientConfiguration(c.ninjaBase, c.ninjaToken, c.timeout) != nil || sui.ValidateClientConfiguration(c.suiBase, c.suiToken, c.timeout) != nil {
		return verifyConfig{}, errors.New("invalid configuration")
	}
	if strings.HasSuffix(strings.TrimRight(c.ninjaBase, "/"), "/api/v1") || strings.HasSuffix(strings.TrimRight(c.suiBase, "/"), "/apiv2") {
		return verifyConfig{}, errors.New("invalid configuration")
	}
	if _, err = loadVerifyLocation(get("TZ")); err != nil {
		return verifyConfig{}, errors.New("invalid configuration")
	}
	c.policy = importverify.Policy{BotID: bot, SourceScopeHash: scope, PilotIDs: pilots, ExcludedIDs: exclusions, NinjaVersion: "5.13.47", SUIVersion: "1.6.4", NativeCurrencyID: strconv.FormatInt(currencyID, 10), NativeCurrencyCode: currencyCode, ProductKey: product, PublicSubscriptionBase: get("SUI_SUBSCRIPTION_BASE_URL"), ExpectedInboundIDs: inbounds, MaxRecords: maxRecords, MaxInventoryRecords: maxInventory, MaxPages: c.maxPages, MaxHistoricalAnchors: anchors, EvidenceTTL: ttl}
	if err = c.policy.Validate(); err != nil {
		return verifyConfig{}, err
	}
	return c, nil
}
func boundedInt(raw string, def, min, max int) (int, error) {
	if raw == "" {
		return def, nil
	}
	n, e := strconv.Atoi(raw)
	if e != nil || n < min || n > max {
		return 0, errors.New("invalid configuration")
	}
	return n, nil
}
func canonicalID(raw string) (int64, error) {
	n, e := strconv.ParseInt(raw, 10, 64)
	if e != nil || n <= 0 || strconv.FormatInt(n, 10) != raw {
		return 0, errors.New("invalid configuration")
	}
	return n, nil
}
func parseIDs(raw string, min, max int, sortResult bool) ([]string, error) {
	if raw == "" {
		return nil, errors.New("invalid configuration")
	}
	parts := strings.Split(raw, ",")
	if len(parts) < min || len(parts) > max {
		return nil, errors.New("invalid configuration")
	}
	seen := map[string]bool{}
	for i, p := range parts {
		trimmed := strings.TrimSpace(p)
		if sortResult && trimmed != p {
			return nil, errors.New("invalid configuration")
		}
		p = trimmed
		if _, e := canonicalID(p); e != nil || seen[p] {
			return nil, errors.New("invalid configuration")
		}
		seen[p] = true
		parts[i] = p
	}
	if sortResult {
		sort.Strings(parts)
	}
	return parts, nil
}
func parseIntIDs(raw string) ([]int64, error) {
	parts := strings.Split(raw, ",")
	if len(parts) == 0 || len(parts) > 100 {
		return nil, errors.New("invalid configuration")
	}
	seen := map[int64]bool{}
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		n, e := strconv.ParseInt(strings.TrimSpace(p), 10, 64)
		if e != nil || n <= 0 || seen[n] {
			return nil, errors.New("invalid configuration")
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}
func loadVerifyLocation(value string) (*time.Location, error) {
	if value == "" {
		value = "UTC"
	}
	return time.LoadLocation(value)
}

func newVerifyProviders(c verifyConfig) (importverify.Providers, error) {
	imp, e := invoiceninja.NewImportReader(c.ninjaBase, c.ninjaToken, c.timeout, c.pageSize, c.maxPages)
	if e != nil {
		return importverify.Providers{}, e
	}
	rec, e := invoiceninja.NewRecurringReader(c.ninjaBase, c.ninjaToken, c.timeout)
	if e != nil {
		return importverify.Providers{}, e
	}
	cyc, e := invoiceninja.NewCycleReader(c.ninjaBase, c.ninjaToken, c.timeout, c.pageSize, c.maxPages)
	if e != nil {
		return importverify.Providers{}, e
	}
	settle, e := invoiceninja.NewSettlementClient(c.ninjaBase, c.ninjaToken, c.timeout, c.pageSize, c.maxPages)
	if e != nil {
		return importverify.Providers{}, e
	}
	ids, e := sui.NewIdentityReader(c.suiBase, c.suiToken, "1.6.4", c.timeout)
	if e != nil {
		return importverify.Providers{}, e
	}
	records, e := sui.NewRecordReader(c.suiBase, c.suiToken, "1.6.4", c.timeout)
	if e != nil {
		return importverify.Providers{}, e
	}
	settings, e := sui.New(c.suiBase, c.suiToken, c.timeout)
	if e != nil {
		return importverify.Providers{}, e
	}
	return importverify.Providers{Customers: imp, Recurring: rec, Invoices: cyc, Payments: settle, Identities: ids, Records: records, Settings: verifySettingsReader{client: settings}}, nil
}

type verifySettingsReader struct{ client *sui.Client }

func (r verifySettingsReader) GetSettings(ctx context.Context) (sui.Settings, error) {
	return r.client.GetSettings(ctx)
}

type privateEvidenceDocument struct {
	SchemaVersion int               `json:"schema_version"`
	PlanHash      string            `json:"plan_hash"`
	EvidenceHash  string            `json:"evidence_hash"`
	Evidence      []json.RawMessage `json:"evidence"`
}

func writePrivateEvidence(path string, result importverify.Result, evidence []importverify.PrivateEvidence) error {
	doc := privateEvidenceDocument{SchemaVersion: 1, PlanHash: result.PlanHash, EvidenceHash: result.EvidenceHash, Evidence: make([]json.RawMessage, 0, len(evidence))}
	for _, item := range evidence {
		privateJSON, err := item.PrivateJSON()
		if err != nil {
			return errors.New("evidence encoding failed")
		}
		doc.Evidence = append(doc.Evidence, privateJSON)
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return errors.New("evidence encoding failed")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return errors.New("evidence file unavailable")
	}
	owned := true
	var opened os.FileInfo
	defer func() {
		_ = f.Close()
		if owned && opened != nil {
			if current, statErr := os.Lstat(path); statErr == nil && os.SameFile(opened, current) {
				_ = os.Remove(path)
			}
		}
	}()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 {
		return errors.New("evidence file unavailable")
	}
	opened = st
	if _, err = f.Write(append(data, '\n')); err != nil {
		return errors.New("evidence file unavailable")
	}
	if err = f.Sync(); err != nil {
		return errors.New("evidence file unavailable")
	}
	if err = f.Close(); err != nil {
		return errors.New("evidence file unavailable")
	}
	owned = false
	return nil
}
