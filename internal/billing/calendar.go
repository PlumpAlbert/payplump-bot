// Package billing implements calendar rules independently of external APIs.
package billing

import (
	"errors"
	"time"
)

func parseDate(value string) (time.Time, error) {
	// Date-only values are calendar dates, not local instants. UTC avoids
	// normalizing dates whose local midnight is skipped or repeated.
	d, err := time.Parse(time.DateOnly, value)
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
	first := time.Date(paid.Year(), paid.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	last := first.AddDate(0, 1, -1).Day()
	day := min(paid.Day(), last)
	next := time.Date(first.Year(), first.Month(), day, 0, 0, 0, 0, time.UTC)
	if next.Year() > 9999 {
		return "", errors.New("next billing date exceeds supported range")
	}
	return next.Format(time.DateOnly), nil
}

// PaymentDeadline is the first instant after the due date's entire local
// calendar day. Date arithmetic stays in UTC; the returned instant is in the
// server's local timezone (UTC when timezone data is unavailable).
// Access is retained throughout the due date; at this instant it is overdue.
func PaymentDeadline(dueDate string) (time.Time, error) {
	due, err := parseDate(dueDate)
	if err != nil {
		return time.Time{}, err
	}
	return endOfLocalDate(due, time.Local), nil
}

// endOfLocalDate finds the end of all occurrences of a local calendar date.
// Walking the nearby constant-offset zone intervals handles midnight gaps and
// folds without assuming that time.Date can represent local midnight.
func endOfLocalDate(date time.Time, location *time.Location) time.Time {
	localStart := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC)
	localEnd := localStart.AddDate(0, 0, 1)
	searchStart, searchEnd := localStart.AddDate(0, 0, -2), localEnd.AddDate(0, 0, 2)

	var lastDateEnd, firstLater time.Time
	for cursor := searchStart; cursor.Before(searchEnd); {
		local := cursor.In(location)
		_, offset := local.Zone()
		_, zoneEnd := local.ZoneBounds()
		intervalEnd := searchEnd
		if !zoneEnd.IsZero() && zoneEnd.Before(intervalEnd) {
			intervalEnd = zoneEnd
		}
		if !intervalEnd.After(cursor) {
			break
		}

		offsetDuration := time.Duration(offset) * time.Second
		dateStart := localStart.Add(-offsetDuration)
		dateEnd := localEnd.Add(-offsetDuration)
		start := maxTime(cursor, dateStart)
		end := minTime(intervalEnd, dateEnd)
		if start.Before(end) && end.After(lastDateEnd) {
			lastDateEnd = end
		}

		// If the zone skipped the requested date entirely, the first instant
		// whose local date is later than it is the appropriate boundary.
		later := maxTime(cursor, dateEnd)
		if later.Before(intervalEnd) && (firstLater.IsZero() || later.Before(firstLater)) {
			firstLater = later
		}
		cursor = intervalEnd
	}
	if !lastDateEnd.IsZero() {
		return lastDateEnd.In(location)
	}
	if !firstLater.IsZero() {
		return firstLater.In(location)
	}
	// The search window spans beyond any real-world UTC offset and date-line
	// shift. Keep a safe fallback for an unusual synthetic Location.
	return time.Date(localEnd.Year(), localEnd.Month(), localEnd.Day(), 0, 0, 0, 0, location)
}

func maxTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return b
	}
	return a
}

func minTime(a, b time.Time) time.Time {
	if a.After(b) {
		return b
	}
	return a
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
