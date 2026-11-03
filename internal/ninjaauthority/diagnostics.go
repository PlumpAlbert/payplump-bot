package ninjaauthority

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

// SourceFailure is an allowlisted public diagnostic, never a provider error.
// Durations/counts are intentionally absent: retries must not rearm notices.
type SourceFailure struct {
	Service    string `json:"service"`
	Operation  string `json:"operation"`
	Category   string `json:"category"`
	HTTPStatus int    `json:"http_status"`
}

func (d SourceFailure) Valid() bool {
	operationOK := d.Service == "ninja" && (d.Operation == "customers" || d.Operation == "recurring" || d.Operation == "invoices") || d.Service == "sui" && d.Operation == "inventory"
	switch d.Category {
	case "http":
		return operationOK && d.HTTPStatus >= 100 && d.HTTPStatus <= 599
	case "canceled", "timeout", "dns", "tls_authority", "tls_hostname", "tls_certificate", "connection_refused", "connection", "invalid_response", "incomplete_inventory", "response_read", "response_too_large", "unavailable":
		return operationOK && d.HTTPStatus == 0
	default:
		return false
	}
}
func (d SourceFailure) Hash() string {
	raw, _ := json.Marshal(d)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func NoticeCode(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:6])
}
func sourceFailure(service, operation string, err error) SourceFailure {
	d := SourceFailure{Service: service, Operation: operation, Category: "unavailable"}
	var status *integrationhttp.HTTPError
	switch {
	case errors.Is(err, context.Canceled):
		d.Category = "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		d.Category = "timeout"
	case errors.As(err, &status):
		if status.StatusCode >= 100 && status.StatusCode <= 599 {
			d.Category = "http"
			d.HTTPStatus = status.StatusCode
		}
	case errors.Is(err, integrationhttp.ErrDNS):
		d.Category = "dns"
	case errors.Is(err, integrationhttp.ErrTLSAuthority):
		d.Category = "tls_authority"
	case errors.Is(err, integrationhttp.ErrTLSHostname):
		d.Category = "tls_hostname"
	case errors.Is(err, integrationhttp.ErrTLSCertificate):
		d.Category = "tls_certificate"
	case errors.Is(err, integrationhttp.ErrConnectionRefused):
		d.Category = "connection_refused"
	case errors.Is(err, integrationhttp.ErrConnection):
		d.Category = "connection"
	case errors.Is(err, integrationhttp.ErrResponseRead):
		d.Category = "response_read"
	case errors.Is(err, integrationhttp.ErrResponseTooLarge):
		d.Category = "response_too_large"
	case errors.Is(err, integrationhttp.ErrInvalidResponse), errors.Is(err, invoiceninja.ErrSettlementContract), errors.Is(err, sui.ErrManagedResponse):
		d.Category = "invalid_response"
	case errors.Is(err, invoiceninja.ErrLookupIncomplete), errors.Is(err, invoiceninja.ErrCycleLookupIncomplete), errors.Is(err, invoiceninja.ErrSettlementIncomplete):
		d.Category = "incomplete_inventory"
	}
	return d
}

// All attributes originate here, never from arbitrary errors/provider strings.
func (s *Service) readContext(ctx context.Context, service, operation, key string) context.Context {
	if !s.logger.Enabled(ctx, slog.LevelDebug) {
		return ctx
	}
	code := NoticeCode(s.cfg.SourceScopeHash + ":" + key)
	return integrationhttp.WithReadObserver(ctx, func(e integrationhttp.ReadEvent) {
		s.logger.DebugContext(ctx, "authority API read", "service", service, "operation", operation, "diagnostic_code", code, "duration_ms", e.Duration.Milliseconds(), "http_status", e.HTTPStatus, "page", e.Page)
	})
}
func (s *Service) readFinished(ctx context.Context, service, operation, key string, started time.Time, count int) {
	s.logger.DebugContext(ctx, "authority inventory read completed", "service", service, "operation", operation, "diagnostic_code", NoticeCode(s.cfg.SourceScopeHash+":"+key), "duration_ms", time.Since(started).Milliseconds(), "records", count)
}
