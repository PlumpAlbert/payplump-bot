package main

import (
	"context"
	"errors"
)

// Cancellation is shared and every worker is joined. The HTTP service owns the
// one 10-second shutdown deadline; this function adds no separate grace period.
func runWorkers(ctx context.Context, workers ...func(context.Context) error) error {
	if len(workers) == 0 {
		return errors.New("no background workers configured")
	}
	for _, worker := range workers {
		if worker == nil {
			return errors.New("nil background worker configured")
		}
	}
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, len(workers))
	for _, run := range workers {
		go func(run func(context.Context) error) { results <- run(child) }(run)
	}
	var failure error
	for range workers {
		err := <-results
		if child.Err() == nil {
			if err == nil {
				err = errors.New("background worker stopped unexpectedly")
			}
			failure = errors.Join(failure, err)
			cancel()
		} else if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, ctx.Err()) {
			failure = errors.Join(failure, err)
		}
	}
	return failure
}
