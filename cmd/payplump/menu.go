package main

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/clientmenu"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
)

type menuStore interface {
	telegram.WebhookStore
	LoadMenuSnapshot(context.Context, int64, int64, int64) (clientmenu.Snapshot, bool, error)
	ApplyMenuUpdate(context.Context, int64, string, telegram.Update, clientmenu.Prepared) error
}
type menuGateway struct {
	store   menuStore
	service *clientmenu.Service
}

// Pure GETs happen before the short commit transaction. Telegram retries an
// uncommitted update; processed-update dedup and outbox cover a lost commit ACK.
func (g *menuGateway) ApplyTelegramUpdate(ctx context.Context, botID int64, username string, update telegram.Update) error {
	identity, action, ok := update.PrivateMenu(username)
	if !ok {
		return g.store.ApplyTelegramUpdate(ctx, botID, username, update)
	}
	request := clientmenu.Request{BotID: botID, UpdateID: update.ID, TelegramID: identity.TelegramID, ChatID: identity.ChatID, Action: action}
	snapshot, duplicate, err := g.store.LoadMenuSnapshot(ctx, botID, update.ID, identity.TelegramID)
	if err != nil {
		return err
	}
	if duplicate {
		return nil
	}
	prepared, err := g.service.Prepare(ctx, request, snapshot)
	if err != nil {
		return err
	}
	return g.store.ApplyMenuUpdate(ctx, botID, username, update, prepared)
}

func newMenuGateway(store menuStore) (*menuGateway, error) {
	cfg := clientmenu.Config{PaymentPhone: os.Getenv("PAYMENT_PHONE"), PaymentBank: os.Getenv("PAYMENT_BANK"), PaymentRecipient: os.Getenv("PAYMENT_RECIPIENT")}
	if raw := strings.TrimSpace(os.Getenv("REGISTRATION_EXCLUDED_TELEGRAM_IDS")); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			id, err := strconv.ParseInt(part, 10, 64)
			if err != nil || id <= 0 || strconv.FormatInt(id, 10) != part {
				return nil, errors.New("invalid client menu exclusions")
			}
			cfg.ExcludedTelegramIDs = append(cfg.ExcludedTelegramIDs, id)
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	var reader clientmenu.Reader
	// Local menus remain usable without API credentials. Construct only the
	// read-only capability, and never probe the API at process startup.
	base, token := os.Getenv("INVOICE_NINJA_BASE_URL"), os.Getenv("INVOICE_NINJA_API_TOKEN")
	if base != "" && token != "" {
		api, err := invoiceninja.New(base, token, 2*time.Second)
		if err != nil {
			return nil, errors.New("invalid menu Invoice Ninja configuration")
		}
		reader = api
	}
	service, err := clientmenu.New(cfg, reader)
	if err != nil {
		return nil, err
	}
	return &menuGateway{store: store, service: service}, nil
}
