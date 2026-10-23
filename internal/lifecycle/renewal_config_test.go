package lifecycle

import (
	"testing"
	"time"
)

func TestRenewalConfigIsDefaultOffAndSettingsAreBounded(t *testing.T) {
	t.Setenv("INVOICE_RENEWAL_ENABLED", "")
	cfg, err := RenewalConfigFromEnv(false, false, false)
	if err != nil || cfg.Enabled {
		t.Fatalf("default config=%+v err=%v", cfg, err)
	}
	if err := cfg.Validate(false, false, false); err != nil {
		t.Fatal(err)
	}

	base := RenewalConfig{Enabled: true, ExclusiveWriter: true, BaseURL: "https://billing.example", Token: "synthetic-token", InvoiceNinjaVersion: "5.13.47", SUIBaseURL: "https://panel.example", SUIToken: "synthetic-panel-token", SUIVersion: "1.6.4", AllowedTelegramIDs: []int64{101}, Workers: 1, MaxAccounts: 1000, PollInterval: time.Second, HTTPTimeout: 5 * time.Second}
	if err := base.Validate(true, true, true); err != nil {
		t.Fatalf("valid pilot settings rejected: %v", err)
	}
	if err := base.Validate(false, true, true); err == nil {
		t.Fatal("missing dependency was accepted")
	}
	cases := map[string]func(*RenewalConfig){
		"exclusive-writer": func(c *RenewalConfig) { c.ExclusiveWriter = false },
		"allowlist-cap": func(c *RenewalConfig) {
			c.AllowedTelegramIDs = make([]int64, 21)
			for i := range c.AllowedTelegramIDs {
				c.AllowedTelegramIDs[i] = int64(i + 1)
			}
		},
		"workers":                     func(c *RenewalConfig) { c.Workers = 5 },
		"max-accounts":                func(c *RenewalConfig) { c.MaxAccounts = 1001 },
		"timeout":                     func(c *RenewalConfig) { c.HTTPTimeout = 11 * time.Second },
		"allowlist-excluded":          func(c *RenewalConfig) { c.ExcludedTelegramIDs = []int64{101} },
		"unsupported-invoice-version": func(c *RenewalConfig) { c.InvoiceNinjaVersion = "5.14.0" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := base
			mutate(&c)
			if err := c.Validate(true, true, true); err == nil {
				t.Fatal("unsafe renewal configuration accepted")
			}
		})
	}
}

func TestRenewalEffectivePilotExclusionsWin(t *testing.T) {
	got := enforcementEffectiveIDs([]int64{101, 202}, []int64{101})
	if len(got) != 1 || got[0] != 202 {
		t.Fatalf("exclusions did not win: %v", got)
	}
}
