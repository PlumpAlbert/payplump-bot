package payments

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type wakeObservation struct {
	botID int64
	hash  [32]byte
}

type wakeStoreFake struct {
	mu     sync.Mutex
	calls  []wakeObservation
	record func(context.Context, int64, [32]byte) (bool, error)
}

func (f *wakeStoreFake) RecordPaymentWake(ctx context.Context, botID int64, hash [32]byte) (bool, error) {
	f.mu.Lock()
	f.calls = append(f.calls, wakeObservation{botID, hash})
	f.mu.Unlock()
	if f.record != nil {
		return f.record(ctx, botID, hash)
	}
	return true, nil
}
func (f *wakeStoreFake) observations() []wakeObservation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]wakeObservation(nil), f.calls...)
}

func webhookForTest(t *testing.T, store WakeStore, active bool) *WebhookHandler {
	t.Helper()
	h, err := NewWebhook(testPaymentSecret, store)
	if err != nil {
		t.Fatal(err)
	}
	if active {
		if err := h.Activate(42); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func webhookRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, WebhookPath, strings.NewReader(body))
	r.Header.Set(WebhookSecretHeader, testPaymentSecret)
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestWebhookNewAndActivationRequirePrivateVerifiedBotIdentity(t *testing.T) {
	store := &wakeStoreFake{}
	for _, secret := range []string{"", strings.Repeat("a", 31), strings.Repeat("a", 257), strings.Repeat("é", 32), strings.Repeat("a", 32) + "\n"} {
		if _, err := NewWebhook(secret, store); err == nil {
			t.Fatal("accepted invalid receiver secret")
		}
	}
	if _, err := NewWebhook(testPaymentSecret, nil); err == nil {
		t.Fatal("accepted missing durable wake store")
	}
	for _, size := range []int{32, 256} {
		if _, err := NewWebhook(strings.Repeat("a", size), store); err != nil {
			t.Fatalf("secret boundary %d: %v", size, err)
		}
	}
	h := webhookForTest(t, store, false)
	if h.Ready() {
		t.Fatal("receiver ready before bot verification/activation")
	}
	for _, id := range []int64{0, -42} {
		if err := h.Activate(id); err == nil || h.Ready() {
			t.Fatal("invalid bot identity activated receiver")
		}
	}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, webhookRequest(`{"id":"untrusted"}`))
	if response.Code != http.StatusServiceUnavailable || len(store.observations()) != 0 {
		t.Fatal("unverified receiver consumed wake")
	}
	if err := h.Activate(42); err != nil || !h.Ready() {
		t.Fatalf("could not activate verified bot: %v", err)
	}
	if err := h.Activate(0); err == nil || !h.Ready() {
		t.Fatal("invalid activation silently changed previously verified identity")
	}
}

type unreadBody struct{ reads int }

func (b *unreadBody) Read([]byte) (int, error) {
	b.reads++
	return 0, errors.New("body must not be read before authentication")
}
func (*unreadBody) Close() error { return nil }

func TestWebhookAuthenticatesCustomHeaderBeforeBodyOrDatabase(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []string
	}{
		{"missing", nil}, {"wrong", []string{"wrong"}},
		{"two identical headers", []string{testPaymentSecret, testPaymentSecret}},
		{"correct then wrong", []string{testPaymentSecret, "wrong"}},
		{"wrong then correct", []string{"wrong", testPaymentSecret}},
		{"comma combined", []string{testPaymentSecret + ", " + testPaymentSecret}},
		{"trailing whitespace", []string{testPaymentSecret + " "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &wakeStoreFake{}
			h := webhookForTest(t, store, true)
			body := &unreadBody{}
			r := webhookRequest(`{}`)
			r.Body = body
			r.Header.Del(WebhookSecretHeader)
			for _, value := range tc.values {
				r.Header.Add(WebhookSecretHeader, value)
			}
			r.Header.Set("Content-Type", "not a valid media type")
			r.Header.Set("X-Signature", testPaymentSecret) // Not an invented HMAC protocol.
			response := httptest.NewRecorder()
			h.ServeHTTP(response, r)
			if response.Code != http.StatusForbidden || body.reads != 0 || len(store.observations()) != 0 {
				t.Fatalf("auth: status=%d reads=%d stores=%d", response.Code, body.reads, len(store.observations()))
			}
			if strings.Contains(response.Body.String(), testPaymentSecret) {
				t.Fatal("response leaks receiver secret")
			}
		})
	}
}

