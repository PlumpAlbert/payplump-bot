package postgres

import (
	"errors"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
)

func TestAuthorityActivationModeCannotBeFreshenedUnderReservation(t *testing.T) {
	// Mode is an independently compared durable field, not inferred from missing
	// SUI receipts or the caller's current configuration.
	a := activation.Snapshot{Mode: activation.NinjaAuthorityMode}
	old := a
	old.Mode = ""
	if err := authorizeActivation(a, old, "2039-02-01"); !errors.Is(err, queue.ErrConflict) {
		t.Fatal("legacy preflight reinterpreted as authority")
	}
	if err := authorizeActivation(old, a, "2039-02-01"); !errors.Is(err, queue.ErrConflict) {
		t.Fatal("authority preflight reinterpreted as legacy")
	}
}

func TestAuthorityActivationStoreRequiresExplicitEnabledPolicy(t *testing.T) {
	for _, cfg := range []activation.Config{{}, {Enabled: true}, {NinjaAuthority: true}, {NinjaAuthority: true, Enabled: true}} {
		if _, err := NewAuthorityActivationStore(&Store{}, cfg); err == nil {
			t.Fatal("invalid authority policy accepted")
		}
	}
	if _, err := NewAuthorityActivationStore(nil, activation.Config{NinjaAuthority: true, Enabled: true}); err == nil {
		t.Fatal("nil store accepted")
	}
}
