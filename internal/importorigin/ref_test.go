package importorigin

import (
	"encoding/json"
	"log/slog"
	"testing"
)

func TestRefValidityComparabilityAndRedaction(t *testing.T) {
	var hash [32]byte
	hash[0] = 1
	r := Ref{ClientID: 1, BotID: 2, AdmissionRevision: 3, RuntimeRevision: 4, StableHash: hash, PolicyHash: hash, SourceScopeHash: hash}
	if !r.Valid() || r != r {
		t.Fatal("valid ref did not remain comparable")
	}
	advanced := r
	advanced.ConfigurationRevision = 1
	if advanced == r || !advanced.Valid() {
		t.Fatal("configuration revision was not part of the comparable reference")
	}
	if (Ref{}).Valid() {
		t.Fatal("zero ref is valid")
	}
	if got := r.String(); got != "<private imported origin reference>" || r.GoString() != got || r.LogValue().Kind() != slog.KindString {
		t.Fatal("ref formatting is not private")
	}
	b, err := json.Marshal(r)
	var redacted string
	if err != nil || json.Unmarshal(b, &redacted) != nil || redacted != "<private imported origin reference>" {
		t.Fatalf("JSON redaction: %s %v", b, err)
	}
	if json.Unmarshal([]byte(`{}`), &r) == nil {
		t.Fatal("decoded untrusted reference")
	}
	parsed, ok := ParseHashHex("0100000000000000000000000000000000000000000000000000000000000000")
	if !ok || parsed != hash {
		t.Fatal("canonical nonzero hash was not parsed")
	}
	for _, invalid := range []string{"", "ABCDEF", "0000000000000000000000000000000000000000000000000000000000000000"} {
		if _, ok := ParseHashHex(invalid); ok {
			t.Fatalf("accepted invalid hash %q", invalid)
		}
	}
}
