package payments

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
)

type settlementFixture struct {
	local    Snapshot
	customer invoiceninja.SettlementCustomer
	invoice  invoiceninja.SettlementInvoice
	payments []invoiceninja.SettlementPayment
	currency string
	today    string
}

func cleanSettlement() settlementFixture {
	return settlementFixture{
		local:    Snapshot{ClientID: 7, TelegramID: 123456789, ChatID: 123456789, BotID: 42, CustomerID: "customer-A", InvoiceID: "invoice-A", FirstInvoiceID: "invoice-A", RecurringID: "recurring-A", State: "awaiting_payment"},
		customer: invoiceninja.SettlementCustomer{ID: "customer-A", IDNumber: "123456789"},
		invoice:  invoiceninja.SettlementInvoice{ID: "invoice-A", ClientID: "customer-A", Number: "INV-200", StatusID: "4", Date: "2026-01-01", DueDate: "2026-01-31", Amount: "200", Balance: "0", PaidToDate: "200", ExchangeRate: "1"},
		payments: []invoiceninja.SettlementPayment{
			cashPayment("payment-A", "allocation-A", "100", "2026-01-17"),
			cashPayment("payment-B", "allocation-B", "100", "2026-01-20"),
		},
		currency: "3", today: "2026-03-15",
	}
}

func cashPayment(id, allocation, amount, date string) invoiceninja.SettlementPayment {
	return invoiceninja.SettlementPayment{
		ID: id, ClientID: "customer-A", Date: date, StatusID: "4", CurrencyID: "3", Amount: json.Number(amount), Refunded: "0", Applied: json.Number(amount), ExchangeRate: "1", IsManual: true,
		Allocations: []invoiceninja.SettlementAllocation{{ID: allocation, InvoiceID: "invoice-A", PaymentID: id, Amount: json.Number(amount), Refunded: "0"}},
	}
}

func (f settlementFixture) evaluate() Decision {
	return Evaluate(f.local, f.customer, f.invoice, f.payments, f.currency, f.today)
}

func expectDecision(t *testing.T, got Decision, kind Kind, code int) {
	t.Helper()
	if got.Kind != kind || got.Code != code {
		t.Fatalf("decision = (%s, %d), want (%s, %d)", got.Kind, got.Code, kind, code)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("invalid decision: %v", err)
	}
	if kind != Verified && got.Proof != nil {
		t.Fatal("nonpositive decision contains payment proof")
	}
}

func TestEvaluateFullCashUsesLatestActualPaymentDate(t *testing.T) {
	f := cleanSettlement()
	d := f.evaluate()
	expectDecision(t, d, Verified, 0)
	p := d.Proof
	if p.PaymentDate != "2026-01-20" || p.NextInvoiceDate != "2026-02-20" || p.Amount != "200" || p.CurrencyID != "3" {
		t.Fatalf("unexpected cash proof: %+v", p.Evidence)
	}
	if p.InvoiceID != f.local.InvoiceID || p.CustomerID != f.local.CustomerID || p.InvoiceNumber != f.invoice.Number || len(p.Allocations) != 2 {
		t.Fatalf("proof does not identify the complete allocation set: %+v", p)
	}
	if !bytes.Equal(p.JSON, mustEvidenceJSON(t, p.Evidence)) || p.Hash != sha256.Sum256(p.JSON) {
		t.Fatal("proof must hash its canonical evidence JSON")
	}
}

