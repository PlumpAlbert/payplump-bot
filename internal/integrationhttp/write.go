package integrationhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Writer must be constructed explicitly. The read-only Client has no mutation
// method, so preflight code cannot accidentally issue a write through it.
// Callers must durably reserve a write intent before Post and reconcile an
// uncertain result; this transport does not provide server-side idempotency.
type Writer struct{ client *Client }

func NewWriter(baseURL string, headers http.Header, timeout time.Duration) (*Writer, error) {
	client, err := New(baseURL, headers, timeout)
	if err != nil {
		return nil, err
	}
	return &Writer{client: client}, nil
}

// PostForm sends one form-urlencoded mutation, needed by the source-pinned
// S-UI API. Encoding snapshots the caller's values before networking. It has
// the same redirect/body-replay restrictions as Post; unknown results are not
// permission to call it again.
func (w *Writer) PostForm(ctx context.Context, segments []string, query, payload url.Values, result any) (http.Header, error) {
	if result == nil {
		return nil, errors.New("integration write requires a response destination")
	}
	body := payload.Encode()
	if len(body) == 0 || len(body) > MaxResponseBytes {
		return nil, errors.New("invalid or oversized integration form payload")
	}
	return w.client.requestType(ctx, http.MethodPost, segments, query, strings.NewReader(body), result, "application/x-www-form-urlencoded")
}

// Post sends one JSON write without redirects or automatic body replay. Any
// error after this method begins must be treated as an unknown external result
// unless the caller can independently prove that no request was issued.
func (w *Writer) Post(ctx context.Context, segments []string, query url.Values, payload, result any) (http.Header, error) {
	return w.jsonWrite(ctx, http.MethodPost, segments, query, payload, result)
}

// Put is the explicit non-replaying update capability. HTTP's idempotent PUT
// semantics do not make a paused sender safe to replay against a mutable record;
// callers still need a durable one-shot reservation and a fresh bound snapshot.
func (w *Writer) Put(ctx context.Context, segments []string, query url.Values, payload, result any) (http.Header, error) {
	return w.jsonWrite(ctx, http.MethodPut, segments, query, payload, result)
}

func (w *Writer) jsonWrite(ctx context.Context, method string, segments []string, query url.Values, payload, result any) (http.Header, error) {
	if result == nil {
		return nil, errors.New("integration write requires a response destination")
	}
	body, err := json.Marshal(payload)
	if err != nil || len(body) > MaxResponseBytes {
		return nil, errors.New("invalid or oversized integration write payload")
	}
	return w.client.request(ctx, method, segments, query, bytes.NewReader(body), result)
}
