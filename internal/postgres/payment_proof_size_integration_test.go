//go:build integration

package postgres

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/payments"
)

func TestPaymentsProofBudgetIncludesPostgresFormatting(t *testing.T) {
	s, ctx := testStore(t)
	build := func(count int) payments.Proof {
		e := payments.Evidence{InvoiceID: "invoice", CustomerID: "customer", Amount: strconv.Itoa(count), CurrencyID: "1", PaymentDate: "2026-01-20", NextInvoiceDate: "2026-02-20"}
		for n := 0; n < count; n++ {
			e.Allocations = append(e.Allocations, payments.Allocation{PaymentID: fmt.Sprintf("pay%08d", n), AllocationID: fmt.Sprintf("all%08d", n), Amount: "1", Date: "2026-01-20"})
		}
		body, _ := json.Marshal(e)
		return payments.Proof{Evidence: e, InvoiceNumber: "TEST", JSON: body, Hash: sha256.Sum256(body)}
	}
	found := false
	for n := 500; n < 800; n++ {
		proof := build(n)
		if len(proof.JSON) < 62000 || len(proof.JSON) > 64000 {
			continue
		}
		var bytes int
		if err := s.pool.QueryRow(ctx, `SELECT octet_length($1::jsonb::text)`, string(proof.JSON)).Scan(&bytes); err != nil {
			t.Fatal(err)
		}
		if bytes <= 65536 {
			t.Fatal("boundary fixture did not exceed the SQL limit")
		}
		if proof.Validate() == nil {
			t.Fatal("oversized SQL proof passed application validation")
		}
		found = true
		break
	}
	if !found {
		t.Fatal("no boundary fixture")
	}
	proof := build(500)
	if proof.Validate() != nil {
		t.Fatal("small valid evidence rejected")
	}
	var fits bool
	if err := s.pool.QueryRow(ctx, `SELECT octet_length($1::jsonb::text)<=65536`, string(proof.JSON)).Scan(&fits); err != nil || !fits {
		t.Fatal("accepted proof did not fit the database")
	}
}
