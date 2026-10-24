package lifecycle

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
)

type Config struct {
	// ManagedClients selects only actors with bot-owned database provenance.
	ManagedClients                             bool
	Enabled                                    bool
	BaseURL, Token, WebhookSecret              string
	AllowedTelegramIDs, ExcludedTelegramIDs    []int64
	Workers, PageSize, MaxPages, MaxAccounts   int
	PollInterval, RecheckInterval, HTTPTimeout time.Duration
}

func (Config) String() string   { return "[sensitive invoice lifecycle configuration]" }
func (Config) GoString() string { return "[sensitive invoice lifecycle configuration]" }

func ConfigFromEnv(telegramEnabled, paymentsEnabled bool, paymentSecret, telegramSecret, telegramToken string) (Config, error) {
	return ConfigFromEnvironment(os.Getenv, telegramEnabled, paymentsEnabled, paymentSecret, telegramSecret, telegramToken, false)
}

// ConfigFromEnvironment reads an explicit environment without mutating process globals.
func ConfigFromEnvironment(getenv func(string) string, telegramEnabled, paymentsEnabled bool, paymentSecret, telegramSecret, telegramToken string, managed bool) (Config, error) {
	cfg := Config{Workers: 1, PageSize: 50, MaxPages: 20, MaxAccounts: 1000, PollInterval: time.Second, RecheckInterval: 5 * time.Minute, HTTPTimeout: 5 * time.Second}
	if raw := getenv("INVOICE_LIFECYCLE_ENABLED"); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, errors.New("INVOICE_LIFECYCLE_ENABLED must be a boolean")
		}
		cfg.Enabled = enabled
	}
	cfg.ManagedClients = managed
	if !cfg.Enabled {
		return cfg, nil
	}
	if !telegramEnabled || !paymentsEnabled {
		return Config{}, errors.New("INVOICE_LIFECYCLE_ENABLED requires Telegram and payment verification")
	}
	cfg.BaseURL, cfg.Token, cfg.WebhookSecret = getenv("INVOICE_NINJA_BASE_URL"), getenv("INVOICE_NINJA_API_TOKEN"), getenv("INVOICE_LIFECYCLE_WEBHOOK_SECRET")
	if version := strings.TrimPrefix(getenv("INVOICE_NINJA_VERSION"), "v"); version != "" && version != "5.13.47" {
		return Config{}, errors.New("invoice lifecycle supports Invoice Ninja 5.13.47 only")
	}
	if cfg.WebhookSecret == paymentSecret || cfg.WebhookSecret == telegramSecret || cfg.WebhookSecret == telegramToken || cfg.WebhookSecret == cfg.Token || !validSecret(cfg.WebhookSecret) {
		return Config{}, errors.New("INVOICE_LIFECYCLE_WEBHOOK_SECRET must be separate from other credentials")
	}
	if raw := strings.TrimSpace(getenv("INVOICE_LIFECYCLE_ALLOWED_TELEGRAM_IDS")); raw != "" {
		var err error
		cfg.AllowedTelegramIDs, err = parseIDs(raw, 20)
		if err != nil {
			return Config{}, errors.New("invalid invoice lifecycle pilot allowlist")
		}
	}
	if raw := strings.TrimSpace(getenv("REGISTRATION_EXCLUDED_TELEGRAM_IDS")); raw != "" {
		var err error
		cfg.ExcludedTelegramIDs, err = parseIDs(raw, 1000)
		if err != nil {
			return Config{}, errors.New("invalid invoice lifecycle exclusions")
		}
	}
	for _, item := range []struct {
		key    string
		target *int
	}{{"INVOICE_LIFECYCLE_WORKERS", &cfg.Workers}, {"INTEGRATION_PAGE_SIZE", &cfg.PageSize}, {"INTEGRATION_MAX_PAGES", &cfg.MaxPages}, {"INVOICE_LIFECYCLE_MAX_ACCOUNTS", &cfg.MaxAccounts}} {
		if raw := getenv(item.key); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil {
				return Config{}, errors.New(item.key + " must be an integer")
			}
			*item.target = value
		}
	}
	for _, item := range []struct {
		key    string
		target *time.Duration
	}{{"INVOICE_LIFECYCLE_POLL_INTERVAL", &cfg.PollInterval}, {"INVOICE_LIFECYCLE_RECHECK_INTERVAL", &cfg.RecheckInterval}, {"INVOICE_LIFECYCLE_HTTP_TIMEOUT", &cfg.HTTPTimeout}} {
		if raw := getenv(item.key); raw != "" {
			value, err := time.ParseDuration(raw)
			if err != nil {
				return Config{}, errors.New(item.key + " must be a duration")
			}
			*item.target = value
		}
	}
	return cfg, cfg.Validate()
}

func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if integrationhttp.ValidateBaseURL(c.BaseURL) != nil || c.Token == "" || strings.ContainsAny(c.Token, " \r\n\t") || !validSecret(c.WebhookSecret) || c.WebhookSecret == c.Token {
		return errors.New("invalid invoice lifecycle API or webhook configuration")
	}
	if (!c.ManagedClients && (!validIDs(c.AllowedTelegramIDs, 1, 20) || len(enforcementEffectiveIDs(c.AllowedTelegramIDs, c.ExcludedTelegramIDs)) == 0)) || !validIDs(c.ExcludedTelegramIDs, 0, 1000) {
		return errors.New("invoice lifecycle requires an effective pilot allowlist")
	}
	if c.Workers < 1 || c.Workers > 4 || c.PageSize < 1 || c.PageSize > 100 || c.MaxPages < 1 || c.MaxPages > 100 || c.MaxAccounts < 1 || c.MaxAccounts > 1000 {
		return errors.New("invalid invoice lifecycle worker or inventory bounds")
	}
	if c.PollInterval < 100*time.Millisecond || c.PollInterval > time.Minute || c.RecheckInterval < 30*time.Second || c.RecheckInterval > time.Hour || c.HTTPTimeout < time.Second || c.HTTPTimeout > 10*time.Second {
		return errors.New("invalid invoice lifecycle timeout or interval")
	}
	return nil
}

func parseIDs(raw string, max int) ([]int64, error) {
	parts := strings.Split(raw, ",")
	if len(parts) == 0 || len(parts) > max {
		return nil, errors.New("invalid ID count")
	}
	ids := make([]int64, 0, len(parts))
	seen := make(map[int64]bool, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil || id <= 0 || strconv.FormatInt(id, 10) != part || seen[id] {
			return nil, errors.New("invalid ID")
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, nil
}

func validIDs(ids []int64, min, max int) bool {
	if len(ids) < min || len(ids) > max {
		return false
	}
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id <= 0 || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

func validSecret(secret string) bool {
	if len(secret) < 32 || len(secret) > 256 {
		return false
	}
	for _, b := range []byte(secret) {
		if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '-') {
			return false
		}
	}
	return true
}
