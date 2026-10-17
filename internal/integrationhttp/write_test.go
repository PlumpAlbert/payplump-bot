package integrationhttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestExplicitWriterPostsJSONOnce(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Token") != "synthetic-token" || r.URL.Path != "/prefix/invoices" {
			t.Error("unexpected write request")
		}
		var payload struct {
			Amount json.Number `json:"amount"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.Amount.String() != "123.450000000000001" {
			t.Error("write payload was changed or rounded")
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"synthetic-object"}`)
	}))
	defer server.Close()
	writer, err := NewWriter(server.URL+"/prefix", http.Header{"Token": {"synthetic-token"}}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		ID string `json:"id"`
	}
	_, err = writer.Post(context.Background(), []string{"invoices"}, nil, struct {
		Amount json.Number `json:"amount"`
	}{json.Number("123.450000000000001")}, &result)
	if err != nil || result.ID != "synthetic-object" || requests.Load() != 1 {
		t.Fatalf("write: count=%d err=%v", requests.Load(), err)
	}
}

func TestWriterNeverFollowsRedirectOrRetriesUnknownResponse(t *testing.T) {
	for _, status := range []int{200, 307, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var received atomic.Int32
			destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("write credentials forwarded") }))
			defer destination.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Location", destination.URL)
				w.WriteHeader(status)
				fmt.Fprint(w, `{"private-token":"private-body"`)
			}))
			defer server.Close()
			writer, err := NewWriter(server.URL, http.Header{"Token": {"private-token"}}, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			var result any
			_, err = writer.Post(context.Background(), []string{"clients"}, nil, map[string]string{"name": "synthetic"}, &result)
			if err == nil || received.Load() != 1 || strings.Contains(err.Error(), "private-token") || strings.Contains(err.Error(), "private-body") || strings.Contains(err.Error(), server.URL) {
				t.Fatal("write repeated or private diagnostics leaked")
			}
		})
	}
}

func TestWriterCancellationDoesNotReplayAcceptedRequest(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-release
	}))
	defer server.Close()
	defer close(release)
	writer, err := NewWriter(server.URL, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		var result any
		_, err := writer.Post(ctx, []string{"clients"}, nil, struct{}{}, &result)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || requests.Load() != 1 {
			t.Fatal("cancellation lost or accepted request replayed")
		}
	case <-time.After(time.Second):
		t.Fatal("write did not cancel")
	}
}