func TestEvaluateAssociationBoundaries(t *testing.T) {
	tests := []struct {
		name string
		edit func(*settlementFixture)
	}{
		{"local client missing", func(f *settlementFixture) { f.local.ClientID = 0 }},
		{"telegram missing", func(f *settlementFixture) { f.local.TelegramID = 0 }},
		{"not private chat", func(f *settlementFixture) { f.local.ChatID++ }},
		{"bot missing", func(f *settlementFixture) { f.local.BotID = 0 }},
		{"customer mismatch", func(f *settlementFixture) { f.customer.ID = "customer-B" }},
		{"telegram id_number mismatch", func(f *settlementFixture) { f.customer.IDNumber = "123456788" }},
		{"id_number leading zero", func(f *settlementFixture) { f.customer.IDNumber = "0123456789" }},
		{"id_number whitespace", func(f *settlementFixture) { f.customer.IDNumber += " " }},
		{"deleted customer", func(f *settlementFixture) { f.customer.IsDeleted = true }},
		{"archived customer", func(f *settlementFixture) { f.customer.ArchivedAt = 1 }},
		{"current invoice mismatch", func(f *settlementFixture) { f.invoice.ID = "invoice-B" }},
		{"invoice customer mismatch", func(f *settlementFixture) { f.invoice.ClientID = "customer-B" }},
		{"first invoice must not recur", func(f *settlementFixture) { f.invoice.RecurringID = f.local.RecurringID }},
		{"later invoice missing expected recurring", func(f *settlementFixture) { f.local.FirstInvoiceID = "old"; f.local.RecurringID = "" }},
		{"later invoice wrong recurring", func(f *settlementFixture) { f.local.FirstInvoiceID = "old"; f.invoice.RecurringID = "other" }},
		{"later invoice absent recurring", func(f *settlementFixture) { f.local.FirstInvoiceID = "old" }},
		{"unsafe local customer id", func(f *settlementFixture) { f.local.CustomerID = "../customer-A" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := cleanSettlement()
			tc.edit(&f)
			expectDecision(t, f.evaluate(), NeedsReview, ReviewAssociation)
		})
	}
	t.Run("later invoice exact recurring", func(t *testing.T) {
		f := cleanSettlement()
		f.local.FirstInvoiceID = "old"
		f.invoice.RecurringID = f.local.RecurringID
		expectDecision(t, f.evaluate(), Verified, 0)
	})
}

func TestEvaluateNativeCurrencyPolicy(t *testing.T) {
	tests := []struct {
		name string
		edit func(*settlementFixture)
		kind Kind
	}{
		{"explicit company default no group", func(f *settlementFixture) {}, Verified},
		{"explicit client native override", func(f *settlementFixture) { f.customer.CurrencyID = "3" }, Verified},
		{"group with explicit native override", func(f *settlementFixture) { f.customer.GroupID = "group-A"; f.customer.CurrencyID = "3" }, Verified},
		{"group default unknown", func(f *settlementFixture) { f.customer.GroupID = "group-A" }, NeedsReview},
		{"client foreign override", func(f *settlementFixture) { f.customer.CurrencyID = "1" }, NeedsReview},
		{"company currency unknown", func(f *settlementFixture) { f.currency = "" }, NeedsReview},
		{"company currency not native id", func(f *settlementFixture) { f.currency = "RUB" }, NeedsReview},
		{"invoice FX", func(f *settlementFixture) { f.invoice.ExchangeRate = "1.01" }, NeedsReview},
		{"invoice FX unknown", func(f *settlementFixture) { f.invoice.ExchangeRate = "" }, NeedsReview},
		{"payment currency unknown", func(f *settlementFixture) { f.payments[0].CurrencyID = "" }, NeedsReview},
		{"foreign payment", func(f *settlementFixture) { f.payments[0].CurrencyID = "1" }, NeedsReview},
		{"foreign exchange currency", func(f *settlementFixture) { f.payments[0].ExchangeCurrencyID = "1" }, NeedsReview},
		{"payment FX", func(f *settlementFixture) { f.payments[0].ExchangeRate = "0.99" }, NeedsReview},
		{"payment FX unknown", func(f *settlementFixture) { f.payments[0].ExchangeRate = "" }, NeedsReview},
		{"explicit native exchange currency", func(f *settlementFixture) { f.payments[0].ExchangeCurrencyID = "3" }, Verified},
		{"exact decimal one", func(f *settlementFixture) { f.invoice.ExchangeRate = "1.000"; f.payments[0].ExchangeRate = "1e0" }, Verified},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := cleanSettlement()
			tc.edit(&f)
			code := 0
			if tc.kind == NeedsReview {
				code = ReviewCurrency
			}
			expectDecision(t, f.evaluate(), tc.kind, code)
		})
	}
}

