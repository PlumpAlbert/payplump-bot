package lifecycle

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

// RenewalConfig gates the paid-renewal writer independently from lifecycle observation and overdue enforcement.
type RenewalConfig struct {
	// ManagedClients selects only actors with bot-owned database provenance.
	ManagedClients                          bool
	Enabled, ExclusiveWriter                bool
	BaseURL, Token, InvoiceNinjaVersion     string
	SUIBaseURL, SUIToken, SUIVersion        string
	AllowedTelegramIDs, ExcludedTelegramIDs []int64
	Workers, MaxAccounts                    int
	PollInterval, HTTPTimeout               time.Duration
}

func (RenewalConfig) String() string   { return "[sensitive invoice renewal configuration]" }
func (RenewalConfig) GoString() string { return "[sensitive invoice renewal configuration]" }

func RenewalConfigFromEnv(stageOneEnabled, telegramEnabled, paymentsEnabled bool) (RenewalConfig, error) {
	return RenewalConfigFromEnvironment(os.Getenv, stageOneEnabled, telegramEnabled, paymentsEnabled, false)
}

// RenewalConfigFromEnvironment reads an explicit environment without mutating process globals.
func RenewalConfigFromEnvironment(getenv func(string) string, stageOneEnabled, telegramEnabled, paymentsEnabled bool, managed bool) (RenewalConfig, error) {
	cfg := RenewalConfig{Workers: 1, MaxAccounts: 1000, PollInterval: time.Second, HTTPTimeout: 5 * time.Second}
	if raw := getenv("INVOICE_RENEWAL_ENABLED"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return RenewalConfig{}, errors.New("INVOICE_RENEWAL_ENABLED must be a boolean")
		}
		cfg.Enabled = value
	}
	cfg.ManagedClients = managed
	if !cfg.Enabled {
		return cfg, nil
	}
	if !stageOneEnabled || !telegramEnabled || !paymentsEnabled {
		return RenewalConfig{}, errors.New("paid renewal requires Stage 1, Telegram and payment verification")
	}
	cfg.BaseURL, cfg.Token = getenv("INVOICE_NINJA_BASE_URL"), getenv("INVOICE_NINJA_API_TOKEN")
	cfg.InvoiceNinjaVersion = getenv("INVOICE_NINJA_VERSION")
	cfg.SUIBaseURL, cfg.SUIToken, cfg.SUIVersion = getenv("SUI_BASE_URL"), getenv("SUI_API_TOKEN"), getenv("SUI_VERSION")
	if raw := getenv("INVOICE_RENEWAL_EXCLUSIVE_WRITER"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return RenewalConfig{}, errors.New("INVOICE_RENEWAL_EXCLUSIVE_WRITER must be a boolean")
		}
		cfg.ExclusiveWriter = value
	}
	if !cfg.ExclusiveWriter {
		return RenewalConfig{}, errors.New("paid renewal requires INVOICE_RENEWAL_EXCLUSIVE_WRITER=true acknowledgement")
	}
	if raw := strings.TrimSpace(getenv("INVOICE_LIFECYCLE_ALLOWED_TELEGRAM_IDS")); raw != "" {
		var err error
		cfg.AllowedTelegramIDs, err = parseIDs(raw, 20)
		if err != nil {
			return RenewalConfig{}, errors.New("invalid invoice renewal pilot allowlist")
		}
	}
	if raw := strings.TrimSpace(getenv("REGISTRATION_EXCLUDED_TELEGRAM_IDS")); raw != "" {
		var err error
		cfg.ExcludedTelegramIDs, err = parseIDs(raw, 1000)
		if err != nil {
			return RenewalConfig{}, errors.New("invalid invoice renewal exclusions")
		}
	}
	for _, item := range []struct {
		key string
		dst *int
	}{{"INVOICE_RENEWAL_WORKERS", &cfg.Workers}, {"INVOICE_RENEWAL_MAX_ACCOUNTS", &cfg.MaxAccounts}} {
		if raw := getenv(item.key); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil {
				return RenewalConfig{}, errors.New(item.key + " must be an integer")
			}
			*item.dst = n
		}
	}
	for _, item := range []struct {
		key string
		dst *time.Duration
	}{{"INVOICE_RENEWAL_POLL_INTERVAL", &cfg.PollInterval}, {"INVOICE_RENEWAL_HTTP_TIMEOUT", &cfg.HTTPTimeout}} {
		if raw := getenv(item.key); raw != "" {
			d, err := time.ParseDuration(raw)
			if err != nil {
				return RenewalConfig{}, errors.New(item.key + " must be a duration")
			}
			*item.dst = d
		}
	}
	return cfg, cfg.Validate(stageOneEnabled, telegramEnabled, paymentsEnabled)
}

func (c RenewalConfig) Validate(stageOneEnabled, telegramEnabled, paymentsEnabled bool) error {
	if !c.Enabled {
		return nil
	}
	if !stageOneEnabled || !telegramEnabled || !paymentsEnabled {
		return errors.New("paid renewal requires Stage 1, Telegram and payment verification")
	}
	if !c.ExclusiveWriter {
		return errors.New("paid renewal requires exclusive writer acknowledgement")
	}
	if invoiceninja.ValidateClientConfiguration(c.BaseURL, c.Token, c.HTTPTimeout) != nil || strings.ContainsAny(c.Token, " ") {
		return errors.New("invalid invoice renewal Invoice Ninja configuration")
	}
	if (!c.ManagedClients && (!validIDs(c.AllowedTelegramIDs, 1, 20) || len(enforcementEffectiveIDs(c.AllowedTelegramIDs, c.ExcludedTelegramIDs)) == 0)) || !validIDs(c.ExcludedTelegramIDs, 0, 1000) {
		return errors.New("invoice renewal requires an effective pilot allowlist")
	}
	if c.Workers < 1 || c.Workers > 4 || c.MaxAccounts < 1 || c.MaxAccounts > 1000 || c.PollInterval < 100*time.Millisecond || c.PollInterval > time.Minute || c.HTTPTimeout < time.Second || c.HTTPTimeout > 10*time.Second {
		return errors.New("invalid invoice renewal worker limits")
	}
	if sui.ValidateManagedClientConfiguration(c.SUIBaseURL, c.SUIToken, c.SUIVersion, c.HTTPTimeout, c.MaxAccounts) != nil {
		return errors.New("invalid invoice renewal S-UI configuration")
	}
	return nil
}
