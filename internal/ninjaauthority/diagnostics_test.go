package ninjaauthority

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

func TestSourceFailureClassificationAndRedaction(t *testing.T) {
	for _, tc := range []struct {
		err      error
		category string
		status   int
	}{
		{context.Canceled, "canceled", 0}, {context.DeadlineExceeded, "timeout", 0},
		{&integrationhttp.HTTPError{StatusCode: 401}, "http", 401}, {&integrationhttp.HTTPError{StatusCode: 503}, "http", 503},
		{integrationhttp.ErrDNS, "dns", 0}, {integrationhttp.ErrTLSAuthority, "tls_authority", 0}, {integrationhttp.ErrTLSHostname, "tls_hostname", 0}, {integrationhttp.ErrTLSCertificate, "tls_certificate", 0},
		{integrationhttp.ErrConnectionRefused, "connection_refused", 0}, {integrationhttp.ErrConnection, "connection", 0},
		{integrationhttp.ErrInvalidResponse, "invalid_response", 0}, {sui.ErrManagedResponse, "invalid_response", 0},
		{integrationhttp.ErrResponseRead, "response_read", 0}, {integrationhttp.ErrResponseTooLarge, "response_too_large", 0},
		{invoiceninja.ErrLookupIncomplete, "incomplete_inventory", 0}, {invoiceninja.ErrCycleLookupIncomplete, "incomplete_inventory", 0},
		{errors.New("PRIVATE_TOKEN https://private.invalid/?password=PRIVATE_PASSWORD"), "unavailable", 0},
	} {
		err := fmt.Errorf("PRIVATE_BODY: %w", tc.err)
		d := sourceFailure("sui", "inventory", err)
		if !d.Valid() || d.Category != tc.category || d.HTTPStatus != tc.status {
			t.Fatalf("wrong safe diagnostic: %+v", d)
		}
		if strings.Contains(fmt.Sprint(d), "PRIVATE") {
			t.Fatal("raw error retained")
		}
	}
	joined := sourceFailure("ninja", "customers", errors.Join(invoiceninja.ErrLookupIncomplete, &integrationhttp.HTTPError{StatusCode: 403}))
	if joined.Category != "http" || joined.HTTPStatus != 403 {
		t.Fatal("inventory wrapper hid HTTP error")
	}
	for _, d := range []SourceFailure{{Service: "private.invalid", Operation: "inventory", Category: "http", HTTPStatus: 401}, {Service: "sui", Operation: "private-id", Category: "http", HTTPStatus: 401}, {Service: "sui", Operation: "inventory", Category: "PRIVATE"}, {Service: "sui", Operation: "inventory", Category: "timeout", HTTPStatus: 401}} {
		if d.Valid() {
			t.Fatal("non-allowlisted diagnostic accepted")
		}
	}
}

func TestSourceFailureLogsAtInfoAndRearmsOnlyForMeaningfulChanges(t *testing.T) {
	s, st, n, p := fixture(t, "2", false)
	var logs bytes.Buffer
	s.logger = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	n.err = fmt.Errorf("PRIVATE_BODY: %w", &integrationhttp.HTTPError{StatusCode: 401})
	for i := 0; i < 2; i++ {
		if err := s.Scan(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if st.noticeCount != 1 || p.posts != 0 {
		t.Fatal("unchanged error repeated notice or wrote account")
	}
	code := NoticeCode(s.cfg.SourceScopeHash + ":source:ninja")
	for _, field := range []string{`"level":"WARN"`, `"service":"ninja"`, `"operation":"customers"`, `"category":"http"`, `"http_status":401`, code} {
		if !strings.Contains(logs.String(), field) {
			t.Fatalf("missing diagnostic field %s", field)
		}
	}
	if strings.Contains(logs.String(), "PRIVATE") || strings.Contains(logs.String(), `"DEBUG"`) {
		t.Fatal("info logs leaked details/debug events")
	}
	n.err = &integrationhttp.HTTPError{StatusCode: 403}
	if err := s.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st.noticeCount != 2 {
		t.Fatal("changed HTTP status did not rearm")
	}
	n.err = context.DeadlineExceeded
	if err := s.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st.noticeCount != 3 {
		t.Fatal("changed category did not rearm")
	}
}

type diagnosticRecurring struct {
	*fakeNinja
	failure error
}

func (n diagnosticRecurring) ListRecurring(context.Context, string) ([]invoiceninja.RecurringRecord, error) {
	return nil, n.failure
}

type diagnosticInvoices struct{ failure error }

func (n diagnosticInvoices) ListInvoices(context.Context, string) ([]invoiceninja.CycleInvoice, error) {
	return nil, n.failure
}
func TestSourceFailuresIdentifyEachReadBoundary(t *testing.T) {
	for _, tc := range []struct{ operation, key string }{{"inventory", "source:sui"}, {"recurring", "actor:42"}, {"invoices", "actor:42:invoices"}} {
		t.Run(tc.operation, func(t *testing.T) {
			s, st, n, p := fixture(t, "2", false)
			var logs bytes.Buffer
			s.logger = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			err := fmt.Errorf("PRIVATE_BODY: %w", &integrationhttp.HTTPError{StatusCode: 503})
			switch tc.operation {
			case "inventory":
				p.inventoryErr = err
			case "recurring":
				s.ninja = diagnosticRecurring{n, err}
			case "invoices":
				s.invoices = diagnosticInvoices{err}
			}
			if err = s.Scan(context.Background()); err != nil {
				t.Fatal(err)
			}
			if p.posts != 0 || len(st.observations) != 0 || st.noticeCount != 1 {
				t.Fatal("failed read crossed a write boundary")
			}
			if !strings.Contains(logs.String(), `"operation":"`+tc.operation+`"`) || !strings.Contains(logs.String(), NoticeCode(s.cfg.SourceScopeHash+":"+tc.key)) || !strings.Contains(logs.String(), `"records":1`) || strings.Contains(logs.String(), "PRIVATE") {
				t.Fatal("missing correlated boundary/counts or secret leakage")
			}
		})
	}
}
func TestRealReadersProduceSafeCorrelatedDebugEvents(t *testing.T) {
	for _, tc := range []struct {
		name, body, category string
		status               int
	}{{"http", "PRIVATE_RESPONSE", "http", 401}, {"json", "{PRIVATE_RESPONSE", "invalid_response", 200}} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			reader, err := invoiceninja.NewCustomerInventoryReader(srv.URL+"/PRIVATE_PREFIX", "PRIVATE_TOKEN", time.Second, 50, 20)
			if err != nil {
				t.Fatal(err)
			}
			s, st, _, p := fixture(t, "2", false)
			s.ninja = reader
			var logs bytes.Buffer
			s.logger = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			if err = s.Scan(context.Background()); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{`"msg":"authority API read"`, `"page":1`, `"duration_ms":`, `"category":"` + tc.category + `"`, NoticeCode(s.cfg.SourceScopeHash + ":source:ninja")} {
				if !strings.Contains(logs.String(), field) {
					t.Fatalf("missing trace field %s", field)
				}
			}
			for _, secret := range []string{"PRIVATE", srv.URL} {
				if strings.Contains(logs.String(), secret) {
					t.Fatal("debug leaked provider data")
				}
			}
			if st.noticeCount != 1 || p.posts != 0 {
				t.Fatal("read failure changed reconciliation safety")
			}
		})
	}
}
