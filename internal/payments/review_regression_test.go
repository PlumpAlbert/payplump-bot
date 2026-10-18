package payments

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
)

func TestWrappedIncompleteTransportFailureNeverInvalidatesPayment(t *testing.T) {
	for _, code := range []int{429, 503} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			f := cleanSettlement()
			f.local.PaymentDate = "2026-01-20"
			f.local.NextInvoiceDate = "2026-02-20"
			store := &paymentStoreFake{snapshot: f.local}
			reader := &settlementReaderFake{fixture: f}
			reader.payments = func(context.Context, int) ([]invoiceninja.SettlementPayment, error) {
				return nil, errors.Join(invoiceninja.ErrSettlementIncomplete, &integrationhttp.HTTPError{StatusCode: code})
			}
			workerForTest(t, store, reader, validPaymentConfig(), nil).handle(context.Background(), paymentClaim())
			finished, deferred := store.results()
			if len(finished) != 0 || len(deferred) != 1 {
				t.Fatal("transport failure reached sticky-review finalization")
			}
		})
	}
}

func TestCancellationWrappedAsIncompleteNeverFinalizesReview(t *testing.T) {
	f := cleanSettlement()
	store := &paymentStoreFake{snapshot: f.local}
	reader := &settlementReaderFake{fixture: f}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader.payments = func(context.Context, int) ([]invoiceninja.SettlementPayment, error) {
		cancel()
		return nil, errors.Join(invoiceninja.ErrSettlementIncomplete, context.Canceled)
	}
	workerForTest(t, store, reader, validPaymentConfig(), nil).handle(ctx, paymentClaim())
	finished, deferred := store.results()
	if len(finished) != 0 || len(deferred) != 0 {
		t.Fatal("shutdown persisted an invented negative observation")
	}
}

func TestEquivalentInvoiceDecimalsCanFinish(t *testing.T) {
	f := cleanSettlement()
	store := &paymentStoreFake{snapshot: f.local}
	reader := &settlementReaderFake{fixture: f}
	reader.invoice = func(_ context.Context, n int) (invoiceninja.SettlementInvoice, error) {
		i := f.invoice
		if n == 2 {
			i.Amount = "200.00"
			i.Balance = "0.000"
			i.PaidToDate = "2e2"
			i.ExchangeRate = "1e0"
		}
		return i, nil
	}
	workerForTest(t, store, reader, validPaymentConfig(), nil).handle(context.Background(), paymentClaim())
	finished, deferred := store.results()
	if len(finished) != 1 || len(deferred) != 0 || finished[0].decision.Kind != Verified {
		t.Fatal("equivalent decimal notation prevented confirmation")
	}
}

func TestProofValidationReservesJSONBFormattingSpace(t *testing.T) {
	found := false
	for count := 500; count <= 800; count++ {
		allocations := make([]Allocation, count)
		for n := range allocations {
			allocations[n] = Allocation{PaymentID: fmt.Sprintf("pay%08d", n), AllocationID: fmt.Sprintf("all%08d", n), Amount: "1", Date: "2026-01-20"}
		}
		e := Evidence{InvoiceID: "invoice", CustomerID: "customer", Amount: strconv.Itoa(count), CurrencyID: "1", PaymentDate: "2026-01-20", NextInvoiceDate: "2026-02-20", Allocations: allocations}
		body, _ := json.Marshal(e)
		if len(body) < 65536 && jsonbSizeBound(body) > 65536 {
			found = true
			proof := Proof{Evidence: e, InvoiceNumber: "TEST", JSON: body, Hash: sha256.Sum256(body)}
			if proof.Validate() == nil {
				t.Fatal("compact proof accepted despite oversized PostgreSQL JSONB representation")
			}
			break
		}
	}
	if !found {
		t.Fatal("fixture did not exercise compact-vs-JSONB boundary")
	}
	if got := jsonbSizeBound([]byte(`{"a":"comma,colon:quote\"slash\\","b":["x","y"]}`)); got != len(`{"a":"comma,colon:quote\"slash\\","b":["x","y"]}`)+4 {
		t.Fatalf("counted separators inside strings: %d", got)
	}
}
