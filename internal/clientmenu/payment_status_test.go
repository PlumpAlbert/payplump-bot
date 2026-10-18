package clientmenu

import (
	"strings"
	"testing"
)

func TestRecordedPaymentAndReviewAreNotShownAsWaitingForFirstPayment(t *testing.T) {
	s := Snapshot{Known: true, State: "awaiting_payment", PaymentDate: "2026-10-17", NextInvoiceDate: "2026-11-17"}
	got := statusText(s)
	if !strings.Contains(got, "оплата проверена") || !strings.Contains(got, "настройка доступа ещё не завершена") || strings.Contains(got, "ожидается подтверждение первой оплаты") {
		t.Fatal("verified payment was obscured or access overstated")
	}
	s.PaymentReview = true
	got = statusText(s)
	if !strings.Contains(got, "ручной перепроверки") || strings.Contains(got, "оплата проверена;") {
		t.Fatal("review-required proof was presented as valid")
	}
}
