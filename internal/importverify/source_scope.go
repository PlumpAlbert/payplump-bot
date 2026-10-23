package importverify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
)

// SourceScope hashes the normalized source tenants, pinned versions, and
// fingerprints of credentials. Raw credentials are never included in output.
func SourceScope(ninjaBase, ninjaToken, ninjaVersion, suiBase, suiToken, suiVersion string) (string, error) {
	if integrationhttp.ValidateBaseURL(ninjaBase) != nil || integrationhttp.ValidateBaseURL(suiBase) != nil ||
		!validSecret(ninjaToken) || !validSecret(suiToken) || normalizeNinjaVersion(ninjaVersion) != "5.13.47" || normalizeSUIVersion(suiVersion) != "1.6.4" {
		return "", errors.New("invalid import verification source scope")
	}
	ninja, err := normalizedBase(ninjaBase)
	if err != nil {
		return "", errors.New("invalid import verification source scope")
	}
	panel, err := normalizedBase(suiBase)
	if err != nil {
		return "", errors.New("invalid import verification source scope")
	}
	value := struct {
		Version      string `json:"version"`
		NinjaBase    string `json:"ninja_base"`
		NinjaVersion string `json:"ninja_version"`
		NinjaToken   string `json:"ninja_token_sha256"`
		SUIBase      string `json:"sui_base"`
		SUIVersion   string `json:"sui_version"`
		SUIToken     string `json:"sui_token_sha256"`
	}{"import-source-scope-v1", ninja, "5.13.47", fingerprint(ninjaToken), panel, "1.6.4", fingerprint(suiToken)}
	data, err := json.Marshal(value)
	if err != nil {
		return "", errors.New("invalid import verification source scope")
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func fingerprint(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
func validSecret(s string) bool {
	return s != "" && utf8.ValidString(s) && strings.IndexFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}
func normalizeNinjaVersion(s string) string { return strings.TrimPrefix(s, "v") }
func normalizeSUIVersion(s string) string   { return strings.TrimPrefix(s, "v") }
func normalizedBase(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	escapedPath := strings.TrimRight(u.EscapedPath(), "/")
	return u.Scheme + "://" + u.Host + escapedPath, nil
}
