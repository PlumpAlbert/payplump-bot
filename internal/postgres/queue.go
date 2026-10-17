package postgres

import (
	"errors"
	"time"
)

func validateClaim(limit int, lease time.Duration) error {
	if limit < 1 || limit > 100 || lease < time.Second || lease > 5*time.Minute {
		return errors.New("queue batch must be 1-100 and lease must be 1s-5m")
	}
	return nil
}

func validateDelay(delay time.Duration) error {
	if delay < 0 {
		return errors.New("queue retry delay must not be negative")
	}
	return nil
}
