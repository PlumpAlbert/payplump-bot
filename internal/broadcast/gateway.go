package broadcast

import (
	"context"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
)

// UpdateStore reparses raw messages and rechecks the current database policy.
type UpdateStore interface {
	ApplyBroadcastUpdate(context.Context, int64, string, telegram.Update, Policy) (bool, error)
}
type gateway struct {
	inner  telegram.WebhookStore
	store  UpdateStore
	policy Policy
}

// NewGateway freezes this process's policy. Non-broadcast/disabled messages pass
// unchanged to the ordinary menu; prepared freeform text is never intercepted.
func NewGateway(inner telegram.WebhookStore, store UpdateStore, cfg Config) telegram.WebhookStore {
	return &gateway{inner: inner, store: store, policy: cfg.Policy()}
}
func (g *gateway) ApplyTelegramUpdate(ctx context.Context, botID int64, username string, u telegram.Update) error {
	if g.policy.Enabled {
		handled, err := g.store.ApplyBroadcastUpdate(ctx, botID, username, u, g.policy)
		if err != nil || handled {
			return err
		}
	}
	return g.inner.ApplyTelegramUpdate(ctx, botID, username, u)
}
