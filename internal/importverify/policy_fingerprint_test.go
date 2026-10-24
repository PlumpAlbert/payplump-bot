package importverify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestPolicyFingerprintMatchesLegacyEncoding(t *testing.T) {
	policy := validPolicy()
	encoded, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(encoded)
	want := hex.EncodeToString(sum[:])
	got, err := policy.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("fingerprint=%s, legacy policy hash=%s", got, want)
	}
}

func TestPolicyFingerprintTracksPolicyFieldsAndRejectsInvalidPolicy(t *testing.T) {
	base := validPolicy()
	baseline, err := base.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	variants := []Policy{base, base, base, base, base, base}
	variants[0].PilotIDs = []string{"43"}
	variants[1].SourceScopeHash = strings.Repeat("b", 64)
	variants[2].NativeCurrencyCode = "EUR"
	variants[3].ExcludedIDs = []string{"42"}
	variants[4].ExpectedInboundIDs = []int64{2}
	variants[5].EvidenceTTL++
	for i, variant := range variants {
		got, err := variant.Fingerprint()
		if err != nil {
			t.Fatalf("variant %d invalid: %v", i, err)
		}
		if got == baseline {
			t.Fatalf("variant %d did not change fingerprint", i)
		}
	}
	invalid := base
	invalid.BotID = 0
	if _, err := invalid.Fingerprint(); err == nil {
		t.Fatal("invalid policy fingerprint accepted")
	}
}
