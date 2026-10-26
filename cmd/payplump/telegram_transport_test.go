package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/httpserver"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
)

type transportAPIFuncs struct {
	me      func(context.Context) (telegram.User, error)
	set     func(context.Context, string, string) error
	remove  func(context.Context) error
	updates func(context.Context, int64) ([]telegram.Update, error)
}

func (a transportAPIFuncs) GetMe(ctx context.Context) (telegram.User, error) {
	return a.me(ctx)
}
func (a transportAPIFuncs) SetWebhook(ctx context.Context, url, secret string) error {
	return a.set(ctx, url, secret)
}
func (a transportAPIFuncs) DeleteWebhook(ctx context.Context) error { return a.remove(ctx) }
func (a transportAPIFuncs) GetUpdates(ctx context.Context, offset int64) ([]telegram.Update, error) {
	return a.updates(ctx, offset)
}

type transportStoreFunc func(context.Context, int64, string, telegram.Update) error

func (f transportStoreFunc) ApplyTelegramUpdate(ctx context.Context, bot int64, name string, update telegram.Update) error {
	return f(ctx, bot, name, update)
}

func transportLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestTelegramTransportSelectsOnlyConfiguredMode(t *testing.T) {
	for _, webhook := range []bool{false, true} {
		t.Run(map[bool]string{false: "polling", true: "webhook"}[webhook], func(t *testing.T) {
			bot := telegram.User{ID: 123, IsBot: true, Username: "SyntheticBot"}
			cfg := telegram.Config{Enabled: true}
			if webhook {
				cfg.WebhookURL = "https://bot.invalid/telegram/webhook"
				cfg.WebhookSecret = "synthetic-webhook-secret"
			}
			var calls []string
			var pollCalls atomic.Int32
			polled := make(chan struct{})
			api := transportAPIFuncs{
				me: func(context.Context) (telegram.User, error) {
					calls = append(calls, "get_me")
					return bot, nil
				},
				set: func(_ context.Context, url, secret string) error {
					calls = append(calls, "set_webhook")
					if url != cfg.WebhookURL || secret != cfg.WebhookSecret {
						t.Error("webhook settings changed")
					}
					return nil
				},
				remove: func(context.Context) error {
					calls = append(calls, "delete_webhook")
					return nil
				},
				updates: func(ctx context.Context, _ int64) ([]telegram.Update, error) {
					pollCalls.Add(1)
					close(polled)
					<-ctx.Done()
					return nil, ctx.Err()
				},
			}
			var applied int
			store := transportStoreFunc(func(_ context.Context, id int64, name string, update telegram.Update) error {
				if id != bot.ID || name != bot.Username || update.ID != 9 {
					t.Error("transport bypassed verified identity or changed the update")
				}
				applied++
				return nil
			})
			transport, err := newTelegramTransport(cfg, api, store, transportLogger())
			if err != nil || transport.Ready() {
				t.Fatal("new transport must remain unready", err)
			}
			verified, err := transport.Configure(context.Background())
			if err != nil || verified != bot || transport.Ready() || pollCalls.Load() != 0 {
				t.Fatal("setup consumed updates or published readiness too soon", err)
			}
			want := []string{"get_me", "delete_webhook"}
			if webhook {
				want[1] = "set_webhook"
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatal("wrong transport setup sequence", calls)
			}
			if err := transport.Activate(bot); err != nil {
				t.Fatal(err)
			}
			if webhook {
				if transport.Worker() != nil || transport.Handler() == nil || !transport.Ready() {
					t.Fatal("webhook mode installed a polling worker or lost its handler")
				}
				r := httptest.NewRequest(http.MethodPost, telegram.WebhookPath, strings.NewReader(`{"update_id":9}`))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set(telegram.WebhookSecretHeader, cfg.WebhookSecret)
				w := httptest.NewRecorder()
				transport.Handler().ServeHTTP(w, r)
				if w.Code != http.StatusOK || applied != 1 || pollCalls.Load() != 0 {
					t.Fatal("webhook did not use the shared update store")
				}
				return
			}
			if transport.Handler() != nil || transport.Worker() == nil || transport.Ready() {
				t.Fatal("polling became ready before its worker or exposed a webhook handler")
			}
			srv := httpserver.New("", func(context.Context) error { return nil }, transport.Handler())
			w := httptest.NewRecorder()
			srv.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, telegram.WebhookPath, nil))
			if w.Code != http.StatusNotFound {
				t.Fatal("polling exposed the Telegram webhook route", w.Code)
			}
			w = httptest.NewRecorder()
			srv.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/livez", nil))
			if w.Code != http.StatusOK {
				t.Fatal("polling removed HTTP health")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- transport.Worker()(ctx) }()
			select {
			case <-polled:
			case <-time.After(time.Second):
				t.Fatal("polling worker did not start")
			}
			if !transport.Ready() {
				t.Fatal("running poller is not ready")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) || transport.Ready() {
					t.Fatal("poller did not stop and revoke readiness", err)
				}
			case <-time.After(time.Second):
				t.Fatal("poller did not join")
			}
		})
	}
}

