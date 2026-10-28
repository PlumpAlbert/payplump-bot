package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/registration"
)

const discoveryProductPath = "/ninja/api/v1/products/synthetic-product"
const discoveryStaticsPath = "/ninja/api/v1/statics"

// Every fixture is local and checks that discovery stays read-only on the
// configured installation prefix, with company settings requested explicitly.
func newServiceDiscoveryFixture(t *testing.T, handler http.HandlerFunc) (map[string]string, func(...string)) {
	t.Helper()
	requests := make(chan string, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case requests <- r.URL.Path:
		default:
			t.Error("too many discovery requests")
		}
		if r.Method != http.MethodGet || r.ContentLength > 0 {
			t.Errorf("discovery must use bodyless GET, got %s", r.Method)
		}
		if r.Header.Get("X-API-TOKEN") != "synthetic-ninja-token" || r.Header.Get("X-Requested-With") != "XMLHttpRequest" {
			t.Error("missing Ninja request headers")
		}
		switch r.URL.Path {
		case discoveryProductPath:
			if r.URL.RawQuery != "include=company" {
				t.Errorf("product query = %q, want include=company", r.URL.RawQuery)
			}
		case discoveryStaticsPath:
			if r.URL.RawQuery != "" {
				t.Errorf("unexpected statics query %q", r.URL.RawQuery)
			}
		default:
			t.Errorf("unexpected discovery path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	values := normalServiceValues()
	values["INVOICE_NINJA_BASE_URL"] = server.URL + "/ninja"
	values["INVOICE_NINJA_PRODUCT_ID"] = "synthetic-product"
	for _, key := range []string{"PAYMENTS_CURRENCY_ID", "PAYMENTS_CURRENCY_CODE", "INVOICE_NINJA_INCLUSIVE_TAXES"} {
		delete(values, key)
	}
	return values, func(want ...string) {
		t.Helper()
		var got []string
		for len(requests) > 0 {
			got = append(got, <-requests)
		}
		if !slices.Equal(got, want) {
			t.Errorf("discovery requests = %v, want %v", got, want)
		}
	}
}

func serviceDiscoveryJSON(product, statics, version string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if version != "" {
			w.Header().Set("X-APP-VERSION", version)
		}
		if r.URL.Path == discoveryProductPath {
			fmt.Fprint(w, product)
		} else {
			fmt.Fprint(w, statics)
		}
	}
}

func TestDiscoverServiceDefaultsAllExplicitSkipsNetwork(t *testing.T) {
	values, requests := newServiceDiscoveryFixture(t, func(http.ResponseWriter, *http.Request) {
		t.Error("explicit defaults must not make HTTP requests")
	})
	values["PAYMENTS_CURRENCY_ID"] = "9"
	values["PAYMENTS_CURRENCY_CODE"] = "EUR"
	values["INVOICE_NINJA_INCLUSIVE_TAXES"] = "true"
	// Even discovery-only configuration is irrelevant on this path.
	values["INTEGRATION_HTTP_TIMEOUT"] = "invalid"
	get, err := resolveServiceEnvironment(context.Background(), func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range values {
		if got := get(key); got != want {
			t.Errorf("%s = %q, want explicit %q", key, got, want)
		}
	}
	requests()
}

func TestDiscoverServiceDefaults(t *testing.T) {
	for _, key := range []string{"PAYMENTS_CURRENCY_ID", "PAYMENTS_CURRENCY_CODE", "INVOICE_NINJA_INCLUSIVE_TAXES"} {
		t.Setenv(key, "process-environment-must-not-change")
	}
	for _, tc := range []struct {
		name, settings, currencies, version string
		overrides                           map[string]string
		id, code, taxes                     string
		paths                               []string
	}{
		{
			name: "string statics ID and false tax", settings: `"currency_id":1,"inclusive_taxes":false`,
			currencies: `[{"id":"1","code":"USD"}]`, version: "5.13.50",
			id: "1", code: "USD", taxes: "false", paths: []string{discoveryProductPath, discoveryStaticsPath},
		},
		{
			name: "numeric statics ID and true tax", settings: `"currency_id":"1","inclusive_taxes":true`,
			currencies: `[{"id":1,"code":"USD"}]`, version: "5.13.50",
			id: "1", code: "USD", taxes: "true", paths: []string{discoveryProductPath, discoveryStaticsPath},
		},
		{
			name: "no version header required", settings: `"currency_id":1,"inclusive_taxes":false`,
			currencies: `[{"id":1,"code":"USD"}]`,
			id:         "1", code: "USD", taxes: "false", paths: []string{discoveryProductPath, discoveryStaticsPath},
		},
		{
			name: "only missing currency ID", settings: `"currency_id":"1"`,
			currencies: `[{"id":1,"code":"USD"}]`, version: "5.13.50",
			overrides: map[string]string{"PAYMENTS_CURRENCY_CODE": "USD", "INVOICE_NINJA_INCLUSIVE_TAXES": "true"},
			id:        "1", code: "USD", taxes: "true", paths: []string{discoveryProductPath, discoveryStaticsPath},
		},
		{
			name: "only missing code skips product", currencies: `[{"id":9,"code":"EUR"}]`, version: "5.13.50",
			overrides: map[string]string{"PAYMENTS_CURRENCY_ID": "9", "INVOICE_NINJA_INCLUSIVE_TAXES": "false"},
			id:        "9", code: "EUR", taxes: "false", paths: []string{discoveryStaticsPath},
		},
		{
			name: "only missing tax preserves explicit currency", settings: `"currency_id":1,"inclusive_taxes":false`, version: "5.13.50",
			overrides: map[string]string{"PAYMENTS_CURRENCY_ID": "9", "PAYMENTS_CURRENCY_CODE": "EUR"},
			id:        "9", code: "EUR", taxes: "false", paths: []string{discoveryProductPath},
		},
		{
			name: "explicit tax overrides company", settings: `"currency_id":1,"inclusive_taxes":false`,
			currencies: `[{"id":"1","code":"USD"}]`, version: "5.13.50",
			overrides: map[string]string{"INVOICE_NINJA_INCLUSIVE_TAXES": "true"},
			id:        "1", code: "USD", taxes: "true", paths: []string{discoveryProductPath, discoveryStaticsPath},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			product := `{"data":{"id":"synthetic-product","company":{"settings":{` + tc.settings + `}}}}`
			values, requests := newServiceDiscoveryFixture(t, serviceDiscoveryJSON(product, `{"currencies":`+tc.currencies+`}`, tc.version))
			maps.Copy(values, tc.overrides)
			before := maps.Clone(values)
			environment := os.Environ()
			get, err := resolveServiceEnvironment(context.Background(), func(key string) string { return values[key] })
			if err != nil {
				t.Fatal(err)
			}
			for key, want := range map[string]string{"PAYMENTS_CURRENCY_ID": tc.id, "PAYMENTS_CURRENCY_CODE": tc.code, "INVOICE_NINJA_INCLUSIVE_TAXES": tc.taxes} {
				if got := get(key); got != want {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
			for key, want := range before {
				if get(key) != want {
					t.Errorf("explicit %s changed", key)
				}
			}
			if !reflect.DeepEqual(values, before) || !reflect.DeepEqual(os.Environ(), environment) {
				t.Error("discovery mutated its source map or global environment")
			}
			requests(tc.paths...)

			// Feed the returned getter directly through the existing service defaults
			// and real consumers, rather than testing only its string overlay.
			get, managed, err := serviceEnvironment(get)
			if err != nil {
				t.Fatal(err)
			}
			p, err := payments.ConfigFromEnvironment(get, true)
			if err != nil || p.CurrencyID != tc.id {
				t.Fatalf("payment config: currency=%q, err=%v", p.CurrencyID, err)
			}
			a, err := activation.ConfigFromEnvironment(get, true, true, managed)
			if err != nil || a.CurrencyID != tc.id || !a.TaxModeConfirmed || a.InclusiveTaxes != (tc.taxes == "true") {
				t.Fatalf("activation did not accept discovered currency/tax: %v", err)
			}
			r, err := registration.ConfigFromEnvironment(get, true)
			if err != nil || !r.TaxModeConfirmed || r.InclusiveTaxes != (tc.taxes == "true") {
				t.Fatalf("registration did not accept discovered tax: %v", err)
			}
		})
	}
}

func TestDiscoverServiceDefaultsMissingProductSettings(t *testing.T) {
	for _, tc := range []struct{ name, product, hint string }{
		{"missing data", `{}`, "PAYMENTS_CURRENCY_ID, PAYMENTS_CURRENCY_CODE and INVOICE_NINJA_INCLUSIVE_TAXES explicitly"},
		{"null data", `{"data":null}`, "explicitly"},
		{"different product", `{"data":{"id":"other","company":{"settings":{}}}}`, "explicitly"},
		{"missing company", `{"data":{"id":"synthetic-product"}}`, "explicitly"},
		{"missing settings", `{"data":{"id":"synthetic-product","company":{}}}`, "explicitly"},
		{"missing currency", `{"data":{"id":"synthetic-product","company":{"settings":{"inclusive_taxes":false}}}}`, "set PAYMENTS_CURRENCY_ID explicitly"},
		{"invalid currency", `{"data":{"id":"synthetic-product","company":{"settings":{"currency_id":"01","inclusive_taxes":false}}}}`, "set PAYMENTS_CURRENCY_ID explicitly"},
		{"missing tax is not false", `{"data":{"id":"synthetic-product","company":{"settings":{"currency_id":1}}}}`, "set INVOICE_NINJA_INCLUSIVE_TAXES explicitly"},
		{"null tax is not false", `{"data":{"id":"synthetic-product","company":{"settings":{"currency_id":1,"inclusive_taxes":null}}}}`, "set INVOICE_NINJA_INCLUSIVE_TAXES explicitly"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values, requests := newServiceDiscoveryFixture(t, serviceDiscoveryJSON(tc.product, `{}`, "5.13.50"))
			get, err := resolveServiceEnvironment(context.Background(), func(key string) string { return values[key] })
			if err == nil || !strings.Contains(err.Error(), tc.hint) || get != nil {
				t.Fatalf("expected actionable discovery failure containing %q, got %v", tc.hint, err)
			}
			requests(discoveryProductPath)
		})
	}
}

func TestDiscoverServiceCurrencyLookupFailures(t *testing.T) {
	for _, tc := range []struct{ name, currencies, code, hint string }{
		{"mismatched explicit code", `[{"id":1,"code":"USD"}]`, "EUR", "PAYMENTS_CURRENCY_CODE does not match"},
		{"duplicate matching ID", `[{"id":"1","code":"USD"},{"id":1,"code":"USD"}]`, "", "ambiguous"},
		{"conflicting matching ID", `[{"id":1,"code":"USD"},{"id":"1","code":"EUR"}]`, "", "ambiguous"},
		{"unmatched ID", `[{"id":2,"code":"EUR"}]`, "", "set PAYMENTS_CURRENCY_ID and PAYMENTS_CURRENCY_CODE explicitly"},
		{"empty currencies", `[]`, "", "set PAYMENTS_CURRENCY_ID and PAYMENTS_CURRENCY_CODE explicitly"},
		{"invalid ISO code", `[{"id":1,"code":"usd"}]`, "", "invalid ISO code"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			product := `{"data":{"id":"synthetic-product","company":{"settings":{"currency_id":1,"inclusive_taxes":false}}}}`
			values, requests := newServiceDiscoveryFixture(t, serviceDiscoveryJSON(product, `{"currencies":`+tc.currencies+`}`, "5.13.50"))
			values["PAYMENTS_CURRENCY_CODE"] = tc.code
			get, err := resolveServiceEnvironment(context.Background(), func(key string) string { return values[key] })
			if err == nil || !strings.Contains(err.Error(), tc.hint) || get != nil {
				t.Fatalf("expected currency lookup failure containing %q, got %v", tc.hint, err)
			}
			requests(discoveryProductPath, discoveryStaticsPath)
		})
	}
}

func TestDiscoverServiceHTTPFailuresPreserveStatusAndRedact(t *testing.T) {
	for _, path := range []string{discoveryProductPath, discoveryStaticsPath} {
		for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
			t.Run(fmt.Sprintf("%s/%d", path, status), func(t *testing.T) {
				const rawBody = "private-company-response-marker"
				values, requests := newServiceDiscoveryFixture(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("X-APP-VERSION", "5.13.50")
					w.WriteHeader(status)
					fmt.Fprint(w, rawBody+":"+r.Header.Get("X-API-TOKEN"))
				})
				if path == discoveryStaticsPath {
					values["PAYMENTS_CURRENCY_ID"] = "1"
					values["INVOICE_NINJA_INCLUSIVE_TAXES"] = "false"
				}
				get, err := resolveServiceEnvironment(context.Background(), func(key string) string { return values[key] })
				var httpErr *integrationhttp.HTTPError
				if !errors.As(err, &httpErr) || httpErr.StatusCode != status || get != nil {
					t.Fatalf("expected wrapped HTTP %d, got %v", status, err)
				}
				if !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", status)) {
					t.Errorf("operator-facing error lost status: %v", err)
				}
				for _, secret := range []string{rawBody, values["INVOICE_NINJA_API_TOKEN"], values["INVOICE_NINJA_BASE_URL"]} {
					if strings.Contains(err.Error(), secret) {
						t.Error("discovery error leaked credentials, response body or URL")
					}
				}
				requests(path)
			})
		}
	}
}

