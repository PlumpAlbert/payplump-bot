package importverify

import (
	"strings"
	"testing"
)

func TestSourceScopeRejectsMalformedCredentials(t *testing.T) {
	bad := []string{"token with space", "token\twith-tab", "token\u0085control", string([]byte{0xff})}
	for _, token := range bad {
		if _, err := SourceScope("https://ninja.invalid", token, "5.13.47", "http://panel.invalid", "panel-token", "1.6.4"); err == nil {
			t.Fatal("malformed Invoice Ninja credential accepted")
		}
		if _, err := SourceScope("https://ninja.invalid", "ninja-token", "5.13.47", "http://panel.invalid", token, "1.6.4"); err == nil {
			t.Fatal("malformed S-UI credential accepted")
		}
	}
}

func TestSourceScopeNormalizationAndBinding(t *testing.T) {
	base, err := SourceScope("https://EXAMPLE.invalid/ninja/", "ninja-secret", "v5.13.47", "http://panel.invalid/prefix/", "panel-secret", "v1.6.4")
	if err != nil {
		t.Fatal(err)
	}
	equivalent, err := SourceScope("https://example.invalid/ninja", "ninja-secret", "5.13.47", "http://panel.invalid/prefix", "panel-secret", "1.6.4")
	if err != nil || equivalent != base {
		t.Fatal("equivalent normalized source scope differed")
	}
	cases := [][6]string{
		{"https://example.invalid/other", "ninja-secret", "5.13.47", "http://panel.invalid/prefix", "panel-secret", "1.6.4"},
		{"https://example.invalid/ninja", "changed", "5.13.47", "http://panel.invalid/prefix", "panel-secret", "1.6.4"},
		{"https://example.invalid/ninja", "ninja-secret", "5.13.47", "http://panel.invalid/other", "panel-secret", "1.6.4"},
		{"https://example.invalid/ninja", "ninja-secret", "5.13.47", "http://panel.invalid/prefix", "changed", "1.6.4"},
	}
	for _, args := range cases {
		got, e := SourceScope(args[0], args[1], args[2], args[3], args[4], args[5])
		if e != nil || got == base {
			t.Fatal("source binding change not reflected")
		}
	}
	for _, args := range [][6]string{{"https://u:p@example.invalid", "x", "5.13.47", "http://panel.invalid", "y", "1.6.4"}, {"https://example.invalid?secret=value", "x", "", "http://panel.invalid", "y", ""}} {
		if _, e := SourceScope(args[0], args[1], args[2], args[3], args[4], args[5]); e == nil {
			t.Fatal("malformed source accepted")
		}
	}
	for _, version := range []string{"", "5.13.50", "1.6.5", "unknown"} {
		got, err := SourceScope("https://example.invalid/ninja", "ninja-secret", version, "http://panel.invalid/prefix", "panel-secret", version)
		if err != nil || got != base {
			t.Fatal("release metadata rejected or changed existing source binding")
		}
	}
	if strings.Contains(base, "secret") {
		t.Fatal("raw credential in hash")
	}
}
