package lifecycle

import (
	"bytes"
	"context"
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

const (
	WebhookPath         = "/invoiceninja/sent-invoice-webhook"
	WebhookSecretHeader = "X-Payplump-Webhook-Secret"
	maxWebhookBytes     = 1 << 20
)

type WakeStore interface {
	RecordInvoiceLifecycleWake(context.Context, int64) error
}

type WebhookHandler struct {
	secret string
	store  WakeStore
	botID  atomic.Int64
}

func (*WebhookHandler) String() string   { return "[invoice lifecycle webhook handler]" }
func (*WebhookHandler) GoString() string { return "[invoice lifecycle webhook handler]" }

func NewWebhook(secret string, store WakeStore) (*WebhookHandler, error) {
	if !validSecret(secret) || store == nil {
		return nil, errors.New("invalid invoice lifecycle webhook configuration")
	}
	return &WebhookHandler{secret: secret, store: store}, nil
}

func (h *WebhookHandler) Activate(botID int64) error {
	if botID <= 0 {
		return errors.New("invalid invoice lifecycle webhook bot identity")
	}
	h.botID.Store(botID)
	return nil
}

func (h *WebhookHandler) Ready() bool { return h.botID.Load() > 0 }

// The route itself identifies sent_invoice. The request body is bounded and
// syntactically checked, but never used to select or authenticate a resource.
func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if len(r.Header.Values(WebhookSecretHeader)) != 1 || subtle.ConstantTimeCompare([]byte(r.Header.Get(WebhookSecretHeader)), []byte(h.secret)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	botID := h.botID.Load()
	if botID <= 0 {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
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
		http.Error(w, "expected application/json", http.StatusUnsupportedMediaType)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBytes)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "invalid request", http.StatusBadRequest)
		}
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		http.Error(w, "invalid event", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := h.store.RecordInvoiceLifecycleWake(ctx, botID); err != nil {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}
