package configdelivery

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

type Config struct {
	Enabled, TelegramEnabled, PaymentsEnabled             bool
	Revision                                              int64
	BaseURL, Token, CurrencyID, TelegramIDField           string
	SUIBaseURL, SUIToken, SUIVersion, SubscriptionBaseURL string
	AllowedTelegramIDs, ExcludedTelegramIDs               []int64
	HTTPTimeout                                           time.Duration
	PageSize, MaxPages                                    int
}

func (Config) String() string   { return "[private configuration delivery policy]" }
func (Config) GoString() string { return "[private configuration delivery policy]" }
func ConfigFromEnv(telegramEnabled, paymentsEnabled bool) (Config, error) {
	c := Config{TelegramEnabled: telegramEnabled, PaymentsEnabled: paymentsEnabled, HTTPTimeout: 3 * time.Second, PageSize: 50, MaxPages: 20}
	if raw := os.Getenv("CONFIGURATION_DELIVERY_ENABLED"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, errors.New("CONFIGURATION_DELIVERY_ENABLED must be a boolean")
		}
		c.Enabled = value
	}
	if !c.Enabled {
		return c, nil
	}
	raw := os.Getenv("CONFIGURATION_POLICY_REVISION")
	revision, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || revision < 1 || strconv.FormatInt(revision, 10) != raw {
		return Config{}, errors.New("CONFIGURATION_POLICY_REVISION must explicitly be a positive canonical integer")
	}
	c.Revision = revision
	c.BaseURL, c.Token, c.CurrencyID, c.TelegramIDField = os.Getenv("INVOICE_NINJA_BASE_URL"), os.Getenv("INVOICE_NINJA_API_TOKEN"), os.Getenv("PAYMENTS_CURRENCY_ID"), os.Getenv("INVOICE_NINJA_TELEGRAM_ID_FIELD")
	c.SUIBaseURL, c.SUIToken, c.SUIVersion, c.SubscriptionBaseURL = os.Getenv("SUI_BASE_URL"), os.Getenv("SUI_API_TOKEN"), os.Getenv("SUI_VERSION"), os.Getenv("SUI_SUBSCRIPTION_BASE_URL")
	if version := os.Getenv("INVOICE_NINJA_VERSION"); version != "" && strings.TrimPrefix(version, "v") != invoiceninja.SupportedSettlementVersion {
		return Config{}, errors.New("configuration delivery supports Invoice Ninja 5.13.47 only")
	}
	for _, item := range []struct {
		key    string
		target *int
	}{{"INTEGRATION_PAGE_SIZE", &c.PageSize}, {"INTEGRATION_MAX_PAGES", &c.MaxPages}} {
		if raw := os.Getenv(item.key); raw != "" {
			v, e := strconv.Atoi(raw)
			if e != nil {
				return Config{}, errors.New(item.key + " must be an integer")
			}
			*item.target = v
		}
	}
	if raw := os.Getenv("CONFIGURATION_HTTP_TIMEOUT"); raw != "" {
		v, e := time.ParseDuration(raw)
		if e != nil {
			return Config{}, errors.New("CONFIGURATION_HTTP_TIMEOUT must be a duration")
		}
		c.HTTPTimeout = v
	}
	for _, item := range []struct {
		key      string
		target   *[]int64
		required bool
	}{{"ACTIVATION_ALLOWED_TELEGRAM_IDS", &c.AllowedTelegramIDs, true}, {"REGISTRATION_EXCLUDED_TELEGRAM_IDS", &c.ExcludedTelegramIDs, false}} {
		raw := strings.TrimSpace(os.Getenv(item.key))
		if raw == "" {
			if item.required {
				return Config{}, errors.New("configuration delivery requires a private pilot allowlist")
			}
			continue
		}
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			id, e := strconv.ParseInt(part, 10, 64)
			if e != nil || id <= 0 || strconv.FormatInt(id, 10) != part {
				return Config{}, errors.New("invalid configuration delivery ID scope")
			}
			*item.target = append(*item.target, id)
		}
	}
	return c, c.Validate()
}
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if !c.TelegramEnabled || !c.PaymentsEnabled {
		return errors.New("configuration delivery requires Telegram and payment verification")
	}
	if c.Revision < 1 || c.TelegramIDField != "id_number" || !nativeCurrency(c.CurrencyID) || !ids(c.AllowedTelegramIDs, 1, 20) || !ids(c.ExcludedTelegramIDs, 0, 1000) {
		return errors.New("invalid configuration delivery policy, currency or actor scope")
	}
	for _, token := range []string{c.Token, c.SUIToken} {
		if token == "" || !utf8.ValidString(token) || strings.IndexFunc(token, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
			return errors.New("invalid configuration delivery API credentials")
		}
	}
	if c.HTTPTimeout < time.Second || c.HTTPTimeout > 5*time.Second || c.PageSize < 1 || c.PageSize > 100 || c.MaxPages < 1 || c.MaxPages > 100 {
		return errors.New("invalid configuration delivery HTTP bounds")
	}
	if len(c.SubscriptionBaseURL) > 1024 || integrationhttp.ValidateBaseURL(c.SubscriptionBaseURL) != nil {
		return errors.New("invalid public subscription base URL")
	}
	u, e := url.Parse(c.SubscriptionBaseURL)
	if e != nil || u.Scheme != "https" {
		return errors.New("personal public subscription URLs require HTTPS; private SUI panel HTTP remains supported")
	}
	if _, e := sui.SubscriptionURL(c.SubscriptionBaseURL, "synthetic-validation"); e != nil {
		return errors.New("invalid public subscription URL policy")
	}
	if _, e := sui.NewRecordReader(c.SUIBaseURL, c.SUIToken, c.SUIVersion, c.HTTPTimeout); e != nil {
		return errors.New("invalid configuration SUI reader")
	}
	if _, e := invoiceninja.NewSettlementClient(c.BaseURL, c.Token, c.HTTPTimeout, c.PageSize, c.MaxPages); e != nil {
		return errors.New("invalid configuration payment reader")
	}
	if _, e := invoiceninja.NewRecurringReader(c.BaseURL, c.Token, c.HTTPTimeout); e != nil {
		return errors.New("invalid configuration recurring reader")
	}
	return nil
}
func ids(values []int64, min, max int) bool {
	if len(values) < min || len(values) > max {
		return false
	}
	seen := map[int64]bool{}
	for _, v := range values {
		if v < 1 || seen[v] {
			return false
		}
		seen[v] = true
	}
	return true
}
func nativeCurrency(value string) bool {
	if len(value) < 1 || len(value) > 10 || value[0] < '1' || value[0] > '9' {
		return false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
func (c Config) Policy() Policy {
	if !c.Enabled {
		return Policy{}
	}
	sorted := func(values []int64) []int64 { v := slices.Clone(values); slices.Sort(v); return v }
	raw, _ := json.Marshal(struct {
		Version                                                                int
		Ninja, SUI, VersionClaim, PublicBase, Currency, Identity, CalendarZone string
		Allowed, Excluded                                                      []int64
	}{1, c.BaseURL, c.SUIBaseURL, c.SUIVersion, c.SubscriptionBaseURL, c.CurrencyID, c.TelegramIDField, time.Local.String(), sorted(c.AllowedTelegramIDs), sorted(c.ExcludedTelegramIDs)})
	return Policy{Enabled: true, Revision: c.Revision, Hash: sha256.Sum256(raw)}
}

// Allows is a local scope check, not entitlement or dispatch authorization.
func (c Config) Allows(actor int64) bool {
	return c.Enabled && actor > 0 && slices.Contains(c.AllowedTelegramIDs, actor) && !slices.Contains(c.ExcludedTelegramIDs, actor)
}
