package main

import (
	"github.com/PlumpAlbert/payplump-bot/internal/broadcast"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
)

type broadcastStore = broadcast.UpdateStore

func newBroadcastGateway(inner telegram.WebhookStore, store broadcastStore, cfg broadcast.Config) telegram.WebhookStore {
	return broadcast.NewGateway(inner, store, cfg)
}
