package activation

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

type Config struct {
	// ManagedClients selects only actors with bot-owned database provenance.
	ManagedClients                                         bool
	Enabled, TelegramEnabled, PaymentsEnabled              bool
	ExclusiveWriter, InclusiveTaxes, TaxModeConfirmed      bool
	BaseURL, Token, ProductID, CurrencyID, TelegramIDField string
	SUIBaseURL, SUIToken, SUIVersion                       string
	InboundIDs, AllowedTelegramIDs, ExcludedTelegramIDs    []int64
	Workers, PageSize, MaxPages, MaxAccounts               int
	PollInterval, HTTPTimeout                              time.Duration
}

func (Config) String() string   { return "[sensitive activation configuration]" }
func (Config) GoString() string { return "[sensitive activation configuration]" }

func ConfigFromEnv(telegramEnabled, paymentsEnabled bool) (Config, error) {
	return ConfigFromEnvironment(os.Getenv, telegramEnabled, paymentsEnabled, false)
}

// ConfigFromEnvironment reads an explicit environment without mutating process globals.
func ConfigFromEnvironment(getenv func(string) string, telegramEnabled, paymentsEnabled bool, managed bool) (Config, error) {
	c := Config{TelegramEnabled: telegramEnabled, PaymentsEnabled: paymentsEnabled, Workers: 1, PageSize: 50, MaxPages: 20, MaxAccounts: 1000, PollInterval: time.Second, HTTPTimeout: 5 * time.Second}
	if raw := getenv("ACTIVATION_ENABLED"); raw != "" {
		v, e := strconv.ParseBool(raw)
		if e != nil {
			return Config{}, errors.New("ACTIVATION_ENABLED must be a boolean")
		}
		c.Enabled = v
	}
	c.ManagedClients = managed
	if !c.Enabled {
		return c, nil
	}
	c.BaseURL, c.Token, c.ProductID = getenv("INVOICE_NINJA_BASE_URL"), getenv("INVOICE_NINJA_API_TOKEN"), getenv("INVOICE_NINJA_PRODUCT_ID")
	c.CurrencyID, c.TelegramIDField = getenv("PAYMENTS_CURRENCY_ID"), getenv("INVOICE_NINJA_TELEGRAM_ID_FIELD")
	c.SUIBaseURL, c.SUIToken, c.SUIVersion = getenv("SUI_BASE_URL"), getenv("SUI_API_TOKEN"), getenv("SUI_VERSION")
	if version := getenv("INVOICE_NINJA_VERSION"); version != "" && strings.TrimPrefix(version, "v") != invoiceninja.SupportedRecurringVersion {
		return Config{}, errors.New("activation supports Invoice Ninja 5.13.47 only")
	}
	for _, item := range []struct {
		key    string
		target *bool
	}{{"ACTIVATION_EXCLUSIVE_WRITER", &c.ExclusiveWriter}, {"INVOICE_NINJA_INCLUSIVE_TAXES", &c.InclusiveTaxes}} {
		if raw := getenv(item.key); raw != "" {
			v, e := strconv.ParseBool(raw)
			if e != nil {
				return Config{}, errors.New(item.key + " must be an explicitly chosen boolean")
			}
			*item.target = v
			if item.key == "INVOICE_NINJA_INCLUSIVE_TAXES" {
				c.TaxModeConfirmed = true
			}
		}
	}
	for _, item := range []struct {
		key    string
		target *int
	}{{"ACTIVATION_WORKERS", &c.Workers}, {"INTEGRATION_PAGE_SIZE", &c.PageSize}, {"INTEGRATION_MAX_PAGES", &c.MaxPages}, {"ACTIVATION_MAX_SUI_ACCOUNTS", &c.MaxAccounts}} {
		if raw := getenv(item.key); raw != "" {
			v, e := strconv.Atoi(raw)
			if e != nil {
				return Config{}, errors.New(item.key + " must be an integer")
			}
			*item.target = v
		}
	}
	for _, item := range []struct {
		key    string
		target *time.Duration
	}{{"ACTIVATION_POLL_INTERVAL", &c.PollInterval}, {"ACTIVATION_HTTP_TIMEOUT", &c.HTTPTimeout}} {
		if raw := getenv(item.key); raw != "" {
			v, e := time.ParseDuration(raw)
			if e != nil {
				return Config{}, errors.New(item.key + " must be a duration")
			}
			*item.target = v
		}
	}
	for _, item := range []struct {
		key      string
		target   *[]int64
		required bool
	}{{"SUI_INBOUND_IDS", &c.InboundIDs, true}, {"ACTIVATION_ALLOWED_TELEGRAM_IDS", &c.AllowedTelegramIDs, !managed}, {"REGISTRATION_EXCLUDED_TELEGRAM_IDS", &c.ExcludedTelegramIDs, false}} {
		raw := strings.TrimSpace(getenv(item.key))
		if raw == "" {
			if item.required {
				return Config{}, errors.New(item.key + " must explicitly list positive IDs")
			}
			continue
		}
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			v, e := strconv.ParseInt(part, 10, 64)
			if e != nil || v <= 0 || strconv.FormatInt(v, 10) != part {
				return Config{}, errors.New("invalid " + item.key)
			}
			*item.target = append(*item.target, v)
		}
	}
	return c, c.Validate()
}
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if !c.TelegramEnabled || !c.PaymentsEnabled {
		return errors.New("ACTIVATION_ENABLED requires Telegram and payment verification")
	}
	if !c.ExclusiveWriter {
		return errors.New("activation requires explicit ACTIVATION_EXCLUSIVE_WRITER=true acknowledgement")
	}
	for _, token := range []string{c.Token, c.SUIToken} {
		if token == "" || !utf8.ValidString(token) || strings.IndexFunc(token, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0 {
			return errors.New("activation API credentials must not contain whitespace or control characters")
		}
	}
	if c.TelegramIDField != "id_number" || !ValidID(c.ProductID) || !nativeCurrency(c.CurrencyID) || !c.TaxModeConfirmed {
		return errors.New("activation requires exact identity, native currency, product and explicit tax policy")
	}
	if c.Workers < 1 || c.Workers > 4 || c.PageSize < 1 || c.PageSize > 100 || c.MaxPages < 1 || c.MaxPages > 100 || c.MaxAccounts < 1 || c.MaxAccounts > 1000 {
		return errors.New("invalid activation worker or inventory bounds")
	}
	if c.PollInterval < 100*time.Millisecond || c.PollInterval > time.Minute || c.HTTPTimeout < time.Second || c.HTTPTimeout > 10*time.Second {
		return errors.New("invalid activation timeouts")
	}
	if !idList(c.InboundIDs, 1, 100) || (!c.ManagedClients && !idList(c.AllowedTelegramIDs, 1, 20)) || !idList(c.ExcludedTelegramIDs, 0, 1000) {
		return errors.New("invalid activation inbound, pilot allowlist or exclusion IDs")
	}
	if _, e := invoiceninja.NewRecurringClient(c.BaseURL, c.Token, c.HTTPTimeout, c.PageSize, c.MaxPages); e != nil {
		return errors.New("invalid activation billing configuration")
	}
	if _, e := sui.NewManagedClient(c.SUIBaseURL, c.SUIToken, c.SUIVersion, c.HTTPTimeout, c.MaxAccounts); e != nil {
		return errors.New("invalid activation SUI configuration")
	}
	return nil
}
func idList(ids []int64, min, max int) bool {
	if len(ids) < min || len(ids) > max {
		return false
	}
	seen := map[int64]bool{}
	for _, id := range ids {
		if id <= 0 || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}
func nativeCurrency(value string) bool {
	if len(value) == 0 || len(value) > 10 || value[0] < '1' || value[0] > '9' {
		return false
	}
	for _, v := range value {
		if v < '0' || v > '9' {
			return false
		}
	}
	return true
}

// PolicyHash is a flow fence, not a service identity attestation. Secret rotation
// and resource limits are intentionally excluded; endpoint/identity/pricing and
// private scope changes require quiescence and review of any unfinished flow.
func (c Config) PolicyHash() [32]byte {
	sorted := func(ids []int64) []int64 { v := slices.Clone(ids); slices.Sort(v); return v }
	raw, _ := json.Marshal(struct {
		Version                                                          int
		BaseURL, SUIBaseURL, SUIVersion, CurrencyID, ProductID, Identity string
		InclusiveTaxes                                                   bool
		Inbounds, Allowed, Excluded                                      []int64
	}{policyVersion(c.ManagedClients), c.BaseURL, c.SUIBaseURL, c.SUIVersion, c.CurrencyID, c.ProductID, c.TelegramIDField, c.InclusiveTaxes, slices.Clone(c.InboundIDs), sorted(c.AllowedTelegramIDs), sorted(c.ExcludedTelegramIDs)})
	return sha256.Sum256(raw)
}

func policyVersion(managed bool) int {
	if managed {
		return 2
	}
	return 1
}
