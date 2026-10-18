package postgres

import (
	"crypto/sha256"
	"encoding/base64"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/broadcast"
)

func TestBroadcastNonceCreationAndPresentationUseSameCommitment(t *testing.T) {
	token, err := broadcast.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := tokenHash(token)
	if !ok || got != sha256.Sum256([]byte(token)) {
		t.Fatal("presented nonce differs from stored commitment")
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != 24 || len(token) != 32 {
		t.Fatal("nonce entropy or encoding changed")
	}
	for _, bad := range []string{token + "=", token + " ", token[:31], "/" + token[1:]} {
		if _, ok := tokenHash(bad); ok {
			t.Fatal("noncanonical confirmation token accepted")
		}
	}
}