func TestEvaluateInvoiceNotZeroBalanceAlone(t *testing.T) {
	tests := []struct {
		name string
		edit func(*settlementFixture)
	}{
		{"paid_to_date short", func(f *settlementFixture) { f.invoice.PaidToDate = "199.99" }},
		{"paid_to_date excessive", func(f *settlementFixture) { f.invoice.PaidToDate = "200.01" }},
		{"paid_to_date missing", func(f *settlementFixture) { f.invoice.PaidToDate = "" }},
		{"paid status positive balance", func(f *settlementFixture) { f.invoice.Balance = "1" }},
		{"paid status negative balance", func(f *settlementFixture) { f.invoice.Balance = "-1" }},
		{"zero amount", func(f *settlementFixture) { f.invoice.Amount = "0"; f.invoice.PaidToDate = "0" }},
		{"negative amount", func(f *settlementFixture) { f.invoice.Amount = "-200" }},
		{"missing balance", func(f *settlementFixture) { f.invoice.Balance = "" }},
		{"deleted", func(f *settlementFixture) { f.invoice.IsDeleted = true }},
		{"archived", func(f *settlementFixture) { f.invoice.ArchivedAt = 1 }},
		{"invalid invoice date", func(f *settlementFixture) { f.invoice.Date = "2026-02-30" }},
		{"invalid due date", func(f *settlementFixture) { f.invoice.DueDate = "2026-01-32" }},
		{"due before invoice", func(f *settlementFixture) { f.invoice.DueDate = "2025-12-31" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := cleanSettlement()
			tc.edit(&f)
			expectDecision(t, f.evaluate(), NeedsReview, ReviewInvoice)
		})
	}
	for _, status := range []string{"", "1", "2", "3", "5", "6", "7", "99"} {
		t.Run("zero balance status "+status, func(t *testing.T) {
			f := cleanSettlement()
			f.invoice.StatusID = status
			expectDecision(t, f.evaluate(), NeedsReview, ReviewInvoice)
		})
	}
	for _, status := range []string{"2", "3"} {
		t.Run("positive balance pending "+status, func(t *testing.T) {
			f := cleanSettlement()
			f.invoice.StatusID, f.invoice.Balance, f.invoice.PaidToDate = status, "100", "100"
			f.payments = nil
			expectDecision(t, f.evaluate(), Pending, 0)
		})
	}
}

