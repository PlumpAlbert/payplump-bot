package sui_test

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

func TestSubscriptionURLPreservesPrefixAndIdentifier(t *testing.T) {
	bases := []struct {
		base   string
		prefix string
	}{
		{"https://subscriptions.invalid", "/"},
		{"http://subscriptions.invalid/synthetic-prefix", "/synthetic-prefix/"},
		{"https://subscriptions.invalid/synthetic-prefix/", "/synthetic-prefix/"},
		{"https://subscriptions.invalid/Kept%20Prefix", "/Kept%20Prefix/"},
		{"https://subscriptions.invalid/kept%2Fprefix", "/kept%2Fprefix/"},
		{"https://subscriptions.invalid/Kept//nested///", "/Kept//nested///"},
		// API suffix validation applies only to the panel constructor.
		{"https://subscriptions.invalid/apiv2/settings", "/apiv2/settings/"},
	}
	names := []string{
		"synthetic-legacy_001",
		" synthetic name ",
		"synthetic?query#fragment/segment",
		"../synthetic",
		"%2e%2e",
		"synthetic%00",
		"synthetic+part:part@part;part=part",
		"synthetic\\segment",
		"синтетический",
	}
	for i, base := range bases {
		for j, name := range names {
			t.Run(fmt.Sprintf("base-%d/name-%d", i, j), func(t *testing.T) {
				built, err := sui.SubscriptionURL(base.base, name)
				if err != nil {
					t.Fatal(err)
				}
				u, err := url.Parse(built)
				if err != nil {
					t.Fatal("constructed URL did not parse")
				}
				if u.EscapedPath() != base.prefix+url.PathEscape(name) {
					t.Error("prefix or exact single-segment escaping was not preserved")
				}
				segment := strings.TrimPrefix(u.EscapedPath(), base.prefix)
				if strings.Contains(segment, "/") {
					t.Error("identifier became multiple path segments")
				}
				decoded, err := url.PathUnescape(segment)
				if err != nil || decoded != name {
					t.Error("legacy identifier was changed or normalized")
				}
				if u.RawQuery != "format=json" || u.Fragment != "" || u.User != nil || u.Hostname() != "subscriptions.invalid" {
					t.Error("identifier changed URL query, fragment, credentials or origin")
				}
			})
		}
	}
}

func TestSubscriptionURLRejectsBadConfiguration(t *testing.T) {
	for i, base := range invalidBaseURLs {
		t.Run(fmt.Sprintf("base-%d", i), func(t *testing.T) {
			built, err := sui.SubscriptionURL(base, "synthetic-name")
			assertInvalidConfiguration(t, built != "", err)
		})
	}
	for i, name := range []string{"", ".", "..", "synthetic\nname", "synthetic\x00name", "synthetic\x7fname", "synthetic\u0085name"} {
		t.Run(fmt.Sprintf("name-%d", i), func(t *testing.T) {
			built, err := sui.SubscriptionURL("https://subscriptions.invalid/synthetic-prefix", name)
			assertInvalidConfiguration(t, built != "", err)
		})
	}
}
