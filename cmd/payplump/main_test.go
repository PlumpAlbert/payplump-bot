package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthcheck(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
		err := healthcheck(srv.URL)
		srv.Close()
		if (err == nil) != (status == http.StatusOK) {
			t.Fatalf("status %d: %v", status, err)
		}
		if err := healthcheck(srv.URL); err == nil {
			t.Fatal("stopped service reported healthy")
		}
	}
}
