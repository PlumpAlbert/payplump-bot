package sui

import (
	"testing"
	"time"
)

func TestClientsAcceptPositiveHTTPTimeouts(t *testing.T) {
	for _, timeout := range []time.Duration{500 * time.Millisecond, 30 * time.Second, 31 * time.Second, 2 * time.Minute} {
		t.Run(timeout.String(), func(t *testing.T) {
			const base, token = "https://synthetic.invalid/panel", "synthetic-token"
			if err := ValidateClientConfiguration(base, token, timeout); err != nil {
				t.Fatal(err)
			}
			if _, err := New(base, token, timeout); err != nil {
				t.Fatal(err)
			}
			if err := ValidateManagedClientConfiguration(base, token, "1.6.4", timeout, 10); err != nil {
				t.Fatal(err)
			}
			if _, err := NewManagedClient(base, token, "1.6.4", timeout, 10); err != nil {
				t.Fatal(err)
			}
			if _, err := NewRecordReader(base, token, "1.6.4", timeout); err != nil {
				t.Fatal(err)
			}
		})
	}
}