func TestWebhookMethodMediaTypeAndSingleBareJSONObject(t *testing.T) {
	for _, tc := range []struct {
		name, method, media, body string
		status                    int
	}{
		{"GET", "GET", "application/json", `{}`, 405},
		{"PUT", "PUT", "application/json", `{}`, 405},
		{"missing media", "POST", "", `{}`, 415},
		{"plain text", "POST", "text/plain", `{}`, 415},
		{"malformed media", "POST", "application/json; charset", `{}`, 415},
		{"vendor JSON", "POST", "application/problem+json", `{}`, 415},
		{"empty", "POST", "application/json", "", 400},
		{"whitespace", "POST", "application/json", " \r\n\t", 400},
		{"array", "POST", "application/json", `[{"id":"payment"}]`, 400},
		{"null", "POST", "application/json", `null`, 400},
		{"number", "POST", "application/json", `123`, 400},
		{"string", "POST", "application/json", `"payment"`, 400},
		{"truncated object", "POST", "application/json", `{"id":`, 400},
		{"two objects", "POST", "application/json", `{} {}`, 400},
		{"trailing scalar", "POST", "application/json", `{} true`, 400},
		{"trailing garbage", "POST", "application/json", `{} not-json`, 400},
		{"empty object is wake only", "POST", "application/json", `{}`, 200},
		{"no UUID event schema", "POST", "application/json", `{"id":"opaque-payment","status_id":"4","paymentables":[]}`, 200},
		{"media parameters", "POST", "application/json; charset=utf-8", `{"id":"opaque-payment"}`, 200},
		{"surrounding whitespace", "POST", "application/json", " \n{\"id\":\"opaque-payment\"}\t ", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &wakeStoreFake{}
			h := webhookForTest(t, store, true)
			r := webhookRequest(tc.body)
			r.Method = tc.method
			r.Header.Set("Content-Type", tc.media)
			response := httptest.NewRecorder()
			h.ServeHTTP(response, r)
			if response.Code != tc.status {
				t.Fatalf("status=%d, want %d", response.Code, tc.status)
			}
			wantCalls := 0
			if tc.status == 200 {
				wantCalls = 1
			}
			if len(store.observations()) != wantCalls {
				t.Fatalf("durable wakes=%d, want %d", len(store.observations()), wantCalls)
			}
			if tc.status == 405 && response.Header().Get("Allow") != "POST" {
				t.Error("missing Allow: POST")
			}
		})
	}
}

func TestWebhookOneMiBBoundary(t *testing.T) {
	// {"x":"..."} is eight bytes plus the ASCII string payload.
	for _, size := range []int{(1 << 20) - 1, 1 << 20, (1 << 20) + 1} {
		t.Run(fmt.Sprintf("bytes%d", size), func(t *testing.T) {
			store := &wakeStoreFake{}
			h := webhookForTest(t, store, true)
			body := `{"x":"` + strings.Repeat("a", size-8) + `"}`
			if len(body) != size {
				t.Fatal("incorrect test request boundary")
			}
			r := webhookRequest(body)
			r.ContentLength = -1 // Bound the actual stream, not its claimed length.
			response := httptest.NewRecorder()
			h.ServeHTTP(response, r)
			want := 200
			if size > 1<<20 {
				want = 413
			}
			if response.Code != want {
				t.Fatalf("%d-byte status=%d, want %d", size, response.Code, want)
			}
			if size > 1<<20 && len(store.observations()) != 0 {
				t.Fatal("oversized body reached storage")
			}
		})
	}
}

