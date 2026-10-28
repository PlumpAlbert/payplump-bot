package registration

import (
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
)

type Config struct {
	Enabled, TelegramEnabled  bool
	Workers                   int
	PollInterval              time.Duration
	HTTPTimeout               time.Duration
	PageSize, MaxPages        int
	BaseURL, Token, ProductID string
	InclusiveTaxes            bool
	TaxModeConfirmed          bool
	PaymentPhone              string
	PaymentBank               string
	PaymentRecipient          string
	ExcludedTelegramIDs       []int64
}

func ConfigFromEnv(telegramEnabled bool) (Config, error) {
	return ConfigFromEnvironment(os.Getenv, telegramEnabled)
}

// ConfigFromEnvironment reads an explicit environment without mutating process globals.
func ConfigFromEnvironment(getenv func(string) string, telegramEnabled bool) (Config, error) {
	cfg := Config{TelegramEnabled: telegramEnabled, Workers: 1, PollInterval: time.Second, HTTPTimeout: 5 * time.Second, PageSize: 50, MaxPages: 20}
	if raw := getenv("REGISTRATION_ENABLED"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, errors.New("REGISTRATION_ENABLED must be a boolean")
		}
		cfg.Enabled = value
	}
	if !cfg.Enabled {
		return cfg, nil
	}
	cfg.BaseURL, cfg.Token, cfg.ProductID = getenv("INVOICE_NINJA_BASE_URL"), getenv("INVOICE_NINJA_API_TOKEN"), getenv("INVOICE_NINJA_PRODUCT_ID")
	cfg.PaymentPhone, cfg.PaymentBank, cfg.PaymentRecipient = getenv("PAYMENT_PHONE"), getenv("PAYMENT_BANK"), getenv("PAYMENT_RECIPIENT")
	if getenv("INVOICE_NINJA_TELEGRAM_ID_FIELD") != "id_number" {
		return Config{}, errors.New("registration writes require INVOICE_NINJA_TELEGRAM_ID_FIELD=id_number")
	}
	if raw := getenv("INVOICE_NINJA_INCLUSIVE_TAXES"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, errors.New("INVOICE_NINJA_INCLUSIVE_TAXES must be an explicitly chosen boolean")
		}
		cfg.InclusiveTaxes, cfg.TaxModeConfirmed = value, true
	}
	if raw := strings.TrimSpace(getenv("REGISTRATION_EXCLUDED_TELEGRAM_IDS")); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
			if err != nil || id <= 0 || strconv.FormatInt(id, 10) != strings.TrimSpace(part) {
				return Config{}, errors.New("invalid REGISTRATION_EXCLUDED_TELEGRAM_IDS")
			}
			cfg.ExcludedTelegramIDs = append(cfg.ExcludedTelegramIDs, id)
		}
	}
	for _, item := range []struct {
		name   string
		target *int
	}{
		{"REGISTRATION_WORKERS", &cfg.Workers}, {"INTEGRATION_PAGE_SIZE", &cfg.PageSize}, {"INTEGRATION_MAX_PAGES", &cfg.MaxPages},
	} {
		if raw := getenv(item.name); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil {
				return Config{}, errors.New(item.name + " must be an integer")
			}
			*item.target = value
		}
	}
	for _, item := range []struct {
		name   string
		target *time.Duration
	}{
		{"REGISTRATION_POLL_INTERVAL", &cfg.PollInterval}, {"REGISTRATION_HTTP_TIMEOUT", &cfg.HTTPTimeout},
	} {
		if raw := getenv(item.name); raw != "" {
			value, err := time.ParseDuration(raw)
			if err != nil {
				return Config{}, errors.New(item.name + " must be a duration")
			}
			*item.target = value
		}
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if !c.TelegramEnabled {
		return errors.New("REGISTRATION_ENABLED requires TELEGRAM_ENABLED=true")
	}
	if integrationhttp.ValidateBaseURL(c.BaseURL) != nil {
		return errors.New("INVOICE_NINJA_BASE_URL must be a service base URL")
	}
	u, _ := url.Parse(c.BaseURL)
	if strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/api/v1") {
		return errors.New("INVOICE_NINJA_BASE_URL must not include /api/v1")
	}
	if c.Token == "" || strings.IndexFunc(c.Token, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0 {
		return errors.New("INVOICE_NINJA_API_TOKEN must be set without whitespace or control characters")
	}
	if c.ProductID == "" || len(c.ProductID) > 256 || c.ProductID == "." || c.ProductID == ".." || strings.ContainsAny(c.ProductID, "/\\") || strings.IndexFunc(c.ProductID, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0 {
		return errors.New("INVOICE_NINJA_PRODUCT_ID must be one opaque API identifier")
	}
	if !c.TaxModeConfirmed {
		return errors.New("explicitly set INVOICE_NINJA_INCLUSIVE_TAXES to match the product pricing convention")
	}
	if c.PaymentPhone == "" || !validText(c.PaymentPhone, 128) {
		return errors.New("PAYMENT_PHONE must be set and contain at most 128 printable characters")
	}
	if !validText(c.PaymentBank, 128) || !validText(c.PaymentRecipient, 256) {
		return errors.New("PAYMENT_BANK and PAYMENT_RECIPIENT must be bounded printable text")
	}
	if c.Workers < 1 || c.Workers > 4 {
		return errors.New("REGISTRATION_WORKERS must be 1-4")
	}
	if c.PollInterval < 100*time.Millisecond || c.PollInterval > time.Minute {
		return errors.New("REGISTRATION_POLL_INTERVAL must be 100ms-1m")
	}
	if c.HTTPTimeout < time.Second || c.HTTPTimeout > 10*time.Second {
		return errors.New("REGISTRATION_HTTP_TIMEOUT must be 1s-10s")
	}
	if c.PageSize < 1 || c.PageSize > 100 || c.MaxPages < 1 || c.MaxPages > 100 {
		return errors.New("registration pagination bounds must each be 1-100")
	}
	if len(c.ExcludedTelegramIDs) > 1000 {
		return errors.New("too many registration exclusions")
	}
	seen := make(map[int64]bool, len(c.ExcludedTelegramIDs))
	for _, id := range c.ExcludedTelegramIDs {
		if id <= 0 || seen[id] {
			return errors.New("invalid registration exclusions")
		}
		seen[id] = true
	}
	return nil
}

func validText(value string, limit int) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) == value && utf8.RuneCountInString(value) <= limit && strings.IndexFunc(value, unicode.IsControl) < 0
}
