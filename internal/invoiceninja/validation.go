package invoiceninja

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

var errInvalidResponse = errors.New("invalid Invoice Ninja API response")

// Number validation is lexical, never a float64 conversion. This also checks
// quoted JSON numbers accepted by encoding/json's json.Number decoder.
var numberPattern = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

// SemVer 2.0: no leading zeroes in numeric version/prerelease identifiers,
// bounded ASCII text, and no arbitrary response header content in Page.
var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?(\+([0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*))?$`)

func appVersion(headers http.Header) (string, error) {
	values := headers.Values("X-APP-VERSION")
	if len(values) == 0 {
		return "", nil
	}
	if len(values) != 1 || len(values[0]) > 128 || !versionPattern.MatchString(values[0]) {
		return "", errInvalidResponse
	}
	return values[0], nil
}

func validServiceBase(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u == nil || !utf8.ValidString(raw) || strings.IndexFunc(raw, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0 || strings.Contains(raw, "#") {
		return false
	}
	if strings.HasSuffix(u.Host, ":") {
		return false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	path := strings.Trim(u.EscapedPath(), "/")
	if path == "" {
		return u.Path == "" || u.Path == "/"
	}
	segments := strings.Split(path, "/")
	for i, escaped := range segments {
		segment, err := url.PathUnescape(escaped)
		if err != nil || !validID(segment) {
			return false
		}
		segments[i] = segment
	}
	for i, segment := range segments {
		// Refuse an API root (including one already followed by a resource)
		// instead of silently producing /api/v1/api/v1 requests.
		if strings.EqualFold(segment, "api") && (i == len(segments)-1 || strings.HasPrefix(strings.ToLower(segments[i+1]), "v")) {
			return false
		}
	}
	return !strings.HasPrefix(u.Path, "//") && !strings.HasSuffix(u.Path, "//")
}

func validID(id string) bool {
	return id != "" && id != "." && id != ".." && utf8.ValidString(id) && strings.TrimSpace(id) == id && !strings.ContainsAny(id, "/\\") && strings.IndexFunc(id, unicode.IsControl) < 0
}

func validMoney(value json.Number) bool { return numberPattern.MatchString(value.String()) }

func validCustomer(c Customer) bool { return validID(c.ID) }

func validProduct(p Product) bool { return validID(p.ID) && validMoney(p.Price) }

func validInvoice(i Invoice) bool {
	return validID(i.ID) && validMoney(i.Amount) && validMoney(i.Balance)
}

func validPayment(p Payment) bool {
	if !validID(p.ID) || !validMoney(p.Amount) || p.Paymentables == nil || p.Invoices == nil {
		return false
	}
	for _, link := range p.Paymentables {
		if !validID(link.ID) || !validMoney(link.Amount) || !validMoney(link.Refunded) {
			return false
		}
	}
	for _, invoice := range p.Invoices {
		if !validInvoice(invoice) {
			return false
		}
	}
	return true
}

func validRecurringInvoice(r RecurringInvoice) bool { return validID(r.ID) }

// Pointers distinguish absent/null pagination properties from valid zeroes.
type wirePagination struct {
	Total       *int `json:"total"`
	Count       *int `json:"count"`
	PerPage     *int `json:"per_page"`
	CurrentPage *int `json:"current_page"`
	TotalPages  *int `json:"total_pages"`
}

func (w wirePagination) validate(requestedPage, length int) (Pagination, bool) {
	if w.Total == nil || w.Count == nil || w.PerPage == nil || w.CurrentPage == nil || w.TotalPages == nil {
		return Pagination{}, false
	}
	p := Pagination{Total: *w.Total, Count: *w.Count, PerPage: *w.PerPage, CurrentPage: *w.CurrentPage, TotalPages: *w.TotalPages}
	if p.CurrentPage != requestedPage || p.CurrentPage < 1 || p.Count != length || p.PerPage < 1 || p.PerPage > 100 || p.PerPage < length || p.Total < p.Count {
		return Pagination{}, false
	}
	if p.Total == 0 {
		// Empty paginator implementations report either zero or one total
		// pages. Both represent only the first, empty page.
		return p, p.CurrentPage == 1 && p.Count == 0 && (p.TotalPages == 0 || p.TotalPages == 1)
	}
	pages := p.Total / p.PerPage
	if p.Total%p.PerPage != 0 {
		pages++
	}
	if p.TotalPages != pages || p.CurrentPage > pages {
		return Pagination{}, false
	}
	// Computing the ceiling above without total+per_page avoids int overflow.
	remaining := p.Total - (p.CurrentPage-1)*p.PerPage
	expected := p.PerPage
	if remaining < expected {
		expected = remaining
	}
	return p, p.Count == expected
}