func TestEvaluatePaymentAndAllocationBoundaries(t *testing.T) {
	tests := []struct {
		name string
		edit func(*settlementFixture)
	}{
		{"no payments", func(f *settlementFixture) { f.payments = nil }},
		{"partial cash", func(f *settlementFixture) { f.payments = f.payments[:1] }},
		{"excess cash not accepted", func(f *settlementFixture) {
			f.payments = append(f.payments, cashPayment("extra", "extra-a", "0.01", "2026-01-21"))
		}},
		{"other customer root", func(f *settlementFixture) { f.payments[0].ClientID = "customer-B" }},
		{"duplicate root", func(f *settlementFixture) { f.payments = append(f.payments, f.payments[0]) }},
		{"invalid root identifier", func(f *settlementFixture) { f.payments[0].ID = "" }},
		{"no allocations", func(f *settlementFixture) { f.payments[0].Allocations = nil }},
		{"wrong invoice allocation", func(f *settlementFixture) { f.payments[0].Allocations[0].InvoiceID = "invoice-B" }},
		{"allocation missing id", func(f *settlementFixture) { f.payments[0].Allocations[0].ID = "" }},
		{"allocation wrong parent", func(f *settlementFixture) { f.payments[0].Allocations[0].PaymentID = "payment-B" }},
		{"duplicate allocation across roots", func(f *settlementFixture) { f.payments[1].Allocations[0].ID = f.payments[0].Allocations[0].ID }},
		{"duplicate allocation inside root", func(f *settlementFixture) {
			f.payments[0].Allocations = append(f.payments[0].Allocations, f.payments[0].Allocations[0])
		}},
		{"root applied over amount", func(f *settlementFixture) { f.payments[0].Amount = "99.99" }},
		{"root applied misses allocation", func(f *settlementFixture) { f.payments[0].Applied = "99.99" }},
		{"root applied missing", func(f *settlementFixture) { f.payments[0].Applied = "" }},
		{"negative allocation", func(f *settlementFixture) { f.payments[0].Allocations[0].Amount = "-100" }},
		{"zero allocation", func(f *settlementFixture) { f.payments[0].Allocations[0].Amount = "0"; f.payments[0].Applied = "0" }},
		{"invalid decimal", func(f *settlementFixture) { f.payments[0].Allocations[0].Amount = "NaN" }},
		{"archived allocation", func(f *settlementFixture) { f.payments[0].Allocations[0].ArchivedAt = 1 }},
		{"refunded allocation", func(f *settlementFixture) { f.payments[0].Allocations[0].Refunded = "0.01" }},
		{"missing refund amount", func(f *settlementFixture) { f.payments[0].Refunded = "" }},
		{"credit bearing root wholly excluded", func(f *settlementFixture) {
			f.payments[0].Allocations = append(f.payments[0].Allocations, invoiceninja.SettlementAllocation{ID: "credit-a", PaymentID: "payment-A", CreditID: "credit-A", Amount: "1", Refunded: "0"})
			f.payments[0].Amount, f.payments[0].Applied = "101", "101"
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := cleanSettlement()
			tc.edit(&f)
			expectDecision(t, f.evaluate(), NeedsReview, ReviewAllocations)
		})
	}
}

func TestEvaluateIgnoresIneligiblePaymentsOnlyWithIndependentFullCash(t *testing.T) {
	tests := []struct {
		name string
		edit func(*invoiceninja.SettlementPayment)
	}{
		{"pending", func(p *invoiceninja.SettlementPayment) { p.StatusID = "1"; p.CurrencyID = "" }},
		{"failed", func(p *invoiceninja.SettlementPayment) { p.StatusID = "2" }},
		{"cancelled", func(p *invoiceninja.SettlementPayment) { p.StatusID = "3" }},
		{"refunded", func(p *invoiceninja.SettlementPayment) { p.Refunded = "1" }},
		{"deleted", func(p *invoiceninja.SettlementPayment) { p.IsDeleted = true }},
		{"archived", func(p *invoiceninja.SettlementPayment) { p.ArchivedAt = 1 }},
		{"nonmanual", func(p *invoiceninja.SettlementPayment) { p.IsManual = false }},
		{"archived allocation", func(p *invoiceninja.SettlementPayment) { p.Allocations[0].ArchivedAt = 1 }},
		{"refunded allocation", func(p *invoiceninja.SettlementPayment) { p.Allocations[0].Refunded = "1" }},
		{"mixed credit", func(p *invoiceninja.SettlementPayment) {
			p.Amount, p.Applied = "201", "201"
			p.Allocations = append(p.Allocations, invoiceninja.SettlementAllocation{ID: "credit-extra", PaymentID: p.ID, CreditID: "credit-extra", Amount: "1", Refunded: "0"})
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := cleanSettlement()
			p := cashPayment("old-payment", "old-allocation", "200", "2026-01-10")
			tc.edit(&p)
			f.payments = append(f.payments, p)
			expectDecision(t, f.evaluate(), Verified, 0)
			f.payments = []invoiceninja.SettlementPayment{p}
			expectDecision(t, f.evaluate(), NeedsReview, ReviewAllocations)
		})
	}
}

