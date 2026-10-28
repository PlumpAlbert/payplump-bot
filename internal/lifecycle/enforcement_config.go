package lifecycle

import (
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

// EnforcementConfig is a separate gate from Stage 1 invoice observations.
// Enabling notices can never authorize writes to Invoice Ninja or S-UI.
type EnforcementConfig struct {
	// ManagedClients selects only actors with bot-owned database provenance.
	ManagedClients            bool
	Enabled, ExclusiveWriter  bool
	BaseURL, Token            string
	InvoiceNinjaVersion       string
	SUIBaseURL, SUIToken      string
	SUIVersion                string
	AllowedTelegramIDs        []int64
	ExcludedTelegramIDs       []int64
	Workers, MaxAccounts      int
	PollInterval, HTTPTimeout time.Duration
}

func (EnforcementConfig) String() string {
	return "[sensitive invoice overdue enforcement configuration]"
}
func (EnforcementConfig) GoString() string {
	return "[sensitive invoice overdue enforcement configuration]"
}

func EnforcementConfigFromEnv(stageOneEnabled, telegramEnabled, paymentsEnabled bool) (EnforcementConfig, error) {
	return EnforcementConfigFromEnvironment(os.Getenv, stageOneEnabled, telegramEnabled, paymentsEnabled, false)
}

// EnforcementConfigFromEnvironment reads an explicit environment without mutating process globals.
func EnforcementConfigFromEnvironment(getenv func(string) string, stageOneEnabled, telegramEnabled, paymentsEnabled bool, managed bool) (EnforcementConfig, error) {
	cfg := EnforcementConfig{Workers: 1, MaxAccounts: 1000, PollInterval: time.Second, HTTPTimeout: 5 * time.Second}
	if raw := getenv("INVOICE_OVERDUE_ENFORCEMENT_ENABLED"); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return EnforcementConfig{}, errors.New("INVOICE_OVERDUE_ENFORCEMENT_ENABLED must be a boolean")
		}
		cfg.Enabled = enabled
	}
	cfg.ManagedClients = managed
	if !cfg.Enabled {
		return cfg, nil
	}
	if !stageOneEnabled || !telegramEnabled || !paymentsEnabled {
		return EnforcementConfig{}, errors.New("invoice overdue enforcement requires Stage 1, Telegram and payment verification")
	}
	cfg.BaseURL, cfg.Token = getenv("INVOICE_NINJA_BASE_URL"), getenv("INVOICE_NINJA_API_TOKEN")
	cfg.InvoiceNinjaVersion = getenv("INVOICE_NINJA_VERSION")
	cfg.SUIBaseURL, cfg.SUIToken, cfg.SUIVersion = getenv("SUI_BASE_URL"), getenv("SUI_API_TOKEN"), getenv("SUI_VERSION")
	if raw := getenv("INVOICE_OVERDUE_EXCLUSIVE_WRITER"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return EnforcementConfig{}, errors.New("INVOICE_OVERDUE_EXCLUSIVE_WRITER must be a boolean")
		}
		cfg.ExclusiveWriter = value
	}
	if !cfg.ExclusiveWriter {
		return EnforcementConfig{}, errors.New("invoice overdue enforcement requires INVOICE_OVERDUE_EXCLUSIVE_WRITER=true acknowledgement")
	}
	if raw := strings.TrimSpace(getenv("INVOICE_LIFECYCLE_ALLOWED_TELEGRAM_IDS")); raw != "" {
		var err error
		cfg.AllowedTelegramIDs, err = parseIDs(raw, 20)
		if err != nil {
			return EnforcementConfig{}, errors.New("invalid invoice overdue pilot allowlist")
		}
	}
	if raw := strings.TrimSpace(getenv("REGISTRATION_EXCLUDED_TELEGRAM_IDS")); raw != "" {
		var err error
		cfg.ExcludedTelegramIDs, err = parseIDs(raw, 1000)
		if err != nil {
			return EnforcementConfig{}, errors.New("invalid invoice overdue exclusions")
		}
	}
	for _, item := range []struct {
		key    string
		target *int
	}{{"INVOICE_OVERDUE_WORKERS", &cfg.Workers}, {"INVOICE_OVERDUE_MAX_ACCOUNTS", &cfg.MaxAccounts}} {
		if raw := getenv(item.key); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil {
				return EnforcementConfig{}, errors.New(item.key + " must be an integer")
			}
			*item.target = value
		}
	}
	for _, item := range []struct {
		key    string
		target *time.Duration
	}{{"INVOICE_OVERDUE_POLL_INTERVAL", &cfg.PollInterval}, {"INVOICE_OVERDUE_HTTP_TIMEOUT", &cfg.HTTPTimeout}} {
		if raw := getenv(item.key); raw != "" {
			value, err := time.ParseDuration(raw)
			if err != nil {
				return EnforcementConfig{}, errors.New(item.key + " must be a duration")
			}
			*item.target = value
		}
	}
	return cfg, cfg.Validate(stageOneEnabled, telegramEnabled, paymentsEnabled)
}

func (c EnforcementConfig) Validate(stageOneEnabled, telegramEnabled, paymentsEnabled bool) error {
	if !c.Enabled {
		return nil
	}
	if !stageOneEnabled || !telegramEnabled || !paymentsEnabled {
		return errors.New("invoice overdue enforcement requires Stage 1, Telegram and payment verification")
	}
	return c.validateSettings()
}

func (c EnforcementConfig) validateSettings() error {
	if !c.Enabled {
		return nil
	}
	if !c.ExclusiveWriter {
		return errors.New("invoice overdue enforcement requires exclusive writer acknowledgement")
	}
	if invoiceninja.ValidateClientConfiguration(c.BaseURL, c.Token, c.HTTPTimeout) != nil || strings.ContainsAny(c.Token, " ") {
		return errors.New("invalid invoice overdue Invoice Ninja configuration")
	}
	if (!c.ManagedClients && (!validIDs(c.AllowedTelegramIDs, 1, 20) || len(enforcementEffectiveIDs(c.AllowedTelegramIDs, c.ExcludedTelegramIDs)) == 0)) || !validIDs(c.ExcludedTelegramIDs, 0, 1000) {
		return errors.New("invoice overdue enforcement requires an effective pilot allowlist")
	}
	if c.Workers < 1 || c.Workers > 4 || c.MaxAccounts < 1 || c.MaxAccounts > 1000 || c.PollInterval < 100*time.Millisecond || c.PollInterval > time.Minute || c.HTTPTimeout < time.Second || c.HTTPTimeout > 10*time.Second {
		return errors.New("invalid invoice overdue worker limits")
	}
	if sui.ValidateManagedClientConfiguration(c.SUIBaseURL, c.SUIToken, c.SUIVersion, c.HTTPTimeout, c.MaxAccounts) != nil {
		return errors.New("invalid S-UI writer configuration")
	}
	return nil
}

func enforcementEffectiveIDs(allowed, excluded []int64) []int64 {
	out := make([]int64, 0, len(allowed))
	for _, id := range allowed {
		if !slices.Contains(excluded, id) {
			out = append(out, id)
		}
	}
	return out
}
