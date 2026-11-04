package clientmenu_test

import (
	"github.com/PlumpAlbert/payplump-bot/internal/clientmenu"
	"github.com/PlumpAlbert/payplump-bot/templates"
	"strings"
	"testing"
)

func TestAuthorityStatusDoesNotClaimOverdueOrPanelMutation(t *testing.T) {
	for _, tc := range []struct {
		state    string
		enabled  bool
		template string
	}{{"active", true, "clientmenu/state_active"}, {"overdue", false, "clientmenu/state_authority_disabled"}, {"awaiting_payment", false, "clientmenu/state_unknown"}} {
		t.Run(tc.state, func(t *testing.T) {
			snapshot := menuSnapshot()
			snapshot.NinjaAuthority = true
			snapshot.PaymentReview = true
			snapshot.State = tc.state
			snapshot.DesiredAccess = tc.enabled
			snapshot.PaymentDate = "2040-01-01"
			reader := menuReader()
			p := prepareMenu(t, newMenuService(t, menuConfig(), reader), clientmenu.Status, snapshot)
			if !strings.Contains(p.Text, templates.Render(tc.template, templates.Data{})) || !strings.Contains(p.Text, templates.Render("clientmenu/authority_footer", templates.Data{})) {
				t.Fatal("authority source status missing")
			}
			for _, forbidden := range []string{templates.Render("clientmenu/payment_date", templates.Data{PaymentDate: snapshot.PaymentDate}), templates.Render("clientmenu/state_payment_review", templates.Data{})} {
				if forbidden != "" && strings.Contains(p.Text, forbidden) {
					t.Fatal("authority claimed access effect or historical payment entitlement")
				}
			}
		})
	}
}
