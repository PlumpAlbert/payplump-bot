package registration

import (
	"io"
	"log/slog"
	"reflect"
	"testing"
)

func TestExplicitExclusionsNeverReachBillingAPI(t *testing.T) {
	f := newWorkerFixture(t)
	cfg := testConfig()
	cfg.ExcludedTelegramIDs = []int64{f.store.states[1].snapshot.TelegramID}
	worker, err := New(f.store, f.api, 77, []int64{91}, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ExcludedTelegramIDs[0] = 999 // Caller cannot mutate the worker's allow/deny policy.
	f.worker = worker
	f.pass()
	f.wantHeld(ReviewExcludedUser)
	f.wantPosts(0, 0, 0)
	if !reflect.DeepEqual(f.store.events, []string{"load", "hold"}) {
		t.Fatalf("excluded account reached external API or journal: %v", f.store.events)
	}
}

func TestExclusionsRequireExactPositiveUniqueTelegramIDs(t *testing.T) {
	configEnvironment(t, true)
	t.Setenv("REGISTRATION_EXCLUDED_TELEGRAM_IDS", "123, 456")
	cfg, err := ConfigFromEnv(true)
	if err != nil || !reflect.DeepEqual(cfg.ExcludedTelegramIDs, []int64{123, 456}) {
		t.Fatal("valid exclusions rejected")
	}
	for _, raw := range []string{"0", "-1", "abc", "0123", "+123", "123,", "123,123"} {
		t.Setenv("REGISTRATION_EXCLUDED_TELEGRAM_IDS", raw)
		if _, err := ConfigFromEnv(true); err == nil {
			t.Fatal("invalid exclusions accepted")
		}
	}
	t.Setenv("REGISTRATION_ENABLED", "false")
	if _, err := ConfigFromEnv(false); err != nil {
		t.Fatal("disabled feature parsed unused exclusions")
	}
}
