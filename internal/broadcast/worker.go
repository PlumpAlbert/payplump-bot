package broadcast

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// BatchStore exposes only the durable, policy-fenced broadcast batch operation.
type BatchStore interface {
	ProcessBroadcastBatch(context.Context, int64, Policy, int) (Progress, error)
}

type Worker struct {
	store  BatchStore
	botID  int64
	cfg    Config
	policy Policy
	logger *slog.Logger
}

func New(store BatchStore, botID int64, cfg Config, logger *slog.Logger) (*Worker, error) {
	if store == nil || botID <= 0 || !cfg.Enabled || cfg.Validate(true) != nil {
		return nil, errors.New("invalid broadcast worker configuration")
	}
	if logger == nil {
		logger = slog.Default()
	}
	policy := cfg.Policy()
	cfg.AdminIDs = nil // The frozen policy owns its private, canonical identity copy.
	return &Worker{store: store, botID: botID, cfg: cfg, policy: policy, logger: logger}, nil
}

// Run bounds each DB operation and joins every poller before returning.
func (w *Worker) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for i := 0; i < w.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.poll(ctx)
		}()
	}
	wg.Wait()
	return nil
}

func (w *Worker) poll(ctx context.Context) {
	for ctx.Err() == nil {
		opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		p, err := w.store.ProcessBroadcastBatch(opCtx, w.botID, w.policy, w.cfg.BatchSize)
		cancel()
		if err == nil && (p.Enqueued != 0 || p.Skipped != 0 || p.Completed != 0) {
			w.logger.Info("broadcast batch processed", "enqueued", p.Enqueued, "skipped", p.Skipped, "completed", p.Completed)
		} else if err != nil && ctx.Err() == nil {
			w.logger.Warn("broadcast batch failed")
		}
		timer := time.NewTimer(w.cfg.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
