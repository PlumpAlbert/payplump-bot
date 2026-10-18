package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/broadcast"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
)

type broadcastRouteStore struct {
	calls   int
	handled bool
	err     error
	got     telegram.Update
}

func (s *broadcastRouteStore) ApplyBroadcastUpdate(_ context.Context, _ int64, _ string, u telegram.Update, _ broadcast.Policy) (bool, error) {
	s.calls++
	s.got = u
	return s.handled, s.err
}

type broadcastInner struct {
	calls int
	got   telegram.Update
	err   error
}

func (s *broadcastInner) ApplyTelegramUpdate(_ context.Context, _ int64, _ string, u telegram.Update) error {
	s.calls++
	s.got = u
	return s.err
}

func TestBroadcastGatewayRouting(t *testing.T) {
	update := telegram.Update{ID: 8, Message: &telegram.Message{Text: "ordinary"}}
	for _, tc := range []struct {
		name                 string
		cfg                  broadcast.Config
		handled              bool
		storeErr             error
		wantStore, wantInner int
		wantErr              error
	}{
		{"disabled", broadcast.Config{}, false, nil, 0, 1, nil},
		{"delegates", broadcast.Config{Enabled: true}, false, nil, 1, 1, nil},
		{"handled", broadcast.Config{Enabled: true}, true, nil, 1, 0, nil},
		{"database error", broadcast.Config{Enabled: true}, false, errors.New("db"), 1, 0, errors.New("db")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &broadcastRouteStore{handled: tc.handled, err: tc.storeErr}
			inner := &broadcastInner{}
			g := newBroadcastGateway(inner, store, tc.cfg)
			err := g.ApplyTelegramUpdate(context.Background(), 123, "bot", update)
			if (err != nil) != (tc.wantErr != nil) {
				t.Fatalf("err %v", err)
			}
			if store.calls != tc.wantStore || inner.calls != tc.wantInner {
				t.Fatalf("calls store=%d inner=%d", store.calls, inner.calls)
			}
			if tc.wantInner == 1 && inner.got != update {
				t.Fatal("delegate did not receive exact update")
			}
			if tc.wantStore == 1 && store.got != update {
				t.Fatal("authorization did not receive exact update")
			}
		})
	}
}

func TestBroadcastConfigFailurePrecedesDatabaseStartup(t *testing.T) {
	t.Setenv("TELEGRAM_ENABLED", "false")
	t.Setenv("BROADCAST_ENABLED", "true")
	t.Setenv("BROADCAST_POLICY_REVISION", "1")
	err := run(slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err == nil {
		t.Fatal("broadcast enabled without Telegram unexpectedly started")
	}
}
