package payments

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
)

type Config struct {
	Enabled, TelegramEnabled                   bool
	BaseURL, Token, WebhookSecret, CurrencyID  string
	Workers, PageSize, MaxPages                int
	PollInterval, RecheckInterval, HTTPTimeout time.Duration
	ExcludedTelegramIDs                        []int64
}

func ConfigFromEnv(telegramEnabled bool) (Config, error) {
	return ConfigFromEnvironment(os.Getenv, telegramEnabled)
}

// ConfigFromEnvironment reads an explicit environment without mutating process globals.
func ConfigFromEnvironment(getenv func(string) string, telegramEnabled bool) (Config, error) {
	cfg := Config{TelegramEnabled: telegramEnabled, Workers: 1, PageSize: 50, MaxPages: 20, PollInterval: time.Second, RecheckInterval: 5 * time.Minute, HTTPTimeout: 5 * time.Second}
	if raw := getenv("PAYMENTS_ENABLED"); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, errors.New("PAYMENTS_ENABLED must be a boolean")
		}
		cfg.Enabled = enabled
	}
	if !cfg.Enabled {
		return cfg, nil
	}
	cfg.BaseURL, cfg.Token = getenv("INVOICE_NINJA_BASE_URL"), getenv("INVOICE_NINJA_API_TOKEN")
	cfg.WebhookSecret, cfg.CurrencyID = getenv("INVOICE_NINJA_WEBHOOK_SECRET"), getenv("PAYMENTS_CURRENCY_ID")
	if getenv("INVOICE_NINJA_TELEGRAM_ID_FIELD") != "id_number" {
		return Config{}, errors.New("payment verification requires INVOICE_NINJA_TELEGRAM_ID_FIELD=id_number")
	}
	for _, item := range []struct {
		key   string
		value *int
	}{{"PAYMENTS_WORKERS", &cfg.Workers}, {"INTEGRATION_PAGE_SIZE", &cfg.PageSize}, {"INTEGRATION_MAX_PAGES", &cfg.MaxPages}} {
		if raw := getenv(item.key); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil {
				return Config{}, errors.New(item.key + " must be an integer")
			}
			*item.value = n
		}
	}
	for _, item := range []struct {
		key   string
		value *time.Duration
	}{{"PAYMENTS_POLL_INTERVAL", &cfg.PollInterval}, {"PAYMENTS_RECHECK_INTERVAL", &cfg.RecheckInterval}, {"PAYMENTS_HTTP_TIMEOUT", &cfg.HTTPTimeout}} {
		if raw := getenv(item.key); raw != "" {
			d, err := time.ParseDuration(raw)
			if err != nil {
				return Config{}, errors.New(item.key + " must be a duration")
			}
			*item.value = d
		}
	}
	if raw := strings.TrimSpace(getenv("REGISTRATION_EXCLUDED_TELEGRAM_IDS")); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			id, err := strconv.ParseInt(part, 10, 64)
			if err != nil || id <= 0 || strconv.FormatInt(id, 10) != part {
				return Config{}, errors.New("invalid payment exclusions")
			}
			cfg.ExcludedTelegramIDs = append(cfg.ExcludedTelegramIDs, id)
		}
	}
	return cfg, cfg.Validate()
}
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if !c.TelegramEnabled {
		return errors.New("PAYMENTS_ENABLED requires TELEGRAM_ENABLED=true")
	}
	if integrationhttp.ValidateBaseURL(c.BaseURL) != nil {
		return errors.New("INVOICE_NINJA_BASE_URL must be a service base URL")
	}
	if c.Token == "" || strings.ContainsAny(c.Token, " \r\n\t") {
		return errors.New("INVOICE_NINJA_API_TOKEN must be set without spaces, tabs or line breaks")
	}
	if !currency(c.CurrencyID) {
		return errors.New("PAYMENTS_CURRENCY_ID must explicitly identify the native billing currency")
	}
	if c.Workers < 1 || c.Workers > 4 || c.PageSize < 1 || c.PageSize > 100 || c.MaxPages < 1 || c.MaxPages > 100 {
		return errors.New("invalid payment worker or pagination bounds")
	}
	if c.PollInterval < 100*time.Millisecond || c.PollInterval > time.Minute || c.RecheckInterval < 30*time.Second || c.RecheckInterval > time.Hour || c.HTTPTimeout < time.Second || c.HTTPTimeout > 10*time.Second {
		return errors.New("invalid payment timeouts or intervals")
	}
	if len(c.ExcludedTelegramIDs) > 1000 {
		return errors.New("too many payment exclusions")
	}
	seen := map[int64]bool{}
	for _, id := range c.ExcludedTelegramIDs {
		if id <= 0 || seen[id] {
			return errors.New("invalid payment exclusions")
		}
		seen[id] = true
	}
	return nil
}
func secret(s string) bool {
	return s != ""
}
