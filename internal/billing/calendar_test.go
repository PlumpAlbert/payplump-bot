package billing

import (
	"os"
	"os/exec"
	"testing"
	"time"
	_ "time/tzdata" // Test DST and TZ resolution without requiring host tzdata.
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

func TestLocalOverdueBoundary(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name          string
		location      *time.Location
		due, deadline string
	}{
		{"UTC", time.UTC, "2026-10-17", "2026-10-18T00:00:00Z"},
		{"east", time.FixedZone("server", 9*60*60), "2026-10-17", "2026-10-17T15:00:00Z"},
		{"west", time.FixedZone("server", -7*60*60), "2026-10-17", "2026-10-18T07:00:00Z"},
		{"DST spring", newYork, "2026-03-08", "2026-03-09T04:00:00Z"},
		{"DST autumn", newYork, "2026-11-01", "2026-11-02T05:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// These tests are sequential because time.Local is process-wide.
			previous := time.Local
			time.Local = tc.location
			defer func() { time.Local = previous }()
			deadline, err := PaymentDeadline(tc.due)
			if err != nil {
				t.Fatal(err)
			}
			if got := deadline.UTC().Format(time.RFC3339); got != tc.deadline {
				t.Fatalf("deadline: %s; want %s", got, tc.deadline)
			}
			for _, boundary := range []struct {
				now     time.Time
				overdue bool
			}{
				{deadline.Add(-time.Nanosecond), false},
				{deadline, true},
				{deadline.Add(time.Hour), true},
			} {
				got, err := IsOverdue(tc.due, boundary.now.UTC())
				if err != nil || got != boundary.overdue {
					t.Fatalf("at %s: %v, %v", boundary.now, got, err)
				}
			}
		})
	}
}

func TestServerTimezoneResolution(t *testing.T) {
	// Local is initialized once by the standard library. Use a fresh process to
	// exercise its real TZ lookup and unavailable-data fallback for each case.
	if os.Getenv("PAYPLUMP_TIMEZONE_TEST") == "1" {
		deadline, err := PaymentDeadline("2026-10-17")
		if err != nil {
			t.Fatal(err)
		}
		if got := deadline.UTC().Format(time.RFC3339); got != os.Getenv("PAYPLUMP_EXPECTED_DEADLINE") {
			t.Fatalf("deadline: %s", got)
		}
		return
	}
	for _, tc := range []struct{ name, zone, expected string }{
		{"configured server zone", "America/New_York", "2026-10-18T04:00:00Z"},
		{"UTC", "", "2026-10-18T00:00:00Z"},
		{"unavailable timezone", ":" + t.TempDir() + "/missing-zone", "2026-10-18T00:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestServerTimezoneResolution$")
			cmd.Env = append(os.Environ(), "TZ="+tc.zone, "PAYPLUMP_TIMEZONE_TEST=1", "PAYPLUMP_EXPECTED_DEADLINE="+tc.expected)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("timezone check: %v\n%s", err, output)
			}
		})
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