func TestEvaluateAppliedIncludesEveryInvoiceNotOnlyTarget(t *testing.T) {
	f := cleanSettlement()
	p := &f.payments[0]
	p.Amount, p.Applied = "150", "150"
	p.Allocations = append(p.Allocations, invoiceninja.SettlementAllocation{ID: "other-allocation", PaymentID: p.ID, InvoiceID: "other-invoice", Amount: "50", Refunded: "0"})
	expectDecision(t, f.evaluate(), Verified, 0)
	p.Applied = "100"
	expectDecision(t, f.evaluate(), NeedsReview, ReviewAllocations)
}

func TestEvaluateExactDecimalsAndStableFingerprint(t *testing.T) {
	for _, amount := range []string{"0.3", "9007199254740993.01", "123456789012345678901234567890.123456789", "1e-20"} {
		t.Run(amount, func(t *testing.T) {
			f := cleanSettlement()
			f.invoice.Amount, f.invoice.PaidToDate = json.Number(amount), json.Number(amount)
			f.payments = []invoiceninja.SettlementPayment{cashPayment("payment-A", "allocation-A", amount, "2026-01-20")}
			expectDecision(t, f.evaluate(), Verified, 0)
		})
	}
	f := cleanSettlement()
	f.invoice.Amount, f.invoice.PaidToDate = "0.3", "3e-1"
	f.payments[0].Amount, f.payments[0].Applied, f.payments[0].Allocations[0].Amount = "0.1", "0.1", "0.1"
	f.payments[1].Amount, f.payments[1].Applied, f.payments[1].Allocations[0].Amount = "0.2", "0.2", "0.2"
	first := f.evaluate()
	expectDecision(t, first, Verified, 0)
	slices.Reverse(f.payments)
	second := f.evaluate()
	expectDecision(t, second, Verified, 0)
	if first.Proof.Hash != second.Proof.Hash || !bytes.Equal(first.Proof.JSON, second.Proof.JSON) {
		t.Fatal("payment order changed the canonical fingerprint")
	}
	f.payments[0].Allocations[0].Amount = "0.20000000000000000001"
	f.payments[0].Amount, f.payments[0].Applied = "0.20000000000000000001", "0.20000000000000000001"
	expectDecision(t, f.evaluate(), NeedsReview, ReviewAllocations)
}

func TestEvaluateDatesAndMonthEndClamp(t *testing.T) {
	for _, tc := range []struct{ paid, next string }{
		{"2026-01-29", "2026-02-28"}, {"2026-01-30", "2026-02-28"}, {"2026-01-31", "2026-02-28"},
		{"2024-01-31", "2024-02-29"}, {"2026-03-31", "2026-04-30"}, {"2025-12-31", "2026-01-31"},
	} {
		t.Run(tc.paid, func(t *testing.T) {
			f := cleanSettlement()
			f.today = "2027-01-01"
			f.payments = []invoiceninja.SettlementPayment{cashPayment("payment", "allocation", "200", tc.paid)}
			d := f.evaluate()
			expectDecision(t, d, Verified, 0)
			if d.Proof.PaymentDate != tc.paid || d.Proof.NextInvoiceDate != tc.next {
				t.Fatalf("wrong dates: %+v", d.Proof.Evidence)
			}
		})
	}
	for _, date := range []string{"", "2026-02-30", "2026-1-20", "2026-01-20T00:00:00Z", "2026-03-16"} {
		t.Run("invalid payment date "+date, func(t *testing.T) {
			f := cleanSettlement()
			f.payments[0].Date = date
			expectDecision(t, f.evaluate(), NeedsReview, ReviewDate)
		})
	}
	f := cleanSettlement()
	f.today = "not-a-date"
	expectDecision(t, f.evaluate(), NeedsReview, ReviewDate)
}

