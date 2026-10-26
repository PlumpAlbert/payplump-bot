//go:build integration

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/broadcast"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
)

// Uses only the existing explicit loopback/disposable-PG17 fixture. Locally this
// file is compiled, not executed; runtime belongs to the required CI job.
func TestTelegramPollingReplayAndWebhookSwitchUseSharedGatewayRealPostgres(t *testing.T) {
	db := newImportApplyPostgresSchema(t)
	for _, key := range []string{"INVOICE_NINJA_BASE_URL", "INVOICE_NINJA_API_TOKEN", "REGISTRATION_EXCLUDED_TELEGRAM_IDS", "PAYMENT_BANK", "PAYMENT_RECIPIENT"} {
		t.Setenv(key, "")
	}
	t.Setenv("PAYMENT_PHONE", "synthetic-payment-phone")
	bot := telegram.User{ID: 7001, IsBot: true, Username: "SyntheticBot"}
	gateway := &startupGateway{}
	command := func(id, actor int64, text string) telegram.Update {
		head, _, _ := strings.Cut(text, " ")
		return telegram.Update{ID: id, Message: &telegram.Message{
			From: &telegram.User{ID: actor, FirstName: "Synthetic"},
			Chat: telegram.Chat{ID: actor, Type: "private"}, Text: text,
			Entities: []telegram.Entity{{Type: "bot_command", Offset: 0, Length: len(head)}},
		}}
	}
	group := command(3, 43, "/start")
	group.Message.Chat.Type = "group"
	forged := command(4, 44, "/start")
	forged.Message.Chat.ID = 45
	updates := []telegram.Update{
		{ID: 0}, command(1, 42, "/start"), command(2, 42, "/status"), group, forged,
		command(5, 43, "/broadcast_text unauthorized"),
		command(6, 9002, "/broadcast_text synthetic broadcast"),
	}
	var methods []string
	api := transportAPIFuncs{
		me: func(context.Context) (telegram.User, error) {
			methods = append(methods, "get_me")
			return bot, nil
		},
		remove: func(context.Context) error { methods = append(methods, "delete_webhook"); return nil },
		set: func(context.Context, string, string) error {
			methods = append(methods, "set_webhook")
			return nil
		},
	}
	initial, err := newTelegramTransport(telegram.Config{}, api, gateway, transportLogger())
	if err != nil {
		t.Fatal(err)
	}
	verified, err := initial.Configure(db.ctx)
	if err != nil || verified != bot || initial.Ready() {
		t.Fatal("polling setup failed", err)
	}
	bc := broadcast.Config{Enabled: true, Revision: 1, MaxRecipients: 1000, AdminIDs: map[int64]struct{}{9002: {}}}
	policy := bc.Policy()
	if err := db.store.InstallServiceBroadcastPolicy(db.ctx, bot.ID, &policy); err != nil {
		t.Fatal(err)
	}
	bc.Revision = policy.Revision
	menu, err := newMenuGateway(db.store)
	if err != nil {
		t.Fatal(err)
	}
	gateway.install(newBroadcastGateway(menu, db.store, bc))

	runPolling := func() {
		t.Helper()
		ctx, cancel := context.WithCancel(db.ctx)
		defer cancel()
		calls := 0
		pollAPI := api
		pollAPI.updates = func(_ context.Context, offset int64) ([]telegram.Update, error) {
			calls++
			if calls == 1 {
				if offset != 0 {
					t.Fatal("restart reused a durable high-water offset")
				}
				return updates, nil
			}
			if offset != 7 {
				t.Fatal("offset advanced before all updates committed", offset)
			}
			cancel() // Lost acknowledgement: replay the same batch on next run.
			return nil, ctx.Err()
		}
		transport, err := newTelegramTransport(telegram.Config{}, pollAPI, gateway, transportLogger())
		if err != nil {
			t.Fatal(err)
		}
		verified, err := transport.Configure(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := transport.Activate(verified); err != nil {
			t.Fatal(err)
		}
		if err := transport.Worker()(ctx); !errors.Is(err, context.Canceled) || transport.Ready() || calls != 2 {
			t.Fatal("polling failed to commit/replay or join", err)
		}
	}
	counts := func() []int {
		t.Helper()
		var got []int
		for _, table := range []string{"telegram_processed_updates", "clients", "subscriptions", "operations", "outbox", "broadcast_drafts", "broadcasts"} {
			var count int
			if err := db.inspect.QueryRow(db.ctx, `SELECT count(*) FROM `+table).Scan(&count); err != nil {
				t.Fatal(err)
			}
			got = append(got, count)
		}
		return got
	}
	runPolling()
	want := []int{7, 1, 1, 1, 3, 1, 0}
	if got := counts(); !reflect.DeepEqual(got, want) {
		t.Fatal("shared menu/admin/private-sender scope changed", got, want)
	}
	runPolling()
	if got := counts(); !reflect.DeepEqual(got, want) {
		t.Fatal("restart replay duplicated business state or outbox", got, want)
	}

	cfg := telegram.Config{WebhookURL: "https://bot.invalid/telegram/webhook", WebhookSecret: "synthetic-webhook-secret"}
	webhook, err := newTelegramTransport(cfg, api, gateway, transportLogger())
	if err != nil {
		t.Fatal(err)
	}
	verified, err = webhook.Configure(db.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := webhook.Activate(verified); err != nil {
		t.Fatal(err)
	}
	if webhook.Worker() != nil || !webhook.Ready() {
		t.Fatal("webhook switch created another polling worker")
	}
	for _, update := range updates {
		// Deduplication is by bot/update ID even if the redelivered body differs.
		request := httptest.NewRequest(http.MethodPost, telegram.WebhookPath, strings.NewReader(fmt.Sprintf(`{"update_id":%d}`, update.ID)))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set(telegram.WebhookSecretHeader, cfg.WebhookSecret)
		response := httptest.NewRecorder()
		webhook.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatal("webhook replay failed", response.Code)
		}
	}
	if got := counts(); !reflect.DeepEqual(got, want) {
		t.Fatal("mode switch duplicated durable state", got, want)
	}
	if methods[len(methods)-1] != "set_webhook" {
		t.Fatal("wrong transport control plane after switch", methods)
	}
}