func TestTelegramPollingWaitsForInstalledGatewayAndCancelsWithServer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	configured, install, applied := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var pollCalls atomic.Int32
	bot := telegram.User{ID: 123, IsBot: true, Username: "SyntheticBot"}
	api := transportAPIFuncs{
		me:     func(context.Context) (telegram.User, error) { return bot, nil },
		remove: func(context.Context) error { return nil },
		updates: func(ctx context.Context, _ int64) ([]telegram.Update, error) {
			if pollCalls.Add(1) == 1 {
				return []telegram.Update{{ID: 9}}, nil
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	gateway := &startupGateway{}
	transport, err := newTelegramTransport(telegram.Config{}, api, gateway, transportLogger())
	if err != nil {
		t.Fatal(err)
	}
	initialize := func(ctx context.Context) error {
		verified, err := transport.Configure(ctx)
		if err != nil {
			return err
		}
		close(configured)
		select {
		case <-install:
		case <-ctx.Done():
			return ctx.Err()
		}
		gateway.install(transportStoreFunc(func(_ context.Context, id int64, name string, update telegram.Update) error {
			if id != bot.ID || name != bot.Username || update.ID != 9 {
				return errors.New("incorrect Telegram identity")
			}
			close(applied)
			return nil
		}))
		return transport.Activate(verified)
	}
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, transportLogger(), &http.Server{Addr: "127.0.0.1:0"}, initialize, func(ctx context.Context) error {
			return runWorkers(ctx, transport.Worker())
		})
	}()
	select {
	case <-configured:
	case <-time.After(time.Second):
		t.Fatal("setup did not start")
	}
	if pollCalls.Load() != 0 || transport.Ready() {
		t.Fatal("polling started before initialization completed")
	}
	close(install)
	select {
	case <-applied:
	case <-time.After(time.Second):
		t.Fatal("installed shared gateway did not receive the update")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil || transport.Ready() {
			t.Fatal("server failed to cancel/join polling", err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not join polling")
	}
}

func TestTelegramPollingConflictStopsPeersAndClearsReadiness(t *testing.T) {
	want := &telegram.APIError{HTTPStatus: http.StatusConflict, Code: http.StatusConflict}
	api := transportAPIFuncs{updates: func(context.Context, int64) ([]telegram.Update, error) { return nil, want }}
	transport, err := newTelegramTransport(telegram.Config{}, api, transportStoreFunc(func(context.Context, int64, string, telegram.Update) error { return nil }), transportLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.Worker()(context.Background()); err == nil || transport.Ready() {
		t.Fatal("uninitialized poller ran")
	}
	if err := transport.Activate(telegram.User{ID: 123, IsBot: true, Username: "SyntheticBot"}); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	err = runWorkers(context.Background(), transport.Worker(), func(ctx context.Context) error {
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	})
	if !errors.Is(err, want) || transport.Ready() {
		t.Fatal("conflict did not fail the worker group", err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("worker group returned without joining its peer")
	}
}
