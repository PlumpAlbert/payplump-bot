package main

import (
	"context"
	"errors"
	"sync"

	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
)

// The endpoint stays unready until identity and durable policies have been
// installed. Publish the gateway only after its exact policy revisions exist.
type startupGateway struct {
	mu    sync.RWMutex
	inner telegram.WebhookStore
}

func (g *startupGateway) install(inner telegram.WebhookStore) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.inner = inner
}
func (g *startupGateway) ApplyTelegramUpdate(ctx context.Context, bot int64, name string, u telegram.Update) error {
	g.mu.RLock()
	inner := g.inner
	g.mu.RUnlock()
	if inner == nil {
		return errors.New("service initialization pending")
	}
	return inner.ApplyTelegramUpdate(ctx, bot, name, u)
}
