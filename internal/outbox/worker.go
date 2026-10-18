package outbox

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
)

// Store fences every finalization with the claimed attempt. Claims return at
// most limit messages; RetryOutbox also persists a bot-wide cooldown for 429.
type Store interface {
	ClaimOutbox(ctx context.Context, botID int64, limit int, lease time.Duration) ([]queue.Message, error)
	CompleteOutbox(ctx context.Context, lease queue.Lease, telegramMessageID int64) error
	RetryOutbox(ctx context.Context, lease queue.Lease, delay time.Duration, errorCode int) error
	FailOutbox(ctx context.Context, lease queue.Lease, errorCode int) error
}

type Sender interface {
	SendMessage(ctx context.Context, chatID int64, text string) (int64, error)
}

// MenuSender is optional for legacy senders; menu messages never silently lose
// their durable keyboard intent when a sender lacks this capability.
type MenuSender interface {
	SendMenuMessage(context.Context, int64, string) (int64, error)
}

var errUnsupportedMenu = errors.New("sender does not support client menus")

type Worker struct {
	store  Store
	sender Sender
	botID  int64
	cfg    Config
	logger *slog.Logger
}

const (
	leaseDuration   = 45 * time.Second
	claimTimeout    = 5 * time.Second
	sendTimeout     = 10 * time.Second
	finalizeTimeout = 5 * time.Second
	maxRetryDelay   = 5 * time.Minute
	retryJitter     = 250 * time.Millisecond
)

// New uses the default logger when logger is nil.
func New(store Store, sender Sender, botID int64, cfg Config, logger *slog.Logger) (*Worker, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if botID <= 0 {
		return nil, errors.New("outbox bot ID must be positive")
	}
	if store == nil || sender == nil {
		return nil, errors.New("outbox store and sender are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{store: store, sender: sender, botID: botID, cfg: cfg, logger: logger}, nil
}

// Run must be called once per worker. It joins all in-flight sends and
// finalizations before returning nil on cancellation. Unacknowledged leases
// remain recoverable; delivery is at-least-once, not exactly-once.
func (w *Worker) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		claimCtx, cancel := context.WithTimeout(ctx, claimTimeout)
		messages, err := w.store.ClaimOutbox(claimCtx, w.botID, w.cfg.Workers, leaseDuration)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				w.logger.Error("outbox claim failed")
			}
		} else {
			var tasks sync.WaitGroup
			for _, message := range messages {
				if ctx.Err() != nil {
					break
				}
				tasks.Add(1)
				go func(message queue.Message) {
					defer tasks.Done()
					w.deliver(ctx, message)
				}(message)
			}
			tasks.Wait()
		}

		// Poll after failures as well as successes; an unavailable database must
		// neither stop the service nor cause a tight retry loop.
		timer := time.NewTimer(w.cfg.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	return nil
}

func (w *Worker) deliver(ctx context.Context, message queue.Message) {
	sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	if sendCtx.Err() != nil {
		cancel()
		return
	}
	var messageID int64
	var err error
	if message.ClientMenu {
		if sender, ok := w.sender.(MenuSender); ok {
			messageID, err = sender.SendMenuMessage(sendCtx, message.ChatID, message.Text)
		} else {
			err = errUnsupportedMenu
		}
	} else {
		messageID, err = w.sender.SendMessage(sendCtx, message.ChatID, message.Text)
	}
	cancel()

	// Persist known responses during shutdown, including bot-wide cooldowns.
	// Unknown canceled sends can safely wait for lease recovery instead.
	var apiErr *telegram.APIError
	knownAPIError := errors.As(err, &apiErr) && apiErr != nil
	if err != nil && ctx.Err() != nil && !knownAPIError {
		return
	}
	finalizeCtx, finalizeCancel := context.WithTimeout(context.Background(), finalizeTimeout)
	defer finalizeCancel()
	if err == nil {
		if w.store.CompleteOutbox(finalizeCtx, message.Lease, messageID) != nil {
			w.logger.Error("outbox completion failed", "work_id", message.ID, "error_code", 0)
		}
		return
	}

	code := 0
	minimum := time.Duration(0)
	permanent := errors.Is(err, errUnsupportedMenu)
	if knownAPIError {
		code = apiErr.Code
		minimum = apiErr.RetryAfter
		// Credentials can recover after configuration changes. Do not discard
		// every pending recipient because the current token is unauthorized.
		permanent = code != 401 && apiErr.Permanent()
	}
	w.logger.Warn("outbox send failed", "work_id", message.ID, "error_code", code)
	if permanent {
		err = w.store.FailOutbox(finalizeCtx, message.Lease, code)
	} else {
		err = w.store.RetryOutbox(finalizeCtx, message.Lease, retryDelay(message.Attempt, minimum), code)
	}
	if err != nil {
		w.logger.Error("outbox finalization failed", "work_id", message.ID, "error_code", code)
	}
}

func retryDelay(attempt int, minimum time.Duration) time.Duration {
	delay := time.Second
	for attempt > 1 && delay < maxRetryDelay {
		delay = min(2*delay, maxRetryDelay)
		attempt--
	}
	delay = min(delay+time.Duration(rand.Int64N(int64(retryJitter))), maxRetryDelay)
	return max(delay, minimum)
}
