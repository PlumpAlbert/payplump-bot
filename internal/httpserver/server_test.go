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
	})
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
