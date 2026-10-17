package registration

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
)

func TestDisplayAmountExactAndBounded(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"0", "0"}, {"0.000", "0"}, {"0e32", "0"}, {"123.4500", "123.45"},
		{"9007199254740993.1234567890123456789", "9007199254740993.1234567890123456789"},
		{"1.2345e3", "1234.5"}, {"123.45E-4", "0.012345"}, {"120e+2", "12000"},
		{"1e32", "1" + strings.Repeat("0", 32)}, {"1e-32", "0." + strings.Repeat("0", 31) + "1"},
		{strings.Repeat("9", 64), strings.Repeat("9", 64)},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := displayAmount(json.Number(tc.raw))
			if err != nil || got != tc.want {
				t.Fatalf("displayAmount = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	for _, raw := range []string{"", "-0", "-1", "+1", "01", "00.1", ".1", "1.", "1e", "1e33", "1e-33", "1e999999999999", "NaN", "Inf", " 1", "1\n", "1/2", strings.Repeat("9", 65)} {
		if got, err := displayAmount(json.Number(raw)); err == nil {
			t.Errorf("accepted %q as %q", raw, got)
		}
	}
}

func TestInvoiceLifecycleBalancesFollowMarkSentContract(t *testing.T) {
	for _, tc := range []struct {
		amount, balance string
		draft, sent     bool
	}{
		{"123.45", "0", true, false}, {"123.4500", "1.2345e2", false, true},
		{"123.45", "123.44", false, false}, {"123.45", "0.01", false, false},
		{"0", "0", false, false}, {"-1", "-1", false, false}, {"1e33", "0", false, false},
	} {
		invoice := invoiceninja.Invoice{Amount: json.Number(tc.amount), Balance: json.Number(tc.balance)}
		if unissued(invoice) != tc.draft || unpaid(invoice) != tc.sent {
			t.Errorf("amount=%s balance=%s: draft=%v sent=%v", tc.amount, tc.balance, unissued(invoice), unpaid(invoice))
		}
	}
}

func messageFixture() (Snapshot, invoiceninja.Invoice) {
	return Snapshot{BotID: 77, TelegramID: 123456, ChatID: 123456, DisplayName: "Имя <client> _plain_", Token: "stable_flow_token_0123456789"},
		invoiceninja.Invoice{ID: "invoice-1", Number: "INV-001", Date: "2035-06-12", DueDate: "2035-06-12",
			Amount: "9007199254740993.123456789", Balance: "9007199254740993.123456789",
			Invitations: []invoiceninja.Invitation{{ID: "not-a-url", Link: "https://public.example.test/view/opaque?signature=provided"}}}
}

func TestInvoiceMessagesExactAmountProvidedLinkPlaintextAndStableKeys(t *testing.T) {
	s, invoice := messageFixture()
	cfg := testConfig()
	admins := []int64{91, 92}
	first, err := invoiceMessages(s, invoice, cfg, admins)
	if err != nil {
		t.Fatal(err)
	}
	again, err := invoiceMessages(s, invoice, cfg, admins)
	if err != nil || !reflect.DeepEqual(first, again) || len(first) != 3 {
		t.Fatalf("notifications not stable: %v", err)
	}
	wantText := "Счёт №INV-001\nСумма по счёту: 9007199254740993.123456789\nОплатите до 2035-06-12 включительно.\nПеревод по телефону: " + cfg.PaymentPhone +
		"\nБанк: " + cfg.PaymentBank + "\nПолучатель: " + cfg.PaymentRecipient +
		"\nОткрыть счёт: " + invoice.Invitations[0].Link + "\nДоступ будет выдан после подтверждения полной оплаты."
	if first[0].Text != wantText || first[0].Key != "registration:"+s.Token+":invoice:client" || first[0].ChatID != s.ChatID {
		t.Fatalf("unexpected client notification: %+v", first[0])
	}
	for i, draft := range first {
		if draft.BotID != s.BotID || draft.Validate() != nil {
			t.Fatalf("invalid draft %d: %+v", i, draft)
		}
		if strings.Contains(draft.Text, cfg.BaseURL) || strings.Contains(draft.Text, cfg.Token) || strings.Contains(draft.Text, s.Token) || strings.Contains(draft.Text, "not-a-url") {
			t.Fatal("constructed URL or exposed internal identity")
		}
	}
	if first[1].ChatID != 91 || first[1].Key != "registration:"+s.Token+":invoice:admin:91" || first[2].Key != "registration:"+s.Token+":invoice:admin:92" {
		t.Fatal("administrator business keys are not stable recipient keys")
	}
	if !strings.Contains(first[1].Text, s.DisplayName) || !strings.Contains(first[1].Text, "Telegram ID 123456") {
		t.Fatal("administrator notification lost plaintext identity")
	}
	cfg.PaymentBank, cfg.PaymentRecipient = "", ""
	s.DisplayName = " "
	minimal, err := invoiceMessages(s, invoice, cfg, admins)
	if err != nil || strings.Contains(minimal[0].Text, "Банк:") || strings.Contains(minimal[0].Text, "Получатель:") || !strings.Contains(minimal[1].Text, "Клиент Telegram") {
		t.Fatalf("optional payment details / fallback: %v", err)
	}
}

