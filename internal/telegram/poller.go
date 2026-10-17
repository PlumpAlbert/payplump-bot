package telegram

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

type BotAPI interface {
	GetMe(context.Context) (User, error)
	GetUpdates(context.Context, int64) ([]Update, error)
}

type UpdateStore interface {
	TelegramOffset(context.Context, int64) (int64, error)
	ApplyTelegramUpdate(context.Context, int64, string, Update) error
}

type Poller struct {
	api    BotAPI
	store  UpdateStore
	logger *slog.Logger
	wait   func(context.Context, time.Duration) error
}

func NewPoller(api BotAPI, store UpdateStore, logger *slog.Logger) *Poller {
	if logger == nil {
		logger = slog.Default()
	}
	return &Poller{api: api, store: store, logger: logger, wait: waitForRetry}
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (p *Poller) retry(ctx context.Context, operation string, err error, backoff *time.Duration) error {
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
	// Neither SQL errors nor Telegram response bodies are logged here.
	p.logger.Warn("Telegram polling retry", "operation", operation, "delay", delay.String())
	*backoff = min(*backoff*2, 30*time.Second)
	return p.wait(ctx, delay)
}

// Run confirms updates to Telegram only by requesting the next persisted
// offset. A database error stops the batch, so the failing update stays queued.
// Permanent API failures (including a competing poller/webhook's 409) stop the
// service; transient failures use cancellable backoff and honor retry_after.
func (p *Poller) Run(ctx context.Context) error {
	backoff := time.Second
	var bot User
	for {
		if ctx.Err() != nil {
			return nil
		}
		var err error
		bot, err = p.api.GetMe(ctx)
		if err == nil {
			break
		}
		if err = p.retry(ctx, "get_me", err, &backoff); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
	p.logger.Info("Telegram polling started", "bot_id", bot.ID)
	backoff = time.Second
	for ctx.Err() == nil {
		lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		offset, err := p.store.TelegramOffset(lookupCtx, bot.ID)
		cancel()
		operation := "load_offset"
		if err == nil {
			operation = "get_updates"
			var updates []Update
			updates, err = p.api.GetUpdates(ctx, offset)
			if err == nil {
				operation = "apply_update"
				for _, update := range updates {
					updateCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
					err = p.store.ApplyTelegramUpdate(updateCtx, bot.ID, bot.Username, update)
					cancel()
					if err != nil {
						break
					}
				}
			}
		}
		if err != nil {
			if err = p.retry(ctx, operation, err, &backoff); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			continue
		}
		backoff = time.Second
	}
	return nil
}
