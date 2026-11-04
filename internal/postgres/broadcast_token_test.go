package postgres

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/broadcast"
)

// Test-only extraction of a displayed command argument. Templates are plain
// text; matching literal backticks are presentation, not part of the nonce.
// Production command parsing and canonical token validation stay strict.
func broadcastPresentedToken(body, separator string) (string, bool) {
	prefix, tail, found := strings.Cut(body, separator)
	fields := strings.Fields(tail)
	if !found || len(fields) == 0 {
		return "", false
	}
	token := fields[0]
	if strings.HasSuffix(prefix, "`") && strings.HasSuffix(token, "`") {
		token = strings.TrimSuffix(token, "`")
	}
	_, valid := tokenHash(token)
	return token, valid
}

func TestBroadcastPresentedTokenKeepsCanonicalValidation(t *testing.T) {
	const token = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	const command = "/broadcast_all "
	for _, body := range []string{command + token, "Команда: `" + command + token + "`", "Предпросмотр\n" + command + token + "\nДалее"} {
		if got, ok := broadcastPresentedToken(body, command); !ok || got != token {
			t.Fatal("displayed canonical token not extracted")
		}
	}
	for _, body := range []string{command, "nothing", command + token + "`", "`" + command + token + "``", command + token + "=", "`" + command + token[:31] + "`", "`" + command + "/" + token[1:] + "`"} {
		if _, ok := broadcastPresentedToken(body, command); ok {
			t.Fatal("malformed displayed token accepted")
		}
	}
}

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
