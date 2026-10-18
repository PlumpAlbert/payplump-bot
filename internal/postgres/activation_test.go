package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestActivationRejectsInvalidBoundsWithoutDatabase(t *testing.T) {
	s, ctx := &Store{}, context.Background()
	policy := sha256.Sum256([]byte("synthetic policy"))
	for _, args := range []struct {
		bot     int64
		allowed []int64
		hash    [32]byte
	}{
		{0, []int64{1}, policy}, {1, make([]int64, 21), policy},
		{1, []int64{-1}, policy}, {1, []int64{1}, [32]byte{}},
	} {
		if _, err := s.SeedActivations(ctx, args.bot, args.allowed, args.hash); err == nil {
			t.Fatal("accepted invalid seed")
		}
	}
	if n, err := s.SeedActivations(ctx, 1, nil, policy); err != nil || n != 0 {
		t.Fatal("empty allowlist did not disable seed")
	}
	for _, limit := range []int{-1, 0, 5, 100} {
		if _, err := s.ClaimActivations(ctx, 1, limit); err == nil {
			t.Fatal("accepted unbounded claim")
		}
	}
	if _, err := s.ClaimActivations(ctx, 0, 1); err == nil {
		t.Fatal("accepted missing bot")
	}
	if _, err := s.LoadActivation(ctx, queue.Lease{}, 1); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatal("accepted invalid lease")
	}
	if granted, err := s.PrepareActivationWrite(ctx, queue.Lease{}, activation.Snapshot{}, "other", []byte(`{}`), "2039-02-01"); err == nil || granted {
		t.Fatal("accepted invalid step")
	}
	for _, reason := range []int{0, 2007, 3000, 3010} {
		if err := s.HoldActivation(ctx, queue.Lease{}, reason, nil); err == nil {
			t.Fatal("accepted invalid hold reason")
		}
	}
	for _, id := range []string{"0", "01", "+1", "-1", "1.0", "9223372036854775808", "https://sensitive.invalid/a"} {
		if err := s.ConfirmActivationStep(ctx, queue.Lease{}, activation.SUICreate, id); !errors.Is(err, queue.ErrConflict) {
			t.Fatal("accepted noncanonical SUI identity")
		}
	}
	for _, step := range []activation.Step{activation.RecurringCreate, activation.RecurringStart} {
		if err := s.ConfirmActivationStep(ctx, queue.Lease{}, step, "recurring-synthetic"); !errors.Is(err, queue.ErrConflict) {
			t.Fatal("generic confirmation accepted recurring without facts")
		}
	}
	for _, args := range []struct {
		step                 activation.Step
		id, amount, datetime string
	}{
		{activation.SUICreate, "77", "125.25", "2039-02-28T12:00:00Z"},
		{activation.SUIEnable, "77", "125.25", "2039-02-28T12:00:00Z"},
		{activation.RecurringCreate, "invalid/id", "125.25", "2039-02-28T12:00:00Z"},
		{activation.RecurringCreate, "recurring", "0", "2039-02-28T12:00:00Z"},
		{activation.RecurringCreate, "recurring", "-1", "2039-02-28T12:00:00Z"},
		{activation.RecurringStart, "recurring", "NaN", "2039-02-28T12:00:00Z"},
		{activation.RecurringStart, "recurring", "125.25", "2039-02-28"},
	} {
		if err := s.ConfirmRecurringActivationStep(ctx, queue.Lease{}, args.step, args.id, json.Number(args.amount), args.datetime); !errors.Is(err, queue.ErrConflict) {
			t.Fatal("accepted invalid recurring receipt")
		}
	}
	if err := s.FinishActivation(ctx, queue.Lease{}, activation.Snapshot{}, "2039-2-01", nil); err == nil {
		t.Fatal("accepted noncanonical date")
	}
	for _, drafts := range [][]queue.Draft{
		make([]queue.Draft, 101),
		{{BotID: 1, ChatID: 1, Key: "synthetic", Text: "https://personal.invalid/secret"}},
	} {
		if err := s.HoldActivation(ctx, queue.Lease{}, activation.ReviewSUI, drafts); err == nil {
			t.Fatal("accepted unsafe notifications")
		}
		if err := s.FinishActivation(ctx, queue.Lease{}, activation.Snapshot{}, "2039-02-01", drafts); err == nil {
			t.Fatal("accepted unsafe notifications")
		}
	}
}

