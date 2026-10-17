// Package billing implements calendar rules independently of external APIs.
package billing

import (
	"errors"
	"time"
)

func parseDate(value string) (time.Time, error) {
	// Go resolves Local from TZ or the server's /etc/localtime and falls back
	// to UTC if timezone data is unavailable.
	d, err := time.ParseInLocation(time.DateOnly, value, time.Local)
	if err != nil || d.Year() < 1 || d.Format(time.DateOnly) != value {
		return time.Time{}, errors.New("expected a valid YYYY-MM-DD date")
	}
	return d, nil
}

// NextBillingDate uses Invoice Ninja Payment.date, never webhook arrival time.
// Days 29–31 are clamped to the last day of the following month. This rule must
// also be checked against the deployed Invoice Ninja version before rollout.
func NextBillingDate(paymentDate string) (string, error) {
	paid, err := parseDate(paymentDate)
	if err != nil {
		return "", err
	}
	first := time.Date(paid.Year(), paid.Month()+1, 1, 0, 0, 0, 0, paid.Location())
	last := first.AddDate(0, 1, -1).Day()
	day := min(paid.Day(), last)
	next := time.Date(first.Year(), first.Month(), day, 0, 0, 0, 0, paid.Location())
	if next.Year() > 9999 {
		return "", errors.New("next billing date exceeds supported range")
	}
	return next.Format(time.DateOnly), nil
}

// PaymentDeadline is the first instant after the due date in the server's local
// timezone (UTC when unavailable).
// Access is retained throughout the due date; at this instant it is overdue.
func PaymentDeadline(dueDate string) (time.Time, error) {
	due, err := parseDate(dueDate)
	if err != nil {
		return time.Time{}, err
	}
	return due.AddDate(0, 0, 1), nil
}

// IsOverdue checks the calendar boundary only. Callers must still verify the
// invoice's current payment state with Invoice Ninja before changing access.
func IsOverdue(dueDate string, now time.Time) (bool, error) {
	deadline, err := PaymentDeadline(dueDate)
	if err != nil {
		return false, err
	}
	return !now.Before(deadline), nil
}
