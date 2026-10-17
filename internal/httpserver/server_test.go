package httpserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthRoutes(t *testing.T) {
	dependencyErr := errors.New("private database details")
	srv := New(":0", func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("readiness check has no deadline")
		}
		return dependencyErr
	}, nil)
	for _, tc := range []struct {
		method, path string
		status       int
		body         string
	}{
		{"GET", "/livez", 200, "ok\n"},
		{"GET", "/readyz", 503, "not ready\n"},
		{"GET", "/missing", 404, "404 page not found\n"},
		{"POST", "/livez", 405, "Method Not Allowed\n"},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			srv.Handler.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
			if w.Code != tc.status || w.Body.String() != tc.body {
				t.Fatalf("got %d %q", w.Code, w.Body.String())
			}
		})
	}
	dependencyErr = nil
	w := httptest.NewRecorder()
	srv.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("recovered dependency: %d", w.Code)
	}
}

func TestWebhookRouteIsOptIn(t *testing.T) {
	ready := func(context.Context) error { return nil }
	called := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(204) })
	for _, tc := range []struct {
		enabled      bool
		method, path string
		status       int
	}{
		{true, "POST", "/telegram/webhook", 204},
		{true, "GET", "/telegram/webhook", 405},
		{true, "POST", "/telegram/webhook/extra", 404},
		{false, "POST", "/telegram/webhook", 404},
	} {
		var webhook http.Handler
		if tc.enabled {
			webhook = handler
		}
		srv := New(":0", ready, webhook)
		w := httptest.NewRecorder()
		srv.Handler.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.status {
			t.Fatalf("%s %s: got %d", tc.method, tc.path, w.Code)
		}
	}
	if !called {
		t.Fatal("webhook route not registered")
	}
}
