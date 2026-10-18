package clientmenu_test

import (
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/clientmenu"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
)

func configurationMenuConfig() clientmenu.Config {
	c := menuConfig()
	c.ConfigurationEnabled = true
	c.ConfigurationPolicyRevision = 7
	c.ConfigurationPolicyHash = [32]byte{1}
	c.ConfigurationAllowedTelegramIDs = []int64{menuTelegramID}
	return c
}
func TestConfigurationMenuPreparesOnlyNonsensitiveTypedIntent(t *testing.T) {
	cfg := configurationMenuConfig()
	snapshot := menuSnapshot()
	snapshot.State = "active"
	snapshot.DesiredAccess = true
	snapshot.SUIID = "17"
	snapshot.RecurringID = "recurring-synthetic"
	reader := menuReader()
	p := prepareMenu(t, newMenuService(t, cfg, reader), clientmenu.Configuration, snapshot)
	assertMenuCalls(t, reader)
	if !p.ProtectedConfiguration || p.Text != queue.ConfigurationIntentBody || p.ConfigurationPolicyRevision != 7 || p.ConfigurationPolicyHash != cfg.ConfigurationPolicyHash {
		t.Fatal("eligible actor did not prepare a secret-free typed intent")
	}
	assertNoMenuText(t, p.Text, snapshot.CustomerID, snapshot.InvoiceID, snapshot.SUIID, snapshot.RecurringID, "https://", "format=json")
	// These are only local preconditions; full provenance and current external
	// entitlement are checked separately at atomic enqueue and actual dispatch.
	for _, change := range []func(*clientmenu.Config, *clientmenu.Snapshot){func(c *clientmenu.Config, _ *clientmenu.Snapshot) { c.ConfigurationEnabled = false }, func(c *clientmenu.Config, _ *clientmenu.Snapshot) {
		c.ConfigurationAllowedTelegramIDs = []int64{menuTelegramID + 1}
	}, func(c *clientmenu.Config, _ *clientmenu.Snapshot) { c.ExcludedTelegramIDs = []int64{menuTelegramID} }, func(_ *clientmenu.Config, s *clientmenu.Snapshot) { s.Known = false }, func(_ *clientmenu.Config, s *clientmenu.Snapshot) { s.State = "overdue" }, func(_ *clientmenu.Config, s *clientmenu.Snapshot) { s.DesiredAccess = false }, func(_ *clientmenu.Config, s *clientmenu.Snapshot) { s.PaymentReview = true }, func(_ *clientmenu.Config, s *clientmenu.Snapshot) { s.RecurringID = "" }, func(_ *clientmenu.Config, s *clientmenu.Snapshot) { s.SUIID = "" }} {
		c, s := configurationMenuConfig(), snapshot
		change(&c, &s)
		reader := menuReader()
		p := prepareMenu(t, newMenuService(t, c, reader), clientmenu.Configuration, s)
		assertMenuCalls(t, reader)
		if p.ProtectedConfiguration || p.Text == queue.ConfigurationIntentBody {
			t.Fatal("ineligible actor received typed configuration intent")
		}
		assertNoMenuText(t, p.Text, "format=json", snapshot.CustomerID, snapshot.InvoiceID, snapshot.RecurringID)
	}
}
func TestConfigurationMenuScopeIsValidatedAndCopied(t *testing.T) {
	for _, change := range []func(*clientmenu.Config){func(c *clientmenu.Config) { c.ConfigurationPolicyRevision = 0 }, func(c *clientmenu.Config) { c.ConfigurationPolicyHash = [32]byte{} }, func(c *clientmenu.Config) { c.ConfigurationAllowedTelegramIDs = nil }, func(c *clientmenu.Config) { c.ConfigurationAllowedTelegramIDs = []int64{0} }, func(c *clientmenu.Config) {
		c.ConfigurationAllowedTelegramIDs = []int64{menuTelegramID, menuTelegramID}
	}, func(c *clientmenu.Config) { c.ConfigurationAllowedTelegramIDs = make([]int64, 21) }} {
		c := configurationMenuConfig()
		change(&c)
		if _, err := clientmenu.New(c, nil); err == nil {
			t.Fatal("invalid enabled configuration policy accepted")
		}
	}
	c := configurationMenuConfig()
	service := newMenuService(t, c, nil)
	c.ConfigurationAllowedTelegramIDs[0] = menuTelegramID + 1
	snapshot := menuSnapshot()
	snapshot.State = "active"
	snapshot.DesiredAccess = true
	snapshot.SUIID = "17"
	snapshot.RecurringID = "recurring-synthetic"
	if !prepareMenu(t, service, clientmenu.Configuration, snapshot).ProtectedConfiguration {
		t.Fatal("caller mutation changed actor authorization")
	}
}
