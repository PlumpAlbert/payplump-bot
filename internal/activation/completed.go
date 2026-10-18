package activation

import (
	"encoding/json"
	"strconv"

	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

// ValidateCompletedSnapshot validates the completed FIRST-cycle audit trail and
// its exact persisted binding. It does not perform fresh provider/payment reads
// and is not, by itself, permission to deliver a personal configuration.
func ValidateCompletedSnapshot(s Snapshot, binding sui.Binding) error {
	a := s.Account
	if a.State != "active" || !a.DesiredAccess || s.PaymentReview || s.ReviewCode != 0 || !ValidSUIID(a.SUIID) || !ValidID(a.RecurringID) || binding.ID <= 0 || strconv.FormatInt(binding.ID, 10) != a.SUIID {
		return ErrReviewRequired
	}
	// The immutable requests describe the pre-activation state. Validate their
	// historical projection ONLY AFTER proving the current completed binding.
	historical := s
	historical.Account.State = "awaiting_payment"
	historical.Account.DesiredAccess = false
	historical.Account.SUIID = ""
	historical.Account.RecurringID = ""
	if ValidateSnapshot(historical) != nil {
		return ErrReviewRequired
	}
	for _, step := range OrderedSteps() {
		intent, ok := s.Steps[step]
		if !ok || intent.ExternalID == "" || ValidateIntent(historical, step, intent.Payload) != nil {
			return ErrReviewRequired
		}
	}
	if next, e := NextStep(historical); e != nil || next != "" {
		return ErrReviewRequired
	}
	created, started := s.Steps[RecurringCreate], s.Steps[RecurringStart]
	if s.Steps[SUICreate].ExternalID != a.SUIID || s.Steps[SUIEnable].ExternalID != a.SUIID || created.ExternalID != a.RecurringID || started.ExternalID != a.RecurringID {
		return ErrReviewRequired
	}
	original, e := SUIBinding(historical)
	if e != nil || original != binding {
		return ErrReviewRequired
	}
	amount, instant, e := NormalizeRecurringFacts(json.Number(created.RecurringAmount), created.RecurringDatetime)
	if e != nil {
		return ErrReviewRequired
	}
	otherAmount, otherInstant, e := NormalizeRecurringFacts(json.Number(started.RecurringAmount), started.RecurringDatetime)
	if e != nil || amount != otherAmount || instant != otherInstant {
		return ErrReviewRequired
	}
	return nil
}
