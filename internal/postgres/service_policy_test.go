package postgres

import (
	"errors"
	"math"
	"testing"
)

func TestServicePolicyRevisionIsMonotonicAndNeverRearmsRevocation(t *testing.T) {
	tests := []struct {
		name           string
		found, enabled bool
		revision       int64
		same           bool
		want           int64
		fail           bool
	}{
		{"first-install", false, false, 0, false, 1, false},
		{"unchanged-restart", true, true, 7, true, 7, false},
		{"changed-enabled-policy", true, true, 7, false, 8, false},
		{"same-revoked-policy", true, false, 7, true, 0, true},
		{"changed-revoked-policy", true, false, 7, false, 0, true},
		{"invalid-zero", true, true, 0, true, 0, true},
		{"exhausted-changed", true, true, math.MaxInt64, false, 0, true},
		{"exhausted-unchanged", true, true, math.MaxInt64, true, math.MaxInt64, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, e := servicePolicyRevision(tt.found, tt.enabled, tt.revision, tt.same)
			if (e != nil) != tt.fail || got != tt.want {
				t.Fatalf("revision=%d error=%v", got, e)
			}
			if tt.found && !tt.enabled && !errors.Is(e, ErrServicePolicyRevoked) {
				t.Fatal("revocation not distinguished")
			}
		})
	}
}