func TestActivationAbsoluteDeadlineUsesEarliestLocalMidnightAndProviderUTC(t *testing.T) {
	localMidnight := time.Date(2039, 2, 28, 0, 0, 0, 0, time.Local)
	snapshot := activation.Snapshot{Proof: payments.Proof{Evidence: payments.Evidence{NextInvoiceDate: "2039-02-28"}}}
	if deadline, err := activationDeadline(snapshot); err != nil || !deadline.Equal(localMidnight) {
		t.Fatal("next period was not interpreted as local midnight")
	}
	for _, offset := range []time.Duration{-time.Hour, time.Hour} {
		providerUTC := localMidnight.Add(offset).UTC()
		snapshot.Steps = map[activation.Step]activation.Intent{activation.RecurringCreate: {
			ExternalID: "recurring", RecurringAmount: "125.25", RecurringDatetime: providerUTC.Format(time.RFC3339Nano),
		}}
		want := localMidnight
		if providerUTC.Before(want) {
			want = providerUTC
		}
		if deadline, err := activationDeadline(snapshot); err != nil || !deadline.Equal(want) {
			t.Fatal("one authorization deadline extended the other")
		}
	}
	snapshot.Steps[activation.RecurringCreate] = activation.Intent{ExternalID: "recurring", RecurringAmount: "125.25", RecurringDatetime: "invalid"}
	if _, err := activationDeadline(snapshot); !errors.Is(err, activation.ErrReviewRequired) {
		t.Fatal("invalid stored UTC schedule authorized a deadline")
	}
	snapshot.Steps = nil
	snapshot.Proof.NextInvoiceDate = "2039-02-29"
	if _, err := activationDeadline(snapshot); !errors.Is(err, activation.ErrReviewRequired) {
		t.Fatal("invalid local billing date authorized a deadline")
	}
}

func TestActivationCanonicalPaymentEvidenceAndSensitiveErrors(t *testing.T) {
	evidence := payments.Evidence{InvoiceID: "invoice", CustomerID: "customer", Amount: "125.25", CurrencyID: "1", PaymentDate: "2039-01-31", NextInvoiceDate: "2039-02-28",
		Allocations: []payments.Allocation{{PaymentID: "payment", AllocationID: "allocation", Amount: "125.25", Date: "2039-01-31"}}}
	canonical, _ := json.Marshal(evidence)
	// Simulate jsonb::text: order and spaces differ from Go's evidence encoding.
	var obj map[string]any
	_ = json.Unmarshal(canonical, &obj)
	raw, _ := json.MarshalIndent(obj, "", "  ")
	hash := sha256.Sum256(canonical)
	c := activationConfirmation{invoiceID: evidence.InvoiceID, customerID: evidence.CustomerID, number: "SYNTHETIC", amount: evidence.Amount,
		currency: evidence.CurrencyID, paid: evidence.PaymentDate, next: evidence.NextInvoiceDate, hash: hash[:], evidence: raw}
	proof, err := c.proof()
	if err != nil || proof.Hash != hash || string(proof.JSON) != string(canonical) {
		t.Fatal("hashed jsonb presentation instead of canonical evidence")
	}
	for _, mutate := range []func(*activationConfirmation){
		func(c *activationConfirmation) { c.invoiceID = "other" }, func(c *activationConfirmation) { c.customerID = "other" },
		func(c *activationConfirmation) { c.amount = "999" }, func(c *activationConfirmation) { c.currency = "2" },
		func(c *activationConfirmation) { c.paid = "2039-01-30" }, func(c *activationConfirmation) { c.next = "2039-03-01" },
		func(c *activationConfirmation) { c.hash = make([]byte, 32) }, func(c *activationConfirmation) { c.evidence = []byte(`{"sensitive":"broken"}`) },
	} {
		wrong := c
		mutate(&wrong)
		if _, err := wrong.proof(); !errors.Is(err, queue.ErrConflict) {
			t.Fatal("accepted mismatched confirmation columns")
		}
	}
	for _, sentinel := range []error{queue.ErrConflict, queue.ErrLeaseLost, activation.ErrGuardBusy, activation.ErrReviewRequired, context.Canceled, context.DeadlineExceeded} {
		if err := activationStageError("safe stage", errors.Join(sentinel, errors.New("sensitive SQL https://personal.invalid"))); !errors.Is(err, sentinel) || strings.Contains(err.Error(), "sensitive") {
			t.Fatal("unsafe classification")
		}
	}
	for _, inner := range []error{errors.New("sensitive SQL JSON"), &pgconn.PgError{Code: "23514", Detail: "sensitive request"}, &pgconn.PgError{Code: "23505", Detail: "sensitive identifier"}} {
		if err := activationStageError("safe stage", inner); strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), "SQL") {
			t.Fatal("driver detail leaked")
		}
	}
	if activationStageError("safe stage", nil) != nil {
		t.Fatal("success became failure")
	}
}
