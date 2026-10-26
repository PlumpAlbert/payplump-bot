package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGetUpdatesRequestAndStrictResponse(t *testing.T) {
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bottoken/getUpdates" {
			t.Errorf("path %q", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":[{"update_id":4,"message":null},{"update_id":8,"future":true}]}`))
	}))
	defer srv.Close()
	c := NewClient("token")
	originalTimeout := c.http.Timeout
	c.baseURL = srv.URL
	c.http = srv.Client()
	got, err := c.GetUpdates(context.Background(), 3)
	if err != nil || len(got) != 2 || got[1].Message != nil {
		t.Fatalf("%#v %v", got, err)
	}
	if payload["offset"] != float64(3) || payload["limit"] != float64(100) || payload["timeout"] != float64(25) || !reflect.DeepEqual(payload["allowed_updates"], []any{"message"}) {
		t.Fatalf("payload %#v", payload)
	}
	if originalTimeout != 10*time.Second {
		t.Fatal("outbound client timeout changed")
	}
}

func TestGetUpdatesRejectsMalformedBatches(t *testing.T) {
	cases := map[string]string{
		"missing ID":         `{"ok":true,"result":[{}]}`,
		"negative ID":        `{"ok":true,"result":[{"update_id":-1}]}`,
		"max ID":             fmt.Sprintf(`{"ok":true,"result":[{"update_id":%d}]}`, int64(math.MaxInt64)),
		"duplicate":          `{"ok":true,"result":[{"update_id":1},{"update_id":1}]}`,
		"out of order":       `{"ok":true,"result":[{"update_id":2},{"update_id":1}]}`,
		"folded key":         `{"ok":true,"result":[{"update_id":1,"UPDATE_ID":2}]}`,
		"duplicate envelope": `{"ok":true,"OK":false,"result":[]}`,
		"contradictory":      `{"ok":true,"error_code":401,"result":[]}`,
		"null result":        `{"ok":true,"result":null}`,
		"trailing":           `{"ok":true,"result":[]} {}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			c := responseClient(t, body)
			if _, err := c.GetUpdates(context.Background(), 0); err == nil {
				t.Fatal("accepted invalid response")
			}
		})
	}
	t.Run("oversized", func(t *testing.T) {
		items := make([]string, 101)
		for i := range items {
			items[i] = fmt.Sprintf(`{"update_id":%d}`, i)
		}
		c := responseClient(t, `{"ok":true,"result":[`+strings.Join(items, ",")+`]}`)
		if _, err := c.GetUpdates(context.Background(), 0); err == nil {
			t.Fatal("accepted oversized batch")
		}
	})
}

type inspectRoundTripper func(*http.Request) (*http.Response, error)

func (f inspectRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestGetUpdatesUsesLongDeadlineAndCancellation(t *testing.T) {
	c := NewClient("synthetic-token")
	original := c.http.Timeout
	c.http = &http.Client{Transport: inspectRoundTripper(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) < 29*time.Second || time.Until(deadline) > 30*time.Second {
			t.Errorf("deadline margin missing: %v %v", deadline, ok)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":[]}`)), Header: make(http.Header)}, nil
	})}
	if _, err := c.GetUpdates(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if original != 10*time.Second {
		t.Fatalf("default outbound timeout %v", original)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.http = &http.Client{Transport: inspectRoundTripper(func(r *http.Request) (*http.Response, error) {
		cancel()
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	if _, err := c.GetUpdates(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel %v", err)
	}
}

func TestGetUpdatesRejectsNegativeOffsetWithoutRequestAndRedirectIsNotFollowed(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; http.Redirect(w, r, "/other", http.StatusFound) }))
	defer srv.Close()
	c := NewClient("synthetic-secret-token")
	c.baseURL = srv.URL
	if _, err := c.GetUpdates(context.Background(), -1); err == nil || calls != 0 {
		t.Fatal("negative offset made request")
	}
	_, err := c.GetUpdates(context.Background(), 0)
	if err == nil || calls != 1 {
		t.Fatalf("redirect followed: calls=%d err=%v", calls, err)
	}
	if strings.Contains(err.Error(), "synthetic-secret-token") {
		t.Fatal("token leaked")
	}
}

func TestDeleteWebhookFalseAndStrictResponse(t *testing.T) {
	for _, tc := range []struct {
		body string
		ok   bool
	}{{`{"ok":true,"result":true}`, true}, {`{"ok":true,"result":false}`, false}, {`{"ok":true,"OK":false,"result":true}`, false}, {`{"ok":true,"result":null}`, false}, {`{"ok":true,"result":"true"}`, false}} {
		t.Run(tc.body, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var p map[string]any
				_ = json.NewDecoder(r.Body).Decode(&p)
				if p["drop_pending_updates"] != false {
					t.Errorf("must preserve queue: %#v", p)
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := NewClient("token")
			c.baseURL = srv.URL
			c.http = srv.Client()
			err := c.DeleteWebhook(context.Background())
			if (err == nil) != tc.ok {
				t.Fatalf("err=%v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := NewClient("x").DeleteWebhook(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

func responseClient(t *testing.T, body string) *Client {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
	t.Cleanup(s.Close)
	c := NewClient("token")
	c.baseURL = s.URL
	c.http = s.Client()
	return c
}

type scriptedAPI struct {
	mu      sync.Mutex
	offsets []int64
	fn      func(int, int64) ([]Update, error)
}

func (a *scriptedAPI) GetUpdates(_ context.Context, o int64) ([]Update, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	i := len(a.offsets)
	a.offsets = append(a.offsets, o)
	return a.fn(i, o)
}

type scriptedStore struct {
	fn func(context.Context, Update) error
}

func (s scriptedStore) ApplyTelegramUpdate(ctx context.Context, _ int64, _ string, u Update) error {
	return s.fn(ctx, u)
}
func newTestPoller(api PollingAPI, store WebhookStore) *Poller {
	p, _ := NewPoller(api, store, User{ID: 42, IsBot: true, Username: "bot"}, nil)
	return p
}

func TestPollerFailedUpdateNotAcknowledgedAndApplyHasDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := &scriptedAPI{fn: func(i int, _ int64) ([]Update, error) {
		if i == 0 {
			return []Update{{ID: 4}, {ID: 5}, {ID: 6}}, nil
		}
		cancel()
		return nil, nil
	}}
	var ids []int64
	store := scriptedStore{fn: func(ctx context.Context, u Update) error {
		ids = append(ids, u.ID)
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 5*time.Second || time.Until(deadline) < 4*time.Second {
			t.Fatalf("apply deadline is not the 5s budget: %v %v", deadline, ok)
		}
		if u.ID == 5 {
			return errors.New("failure")
		}
		return nil
	}}
	p := newTestPoller(api, store)
	p.wait = func(context.Context, time.Duration) error { return nil }
	_ = p.Run(ctx)
	if !reflect.DeepEqual(ids, []int64{4, 5}) || !reflect.DeepEqual(api.offsets, []int64{0, 5}) {
		t.Fatalf("ids=%v offsets=%v", ids, api.offsets)
	}
}

func TestPollerApplyBackoffSurvivesSuccessfulFetchAndDuplicatePrefix(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := &scriptedAPI{fn: func(i int, _ int64) ([]Update, error) {
		if i == 9 {
			cancel()
			return nil, nil
		}
		if i == 8 {
			return []Update{{ID: 7}}, nil
		}
		// A replayed committed prefix must not reset the failed batch's delay.
		return []Update{{ID: 4}, {ID: 5}, {ID: 6}}, nil
	}}
	failures := 0
	store := scriptedStore{fn: func(_ context.Context, u Update) error {
		if u.ID == 5 {
			failures++
			if failures <= 7 {
				return errors.New("synthetic persistent database outage")
			}
		}
		if u.ID == 7 {
			return errors.New("synthetic new failure after recovery")
		}
		return nil
	}}
	p := newTestPoller(api, store)
	var delays []time.Duration
	p.wait = func(_ context.Context, d time.Duration) error {
		delays = append(delays, d)
		return nil
	}
	_ = p.Run(ctx)
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second, time.Second}
	if !reflect.DeepEqual(delays, want) {
		t.Fatalf("database failure/recovery delays %v, want %v", delays, want)
	}
}

func TestPollerRetryAfterCapConflictAndErrorReset(t *testing.T) {
	tests := []struct {
		name        string
		errs        []error
		wantDelay   []time.Duration
		wantOffsets []int64
		terminal    bool
	}{
		{"429 minimum", []error{&APIError{HTTPStatus: 429, Code: 429, RetryAfter: 7 * time.Second}}, []time.Duration{7 * time.Second}, []int64{0, 0}, false},
		{"429 capped", []error{&APIError{HTTPStatus: 429, Code: 429, RetryAfter: 40 * time.Second}}, []time.Duration{40 * time.Second}, []int64{0, 0}, false},
		{"conflict terminal", []error{&APIError{HTTPStatus: 409, Code: 409}}, nil, []int64{0}, true},
		{"transient reset", []error{errors.New("bad response")}, []time.Duration{time.Second}, []int64{0, 0}, false},
	}
	t.Run("exponential backoff cap", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		api := &scriptedAPI{fn: func(i int, _ int64) ([]Update, error) {
			if i == 7 {
				cancel()
				return nil, nil
			}
			return nil, errors.New("transient")
		}}
		p := newTestPoller(api, scriptedStore{fn: func(context.Context, Update) error { return nil }})
		var delays []time.Duration
		p.wait = func(_ context.Context, d time.Duration) error { delays = append(delays, d); return nil }
		_ = p.Run(ctx)
		want := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
		if !reflect.DeepEqual(delays, want) {
			t.Fatalf("delays %v", delays)
		}
	})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var delays []time.Duration
			api := &scriptedAPI{fn: func(i int, _ int64) ([]Update, error) {
				if i < len(tt.errs) {
					return nil, tt.errs[i]
				}
				cancel()
				return nil, nil
			}}
			p := newTestPoller(api, scriptedStore{fn: func(context.Context, Update) error { return nil }})
			p.wait = func(_ context.Context, d time.Duration) error { delays = append(delays, d); return nil }
			err := p.Run(ctx)
			if tt.terminal {
				var ae *APIError
				if !errors.As(err, &ae) || len(api.offsets) != 1 {
					t.Fatalf("err=%v offsets=%v", err, api.offsets)
				}
			} else if !reflect.DeepEqual(api.offsets, tt.wantOffsets) {
				t.Fatalf("offsets %v", api.offsets)
			}
			if !reflect.DeepEqual(delays, tt.wantDelay) {
				t.Fatalf("delays %v", delays)
			}
		})
	}
}

func TestPollerRebasesOnEmptyFetchElapsedFetchStoreAndClockRollback(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*time.Time, int)
		want   []int64
	}{
		{"empty rebase lower ID", func(now *time.Time, i int) {
			if i == 0 {
				*now = now.Add(time.Second)
			}
		}, []int64{0, 0, 2}},
		{"week gap during fetch", func(now *time.Time, i int) {
			if i == 0 {
				*now = now.Add(8 * 24 * time.Hour)
			}
		}, []int64{0, 0}},
		{"clock rollback", func(now *time.Time, i int) {
			if i == 0 {
				*now = now.Add(-time.Hour)
			}
		}, []int64{0, 0}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
			api := &scriptedAPI{fn: func(i int, _ int64) ([]Update, error) {
				if i == 0 {
					tt.mutate(&now, i)
					if tt.name == "empty rebase lower ID" {
						return nil, nil
					}
					return []Update{{ID: 100}}, nil
				}
				if i == 1 && tt.name == "empty rebase lower ID" {
					return []Update{{ID: 1}}, nil
				}
				cancel()
				return nil, nil
			}}
			store := scriptedStore{fn: func(context.Context, Update) error { return nil }}
			p := newTestPoller(api, store)
			p.now = func() time.Time { return now }
			p.wait = func(context.Context, time.Duration) error { return nil }
			_ = p.Run(ctx)
			if !reflect.DeepEqual(api.offsets, tt.want) {
				t.Fatalf("offsets=%v want=%v", api.offsets, tt.want)
			}
		})
	}
}

