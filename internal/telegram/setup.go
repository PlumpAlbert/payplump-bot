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

type PollingSetupAPI interface {
	GetMe(context.Context) (User, error)
	DeleteWebhook(context.Context) error
}

// ConfigurePolling verifies bot identity and removes any prior webhook while
// preserving pending updates. Polling itself starts only after common setup.
func ConfigurePolling(ctx context.Context, api PollingSetupAPI, logger *slog.Logger) (User, error) {
	return configurePolling(ctx, api, logger, waitForWebhookRetry)
}

func configurePolling(ctx context.Context, api PollingSetupAPI, logger *slog.Logger, wait func(context.Context, time.Duration) error) (User, error) {
	if logger == nil {
		logger = slog.Default()
	}
	var bot User
	backoff := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return User{}, err
		}
		var err error
		bot, err = api.GetMe(ctx)
		if err == nil {
			if bot.ID <= 0 || !bot.IsBot || bot.Username == "" {
				return User{}, errors.New("invalid Telegram bot identity")
			}
			break
		}
		setup := webhookSetup{logger: logger, wait: wait}
		if retryErr := setup.retry(ctx, "get_me", err, &backoff); retryErr != nil {
			return User{}, retryErr
		}
	}
	backoff = time.Second
	for {
		if err := ctx.Err(); err != nil {
			return User{}, err
		}
		err := api.DeleteWebhook(ctx)
		if err == nil {
			logger.Info("Telegram polling initialized", "bot_id", bot.ID)
			return bot, nil
		}
		setup := webhookSetup{logger: logger, wait: wait}
		if retryErr := setup.retry(ctx, "delete_webhook", err, &backoff); retryErr != nil {
			return User{}, retryErr
		}
	}
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
	s.logger.Warn("Telegram transport setup retry", "operation", operation, "delay", delay.String())
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
