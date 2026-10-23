package lifecycle

// NextOverdueStep validates the confirmed prefix and returns its next step.
// pending is true when that step already has a durable, unconfirmed intent;
// complete is true only when all three receipts are confirmed.
func NextOverdueStep(steps map[EnforcementStep]OverdueStep) (next EnforcementStep, pending, complete, valid bool) {
	order := [...]EnforcementStep{OverduePauseRecurring, OverdueDisableSUI, OverdueCloseSessions}
	for i, step := range order {
		value, exists := steps[step]
		if !exists {
			if hasLaterOverdueStep(steps, order[i+1:]) {
				return "", false, false, false
			}
			return step, false, false, true
		}
		if !value.Confirmed {
			if hasLaterOverdueStep(steps, order[i+1:]) {
				return "", false, false, false
			}
			return step, true, false, true
		}
	}
	if len(steps) != len(order) {
		return "", false, false, false
	}
	return "", false, true, true
}

func hasLaterOverdueStep(steps map[EnforcementStep]OverdueStep, later []EnforcementStep) bool {
	for _, step := range later {
		if _, exists := steps[step]; exists {
			return true
		}
	}
	for step := range steps {
		if !step.Valid() {
			return true
		}
	}
	return false
}
