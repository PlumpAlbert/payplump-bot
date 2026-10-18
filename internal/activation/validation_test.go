package activation

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

func validationSnapshot(t *testing.T) Snapshot {
	t.Helper()
	evidence := payments.Evidence{InvoiceID: "invoice-A", CustomerID: "customer-A", Amount: "100", CurrencyID: "1", PaymentDate: "2040-01-31", NextInvoiceDate: "2040-02-29", Allocations: []payments.Allocation{{PaymentID: "payment-A", AllocationID: "allocation-A", Amount: "100", Date: "2040-01-31"}}}
	raw, _ := json.Marshal(evidence)
	proof := payments.Proof{Evidence: evidence, InvoiceNumber: "SYN-1", JSON: raw, Hash: sha256.Sum256(raw)}
	return Snapshot{Account: payments.Snapshot{ClientID: 1, TelegramID: 101, ChatID: 101, BotID: 1, CustomerID: evidence.CustomerID, InvoiceID: evidence.InvoiceID, FirstInvoiceID: evidence.InvoiceID, State: "awaiting_payment", PaymentDate: evidence.PaymentDate, NextInvoiceDate: evidence.NextInvoiceDate}, Token: "abcdefghijklmnopqrstuvwx", PolicyHash: activationTestConfig().PolicyHash(), Proof: proof, Steps: map[Step]Intent{}}
}
func validationPayload(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, e := json.Marshal(value)
	if e != nil {
		t.Fatal("invalid synthetic payload")
	}
	return raw
}
func validationFullSteps(t *testing.T) Snapshot {
	t.Helper()
	s := validationSnapshot(t)
	create, e := sui.NewDisabledDraft([]int64{7})
	if e != nil {
		t.Fatal(e)
	}
	s.Steps[SUICreate] = Intent{Payload: validationPayload(t, create), ExternalID: "17", DispatchAttempt: 1}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(s.Steps[SUICreate].Payload, &fields)
	fields["id"] = json.RawMessage(`17`)
	record, e := sui.ParseClientRecord(validationPayload(t, fields))
	if e != nil {
		t.Fatal("synthetic full record invalid")
	}
	product := invoiceninja.Product{ID: "product-A", ProductKey: "monthly", Price: "100", TaxRate1: "0", TaxRate2: "0", TaxRate3: "0", TaxID: "0"}
	recurring, e := invoiceninja.BuildRecurringDraft(s.Account.CustomerID, s.Proof.PaymentDate, "2040-01-31", product, false, Marker(s.Token))
	if e != nil {
		t.Fatal(e)
	}
	s.Steps[RecurringCreate] = Intent{Payload: validationPayload(t, recurring), ExternalID: "recurring-A", DispatchAttempt: 2, RecurringAmount: "100", RecurringDatetime: "2040-02-29T12:00:00Z"}
	binding, e := SUIBinding(s)
	if e != nil {
		t.Fatal(e)
	}
	enable, e := sui.PrepareEnable(record, binding)
	if e != nil {
		t.Fatal(e)
	}
	s.Steps[SUIEnable] = Intent{Payload: validationPayload(t, enable), ExternalID: "17", DispatchAttempt: 3}
	s.Steps[RecurringStart] = Intent{Payload: validationPayload(t, StartRequest{IDs: []string{"recurring-A"}, Action: "start"}), ExternalID: "recurring-A", DispatchAttempt: 4, RecurringAmount: "100", RecurringDatetime: "2040-02-29T12:00:00Z"}
	return s
}
func TestActivationValidationBindsOriginalPaymentAndObjects(t *testing.T) {
	s := validationFullSteps(t)
	if ValidateSnapshot(s) != nil {
		t.Fatal("valid snapshot rejected")
	}
	for _, step := range OrderedSteps() {
		if ValidateIntent(s, step, s.Steps[step].Payload) != nil {
			t.Fatal("valid intent rejected")
		}
	}
	binding, e := SUIBinding(s)
	if e != nil || binding.ID != 17 || binding.Name == "" || binding.CredentialsHash == ([32]byte{}) {
		t.Fatal("saved create identity was not bound")
	}
	if next, e := NextStep(s); e != nil || next != "" {
		t.Fatal("complete prefix considered pending")
	}
	for _, change := range []func(*Snapshot){func(s *Snapshot) { s.Account.ChatID++ }, func(s *Snapshot) { s.Account.BotID = 0 }, func(s *Snapshot) { s.Account.InvoiceID = "other" }, func(s *Snapshot) { s.Account.SUIID = "17" }, func(s *Snapshot) { s.Account.RecurringID = "foreign" }, func(s *Snapshot) { s.Account.State = "active" }, func(s *Snapshot) { s.Account.DesiredAccess = true }, func(s *Snapshot) { s.Account.PaymentDate = "2040-01-30" }, func(s *Snapshot) { s.Token = "short" }, func(s *Snapshot) { s.PolicyHash = [32]byte{} }, func(s *Snapshot) { s.Proof.Hash[0] ^= 1 }, func(s *Snapshot) {
		s.Steps[Step("foreign")] = Intent{Payload: json.RawMessage(`{}`), DispatchAttempt: 1}
	}} {
		changed := validationFullSteps(t)
		change(&changed)
		if ValidateSnapshot(changed) == nil {
			t.Fatal("inconsistent activation snapshot accepted")
		}
	}
	s.PaymentReview = true
	s.ReviewCode = ReviewPayment
	if ValidateSnapshot(s) != nil {
		t.Fatal("review erased the original audit snapshot")
	}
}
func TestActivationIntentRejectsWrongBindingsAndAction(t *testing.T) {
	s := validationFullSteps(t)
	for _, step := range OrderedSteps() {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(s.Steps[step].Payload, &fields)
		switch step {
		case SUICreate:
			fields["enable"] = json.RawMessage(`true`)
		case RecurringCreate:
			fields["client_id"] = json.RawMessage(`"foreign"`)
		case SUIEnable:
			var binding sui.Binding
			_ = json.Unmarshal(fields["binding"], &binding)
			binding.ID++
			fields["binding"] = validationPayload(t, binding)
		case RecurringStart:
			fields["action"] = json.RawMessage(`"send_now"`)
		}
		if ValidateIntent(s, step, validationPayload(t, fields)) == nil {
			t.Fatal("foreign or changed intent accepted")
		}
	}
	for _, raw := range []string{`{"ids":["foreign"],"action":"start"}`, `{"ids":["recurring-A","foreign"],"action":"start"}`, `{"ids":["recurring-A"],"action":"start","extra":true}`, `{"ids":["recurring-A"],"action":"start","action":"start"}`} {
		if ValidateIntent(s, RecurringStart, []byte(raw)) == nil {
			t.Fatal("ambiguous first-start intent accepted")
		}
	}
	for _, id := range []string{"0", "01", "-1", "1.0", "1e1", "9223372036854775808"} {
		changed := validationFullSteps(t)
		intent := changed.Steps[SUICreate]
		intent.ExternalID = id
		changed.Steps[SUICreate] = intent
		if _, e := SUIBinding(changed); e == nil {
			t.Fatal("noncanonical SUI identity accepted")
		}
	}
}
func TestActivationStepPrefixNeverRearmsUnknown(t *testing.T) {
	s := validationFullSteps(t)
	for n, step := range OrderedSteps() {
		current := s
		current.Steps = map[Step]Intent{}
		for _, prior := range OrderedSteps()[:n] {
			current.Steps[prior] = s.Steps[prior]
		}
		next, e := NextStep(current)
		if e != nil || next != step {
			t.Fatal("wrong next step")
		}
		unknown := s.Steps[step]
		unknown.ExternalID = ""
		current.Steps[step] = unknown
		next, e = NextStep(current)
		if e != nil || next != step {
			t.Fatal("unknown dispatch was skipped")
		}
		if n < 3 {
			future := OrderedSteps()[n+1]
			current.Steps[future] = s.Steps[future]
			if _, e := NextStep(current); e == nil {
				t.Fatal("out-of-order dispatch accepted")
			}
		}
	}
	copySteps := OrderedSteps()
	copySteps[0] = "foreign"
	if OrderedSteps()[0] != SUICreate {
		t.Fatal("caller changed global step ordering")
	}
}
func TestActivationDecodeIntentStrictBoundedAndRedacted(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{} {}`, `{"x":1,"x":2}`, `{"x":{"secret":1,"secret":2}}`, `{"x":[{"secret":1,"secret":2}]}`, `{"x":` + strings.Repeat(`[`, 66) + `0` + strings.Repeat(`]`, 66) + `}`, `{"x":"` + strings.Repeat("x", 32768) + `"}`} {
		var dest map[string]any
		if DecodeIntent([]byte(raw), &dest) == nil {
			t.Fatal("ambiguous/oversized JSON intent accepted")
		}
	}
	var dest struct {
		ID string `json:"id"`
	}
	if DecodeIntent([]byte(`{"private-secret":"synthetic"}`), &dest) == nil {
		t.Fatal("unknown top-level intent field accepted")
	}
	if DecodeIntent([]byte(`{"id":"synthetic"}`), &dest) != nil || dest.ID != "synthetic" {
		t.Fatal("valid bounded object rejected")
	}
	for _, value := range []any{validationFullSteps(t), Intent{Payload: json.RawMessage(`{"password":"synthetic-private-secret"}`)}} {
		for _, format := range []string{"%v", "%+v", "%#v"} {
			if strings.Contains(fmt.Sprintf(format, value), "synthetic-private-secret") || strings.Contains(fmt.Sprintf(format, value), "pp-") {
				t.Fatal("sensitive journal formatting leaked")
			}
		}
	}
}
