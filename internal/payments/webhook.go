package payments

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

const WebhookPath = "/invoiceninja/webhook"
const WebhookSecretHeader = "X-Payplump-Webhook-Secret"
const maxWebhookBytes = 1 << 20

type WakeStore interface {
	RecordPaymentWake(context.Context, int64, [32]byte) (bool, error)
}
type WebhookHandler struct {
	secret string
	store  WakeStore
	botID  atomic.Int64
}

func NewWebhook(secretValue string, store WakeStore) (*WebhookHandler, error) {
	if !secret(secretValue) || store == nil {
		return nil, errors.New("invalid payment webhook configuration")
	}
	return &WebhookHandler{secret: secretValue, store: store}, nil
}
func (h *WebhookHandler) Activate(botID int64) error {
	if botID <= 0 {
		return errors.New("invalid payment webhook bot identity")
	}
	h.botID.Store(botID)
	return nil
}
func (h *WebhookHandler) Ready() bool { return h.botID.Load() > 0 }

// The bare entity is deliberately NOT interpreted as a payment, event type or
// trustworthy resource selector. It only wakes bounded reconciliation via GET.
func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", 405)
		return
	}
	if len(r.Header.Values(WebhookSecretHeader)) != 1 || subtle.ConstantTimeCompare([]byte(r.Header.Get(WebhookSecretHeader)), []byte(h.secret)) != 1 {
		http.Error(w, "forbidden", 403)
		return
	}
	botID := h.botID.Load()
	if botID <= 0 {
		http.Error(w, "not ready", 503)
		return
	}
	mediaType, parameters, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	invalidParameter := false
	for _, value := range parameters {
		if strings.TrimSpace(value) == "" {
			invalidParameter = true
		}
	}
	if len(r.Header.Values("Content-Type")) != 1 || err != nil || invalidParameter || mediaType != "application/json" {
		http.Error(w, "expected application/json", 415)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBytes)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			http.Error(w, "request too large", 413)
		} else {
			http.Error(w, "invalid request", 400)
		}
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		http.Error(w, "invalid event", 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if _, err := h.store.RecordPaymentWake(ctx, botID, sha256.Sum256(body)); err != nil {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "temporarily unavailable", 503)
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}
