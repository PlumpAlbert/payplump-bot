// Package telegram implements the bot's incoming Telegram transport.
package telegram

import (
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Enabled       bool
	Token         string
	AdminIDs      map[int64]struct{}
	WebhookURL    string
	WebhookSecret string
}

// ConfigFromEnv keeps Telegram opt-in so development and CI do not reconfigure
// a real bot merely because its token was injected into the environment.
func ConfigFromEnv() (Config, error) {
	return ConfigFromEnvironment(os.Getenv)
}

// ConfigFromEnvironment reads an explicit environment without mutating process globals.
func ConfigFromEnvironment(getenv func(string) string) (Config, error) {
	cfg := Config{AdminIDs: make(map[int64]struct{})}
	if value := getenv("TELEGRAM_ENABLED"); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return Config{}, errors.New("TELEGRAM_ENABLED must be a boolean")
		}
		cfg.Enabled = enabled
	}
	if !cfg.Enabled {
		return cfg, nil
	}
	cfg.Token = getenv("TELEGRAM_BOT_TOKEN")
	if cfg.Token == "" || strings.TrimSpace(cfg.Token) != cfg.Token {
		return Config{}, errors.New("TELEGRAM_BOT_TOKEN must be set without surrounding whitespace")
	}
	for _, value := range strings.Split(getenv("TELEGRAM_ADMIN_IDS"), ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || id <= 0 {
			return Config{}, errors.New("TELEGRAM_ADMIN_IDS must be a comma-separated list of positive Telegram IDs")
		}
		cfg.AdminIDs[id] = struct{}{}
	}
	cfg.WebhookURL = getenv("TELEGRAM_WEBHOOK_URL")
	if cfg.WebhookURL == "" {
		// A webhook secret has no meaning in polling mode.
		return cfg, nil
	}
	if !validWebhookURL(cfg.WebhookURL) {
		return Config{}, errors.New("TELEGRAM_WEBHOOK_URL must be an HTTPS URL with path /telegram/webhook without userinfo, query or fragment")
	}
	cfg.WebhookSecret = getenv("TELEGRAM_WEBHOOK_SECRET")
	if !validWebhookSecret(cfg.WebhookSecret) {
		return Config{}, errors.New("TELEGRAM_WEBHOOK_SECRET must contain 1-256 ASCII letters, digits, underscores or hyphens")
	}
	return cfg, nil
}

const WebhookPath = "/telegram/webhook"

func validWebhookURL(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Path == WebhookPath && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.Opaque == ""
}

func validWebhookSecret(value string) bool {
	if len(value) == 0 || len(value) > 256 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func (c Config) UsesWebhook() bool { return c.WebhookURL != "" }

func (c Config) IsAdmin(senderID int64) bool {
	_, allowed := c.AdminIDs[senderID]
	return allowed
}