func TestWebhookUntrustedEntityOnlyHashesExactRawBodyAndVerifiedBot(t *testing.T) {
	store := &wakeStoreFake{}
	h := webhookForTest(t, store, true)
	for _, body := range []string{
		`{"bot_id":999,"client_id":"other","id_number":"someone-else","invoice_id":"foreign","status_id":"4","amount":"999999","payment_date":"2099-12-31","grant":true}`,
		" \n{\"id\":\"same-entity\",\"amount\":200}\n",
		`{"id":"same-entity","amount":200}`,
	} {
		response := httptest.NewRecorder()
		h.ServeHTTP(response, webhookRequest(body))
		if response.Code != 200 {
			t.Fatalf("bare entity rejected: %d", response.Code)
		}
		calls := store.observations()
		got := calls[len(calls)-1]
		if got.botID != 42 || got.hash != sha256.Sum256([]byte(body)) {
			t.Fatal("receiver trusted entity fields or hashed normalized rather than raw body")
		}
	}
	calls := store.observations()
	if calls[1].hash == calls[2].hash {
		t.Fatal("raw body whitespace must remain part of receipt identity")
	}
}

type responseStartRecorder struct {
	*httptest.ResponseRecorder
	started bool
}

func (r *responseStartRecorder) WriteHeader(status int) {
	r.started = true
	r.ResponseRecorder.WriteHeader(status)
}
func (r *responseStartRecorder) Write(body []byte) (int, error) {
	r.started = true
	return r.ResponseRecorder.Write(body)
}

func TestWebhookCommitsWakeBeforeResponseAndAcknowledgesDuplicates(t *testing.T) {
	var response *responseStartRecorder
	store := &wakeStoreFake{}
	store.record = func(ctx context.Context, _ int64, _ [32]byte) (bool, error) {
		if response.started {
			t.Error("HTTP success started before durable receipt commit")
		}
		checkDeadline(t, observeDeadline("wake commit", ctx), 5*time.Second)
		return len(store.observations()) == 1, nil // false is an already committed duplicate.
	}
	h := webhookForTest(t, store, true)
	for range 2 {
		response = &responseStartRecorder{ResponseRecorder: httptest.NewRecorder()}
		h.ServeHTTP(response, webhookRequest(`{"id":"opaque-entity"}`))
		if response.Code != 200 || !response.started {
			t.Fatalf("committed receipt status=%d", response.Code)
		}
	}
	calls := store.observations()
	if len(calls) != 2 || calls[0] != calls[1] {
		t.Fatal("duplicate receipt identity changed")
	}
}

func TestWebhookStoreErrorIsRetryableWithoutLeakingError(t *testing.T) {
	private := "private-database-error-" + testPaymentSecret
	store := &wakeStoreFake{record: func(context.Context, int64, [32]byte) (bool, error) { return false, errors.New(private) }}
	h := webhookForTest(t, store, true)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, webhookRequest(`{"id":"opaque-entity"}`))
	if response.Code != 503 || response.Header().Get("Retry-After") == "" {
		t.Fatal("uncommitted wake acknowledged without retry")
	}
	if strings.Contains(response.Body.String(), private) || strings.Contains(response.Body.String(), testPaymentSecret) {
		t.Fatal("receiver leaks storage errors or credentials")
	}
}

func TestWebhookReadFailureDoesNotReachStore(t *testing.T) {
	store := &wakeStoreFake{}
	h := webhookForTest(t, store, true)
	r := webhookRequest(`{}`)
	r.Body = io.NopCloser(failingWebhookReader{})
	response := httptest.NewRecorder()
	h.ServeHTTP(response, r)
	if response.Code != 400 || len(store.observations()) != 0 {
		t.Fatal("failed body read reached durable storage")
	}
}

type failingWebhookReader struct{}

func (failingWebhookReader) Read([]byte) (int, error) {
	return 0, errors.New("private network body error")
}