func TestPollerRebasesAfterWeekGapDuringStore(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	api := &scriptedAPI{fn: func(i int, _ int64) ([]Update, error) {
		if i == 0 {
			return []Update{{ID: 100}}, nil
		}
		cancel()
		return nil, nil
	}}
	p := newTestPoller(api, scriptedStore{fn: func(context.Context, Update) error { now = now.Add(8 * 24 * time.Hour); return nil }})
	p.now = func() time.Time { return now }
	_ = p.Run(ctx)
	if !reflect.DeepEqual(api.offsets, []int64{0, 0}) {
		t.Fatalf("offsets=%v", api.offsets)
	}
}

func TestPollerFailedZeroIDKeepsOffsetAndDoesNotApplyLaterBatchEntries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := &scriptedAPI{fn: func(i int, _ int64) ([]Update, error) {
		if i == 0 {
			return []Update{{ID: 0}, {ID: 1}}, nil
		}
		cancel()
		return nil, nil
	}}
	var ids []int64
	p := newTestPoller(api, scriptedStore{fn: func(_ context.Context, u Update) error { ids = append(ids, u.ID); return errors.New("fail") }})
	p.wait = func(context.Context, time.Duration) error { return nil }
	_ = p.Run(ctx)
	if !reflect.DeepEqual(ids, []int64{0}) || !reflect.DeepEqual(api.offsets, []int64{0, 0}) {
		t.Fatalf("ids=%v offsets=%v", ids, api.offsets)
	}
}

