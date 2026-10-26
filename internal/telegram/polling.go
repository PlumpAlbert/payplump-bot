package telegram

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"time"
)

const (
	pollMinWait        = time.Second
	pollMaxBackoff     = 30 * time.Second
	pollOffsetLifetime = time.Minute
)

type PollingAPI interface {
	GetUpdates(context.Context, int64) ([]Update, error)
}

type Poller struct {
	api    PollingAPI
	store  WebhookStore
	bot    User
	logger *slog.Logger
	now    func() time.Time
	wait   func(context.Context, time.Duration) error
}

func NewPoller(api PollingAPI, store WebhookStore, bot User, logger *slog.Logger) (*Poller, error) {
	if api == nil || store == nil || bot.ID <= 0 || !bot.IsBot || bot.Username == "" {
		return nil, errors.New("invalid Telegram polling configuration")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Poller{api: api, store: store, bot: bot, logger: logger, now: time.Now, wait: waitForWebhookRetry}, nil
}

func (p *Poller) Run(ctx context.Context) error {
	var offset int64
	var lastBatch time.Time
	backoff, applyBackoff := time.Second, time.Second
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		fetchStarted := p.now().Round(0)
		if !lastBatch.IsZero() && (fetchStarted.Before(lastBatch) || fetchStarted.Sub(lastBatch) >= pollOffsetLifetime) {
			offset = 0
		}
		updates, err := p.api.GetUpdates(ctx, offset)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			offset = 0
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.Permanent() {
				return apiErr
			}
			delay := backoff
			if errors.As(err, &apiErr) {
				delay = max(delay, apiErr.RetryAfter)
			}
			p.logger.Warn("Telegram polling retry", "operation", "get_updates", "delay", delay.String())
			backoff = min(backoff*2, pollMaxBackoff)
			if err := p.wait(ctx, delay); err != nil {
				return err
			}
			continue
		}
		backoff = time.Second
		lastBatch = fetchStarted
		if len(updates) == 0 {
			offset = 0
			applyBackoff = time.Second
			if err := p.wait(ctx, pollMinWait); err != nil {
				return err
			}
			continue
		}
		applyFailed := false
		for _, update := range updates {
			applyCtx, cancelApply := context.WithTimeout(ctx, 5*time.Second)
			err := p.store.ApplyTelegramUpdate(applyCtx, p.bot.ID, p.bot.Username, update)
			cancelApply()
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				applyFailed = true
				p.logger.Warn("Telegram update processing retry", "operation", "apply_update", "delay", applyBackoff.String())
				delay := applyBackoff
				applyBackoff = min(applyBackoff*2, pollMaxBackoff)
				if waitErr := p.wait(ctx, delay); waitErr != nil {
					return waitErr
				}
				// Do not acknowledge the failed update or anything after it.
				if update.ID == 0 {
					offset = 0
				} else {
					offset = min(offset, update.ID)
				}
				break
			}
			if update.ID == math.MaxInt64 {
				return errors.New("invalid Telegram update identifier")
			}
			offset = update.ID + 1
		}
		if !applyFailed {
			applyBackoff = time.Second
		}
	}
}
