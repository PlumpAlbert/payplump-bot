// Package billing implements calendar rules independently of external APIs.
package billing

import (
	"errors"
	"time"
	_ "time/tzdata" // Keep Moscow rules available in a static executable.
)

var moscow = mustMoscow()

func mustMoscow() *time.Location {
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		panic(err)
	}
	return loc
}

func parseDate(value string) (time.Time, error) {
	d, err := time.ParseInLocation(time.DateOnly, value, moscow)
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
	first := time.Date(paid.Year(), paid.Month()+1, 1, 0, 0, 0, 0, moscow)
	last := first.AddDate(0, 1, -1).Day()
	day := min(paid.Day(), last)
	next := time.Date(first.Year(), first.Month(), day, 0, 0, 0, 0, moscow)
	if next.Year() > 9999 {
		return "", errors.New("next billing date exceeds supported range")
	}
	return next.Format(time.DateOnly), nil
}

// PaymentDeadline is the first instant after the due date in Europe/Moscow.
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
