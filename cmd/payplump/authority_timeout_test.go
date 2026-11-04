package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/postgres"
)

func TestConfiguredAuthorityTimeoutReachesAllProviderConstructors(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	for _, raw := range []string{"500ms", "30s", "31s", "2m"} {
		t.Run(raw, func(t *testing.T) {
			values := normalServiceValues()
			values["INVOICE_NINJA_BASE_URL"] = server.URL + "/ninja"
			values["SUI_BASE_URL"] = server.URL + "/panel"
			values["ACTIVATION_HTTP_TIMEOUT"] = raw
			values["ACTIVATION_POLL_INTERVAL"] = "10s"
			if err := parseNormalService(values); err != nil {
				t.Fatalf("normal service settings rejected: %v", err)
			}
			get, managed, err := serviceEnvironment(func(key string) string { return values[key] })
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := activation.AuthorityConfigFromEnvironment(get, true, true, managed)
			if err != nil {
				t.Fatal(err)
			}
			want, err := time.ParseDuration(raw)
			if err != nil || cfg.HTTPTimeout != want || cfg.PollInterval != 10*time.Second {
				t.Fatal("configured timeout or poll interval was replaced")
			}
			providers, err := newAuthorityActivationProviders(cfg)
			if err != nil || providers.Payments == nil || providers.Products == nil || providers.Recurring == nil || providers.SUI != nil {
				t.Fatalf("authority activation provider construction: %v", err)
			}
			// Construction must not use storage or call any provider. No database
			// is opened; an uninitialized Store would fail on accidental DB use.
			reconciler, err := newAuthorityReconciler(&postgres.Store{}, 123, []int64{101}, cfg, nil)
			if err != nil || reconciler == nil {
				t.Fatalf("authority inventory/create-only provider construction: %v", err)
			}
			if requests.Load() != 0 {
				t.Fatal("provider construction issued an HTTP request")
			}
		})
	}
}
