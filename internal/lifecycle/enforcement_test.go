package lifecycle

import (
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

func validEnforcementSnapshot() EnforcementSnapshot {
	const token = "synthetic_activation_token_123456"
	return EnforcementSnapshot{
		ClientID: 1, TelegramID: 101, ChatID: 101, BotID: 7,
		CustomerID: "customer-1", FirstInvoiceID: "invoice-first", CurrentInvoiceID: "invoice-overdue", InvoiceID: "invoice-overdue",
		RecurringID: "recurring-1", SUIID: "71", RegistrationToken: "synthetic_registration_token_123456", ActivationToken: token,
		State: "active", DesiredAccess: true, ManagedProvenance: true,
		Invoice: Invoice{ID: "invoice-overdue", Number: "SYNTHETIC-2", Status: "sent", SentDate: "2040-01-01", Amount: "100", Balance: "100", PaidToDate: "0"},
		RecurringDraft: invoiceninja.RecurringDraft{
			ClientID: "customer-1", FrequencyID: 5, NextSendDate: "2040-02-01", DueDateDays: "on_receipt", AutoBill: "off", RemainingCycles: -1,
			PrivateNotes: activation.Marker(token), UsesInclusiveTaxes: false,
			LineItems: []invoiceninja.InvoiceLine{{ProductKey: "product-1", Notes: "subscription", Cost: "100", Quantity: "1", Discount: "0", TypeID: "1", TaxRate1: "0", TaxRate2: "0", TaxRate3: "0"}},
		},
		SUIBinding: sui.Binding{ID: 71, Name: "pp-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		Steps:      make(map[EnforcementStep]OverdueStep),
	}
}

func TestOverduePolicyUsesNextLocalCalendarDay(t *testing.T) {
	s := validEnforcementSnapshot()
	allowed := []int64{101}
	midnight := time.Date(2040, 1, 2, 0, 0, 0, 0, time.Local)
	if code := validateEnforcementSnapshot(s, 7, allowed, nil, midnight); code != 0 {
		t.Fatalf("invoice was not overdue at next local midnight: review=%d", code)
	}
	before := midnight.Add(-time.Second)
	if code := validateEnforcementSnapshot(s, 7, allowed, nil, before); code != ReviewOverdueInvoice {
		t.Fatalf("invoice became overdue before next local midnight: review=%d", code)
	}
}

func TestOverduePolicyRejectsPartialAndUnboundInvoices(t *testing.T) {
	s := validEnforcementSnapshot()
	now := time.Date(2040, 1, 2, 12, 0, 0, 0, time.Local)
	s.Invoice.Status = "partial"
	if code := validateEnforcementSnapshot(s, 7, []int64{101}, nil, now); code != ReviewOverdueInvoice {
		t.Fatalf("partial invoice was eligible: review=%d", code)
	}
	s = validEnforcementSnapshot()
	s.SUIBinding.ID = 72
	if code := validateEnforcementSnapshot(s, 7, []int64{101}, nil, now); code != ReviewOverdueAssociation {
		t.Fatalf("mismatched S-UI binding was eligible: review=%d", code)
	}
	s = validEnforcementSnapshot()
	if code := validateEnforcementSnapshot(s, 7, []int64{101}, []int64{101}, now); code != ReviewOverdueAssociation {
		t.Fatalf("excluded pilot account was eligible: review=%d", code)
	}
}

func TestOverdueSequenceRequiresConfirmedPrefix(t *testing.T) {
	steps := map[EnforcementStep]OverdueStep{}
	if got, ok := nextEnforcementStep(steps); !ok || got != OverduePauseRecurring {
		t.Fatalf("first step=%q ok=%v", got, ok)
	}
	steps[OverduePauseRecurring] = OverdueStep{Confirmed: true}
	if got, ok := nextEnforcementStep(steps); !ok || got != OverdueDisableSUI {
		t.Fatalf("second step=%q ok=%v", got, ok)
	}
	steps[OverdueDisableSUI] = OverdueStep{Confirmed: true}
	if got, ok := nextEnforcementStep(steps); !ok || got != OverdueCloseSessions {
		t.Fatalf("third step=%q ok=%v", got, ok)
	}
	steps[OverdueCloseSessions] = OverdueStep{Confirmed: true}
	if _, ok := nextEnforcementStep(steps); ok {
		t.Fatal("completed action still had a pending step")
	}
	steps[OverdueCloseSessions] = OverdueStep{Confirmed: false}
	if step, ok := unconfirmedEnforcementStep(steps); !ok || step != OverdueCloseSessions {
		t.Fatalf("unconfirmed intent was not detected: %q %v", step, ok)
	}
}