func TestPollerCancellationDuringStoreAndBackoff(t *testing.T) {
	t.Run("store", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		api := &scriptedAPI{fn: func(int, int64) ([]Update, error) { return []Update{{ID: 1}}, nil }}
		p := newTestPoller(api, scriptedStore{fn: func(ctx context.Context, _ Update) error { cancel(); <-ctx.Done(); return ctx.Err() }})
		if err := p.Run(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("backoff", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		api := &scriptedAPI{fn: func(int, int64) ([]Update, error) { return nil, &APIError{HTTPStatus: 429, Code: 429} }}
		p := newTestPoller(api, scriptedStore{fn: func(context.Context, Update) error { return nil }})
		p.wait = func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }
		if err := p.Run(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("%v", err)
		}
	})
}

func TestPollerDuplicateReplayAcrossRestart(t *testing.T) {
	var mu sync.Mutex
	seen := map[int64]bool{}
	deliveries, effects := 0, 0
	apply := scriptedStore{fn: func(_ context.Context, u Update) error {
		mu.Lock()
		defer mu.Unlock()
		deliveries++
		if seen[u.ID] {
			return nil
		}
		seen[u.ID] = true
		effects++
		return nil
	}}
	for n := 0; n < 2; n++ {
		ctx, cancel := context.WithCancel(context.Background())
		api := &scriptedAPI{fn: func(i int, _ int64) ([]Update, error) {
			if i == 0 {
				return []Update{{ID: 7}}, nil
			}
			cancel()
			return nil, nil
		}}
		p := newTestPoller(api, apply)
		p.wait = func(context.Context, time.Duration) error { return nil }
		_ = p.Run(ctx)
		cancel()
	}
	if len(seen) != 1 || deliveries != 2 || effects != 1 {
		t.Fatalf("deliveries=%d effects=%d dedup=%v", deliveries, effects, seen)
	}
}
