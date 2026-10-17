package invoiceninja_test

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
)

func TestTelegramIDAllowedFields(t *testing.T) {
	customer := invoiceninja.Customer{
		ID: "100", Number: "200", Name: "Synthetic private customer",
		IDNumber: "9007199254740993", CustomValue1: "9223372036854775807",
		CustomValue2: "1", CustomValue3: "314159265", CustomValue4: "42",
	}
	for field, want := range map[string]int64{
		"id_number": 9007199254740993, "custom_value1": math.MaxInt64,
		"custom_value2": 1, "custom_value3": 314159265, "custom_value4": 42,
	} {
		got, err := customer.TelegramID(field)
		if err != nil || got != want {
			t.Errorf("field %s: got %d, %v; want %d", field, got, err, want)
		}
	}
	for _, field := range []string{"id", "number", "name", "", "ID_NUMBER", "custom_value5", " id_number", "synthetic-private-field"} {
		got, err := customer.TelegramID(field)
		if got != 0 || err == nil {
			t.Errorf("accepted unsupported field %q: %d, %v", field, got, err)
		}
		if err != nil && (strings.Contains(err.Error(), customer.Name) || strings.Contains(err.Error(), "synthetic-private-field")) {
			t.Fatal("field error leaked customer/configuration text")
		}
	}
}

func TestTelegramIDRejectsNoncanonicalAndOverflow(t *testing.T) {
	for _, value := range []string{
		"", "0", "00", "01", "000123", "+1", "-1", " 1", "1 ", "1\n", "1\t", "1\x00", "1.0", "1e3", "1_000", "１２３", "١٢٣",
		"9223372036854775808", "99999999999999999999999999999999999999999999", "synthetic-private-identifier",
	} {
		customer := invoiceninja.Customer{Name: "Synthetic private customer", IDNumber: value}
		got, err := customer.TelegramID("id_number")
		if got != 0 || err == nil {
			t.Errorf("accepted noncanonical ID %q: %d, %v", value, got, err)
		}
		if err != nil && (strings.Contains(err.Error(), customer.Name) || (len(value) > 2 && strings.Contains(err.Error(), value))) {
			t.Fatal("identifier error leaked customer text")
		}
	}
}

func TestActive(t *testing.T) {
	for _, tt := range []struct {
		deleted bool
		archive int64
		active  bool
	}{
		{false, 0, true}, {true, 0, false}, {false, 2200000000, false},
		{true, 2200000000, false}, {false, -1, false},
	} {
		if got := (invoiceninja.Customer{IsDeleted: tt.deleted, ArchivedAt: tt.archive}).Active(); got != tt.active {
			t.Errorf("customer active = %v, want %v", got, tt.active)
		}
		if got := (invoiceninja.Product{IsDeleted: tt.deleted, ArchivedAt: tt.archive}).Active(); got != tt.active {
			t.Errorf("product active = %v, want %v", got, tt.active)
		}
	}
}

func TestNewRejectsInvalidConfiguration(t *testing.T) {
	for _, token := range []string{"", " synthetic-secret", "synthetic-secret ", "synthetic\nsecret", "synthetic\rsecret", "synthetic\tsecret", "synthetic\x00secret", "synthetic\x7fsecret", "\u00a0synthetic-secret", "synthetic\u0085secret", "synthetic\xffsecret"} {
		c, err := invoiceninja.New("https://synthetic.invalid/panel", token, time.Second)
		if c != nil || err == nil {
			t.Errorf("accepted invalid token %q", token)
		}
		if err != nil && strings.Contains(err.Error(), "synthetic") {
			t.Fatal("configuration error leaked token text")
		}
	}
	for _, base := range []string{
		"", "://synthetic-private", "synthetic.invalid", "ftp://synthetic.invalid", "https:///panel",
		"https://synthetic-user:synthetic-secret@synthetic.invalid", "https://synthetic.invalid?token=synthetic-secret", "https://synthetic.invalid?", "https://synthetic.invalid#synthetic-secret", "https://synthetic.invalid#",
		" https://synthetic.invalid", "https://synthetic.invalid ", "https://synthetic.invalid/\nsecret", "https://synthetic.invalid/%00secret",
		"https://synthetic.invalid/api/v1", "https://synthetic.invalid/api/v1/", "https://synthetic.invalid/panel/api/v1/products", "https://synthetic.invalid/panel/API/V1", "https://synthetic.invalid/panel/%61pi/%761", "https://synthetic.invalid/panel/api", "https://synthetic.invalid/panel/api/v2",
		"https://synthetic.invalid/panel/../secret", "https://synthetic.invalid/panel/%2e%2e/secret", "https://synthetic.invalid/panel/./secret", "https://synthetic.invalid/panel//secret", "https://synthetic.invalid//panel", "https://synthetic.invalid/panel//",
		"https://synthetic.invalid/panel%2fsecret", "https://synthetic.invalid/panel%5csecret", "https://synthetic.invalid/%zz", "https://synthetic.invalid:/panel", "https://synthetic.invalid:65536/panel", "https://synthetic.invalid:0/panel",
	} {
		c, err := invoiceninja.New(base, syntheticToken, time.Second)
		if c != nil || err == nil {
			t.Errorf("accepted invalid base %q", base)
		}
		if err != nil && strings.Contains(err.Error(), "synthetic") {
			t.Fatal("configuration error leaked URL/credential text")
		}
	}
	for _, timeout := range []time.Duration{0, -time.Second, 31 * time.Second} {
		if c, err := invoiceninja.New("https://synthetic.invalid/panel", syntheticToken, timeout); err == nil || c != nil {
			t.Errorf("accepted invalid timeout %v", timeout)
		}
	}
}

func TestNewAcceptsServiceBasesWithoutNetwork(t *testing.T) {
	for _, base := range []string{"https://synthetic.invalid", "https://synthetic.invalid/", "http://synthetic.invalid:8080/panel", "https://synthetic.invalid/installed/panel/", "https://synthetic.invalid/installed%20panel", "http://[::1]:8080/panel"} {
		if _, err := invoiceninja.New(base, syntheticToken, time.Second); err != nil {
			t.Errorf("rejected valid service base %q: %v", base, err)
		}
	}
}
