package telegram

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

type WebhookAPI interface {
	GetMe(context.Context) (User, error)
	SetWebhook(context.Context, string, string) error
}

type webhookSetup struct {
	api    WebhookAPI
	logger *slog.Logger
	wait   func(context.Context, time.Duration) error
}

// ConfigureWebhook runs once during startup, retrying transient setup failures.
// Once it succeeds, incoming delivery is entirely driven by Telegram HTTP POSTs.
func ConfigureWebhook(ctx context.Context, api WebhookAPI, cfg Config, logger *slog.Logger) (User, error) {
	if logger == nil {
		logger = slog.Default()
	}
	s := webhookSetup{api: api, logger: logger, wait: waitForWebhookRetry}
	return s.run(ctx, cfg)
}

func waitForWebhookRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *webhookSetup) retry(ctx context.Context, operation string, err error, backoff *time.Duration) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var apiErr *APIError
	delay := *backoff
	if errors.As(err, &apiErr) {
		if apiErr.Permanent() {
			return apiErr
		}
		delay = max(delay, apiErr.RetryAfter)
	}
	s.logger.Warn("Telegram webhook setup retry", "operation", operation, "delay", delay.String())
	*backoff = min(*backoff*2, 30*time.Second)
	return s.wait(ctx, delay)
}

func (s *webhookSetup) run(ctx context.Context, cfg Config) (User, error) {
	var bot User
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return User{}, ctx.Err()
		}
		var err error
		bot, err = s.api.GetMe(ctx)
		if err == nil {
			break
		}
		if err = s.retry(ctx, "get_me", err, &backoff); err != nil {
			return User{}, err
		}
	}
	backoff = time.Second
	for {
		if ctx.Err() != nil {
			return User{}, ctx.Err()
		}
		err := s.api.SetWebhook(ctx, cfg.WebhookURL, cfg.WebhookSecret)
		if err == nil {
			s.logger.Info("Telegram webhook configured", "bot_id", bot.ID)
			return bot, nil
		}
		if err = s.retry(ctx, "set_webhook", err, &backoff); err != nil {
			return User{}, err
		}
	}
}
