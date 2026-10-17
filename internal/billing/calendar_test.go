package billing

import (
	"testing"
	"time"
)

func TestNextBillingDate(t *testing.T) {
	for _, tc := range []struct{ paid, next string }{
		{"2026-10-20", "2026-11-20"}, // Paid on 20th, confirmed on 21st.
		{"2026-10-17", "2026-11-17"}, // Paid on 17th, confirmed on 18th.
		{"2026-01-29", "2026-02-28"},
		{"2026-01-30", "2026-02-28"},
		{"2026-01-31", "2026-02-28"},
		{"2028-01-31", "2028-02-29"},
		{"2028-02-29", "2028-03-29"},
		{"2026-03-31", "2026-04-30"},
		{"2026-12-31", "2027-01-31"},
	} {
		t.Run(tc.paid, func(t *testing.T) {
			got, err := NextBillingDate(tc.paid)
			if err != nil || got != tc.next {
				t.Fatalf("got %q, %v; want %s", got, err, tc.next)
			}
		})
	}
}

func TestMoscowOverdueBoundary(t *testing.T) {
	deadline, err := PaymentDeadline("2026-10-17")
	if err != nil {
		t.Fatal(err)
	}
	if got := deadline.UTC().Format(time.RFC3339); got != "2026-10-17T21:00:00Z" {
		t.Fatalf("deadline: %s", got)
	}
	for _, tc := range []struct {
		now     time.Time
		overdue bool
	}{
		{deadline.Add(-time.Nanosecond), false},
		{deadline, true},
		{deadline.Add(time.Hour), true},
	} {
		got, err := IsOverdue("2026-10-17", tc.now.UTC())
		if err != nil || got != tc.overdue {
			t.Fatalf("at %s: %v, %v", tc.now, got, err)
		}
	}
}

func TestInvalidDates(t *testing.T) {
	for _, value := range []string{"", "2026-02-29", "2026-04-31", "2026-2-03", "0000-01-01", "2026-10-17T00:00:00Z"} {
		t.Run(value, func(t *testing.T) {
			if _, err := NextBillingDate(value); err == nil {
				t.Fatal("invalid payment date accepted")
			}
			if _, err := PaymentDeadline(value); err == nil {
				t.Fatal("invalid due date accepted")
			}
			if _, err := IsOverdue(value, time.Now()); err == nil {
				t.Fatal("invalid due date accepted")
			}
		})
	}
	if _, err := NextBillingDate("9999-12-31"); err == nil {
		t.Fatal("overflow accepted")
	}
}
