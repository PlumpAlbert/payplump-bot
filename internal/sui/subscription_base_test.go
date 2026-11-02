package sui

import "testing"

func TestEquivalentSubscriptionBaseUsesActualURLConstruction(t *testing.T) {
	for _, test := range []struct {
		name, a, b string
		want       bool
	}{
		{"same", "https://subs.invalid", "https://subs.invalid", true},
		{"root slash", "https://subs.invalid", "https://subs.invalid/", true},
		{"prefix slash", "https://subs.invalid/sub", "https://subs.invalid/sub/", true},
		{"encoded prefix slash", "https://subs.invalid/a%2Fb", "https://subs.invalid/a%2Fb/", true},
		{"different host", "https://one.invalid/sub", "https://two.invalid/sub", false},
		{"different path", "https://subs.invalid/a", "https://subs.invalid/b", false},
		{"different scheme", "https://subs.invalid/sub", "http://subs.invalid/sub", false},
		{"different port", "https://subs.invalid:8443/sub", "https://subs.invalid/sub", false},
		{"double slash", "https://subs.invalid/sub//", "https://subs.invalid/sub/", false},
		{"escaped separator", "https://subs.invalid/a%2Fb", "https://subs.invalid/a/b", false},
		{"query", "https://subs.invalid/sub?x=1", "https://subs.invalid/sub", false},
		{"credentials", "https://user:pass@subs.invalid/sub", "https://subs.invalid/sub", false},
		{"empty", "", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := EquivalentSubscriptionBase(test.a, test.b); got != test.want {
				t.Fatalf("equivalence=%t, want %t", got, test.want)
			}
			if got := EquivalentSubscriptionBase(test.b, test.a); got != test.want {
				t.Fatalf("reverse equivalence=%t, want %t", got, test.want)
			}
		})
	}
}
