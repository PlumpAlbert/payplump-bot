package clientmenu

import (
	"github.com/PlumpAlbert/payplump-bot/templates"
	"strings"
	"testing"
)

func TestRecordedPaymentAndReviewAreNotShownAsWaitingForFirstPayment(t *testing.T) {
	s := Snapshot{Known: true, State: "awaiting_payment", PaymentDate: "2026-10-17", NextInvoiceDate: "2026-11-17"}
	got := statusText(s)
	if !strings.Contains(got, templates.Render("clientmenu/state_setting_up", templates.Data{})) || strings.Contains(got, templates.Render("clientmenu/state_awaiting_payment", templates.Data{})) {
		t.Fatal("verified payment was obscured or access overstated")
	}
	s.PaymentReview = true
	got = statusText(s)
	if !strings.Contains(got, templates.Render("clientmenu/state_payment_review", templates.Data{})) || strings.Contains(got, templates.Render("clientmenu/state_setting_up", templates.Data{})) {
		t.Fatal("review-required proof was presented as valid")
	}
}
