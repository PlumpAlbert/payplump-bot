package telegram

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"sync/atomic"
	"time"
)

const maxWebhookBytes = 1 << 20
const WebhookSecretHeader = "X-Telegram-Bot-Api-Secret-Token"

type WebhookStore interface {
	ApplyTelegramUpdate(context.Context, int64, string, Update) error
}

type WebhookHandler struct {
	secret string
	store  WebhookStore
	bot    atomic.Pointer[User]
}

func NewWebhookHandler(secret string, store WebhookStore) (*WebhookHandler, error) {
	if !validWebhookSecret(secret) || store == nil {
		return nil, errors.New("invalid Telegram webhook configuration")
	}
	return &WebhookHandler{secret: secret, store: store}, nil
}

// Activate publishes the verified bot identity only after setWebhook succeeds.
// Until then the endpoint returns 503 and readiness stays false.
func (h *WebhookHandler) Activate(bot User) error {
	if bot.ID <= 0 || !bot.IsBot || bot.Username == "" {
		return errors.New("invalid Telegram bot identity")
	}
	h.bot.Store(&bot)
	return nil
}

func (h *WebhookHandler) Ready() bool { return h.bot.Load() != nil }

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
	bot := h.bot.Load()
	if bot == nil {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		http.Error(w, "expected application/json", http.StatusUnsupportedMediaType)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBytes)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	var wire struct {
		ID      *int64   `json:"update_id"`
		Message *Message `json:"message"`
	}
	if err := decoder.Decode(&wire); err != nil {
		webhookBodyError(w, err)
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		webhookBodyError(w, err)
		return
	}
	if wire.ID == nil || *wire.ID < 0 {
		http.Error(w, "invalid update", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := h.store.ApplyTelegramUpdate(ctx, bot.ID, bot.Username, Update{ID: *wire.ID, Message: wire.Message}); err != nil {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

func webhookBodyError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	http.Error(w, "invalid update", http.StatusBadRequest)
}
