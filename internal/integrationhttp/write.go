package integrationhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
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

// Post sends one JSON write without redirects or automatic body replay. Any
// error after this method begins must be treated as an unknown external result
// unless the caller can independently prove that no request was issued.
func (w *Writer) Post(ctx context.Context, segments []string, query url.Values, payload, result any) (http.Header, error) {
	if result == nil {
		return nil, errors.New("integration write requires a response destination")
	}
	body, err := json.Marshal(payload)
	if err != nil || len(body) > MaxResponseBytes {
		return nil, errors.New("invalid or oversized integration write payload")
	}
	return w.client.request(ctx, http.MethodPost, segments, query, bytes.NewReader(body), result)
}
