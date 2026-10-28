// Package integrations provides an explicit read-only integration preflight.
// The regular bot process does not activate external billing or access changes.
package integrations

import (
	"errors"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
)

type InvoiceNinjaConfig struct {
	BaseURL         string
	Token           string
	ProductID       string
	TelegramIDField string
	ExpectedVersion string
}

type SUIConfig struct {
	BaseURL             string
	Token               string
	InboundIDs          []int64
	SubscriptionBaseURL string
	DeclaredVersion     string
}

type Config struct {
	InvoiceNinja InvoiceNinjaConfig
	SUI          SUIConfig
	HTTPTimeout  time.Duration
	PageSize     int
	MaxPages     int
}

var versionPattern = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9.-]+)?$`)

func ConfigFromEnv() (Config, error) {
	cfg := Config{
		InvoiceNinja: InvoiceNinjaConfig{
			BaseURL: os.Getenv("INVOICE_NINJA_BASE_URL"), Token: os.Getenv("INVOICE_NINJA_API_TOKEN"),
			ProductID: os.Getenv("INVOICE_NINJA_PRODUCT_ID"), TelegramIDField: os.Getenv("INVOICE_NINJA_TELEGRAM_ID_FIELD"),
			ExpectedVersion: os.Getenv("INVOICE_NINJA_VERSION"),
		},
		SUI: SUIConfig{
			BaseURL: os.Getenv("SUI_BASE_URL"), Token: os.Getenv("SUI_API_TOKEN"),
			SubscriptionBaseURL: os.Getenv("SUI_SUBSCRIPTION_BASE_URL"), DeclaredVersion: os.Getenv("SUI_VERSION"),
		},
		HTTPTimeout: 10 * time.Second, PageSize: 50, MaxPages: 20,
	}
	for _, value := range strings.Split(os.Getenv("SUI_INBOUND_IDS"), ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || id <= 0 {
			return Config{}, errors.New("SUI_INBOUND_IDS must contain positive integer IDs separated by commas")
		}
		cfg.SUI.InboundIDs = append(cfg.SUI.InboundIDs, id)
	}
	if raw := os.Getenv("INTEGRATION_HTTP_TIMEOUT"); raw != "" {
		value, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, errors.New("INTEGRATION_HTTP_TIMEOUT must be a duration from 1s to 30s")
		}
		cfg.HTTPTimeout = value
	}
	for _, setting := range []struct {
		name   string
		target *int
	}{
		{"INTEGRATION_PAGE_SIZE", &cfg.PageSize}, {"INTEGRATION_MAX_PAGES", &cfg.MaxPages},
	} {
		if raw := os.Getenv(setting.name); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil {
				return Config{}, errors.New(setting.name + " must be an integer from 1 to 100")
			}
			*setting.target = value
		}
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	for _, item := range []struct{ name, value, suffix string }{
		{"INVOICE_NINJA_BASE_URL", c.InvoiceNinja.BaseURL, "/api/v1"},
		{"SUI_BASE_URL", c.SUI.BaseURL, "/apiv2"},
		{"SUI_SUBSCRIPTION_BASE_URL", c.SUI.SubscriptionBaseURL, ""},
	} {
		if integrationhttp.ValidateBaseURL(item.value) != nil {
			return errors.New(item.name + " must be an HTTP(S) base URL without credentials, query or fragment")
		}
		u, _ := url.Parse(item.value)
		if item.suffix != "" && strings.HasSuffix(strings.TrimRight(u.Path, "/"), item.suffix) {
			return errors.New(item.name + " must not include " + item.suffix)
		}
	}
	for _, item := range []struct{ name, value string }{
		{"INVOICE_NINJA_API_TOKEN", c.InvoiceNinja.Token}, {"SUI_API_TOKEN", c.SUI.Token},
	} {
		if item.value == "" || strings.IndexFunc(item.value, unicode.IsSpace) >= 0 || strings.IndexFunc(item.value, unicode.IsControl) >= 0 {
			return errors.New(item.name + " must be set without whitespace or control characters")
		}
	}
	if c.InvoiceNinja.ProductID == "" || len(c.InvoiceNinja.ProductID) > 256 || strings.ContainsAny(c.InvoiceNinja.ProductID, "/\\") || c.InvoiceNinja.ProductID == "." || c.InvoiceNinja.ProductID == ".." || strings.IndexFunc(c.InvoiceNinja.ProductID, unicode.IsSpace) >= 0 || strings.IndexFunc(c.InvoiceNinja.ProductID, unicode.IsControl) >= 0 {
		return errors.New("INVOICE_NINJA_PRODUCT_ID must contain one nonempty API identifier")
	}
	switch c.InvoiceNinja.TelegramIDField {
	case "id_number", "custom_value1", "custom_value2", "custom_value3", "custom_value4":
	default:
		return errors.New("INVOICE_NINJA_TELEGRAM_ID_FIELD must be id_number or custom_value1-custom_value4, not id or number")
	}
	if len(c.SUI.InboundIDs) == 0 || len(c.SUI.InboundIDs) > 100 {
		return errors.New("SUI_INBOUND_IDS must contain 1-100 unique positive integer IDs")
	}
	seen := make(map[int64]bool)
	for _, id := range c.SUI.InboundIDs {
		if id <= 0 || seen[id] {
			return errors.New("SUI_INBOUND_IDS must contain 1-100 unique positive integer IDs")
		}
		seen[id] = true
	}
	if c.HTTPTimeout < time.Second || c.HTTPTimeout > 30*time.Second {
		return errors.New("INTEGRATION_HTTP_TIMEOUT must be a duration from 1s to 30s")
	}
	if c.PageSize < 1 || c.PageSize > 100 {
		return errors.New("INTEGRATION_PAGE_SIZE must be an integer from 1 to 100")
	}
	if c.MaxPages < 1 || c.MaxPages > 100 {
		return errors.New("INTEGRATION_MAX_PAGES must be an integer from 1 to 100")
	}
	return nil
}
