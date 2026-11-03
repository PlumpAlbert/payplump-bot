package integrationhttp

import (
	"context"
	"errors"
	"time"
)

// Stable sentinels preserve classifications without retaining provider URLs,
// certificate contents, resolver messages or response bodies.
var (
	ErrDNS               = errors.New("DNS lookup failed")
	ErrTLSAuthority      = errors.New("TLS certificate authority is not trusted")
	ErrTLSHostname       = errors.New("TLS certificate does not match the server hostname")
	ErrTLSCertificate    = errors.New("TLS certificate is invalid or expired")
	ErrConnectionRefused = errors.New("connection refused")
	ErrResponseRead      = errors.New("could not read integration API response")
	ErrResponseTooLarge  = errors.New("integration API response exceeds 4 MiB limit")
)

// ReadEvent deliberately cannot contain a URL, query, header, body or error.
// Page is the bounded numeric pagination index, not an identifier.
type ReadEvent struct {
	Duration         time.Duration
	HTTPStatus, Page int
}
type readObserverKey struct{}

func WithReadObserver(ctx context.Context, observer func(ReadEvent)) context.Context {
	return context.WithValue(ctx, readObserverKey{}, observer)
}
func readObserver(ctx context.Context) func(ReadEvent) {
	observer, _ := ctx.Value(readObserverKey{}).(func(ReadEvent))
	return observer
}
