package integrationhttp

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestConnectionClassificationSurvivesSanitization(t *testing.T) {
	for _, tc := range []struct{ cause, want error }{{&net.DNSError{Name: "PRIVATE_HOST", Err: "PRIVATE_BODY"}, ErrDNS}, {x509.UnknownAuthorityError{}, ErrTLSAuthority}, {x509.HostnameError{Host: "PRIVATE_HOST"}, ErrTLSHostname}, {x509.CertificateInvalidError{}, ErrTLSCertificate}, {syscall.ECONNREFUSED, ErrConnectionRefused}} {
		err := connectionError(&url.Error{Op: "Get", URL: "https://PRIVATE_HOST/?token=PRIVATE_TOKEN", Err: tc.cause})
		if !errors.Is(err, tc.want) || !errors.Is(err, ErrConnection) || strings.Contains(err.Error(), "PRIVATE") {
			t.Fatal("safe typed connection category lost")
		}
	}
}
func TestActualResponseFailuresStayTypedAndRedacted(t *testing.T) {
	for _, tc := range []struct {
		name    string
		want    error
		handler http.HandlerFunc
	}{
		{"body_read", ErrResponseRead, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "1000")
			_, _ = w.Write([]byte("PRIVATE_BODY"))
		}},
		{"too_large", ErrResponseTooLarge, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat("x", MaxResponseBytes+1)))
		}},
		{"body_timeout", context.DeadlineExceeded, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			c, err := New(srv.URL+"/PRIVATE_PREFIX", nil, 250*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			var events []ReadEvent
			ctx := WithReadObserver(context.Background(), func(e ReadEvent) { events = append(events, e) })
			var out any
			_, err = c.Get(ctx, []string{"PRIVATE_ID"}, nil, &out)
			if !errors.Is(err, tc.want) || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("response category missing or unsafe: %v", err)
			}
			if len(events) != 1 {
				t.Fatal("failure did not produce a bounded trace")
			}
		})
	}
}
func TestReadObserverContainsOnlyBoundedPublicMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"password":"PRIVATE_BODY"}`)) }))
	defer srv.Close()
	c, err := New(srv.URL+"/PRIVATE_PREFIX", http.Header{"Token": {"PRIVATE_TOKEN"}}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var events []ReadEvent
	ctx := WithReadObserver(context.Background(), func(e ReadEvent) { events = append(events, e) })
	var out any
	if _, err = c.Get(ctx, []string{"PRIVATE_ID"}, url.Values{"page": {"2"}, "id": {"PRIVATE_QUERY"}}, &out); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].HTTPStatus != 200 || events[0].Page != 2 || events[0].Duration < 0 || strings.Contains(fmt.Sprint(events), "PRIVATE") {
		t.Fatal("invalid or sensitive read event")
	}
	if _, err = c.request(ctx, http.MethodPost, []string{"PRIVATE_ID"}, nil, strings.NewReader(`{}`), &out); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatal("write entered read trace")
	}
	if _, err = c.Get(ctx, []string{"PRIVATE_ID"}, url.Values{"page": {"PRIVATE_QUERY"}}, &out); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Page != 0 {
		t.Fatal("untrusted page escaped numeric boundary")
	}
}
