// Package outbox delivers durable Telegram messages independently of producers.
package outbox

import (
	"errors"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Workers      int
	PollInterval time.Duration
}

const (
	workersConfigError = "OUTBOX_WORKERS must be an integer from 1 to 16"
	pollConfigError    = "OUTBOX_POLL_INTERVAL must be a duration from 100ms to 1m"
)

func ConfigFromEnv() (Config, error) {
	cfg := Config{Workers: 2, PollInterval: time.Second}
	if value := os.Getenv("OUTBOX_WORKERS"); value != "" {
		workers, err := strconv.Atoi(value)
		if err != nil {
			return Config{}, errors.New(workersConfigError)
		}
		cfg.Workers = workers
	}
	if value := os.Getenv("OUTBOX_POLL_INTERVAL"); value != "" {
		interval, err := time.ParseDuration(value)
		if err != nil {
			return Config{}, errors.New(pollConfigError)
		}
		cfg.PollInterval = interval
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) validate() error {
	if c.Workers < 1 || c.Workers > 16 {
		return errors.New(workersConfigError)
	}
	if c.PollInterval < 100*time.Millisecond || c.PollInterval > time.Minute {
		return errors.New(pollConfigError)
	}
	return nil
}