func TestInvoiceLinksHTTPSOnlyAndNeverConstructed(t *testing.T) {
	for _, link := range []string{
		"", "http://example.test/invoice", "//example.test/invoice", "javascript:alert(1)",
		"https:opaque", "https:///invoice", "https://user:pass@example.test/view", "https://example.test/view#secret",
		" https://example.test/view", "https://example.test/view\n", "https://example.test/\x00", "https://example.test/" + strings.Repeat("x", 2048),
	} {
		t.Run(link, func(t *testing.T) {
			if validInvoiceLink(link) {
				t.Fatal("unsafe link accepted")
			}
			s, invoice := messageFixture()
			invoice.Invitations = []invoiceninja.Invitation{{ID: "opaque-id-must-not-be-expanded", Link: link}}
			if drafts, err := invoiceMessages(s, invoice, testConfig(), []int64{91}); err == nil || len(drafts) != 0 {
				t.Fatal("unsafe/missing invitation replaced with a constructed link")
			}
		})
	}
	s, invoice := messageFixture()
	good := invoice.Invitations[0]
	invoice.Invitations = append([]invoiceninja.Invitation{{Link: "http://bad.test"}}, good)
	drafts, err := invoiceMessages(s, invoice, testConfig(), nil)
	if err != nil || !strings.Contains(drafts[0].Text, good.Link) {
		t.Fatalf("did not use provided verified invitation: %v", err)
	}
}

func TestInvoiceMessagesRejectUnboundedOrInvalidBodies(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Snapshot, *invoiceninja.Invoice)
	}{
		{"zero", func(_ *Snapshot, i *invoiceninja.Invoice) { i.Amount = "0" }},
		{"exponent", func(_ *Snapshot, i *invoiceninja.Invoice) { i.Amount = "1e100000" }},
		{"number-empty", func(_ *Snapshot, i *invoiceninja.Invoice) { i.Number = "" }},
		{"number-control", func(_ *Snapshot, i *invoiceninja.Invoice) { i.Number = "INV\nspoof" }},
		{"number-long", func(_ *Snapshot, i *invoiceninja.Invoice) { i.Number = strings.Repeat("я", 129) }},
		{"admin-body-long", func(s *Snapshot, _ *invoiceninja.Invoice) { s.DisplayName = strings.Repeat("я", 4096) }},
		{"admin-body-nul", func(s *Snapshot, _ *invoiceninja.Invoice) { s.DisplayName = "name\x00" }},
		{"business-key-long", func(s *Snapshot, _ *invoiceninja.Invoice) { s.Token = strings.Repeat("x", 256) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, invoice := messageFixture()
			tc.edit(&s, &invoice)
			if drafts, err := invoiceMessages(s, invoice, testConfig(), []int64{91}); err == nil || len(drafts) != 0 {
				t.Fatal("invalid notification accepted")
			}
		})
	}
}

func TestReviewMessagesStableKeysAndNoPrivateData(t *testing.T) {
	s, _ := messageFixture()
	for _, reason := range []int{ReviewExistingCustomer, ReviewCustomerConflict, ReviewInvoiceConflict, ReviewUnknownWrite, ReviewInvoiceState, ReviewContract, ReviewLocalAssociation, 9999} {
		first := reviewMessages(s, 10, reason, []int64{91})
		if len(first) != 2 || !reflect.DeepEqual(first, reviewMessages(s, 10, reason, []int64{91})) {
			t.Fatal("unstable review notifications")
		}
		for _, draft := range first {
			if draft.Validate() != nil || strings.Contains(draft.Text, s.Token) || strings.Contains(draft.Text, s.DisplayName) || strings.Contains(draft.Text, "payplump:") {
				t.Fatal("invalid or private review notification")
			}
		}
		if first[0].Key != "registration:"+s.Token+":review:client" || first[1].Key == first[0].Key {
			t.Fatal("invalid review business key")
		}
		if changed := reviewMessages(s, 999, reason, []int64{91}); changed[0].Key != first[0].Key || changed[1].Key != first[1].Key {
			t.Fatal("review business key depends on attempt/operation ID")
		}
	}
	s.BotID = 0
	if got := reviewMessages(s, 1, ReviewContract, []int64{91}); len(got) != 0 {
		t.Fatal("unverified bot can enqueue notifications")
	}
	s.BotID, s.Token = 77, ""
	if got := reviewMessages(s, 1, ReviewContract, []int64{91}); len(got) != 0 {
		t.Fatal("missing flow token can enqueue notifications")
	}
}
