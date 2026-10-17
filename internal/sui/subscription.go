package sui

import (
	"errors"
	"net/url"
	"strings"
	"unicode"
)

// SubscriptionURL constructs a URL only; it never fetches a subscription.
// baseURL is the public subscription base (not necessarily the panel URL).
// The existing name is preserved verbatim as one escaped path segment.
func SubscriptionURL(baseURL, name string) (string, error) {
	base, err := parseBaseURL(baseURL)
	if err != nil {
		return "", err
	}
	if name == "" || name == "." || name == ".." || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", errors.New("invalid SUI subscription name")
	}

	// Append to the escaped prefix without path.Join/Clean or decoding the name.
	// In particular, a slash in name must not create another path segment.
	escaped := base.EscapedPath()
	if !strings.HasSuffix(escaped, "/") {
		escaped += "/"
	}
	escaped += url.PathEscape(name)
	base.Path, err = url.PathUnescape(escaped)
	if err != nil {
		return "", errors.New("invalid SUI subscription URL")
	}
	base.RawPath = escaped
	base.RawQuery = "format=json"
	return base.String(), nil
}
