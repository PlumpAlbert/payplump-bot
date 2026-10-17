// Package integrationhttp provides a bounded, read-only HTTP transport. Errors
// never include URLs, headers, response bodies or remote error descriptions.
package integrationhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
)

const MaxResponseBytes = 4 << 20

var ErrInvalidResponse = errors.New("invalid integration API response")

type HTTPError struct {
	StatusCode int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("integration API returned HTTP %d", e.StatusCode)
}

type Client struct {
	base    *url.URL
	headers http.Header
	http    *http.Client
}

// ValidateBaseURL also supports HTTP for explicitly configured private networks
// such as NetBird. It never disables TLS verification for HTTPS endpoints.
func ValidateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u == nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "\r\n") {
		return errors.New("expected HTTP(S) base URL without credentials, query or fragment")
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return errors.New("base URL must not contain dot path segments")
		}
	}
	return nil
}

func New(baseURL string, headers http.Header, timeout time.Duration) (*Client, error) {
	if err := ValidateBaseURL(baseURL); err != nil {
		return nil, err
	}
	if timeout <= 0 || timeout > 30*time.Second {
		return nil, errors.New("integration HTTP timeout must be positive and at most 30s")
	}
	for key, values := range headers {
		if key == "" || strings.IndexFunc(key, unicode.IsControl) >= 0 {
			return nil, errors.New("invalid integration request headers")
		}
		for _, value := range values {
			if value == "" || strings.IndexFunc(value, unicode.IsControl) >= 0 {
				return nil, errors.New("invalid integration request headers")
			}
		}
	}
	u, _ := url.Parse(baseURL)
	cloned := headers.Clone()
	if cloned == nil {
		cloned = make(http.Header)
	}
	return &Client{base: u, headers: cloned, http: &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// Get appends escaped path segments, preserving an installed panel's prefix.
// It deliberately does not follow pagination links supplied by a remote API:
// adapters construct each page on the configured origin themselves.
func (c *Client) Get(ctx context.Context, segments []string, query url.Values, result any) (http.Header, error) {
	u := *c.base
	escaped := strings.TrimRight(u.EscapedPath(), "/")
	decoded, err := url.PathUnescape(escaped)
	if err != nil {
		return nil, errors.New("invalid integration API prefix")
	}
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." || strings.ContainsAny(segment, "/\\") || strings.IndexFunc(segment, unicode.IsControl) >= 0 {
			return nil, errors.New("invalid integration API path")
		}
		decoded += "/" + segment
		escaped += "/" + url.PathEscape(segment)
	}
	u.Path, u.RawPath, u.RawQuery = decoded, escaped, query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("invalid integration API request")
	}
	req.Header = c.headers.Clone()
	req.Header.Set("Accept", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, context.DeadlineExceeded
		}
		return nil, errors.New("integration API connection failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &HTTPError{StatusCode: response.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("could not read integration API response")
	}
	if len(body) > MaxResponseBytes {
		return nil, errors.New("integration API response exceeds 4 MiB limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if decoder.Decode(result) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, ErrInvalidResponse
	}
	return response.Header.Clone(), nil
}
