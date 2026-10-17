// Package telegram implements the bot's incoming Telegram transport.
package telegram

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Enabled  bool
	Token    string
	AdminIDs map[int64]struct{}
}

// ConfigFromEnv keeps polling opt-in so development and CI do not consume a
// real bot's updates merely because a token was injected into the environment.
func ConfigFromEnv() (Config, error) {
	cfg := Config{AdminIDs: make(map[int64]struct{})}
	if value := os.Getenv("TELEGRAM_ENABLED"); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return Config{}, errors.New("TELEGRAM_ENABLED must be a boolean")
		}
		cfg.Enabled = enabled
	}
	if !cfg.Enabled {
		return cfg, nil
	}
	cfg.Token = os.Getenv("TELEGRAM_BOT_TOKEN")
	if cfg.Token == "" || strings.TrimSpace(cfg.Token) != cfg.Token {
		return Config{}, errors.New("TELEGRAM_BOT_TOKEN must be set without surrounding whitespace")
	}
	for _, value := range strings.Split(os.Getenv("TELEGRAM_ADMIN_IDS"), ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || id <= 0 {
			return Config{}, errors.New("TELEGRAM_ADMIN_IDS must be a comma-separated list of positive Telegram IDs")
		}
		cfg.AdminIDs[id] = struct{}{}
	}
	return cfg, nil
}

func (c Config) IsAdmin(senderID int64) bool {
	_, allowed := c.AdminIDs[senderID]
	return allowed
}
