package main

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
)

type startupProductReader interface {
	ProductKey(context.Context, string) (string, error)
}

// Only this read-only lookup is retried. A cold/temporarily unavailable Ninja
// should not force a Telegram transport restart after the first slow GET.
// Permanent authorization, missing-product and response-contract errors fail
// immediately. No external writes or process-global settings are changed.
func startupProductKey(ctx context.Context, reader startupProductReader, id string) (string, error) {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		key, err := reader.ProductKey(ctx, id)
		if err == nil || attempt == 2 || !transientStartupRead(err) {
			return key, err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
}

func transientStartupRead(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, integrationhttp.ErrConnection) {
		return true
	}
	var status *integrationhttp.HTTPError
	return errors.As(err, &status) && (status.StatusCode == http.StatusRequestTimeout || status.StatusCode == http.StatusTooManyRequests || status.StatusCode >= 500)
}
