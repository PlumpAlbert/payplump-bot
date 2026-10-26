package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
)

type telegramTransportAPI interface {
	telegram.WebhookAPI
	telegram.PollingSetupAPI
	telegram.PollingAPI
}

// telegramTransport selects only the incoming transport. Both modes deliver to
// the same gateway, installed by normal startup after durable policies exist.
type telegramTransport struct {
	config       telegram.Config
	api          telegramTransportAPI
	store        telegram.WebhookStore
	logger       *slog.Logger
	webhook      *telegram.WebhookHandler
	poller       *telegram.Poller
	pollingReady atomic.Bool
}

func newTelegramTransport(cfg telegram.Config, api telegramTransportAPI, store telegram.WebhookStore, logger *slog.Logger) (*telegramTransport, error) {
	if api == nil || store == nil {
		return nil, errors.New("invalid Telegram transport configuration")
	}
	t := &telegramTransport{config: cfg, api: api, store: store, logger: logger}
	if cfg.UsesWebhook() {
		var err error
		t.webhook, err = telegram.NewWebhookHandler(cfg.WebhookSecret, store)
		if err != nil {
			return nil, err
		}
	}
	return t, nil
}

func (t *telegramTransport) Handler() http.Handler {
	if t.webhook == nil {
		return nil
	}
	return t.webhook
}

func (t *telegramTransport) Configure(ctx context.Context) (telegram.User, error) {
	if t.webhook != nil {
		return telegram.ConfigureWebhook(ctx, t.api, t.config, t.logger)
	}
	return telegram.ConfigurePolling(ctx, t.api, t.logger)
}

// Activate is called only after the gateway and all startup policies are ready.
// Polling remains unready until its worker actually starts.
func (t *telegramTransport) Activate(bot telegram.User) error {
	if t.webhook != nil {
		return t.webhook.Activate(bot)
	}
	p, err := telegram.NewPoller(t.api, t.store, bot, t.logger)
	if err != nil {
		return err
	}
	t.poller = p
	return nil
}

func (t *telegramTransport) Ready() bool {
	if t.webhook != nil {
		return t.webhook.Ready()
	}
	return t.pollingReady.Load()
}

// Worker is absent for webhook mode; HTTP owns its incoming requests.
func (t *telegramTransport) Worker() func(context.Context) error {
	if t.webhook != nil {
		return nil
	}
	return func(ctx context.Context) error {
		if t.poller == nil {
			return errors.New("Telegram polling initialization pending")
		}
		t.pollingReady.Store(true)
		defer t.pollingReady.Store(false)
		return t.poller.Run(ctx)
	}
}
