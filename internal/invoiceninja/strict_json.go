package invoiceninja

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/url"
	"unicode"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
)

// uniqueNinjaJSON validates one bounded value before encoding/json can erase
// conflicting authority fields, including in unknown metadata. Numbers remain
// lexical tokens, never float64. Direct DTO decoding has the same read bounds.
func uniqueNinjaJSON(raw []byte) bool {
	if len(raw) > integrationhttp.MaxResponseBytes {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if !ninjaJSONValue(d, 0) {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}

func ninjaJSONValue(d *json.Decoder, depth int) bool {
	if depth > 128 {
		return false
	}
	token, err := d.Token()
	if err != nil {
		return false
	}
	switch token {
	case json.Delim('{'):
		seen := make(map[string]bool)
		for d.More() {
			key, err := d.Token()
			name, ok := key.(string)
			if err != nil || !ok {
				return false
			}
			name = ninjaJSONFold(name)
			if seen[name] {
				return false
			}
			seen[name] = true
			if !ninjaJSONValue(d, depth+1) {
				return false
			}
		}
		token, err = d.Token()
		return err == nil && token == json.Delim('}')
	case json.Delim('['):
		for d.More() {
			if !ninjaJSONValue(d, depth+1) {
				return false
			}
		}
		token, err = d.Token()
		return err == nil && token == json.Delim(']')
	default:
		_, delimiter := token.(json.Delim)
		return !delimiter
	}
}

// getUniqueNinjaJSON applies the strict read pipeline used by pinned Invoice
// Ninja capabilities. Callers keep their own version and structural contracts.
func getUniqueNinjaJSON(ctx context.Context, client *integrationhttp.Client, path []string, query url.Values, expectedVersion string, versionErr error, result any) error {
	var raw json.RawMessage
	headers, err := client.Get(ctx, path, query, &raw)
	if err != nil {
		return err
	}
	versions := headers.Values("X-APP-VERSION")
	if len(versions) != 1 || versions[0] != expectedVersion {
		return versionErr
	}
	if !uniqueNinjaJSON(raw) || json.Unmarshal(raw, result) != nil {
		return errInvalidResponse
	}
	return nil
}

// encoding/json matches field names with Unicode simple folding, not lower
// casing: long s and Kelvin sign also alias ASCII S and K. Use the minimum rune
// of each complete fold cycle without rejecting unrelated non-ASCII names.
func ninjaJSONFold(name string) string {
	folded := []rune(name)
	for i, r := range folded {
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < folded[i] {
				folded[i] = next
			}
		}
	}
	return string(folded)
}
