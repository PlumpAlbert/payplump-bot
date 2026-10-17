package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/integrations"
)

func commandConfigEnv(t *testing.T, base string) {
	t.Helper()
	for key, value := range map[string]string{
		"INVOICE_NINJA_BASE_URL": base + "/billing", "INVOICE_NINJA_API_TOKEN": "never-print-invoice-token",
		"INVOICE_NINJA_PRODUCT_ID": "never-print-product-id", "INVOICE_NINJA_TELEGRAM_ID_FIELD": "id_number", "INVOICE_NINJA_VERSION": "",
		"SUI_BASE_URL": base + "/panel", "SUI_API_TOKEN": "never-print-panel-token", "SUI_INBOUND_IDS": "2", "SUI_VERSION": "",
		"SUI_SUBSCRIPTION_BASE_URL": "https://subscriptions.invalid/never-print-prefix",
		"INTEGRATION_HTTP_TIMEOUT":  "", "INTEGRATION_PAGE_SIZE": "", "INTEGRATION_MAX_PAGES": "",
		// Neither configuration is relevant to the read-only subcommand.
		"DATABASE_URL": "invalid-database-url", "PGHOST": "invalid-database-host", "TELEGRAM_ENABLED": "invalid-boolean",
	} {
		t.Setenv(key, value)
	}
}

func TestIntegrationCommandBypassesDatabaseAndTelegram(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/billing/api/v1/clients" {
			t.Error("unexpected preflight request")
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"never-print-invoice-token never-print-server-body"}`)
	}))
	defer server.Close()
	commandConfigEnv(t, server.URL)
	var output bytes.Buffer
	err := checkIntegrations(context.Background(), &output)
	if err == nil || requests.Load() != 1 {
		t.Fatal("command did not reach the API independently of database/Telegram configuration")
	}
	decoder := json.NewDecoder(strings.NewReader(output.String()))
	var report integrations.Report
	if err := decoder.Decode(&report); err != nil || !report.ReadOnly || report.Status != "failed" || report.FailedCheck != "invoice_ninja_clients" {
		t.Fatal("missing failed read-only JSON report")
	}
	if decoder.Decode(new(any)) != io.EOF {
		t.Fatal("command output contains more than one JSON document")
	}
	for _, private := range []string{"never-print", server.URL, "invalid-database"} {
		if strings.Contains(output.String(), private) || strings.Contains(err.Error(), private) {
			t.Fatal("command exposed private data in report or diagnostics")
		}
	}
}

func TestIntegrationCommandRejectsConfigBeforeNetwork(t *testing.T) {
	commandConfigEnv(t, "https://example.invalid")
	t.Setenv("INVOICE_NINJA_TELEGRAM_ID_FIELD", "number")
	var output bytes.Buffer
	if err := checkIntegrations(context.Background(), &output); err == nil || output.Len() != 0 {
		t.Fatal("invalid configuration was not rejected before checking integrations")
	}
}

type failedReportWriter struct{}

func (failedReportWriter) Write([]byte) (int, error) {
	return 0, errors.New("never-print-writer-detail")
}

func TestIntegrationCommandRedactsWriterErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer server.Close()
	commandConfigEnv(t, server.URL)
	err := checkIntegrations(context.Background(), failedReportWriter{})
	if err == nil || strings.Contains(err.Error(), "never-print") || !strings.Contains(err.Error(), "could not write integration report") {
		t.Fatal("writer failure was lost or exposed private diagnostics")
	}
}