func mustEvidenceJSON(t *testing.T, e Evidence) []byte {
	t.Helper()
	body, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestProofValidateCanonicalHashCoverageAndDates(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Proof)
	}{
		{"hash mismatch", func(p *Proof) { p.Hash[0] ^= 1 }},
		{"evidence mutation", func(p *Proof) { p.Amount = "201" }},
		{"empty JSON", func(p *Proof) { p.JSON = nil }},
		{"noncanonical whitespace", func(p *Proof) { p.JSON = append([]byte(" "), p.JSON...); p.Hash = sha256.Sum256(p.JSON) }},
		{"trailing object", func(p *Proof) { p.JSON = append(p.JSON, []byte("{}")...); p.Hash = sha256.Sum256(p.JSON) }},
		{"missing invoice number", func(p *Proof) { p.InvoiceNumber = "" }},
		{"control in invoice number", func(p *Proof) { p.InvoiceNumber = "INV\n200" }},
		{"unsafe invoice identifier", func(p *Proof) { p.InvoiceID = "../invoice" }},
		{"unknown currency", func(p *Proof) { p.CurrencyID = "" }},
		{"no allocations", func(p *Proof) { p.Allocations = nil }},
		{"duplicate allocation IDs", func(p *Proof) { p.Allocations[1].AllocationID = p.Allocations[0].AllocationID }},
		{"unsafe payment ID", func(p *Proof) { p.Allocations[0].PaymentID = "../payment" }},
		{"undercoverage", func(p *Proof) { p.Allocations[0].Amount = "99" }},
		{"overcoverage", func(p *Proof) { p.Allocations[0].Amount = "101" }},
		{"zero allocation", func(p *Proof) { p.Allocations[0].Amount = "0" }},
		{"noncanonical allocation amount", func(p *Proof) { p.Allocations[0].Amount = "100.00" }},
		{"noncanonical invoice amount", func(p *Proof) { p.Amount = "200.0" }},
		{"payment date not latest", func(p *Proof) { p.PaymentDate = "2026-01-17"; p.NextInvoiceDate = "2026-02-17" }},
		{"wrong next month", func(p *Proof) { p.NextInvoiceDate = "2026-02-21" }},
		{"invalid allocation date", func(p *Proof) { p.Allocations[0].Date = "2026-02-30" }},
		{"oversized JSON", func(p *Proof) { p.JSON = []byte(strings.Repeat(" ", 64*1024+1)); p.Hash = sha256.Sum256(p.JSON) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := *cleanSettlement().evaluate().Proof
			tc.edit(&p)
			if p.Validate() == nil {
				t.Fatal("accepted invalid proof")
			}
			// Except for explicitly corrupted wire representations, also rebuild
			// a perfectly matching hash/JSON so semantic checks cannot hide behind
			// an unrelated fingerprint mismatch.
			p.JSON = mustEvidenceJSON(t, p.Evidence)
			p.Hash = sha256.Sum256(p.JSON)
			switch tc.name {
			case "hash mismatch", "empty JSON", "noncanonical whitespace", "trailing object", "oversized JSON":
				return
			}
			if p.Validate() == nil {
				t.Fatal("accepted semantically invalid proof with matching hash")
			}
		})
	}
}

func TestDecisionValidateRejectsProofOnNegativeOutcomes(t *testing.T) {
	proof := cleanSettlement().evaluate().Proof
	for _, d := range []Decision{
		{}, {Kind: "granted"}, {Kind: Verified}, {Kind: Verified, Proof: proof, Code: ReviewInvoice},
		{Kind: Pending, Proof: proof}, {Kind: Pending, Code: ReviewInvoice},
		{Kind: NeedsReview, Proof: proof, Code: ReviewInvoice}, {Kind: NeedsReview, Code: 2000}, {Kind: NeedsReview, Code: 2009},
	} {
		if d.Validate() == nil {
			t.Fatalf("accepted invalid decision %+v", d)
		}
	}
	for _, d := range []Decision{{Kind: Pending}, {Kind: Verified, Proof: proof}, review(ReviewAssociation), review(ReviewExcluded)} {
		if err := d.Validate(); err != nil {
			t.Fatalf("rejected valid decision: %v", err)
		}
	}
}
