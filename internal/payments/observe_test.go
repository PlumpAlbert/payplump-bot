package payments

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
)

func TestObserveReusableFullReadProtocol(t *testing.T) {
	f := cleanSettlement()
	reader := &settlementReaderFake{fixture: f}
	decision, err := Observe(context.Background(), reader, f.local, f.currency, f.today)
	if err != nil {
		t.Fatal("reusable observer failed")
	}
	expectDecision(t, decision, Verified, 0)
	want := []string{"customer", "invoice", "payments", "payments", "invoice", "customer"}
	if !reflect.DeepEqual(reader.callList(), want) || decision.Proof.Hash != f.evaluate().Proof.Hash {
		t.Fatal("observer omitted complete repeat reads or changed Evaluate policy")
	}
	workerReader := &settlementReaderFake{fixture: f}
	worker := workerForTest(t, &paymentStoreFake{snapshot: f.local}, workerReader, validPaymentConfig(), nil)
	wrapped, err := worker.observe(context.Background(), f.local)
	if err != nil || wrapped.Proof == nil || wrapped.Proof.Hash != decision.Proof.Hash || !reflect.DeepEqual(workerReader.callList(), want) {
		t.Fatal("worker does not reuse the same observation protocol")
	}
}

func TestObserveRejectsInvalidPublicInputsBeforeReads(t *testing.T) {
	f := cleanSettlement()
	for _, input := range []struct{ today, currency string }{
		{"", f.currency}, {"2026-3-15", f.currency}, {"2026-02-30", f.currency}, {"0000-01-01", f.currency},
		{f.today, ""}, {f.today, "0"}, {f.today, "03"}, {f.today, "3 "},
	} {
		reader := &settlementReaderFake{fixture: f}
		d, err := Observe(context.Background(), reader, f.local, input.currency, input.today)
		if err == nil || d != (Decision{}) || len(reader.callList()) != 0 {
			t.Fatal("invalid public date/currency reached reads or became review")
		}
	}
	if d, err := Observe(context.Background(), nil, f.local, f.currency, f.today); err == nil || d != (Decision{}) {
		t.Fatal("nil reader accepted")
	}
	reader := &settlementReaderFake{fixture: f}
	if d, err := Observe(nil, reader, f.local, f.currency, f.today); err == nil || d != (Decision{}) || len(reader.callList()) != 0 {
		t.Fatal("nil context reached reads")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if d, err := Observe(ctx, reader, f.local, f.currency, f.today); err != context.Canceled || d != (Decision{}) || len(reader.callList()) != 0 {
		t.Fatal("canceled context reached reads or became review")
	}
}

func TestObserveLeavesAssociationAndFinancialRulesToEvaluate(t *testing.T) {
	for _, mutate := range []func(*settlementFixture){
		func(f *settlementFixture) { f.local.TelegramID++ },
		func(f *settlementFixture) { f.customer.IDNumber = "other-owner" },
		func(f *settlementFixture) { f.invoice.ClientID = "other-customer" },
		func(f *settlementFixture) { f.invoice.ExchangeRate = "2" },
		func(f *settlementFixture) {
			f.invoice.StatusID = "2"
			f.invoice.Balance = "200"
			f.invoice.PaidToDate = "0"
		},
	} {
		f := cleanSettlement()
		mutate(&f)
		reader := &settlementReaderFake{fixture: f}
		d, err := Observe(context.Background(), reader, f.local, f.currency, f.today)
		want := f.evaluate()
		if err != nil || d.Kind != want.Kind || d.Code != want.Code || d.Validate() != nil {
			t.Fatal("public observer changed Evaluate's ownership/financial policy")
		}
		calls := []string{"customer", "invoice", "payments"}
		if f.invoice.StatusID != "4" {
			calls = calls[:2]
		}
		if !reflect.DeepEqual(reader.callList(), calls) {
			t.Fatal("non-verified observation changed the existing read protocol")
		}
	}
}

func TestObserveTransientReadsAfterConfirmationNeverBecomeReview(t *testing.T) {
	f := cleanSettlement()
	confirmed, err := Observe(context.Background(), &settlementReaderFake{fixture: f}, f.local, f.currency, f.today)
	if err != nil || confirmed.Kind != Verified {
		t.Fatal("synthetic prior confirmation failed")
	}
	f.local.PaymentDate = confirmed.Proof.PaymentDate
	f.local.NextInvoiceDate = confirmed.Proof.NextInvoiceDate
	f.local.DesiredAccess = true
	for _, failure := range []string{"customer-first", "invoice-first", "payments-first", "payments-repeat", "invoice-repeat", "customer-repeat"} {
		t.Run(failure, func(t *testing.T) {
			reader := &settlementReaderFake{fixture: f}
			transient := errors.Join(invoiceninja.ErrSettlementIncomplete, &integrationhttp.HTTPError{StatusCode: 503})
			reader.customer = func(_ context.Context, n int) (invoiceninja.SettlementCustomer, error) {
				if failure == "customer-first" && n == 1 || failure == "customer-repeat" && n == 2 {
					return invoiceninja.SettlementCustomer{}, transient
				}
				return f.customer, nil
			}
			reader.invoice = func(_ context.Context, n int) (invoiceninja.SettlementInvoice, error) {
				if failure == "invoice-first" && n == 1 || failure == "invoice-repeat" && n == 2 {
					return invoiceninja.SettlementInvoice{}, transient
				}
				return f.invoice, nil
			}
			reader.payments = func(_ context.Context, n int) ([]invoiceninja.SettlementPayment, error) {
				if failure == "payments-first" && n == 1 || failure == "payments-repeat" && n == 2 {
					return nil, transient
				}
				return cloneSettlementPayments(f.payments), nil
			}
			d, err := Observe(context.Background(), reader, f.local, f.currency, f.today)
			if err != transient || d != (Decision{}) || !f.local.DesiredAccess || confirmed.Validate() != nil {
				t.Fatal("incomplete GET after prior confirmation became authoritative review")
			}
		})
	}
}

func TestObserveRepeatsWholeInventoriesAndSurroundingFacts(t *testing.T) {
	for _, changed := range []string{"ignored-payment", "invoice", "customer"} {
		t.Run(changed, func(t *testing.T) {
			f := cleanSettlement()
			reader := &settlementReaderFake{fixture: f}
			switch changed {
			case "ignored-payment":
				reader.payments = func(_ context.Context, n int) ([]invoiceninja.SettlementPayment, error) {
					payments := cloneSettlementPayments(f.payments)
					if n == 1 {
						ignored := cashPayment("pending", "pending-allocation", "1", "2026-01-21")
						ignored.StatusID = "1"
						payments = append(payments, ignored)
					}
					return payments, nil
				}
			case "invoice":
				reader.invoice = func(_ context.Context, n int) (invoiceninja.SettlementInvoice, error) {
					invoice := f.invoice
					if n == 2 {
						invoice.Number = "changed"
					}
					return invoice, nil
				}
			case "customer":
				reader.customer = func(_ context.Context, n int) (invoiceninja.SettlementCustomer, error) {
					customer := f.customer
					if n == 2 {
						customer.IDNumber = "other-owner"
					}
					return customer, nil
				}
			}
			d, err := Observe(context.Background(), reader, f.local, f.currency, f.today)
			if !errors.Is(err, errUnstableEvidence) || d != (Decision{}) {
				t.Fatal("unstable surrounding evidence was ignored or became review")
			}
		})
	}
}
