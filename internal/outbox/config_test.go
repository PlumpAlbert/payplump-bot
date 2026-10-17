package outbox

import (
	"strings"
	"testing"
	"time"
)

func TestConfigFromEnv(t *testing.T) {
	for _, tc := range []struct {
		name, workers, interval string
		want                    Config
	}{
		{"defaults", "", "", Config{2, time.Second}},
		{"minimum", "1", "100ms", Config{1, 100 * time.Millisecond}},
		{"maximum", "16", "1m", Config{16, time.Minute}},
		{"custom", "4", "2.5s", Config{4, 2500 * time.Millisecond}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OUTBOX_WORKERS", tc.workers)
			t.Setenv("OUTBOX_POLL_INTERVAL", tc.interval)
			got, err := ConfigFromEnv()
			if err != nil || got != tc.want {
				t.Fatalf("ConfigFromEnv() = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func TestConfigRejectsInvalidValues(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"OUTBOX_WORKERS", "0"},
		{"OUTBOX_WORKERS", "-1"},
		{"OUTBOX_WORKERS", "17"},
		{"OUTBOX_WORKERS", "1.5"},
		{"OUTBOX_WORKERS", "99999999999999999999999999"},
		{"OUTBOX_WORKERS", "private-config-value"},
		{"OUTBOX_POLL_INTERVAL", "0"},
		{"OUTBOX_POLL_INTERVAL", "-1s"},
		{"OUTBOX_POLL_INTERVAL", "99.999999ms"},
		{"OUTBOX_POLL_INTERVAL", "1m1ns"},
		{"OUTBOX_POLL_INTERVAL", "100"},
		{"OUTBOX_POLL_INTERVAL", "private-config-value"},
	} {
		t.Run(tc.key+"/"+tc.value, func(t *testing.T) {
			t.Setenv("OUTBOX_WORKERS", "")
			t.Setenv("OUTBOX_POLL_INTERVAL", "")
			t.Setenv(tc.key, tc.value)
			_, err := ConfigFromEnv()
			if err == nil || !strings.Contains(err.Error(), tc.key) || strings.Contains(err.Error(), "private-config-value") {
				t.Fatalf("expected a useful sanitized error, got %v", err)
			}
		})
	}
}

func TestConfigValidation(t *testing.T) {
	for _, cfg := range []Config{
		{}, {0, time.Second}, {17, time.Second}, {1, 0},
		{1, 100*time.Millisecond - time.Nanosecond},
		{1, time.Minute + time.Nanosecond},
	} {
		if cfg.validate() == nil {
			t.Errorf("accepted invalid config %+v", cfg)
		}
	}
}