func TestDiscoverServiceTransportBounds(t *testing.T) {
	t.Run("invalid timeouts fail before HTTP", func(t *testing.T) {
		values, requests := newServiceDiscoveryFixture(t, func(http.ResponseWriter, *http.Request) {
			t.Error("invalid timeout must not make HTTP requests")
		})
		for _, timeout := range []string{"invalid", "0s", "-1s", "31s"} {
			values["INTEGRATION_HTTP_TIMEOUT"] = timeout
			_, err := resolveServiceEnvironment(context.Background(), func(key string) string { return values[key] })
			if err == nil || !strings.Contains(err.Error(), "INTEGRATION_HTTP_TIMEOUT") {
				t.Errorf("timeout %q: expected configuration error, got %v", timeout, err)
			}
		}
		requests()
	})
	t.Run("response size bounded", func(t *testing.T) {
		values, requests := newServiceDiscoveryFixture(t, func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, strings.Repeat(" ", integrationhttp.MaxResponseBytes+1))
		})
		_, err := resolveServiceEnvironment(context.Background(), func(key string) string { return values[key] })
		if err == nil || !strings.Contains(err.Error(), "4 MiB") {
			t.Fatalf("expected bounded response error, got %v", err)
		}
		requests(discoveryProductPath)
	})
	t.Run("request honors context cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		values, requests := newServiceDiscoveryFixture(t, func(_ http.ResponseWriter, r *http.Request) {
			cancel()
			<-r.Context().Done()
		})
		_, err := resolveServiceEnvironment(ctx, func(key string) string { return values[key] })
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context cancellation, got %v", err)
		}
		requests(discoveryProductPath)
	})
}
