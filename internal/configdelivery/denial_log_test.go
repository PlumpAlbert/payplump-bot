package configdelivery

import (
	"strings"
	"testing"
)

func TestConfigurationDenialLogsOnlyTechnicalReason(t *testing.T) {
	f := newDeliveryFixture(t)
	f.s.cfg.Enabled = false
	if _, err := f.deliver(); err != nil {
		t.Fatal("synthetic denial failed")
	}
	output := f.log.String()
	if !strings.Contains(output, "configuration denial authorized") || !strings.Contains(output, "reason_code=4001") {
		t.Fatal("denial has no classified technical observation")
	}
	for _, private := range []string{f.cfg.Token, f.cfg.SUIToken, f.cfg.SubscriptionBaseURL, f.store.view.Entitlement.Binding.Name, f.store.view.Entitlement.Activation.Token} {
		if strings.Contains(output, private) {
			t.Fatal("technical denial log exposed private values")
		}
	}
}
