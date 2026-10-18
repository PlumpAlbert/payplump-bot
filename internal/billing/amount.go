package billing

import (
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
)

var decimalPattern = regexp.MustCompile(`^([0-9]+)(?:\.([0-9]+))?(?:[eE]([+-]?[0-9]+))?$`)

// Expand a bounded JSON decimal exactly. No float, currency conversion or
// rounding is involved; the amount always comes from the concrete invoice.
// FormatAmount expands a bounded JSON decimal without floating-point rounding.
func FormatAmount(value json.Number) (string, error) {
	raw := value.String()
	if len(raw) > 64 {
		return "", errors.New("unsupported invoice amount")
	}
	parts := decimalPattern.FindStringSubmatch(raw)
	if parts == nil || len(parts[1]) > 1 && parts[1][0] == '0' {
		return "", errors.New("unsupported invoice amount")
	}
	exponent := 0
	if parts[3] != "" {
		var err error
		exponent, err = strconv.Atoi(parts[3])
		if err != nil || exponent < -32 || exponent > 32 {
			return "", errors.New("unsupported invoice amount")
		}
	}
	digits := parts[1] + parts[2]
	point := len(parts[1]) + exponent
	var whole, fraction string
	switch {
	case point <= 0:
		whole = "0"
		fraction = strings.Repeat("0", -point) + digits
	case point >= len(digits):
		whole = digits + strings.Repeat("0", point-len(digits))
	default:
		whole, fraction = digits[:point], digits[point:]
	}
	whole = strings.TrimLeft(whole, "0")
	if whole == "" {
		whole = "0"
	}
	fraction = strings.TrimRight(fraction, "0")
	if fraction != "" {
		return whole + "." + fraction, nil
	}
	return whole, nil
}
