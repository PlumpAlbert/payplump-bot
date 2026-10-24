package broadcast

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"time"
)

// Config contains only local authorization and bounded fanout policy.
type Config struct {
	Enabled       bool
	Revision      int64
	MaxRecipients int
	BatchSize     int
	Workers       int
	PollInterval  time.Duration
	AdminIDs      map[int64]struct{}
}

func (Config) String() string               { return "[private broadcast policy]" }
func (Config) GoString() string             { return "[private broadcast policy]" }
func (Config) LogValue() slog.Value         { return slog.StringValue("[private broadcast policy]") }
func (Config) MarshalJSON() ([]byte, error) { return json.Marshal("[private broadcast policy]") }

func ConfigFromEnv(telegramEnabled bool, telegramAdmins map[int64]struct{}) (Config, error) {
	return ConfigFromEnvironment(os.Getenv, telegramEnabled, telegramAdmins)
}

// ConfigFromEnvironment reads an explicit environment without mutating process globals.
func ConfigFromEnvironment(getenv func(string) string, telegramEnabled bool, telegramAdmins map[int64]struct{}) (Config, error) {
	c := Config{AdminIDs: cloneAdmins(telegramAdmins), MaxRecipients: 1000, BatchSize: 50, Workers: 1, PollInterval: time.Second}
	if v := getenv("BROADCAST_ENABLED"); v != "" {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, errors.New("BROADCAST_ENABLED must be a boolean")
		}
		c.Enabled = enabled
	}
	if !c.Enabled {
		return c, nil
	}
	if !telegramEnabled {
		return Config{}, errors.New("broadcast requires Telegram")
	}
	raw := getenv("BROADCAST_POLICY_REVISION")
	rev, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || rev < 1 || strconv.FormatInt(rev, 10) != raw {
		return Config{}, errors.New("BROADCAST_POLICY_REVISION must be an explicit positive canonical integer")
	}
	c.Revision = rev
	if raw := getenv("BROADCAST_POLL_INTERVAL"); raw != "" {
		d, parseErr := time.ParseDuration(raw)
		if parseErr != nil || d < 100*time.Millisecond || d > time.Minute {
			return Config{}, errors.New("invalid broadcast poll interval")
		}
		c.PollInterval = d
	}
	if raw := getenv("BROADCAST_WORKERS"); raw != "" {
		c.Workers, err = strconv.Atoi(raw)
		if err != nil {
			return Config{}, errors.New("invalid broadcast worker count")
		}
	}
	if raw := getenv("BROADCAST_BATCH_SIZE"); raw != "" {
		c.BatchSize, err = strconv.Atoi(raw)
		if err != nil {
			return Config{}, errors.New("invalid broadcast batch size")
		}
	}
	if raw := getenv("BROADCAST_MAX_RECIPIENTS"); raw != "" {
		c.MaxRecipients, err = strconv.Atoi(raw)
		if err != nil {
			return Config{}, errors.New("invalid broadcast recipient limit")
		}
	}
	if err := c.Validate(telegramEnabled); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) Policy() Policy {
	if !c.Enabled {
		return Policy{}
	}
	ids := make([]int64, 0, len(c.AdminIDs))
	for id := range c.AdminIDs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return Policy{Enabled: true, Revision: c.Revision, Hash: policyHash(ids, c.MaxRecipients), AdminIDs: ids, MaxRecipients: c.MaxRecipients}
}

func (c Config) Validate(telegramEnabled bool) error {
	if !c.Enabled {
		return nil
	}
	if !telegramEnabled || c.Revision < 1 || len(c.AdminIDs) == 0 || len(c.AdminIDs) > 1000 || c.MaxRecipients < 1 || c.MaxRecipients > 10000 || c.BatchSize < 1 || c.BatchSize > 50 || c.Workers < 1 || c.Workers > 8 || c.PollInterval < 100*time.Millisecond || c.PollInterval > time.Minute {
		return errors.New("invalid broadcast policy")
	}
	for id := range c.AdminIDs {
		if id <= 0 {
			return errors.New("invalid broadcast administrator scope")
		}
	}
	return nil
}
func cloneAdmins(ids map[int64]struct{}) map[int64]struct{} {
	out := make(map[int64]struct{}, len(ids))
	for id := range ids {
		out[id] = struct{}{}
	}
	return out
}
func (c Config) Allows(id int64) bool { _, ok := c.AdminIDs[id]; return c.Enabled && id > 0 && ok }
