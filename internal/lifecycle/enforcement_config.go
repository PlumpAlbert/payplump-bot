package lifecycle

import (
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

// EnforcementConfig is a separate gate from Stage 1 invoice observations.
// Enabling notices can never authorize writes to Invoice Ninja or S-UI.
type EnforcementConfig struct {
	Enabled, ExclusiveWriter  bool
	BaseURL, Token            string
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
	cfg := EnforcementConfig{Workers: 1, MaxAccounts: 1000, PollInterval: time.Second, HTTPTimeout: 5 * time.Second}
	if raw := os.Getenv("INVOICE_OVERDUE_ENFORCEMENT_ENABLED"); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return EnforcementConfig{}, errors.New("INVOICE_OVERDUE_ENFORCEMENT_ENABLED must be a boolean")
		}
		cfg.Enabled = enabled
	}
	if !cfg.Enabled {
		return cfg, nil
	}
	if !stageOneEnabled || !telegramEnabled || !paymentsEnabled {
		return EnforcementConfig{}, errors.New("invoice overdue enforcement requires Stage 1, Telegram and payment verification")
	}
	cfg.BaseURL, cfg.Token = os.Getenv("INVOICE_NINJA_BASE_URL"), os.Getenv("INVOICE_NINJA_API_TOKEN")
	cfg.SUIBaseURL, cfg.SUIToken, cfg.SUIVersion = os.Getenv("SUI_BASE_URL"), os.Getenv("SUI_API_TOKEN"), os.Getenv("SUI_VERSION")
	if version := strings.TrimPrefix(os.Getenv("INVOICE_NINJA_VERSION"), "v"); version != "" && version != invoiceninja.SupportedRecurringVersion {
		return EnforcementConfig{}, errors.New("invoice overdue enforcement supports Invoice Ninja 5.13.47 only")
	}
	if raw := os.Getenv("INVOICE_OVERDUE_EXCLUSIVE_WRITER"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return EnforcementConfig{}, errors.New("INVOICE_OVERDUE_EXCLUSIVE_WRITER must be a boolean")
		}
		cfg.ExclusiveWriter = value
	}
	if !cfg.ExclusiveWriter {
		return EnforcementConfig{}, errors.New("invoice overdue enforcement requires INVOICE_OVERDUE_EXCLUSIVE_WRITER=true acknowledgement")
	}
	if raw := strings.TrimSpace(os.Getenv("INVOICE_LIFECYCLE_ALLOWED_TELEGRAM_IDS")); raw != "" {
		var err error
		cfg.AllowedTelegramIDs, err = parseIDs(raw, 20)
		if err != nil {
			return EnforcementConfig{}, errors.New("invalid invoice overdue pilot allowlist")
		}
	}
	if raw := strings.TrimSpace(os.Getenv("REGISTRATION_EXCLUDED_TELEGRAM_IDS")); raw != "" {
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
		if raw := os.Getenv(item.key); raw != "" {
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
		if raw := os.Getenv(item.key); raw != "" {
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
	if !stageOneEnabled || !telegramEnabled || !paymentsEnabled || !c.ExclusiveWriter {
		return errors.New("invoice overdue enforcement requires Stage 1, Telegram, payment verification and exclusive writer acknowledgement")
	}
	if integrationhttp.ValidateBaseURL(c.BaseURL) != nil || c.Token == "" || strings.ContainsAny(c.Token, " \r\n\t") {
		return errors.New("invalid invoice overdue Invoice Ninja configuration")
	}
	if !validIDs(c.AllowedTelegramIDs, 1, 20) || !validIDs(c.ExcludedTelegramIDs, 0, 1000) || len(enforcementEffectiveIDs(c.AllowedTelegramIDs, c.ExcludedTelegramIDs)) == 0 {
		return errors.New("invoice overdue enforcement requires an effective pilot allowlist")
	}
	if c.Workers < 1 || c.Workers > 4 || c.MaxAccounts < 1 || c.MaxAccounts > 1000 || c.PollInterval < 100*time.Millisecond || c.PollInterval > time.Minute || c.HTTPTimeout < time.Second || c.HTTPTimeout > 10*time.Second {
		return errors.New("invalid invoice overdue worker limits")
	}
	if _, err := invoiceninja.NewRecurringClient(c.BaseURL, c.Token, c.HTTPTimeout, 50, 20); err != nil {
		return errors.New("invalid Invoice Ninja recurring writer configuration")
	}
	if _, err := sui.NewManagedClient(c.SUIBaseURL, c.SUIToken, c.SUIVersion, c.HTTPTimeout, c.MaxAccounts); err != nil {
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
