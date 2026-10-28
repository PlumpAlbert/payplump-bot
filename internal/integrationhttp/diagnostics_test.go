package integrationhttp

import (
	"context"
	"crypto/x509"
	"fmt"
	"net"
	"net/url"
	"strings"
	"syscall"
	"testing"
)

func TestConnectionDiagnosticsDoNotEchoProviderDetails(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"dns", &net.DNSError{Name: "private.invalid", Err: "private-provider-message"}, "DNS lookup failed"},
		{"authority", x509.UnknownAuthorityError{}, "TLS certificate authority is not trusted"},
		{"hostname", x509.HostnameError{Host: "private.invalid"}, "TLS certificate does not match"},
		{"certificate", x509.CertificateInvalidError{}, "TLS certificate is invalid or expired"},
		{"refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, "connection refused"},
		{"timeout", &net.OpError{Op: "dial", Err: context.DeadlineExceeded}, "context deadline exceeded"},
		{"unknown", fmt.Errorf("private-provider-message"), "integration API connection failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := &url.Error{Op: "Get", URL: "https://private.invalid/path?token=private-token", Err: tc.err}
			message := connectionError(wrapped).Error()
			if !strings.Contains(message, tc.want) {
				t.Fatalf("failure category missing: %s", message)
			}
			for _, sensitive := range []string{"private.invalid", "private-token", "private-provider-message", "https://"} {
				if strings.Contains(message, sensitive) {
					t.Fatal("provider details leaked")
				}
			}
		})
	}
}
