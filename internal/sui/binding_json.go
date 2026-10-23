package sui

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A binding is authorization metadata, not a provider record. Its exact field
// names are frozen by our journal encoder; reject aliases/extra facts before
// encoding/json can erase conflicting IDs or fingerprints.
func (b *Binding) UnmarshalJSON(raw []byte) error {
	if len(raw) > 4096 {
		return ErrManagedInvalid
	}
	value, err := uniqueJSON(raw)
	if err != nil {
		return ErrManagedInvalid
	}
	fields, ok := value.(map[string]any)
	if !ok || len(fields) != 3 || fields["ID"] == nil || fields["Name"] == nil || fields["CredentialsHash"] == nil {
		return ErrManagedInvalid
	}
	hashValues, ok := fields["CredentialsHash"].([]any)
	if !ok || len(hashValues) != 32 {
		return ErrManagedInvalid
	}
	var hash [32]byte
	for i, value := range hashValues {
		number, ok := value.(json.Number)
		if !ok {
			return ErrManagedInvalid
		}
		byteValue, err := strconv.ParseUint(number.String(), 10, 8)
		if err != nil {
			return ErrManagedInvalid
		}
		hash[i] = byte(byteValue)
	}
	type plain Binding
	var decoded plain
	if json.Unmarshal(raw, &decoded) != nil || decoded.ID <= 0 || !validBindingName(decoded.Name) || hash == ([32]byte{}) {
		return ErrManagedInvalid
	}
	decoded.CredentialsHash = hash
	*b = Binding(decoded)
	return nil
}
func validBindingName(name string) bool {
	return len(name) > 0 && len(name) <= 256 && utf8.ValidString(name) && name != "." && name != ".." && strings.IndexFunc(name, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}
