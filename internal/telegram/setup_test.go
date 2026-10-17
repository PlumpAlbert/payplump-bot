package telegram

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"
)

type setupAPI struct {
	me  func(context.Context) (User, error)
	set func(context.Context, string, string) error
}

func (a setupAPI) GetMe(ctx context.Context) (User, error) { return a.me(ctx) }
func (a setupAPI) SetWebhook(ctx context.Context, url, secret string) error {
	return a.set(ctx, url, secret)
}

func TestConfigureWebhookRegistersOnce(t *testing.T) {
	var methods []string
	var mu sync.Mutex
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/bottest-only-token/getMe":
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":123,"is_bot":true,"username":"PayplumpBot"}}`))
		case "/bottest-only-token/setWebhook":
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		default:
			t.Error("unexpected Telegram API method")
			w.WriteHeader(404)
		}
	})
	cfg := Config{WebhookURL: "https://bot.example.com/telegram/webhook", WebhookSecret: "test-webhook-secret"}
	bot, err := ConfigureWebhook(context.Background(), client, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || bot.ID != 123 {
		t.Fatalf("webhook setup: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(methods, []string{"/bottest-only-token/getMe", "/bottest-only-token/setWebhook"}) {
		t.Fatalf("unexpected API requests: %v", methods)
	}
}

func TestWebhookSetupHonorsRetryAfter(t *testing.T) {
	meCalls, setCalls := 0, 0
	api := setupAPI{
		me: func(context.Context) (User, error) {
			meCalls++
			if meCalls == 1 {
				return User{}, &APIError{Code: 429, RetryAfter: 17 * time.Second}
			}
			return User{ID: 123, IsBot: true, Username: "PayplumpBot"}, nil
		},
		set: func(context.Context, string, string) error {
			setCalls++
			if setCalls == 1 {
				return &APIError{Code: 429, RetryAfter: 7 * time.Second}
			}
			return nil
		},
	}
	var delays []time.Duration
	s := webhookSetup{api: api, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), wait: func(_ context.Context, d time.Duration) error { delays = append(delays, d); return nil }}
	if _, err := s.run(context.Background(), Config{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(delays, []time.Duration{17 * time.Second, 7 * time.Second}) {
		t.Fatalf("retry delays: %v", delays)
	}
}

func TestWebhookSetupStopsOnPermanentErrorAndCancellation(t *testing.T) {
	want := &APIError{Code: 401, HTTPStatus: 401}
	api := setupAPI{me: func(context.Context) (User, error) { return User{}, want }, set: func(context.Context, string, string) error { t.Error("registered without bot identity"); return nil }}
	if _, err := ConfigureWebhook(context.Background(), api, Config{}, nil); !errors.Is(err, want) {
		t.Fatalf("authentication error: %v", err)
	}
	api.me = func(context.Context) (User, error) { return User{ID: 123, IsBot: true, Username: "PayplumpBot"}, nil }
	api.set = func(context.Context, string, string) error { return want }
	if _, err := ConfigureWebhook(context.Background(), api, Config{}, nil); !errors.Is(err, want) {
		t.Fatalf("registration error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api.set = func(context.Context, string, string) error { return errors.New("temporary transport error") }
	s := webhookSetup{api: api, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), wait: func(ctx context.Context, _ time.Duration) error { cancel(); return waitForWebhookRetry(ctx, time.Hour) }}
	if _, err := s.run(ctx, Config{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled setup: %v", err)
	}
}
