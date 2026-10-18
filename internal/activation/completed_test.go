package activation

import (
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

func completedValidationFixture(t *testing.T) (Snapshot, sui.Binding) {
	t.Helper()
	s := validationFullSteps(t)
	binding, err := SUIBinding(s)
	if err != nil {
		t.Fatal("invalid synthetic binding")
	}
	s.Account.State = "active"
	s.Account.DesiredAccess = true
	s.Account.SUIID = s.Steps[SUICreate].ExternalID
	s.Account.RecurringID = s.Steps[RecurringCreate].ExternalID
	return s, binding
}
func TestCompletedActivationRequiresExactFourStepAuditAndBinding(t *testing.T) {
	s, binding := completedValidationFixture(t)
	if ValidateCompletedSnapshot(s, binding) != nil {
		t.Fatal("completed audit rejected")
	}
	if ValidateSnapshot(s) == nil {
		t.Fatal("completed snapshot bypassed unactivated writer validator")
	}
	for _, change := range []func(*Snapshot, *sui.Binding){func(s *Snapshot, _ *sui.Binding) { s.Account.State = "awaiting_payment" }, func(s *Snapshot, _ *sui.Binding) { s.Account.DesiredAccess = false }, func(s *Snapshot, _ *sui.Binding) { s.Account.RecurringID = "other-recurring" }, func(s *Snapshot, _ *sui.Binding) { s.Account.InvoiceID = "renewal" }, func(s *Snapshot, _ *sui.Binding) { s.PaymentReview = true }, func(s *Snapshot, _ *sui.Binding) { s.ReviewCode = ReviewContract }, func(s *Snapshot, _ *sui.Binding) { delete(s.Steps, RecurringStart) }, func(s *Snapshot, _ *sui.Binding) {
		v := s.Steps[SUIEnable]
		v.ExternalID = "18"
		s.Steps[SUIEnable] = v
	}, func(s *Snapshot, _ *sui.Binding) {
		v := s.Steps[RecurringStart]
		v.RecurringAmount = "101"
		s.Steps[RecurringStart] = v
	}, func(s *Snapshot, _ *sui.Binding) {
		v := s.Steps[RecurringStart]
		v.RecurringDatetime = "2040-03-01T12:00:00Z"
		s.Steps[RecurringStart] = v
	}, func(_ *Snapshot, b *sui.Binding) { b.ID++ }, func(_ *Snapshot, b *sui.Binding) { b.Name = "other" }, func(_ *Snapshot, b *sui.Binding) { b.CredentialsHash[0] ^= 1 }} {
		s, b := completedValidationFixture(t)
		change(&s, &b)
		if ValidateCompletedSnapshot(s, b) == nil {
			t.Fatal("incomplete or changed completed audit accepted")
		}
	}
	// Provider serialization changes are not a different schedule or amount.
	s, binding = completedValidationFixture(t)
	v := s.Steps[RecurringStart]
	v.RecurringAmount = "100.00"
	v.RecurringDatetime = "2040-02-29T15:00:00.000+03:00"
	s.Steps[RecurringStart] = v
	if ValidateCompletedSnapshot(s, binding) != nil {
		t.Fatal("equivalent receipt rejected")
	}
}
func TestActiveFlagWithoutCompletedOwnAuditNeverSuffices(t *testing.T) {
	s := validationSnapshot(t)
	s.Account.State = "active"
	s.Account.DesiredAccess = true
	s.Account.SUIID = "17"
	s.Account.RecurringID = "recurring-A"
	if ValidateCompletedSnapshot(s, sui.Binding{ID: 17, Name: "not-a-proof"}) == nil {
		t.Fatal("local active flag authorized personal disclosure")
	}
}
