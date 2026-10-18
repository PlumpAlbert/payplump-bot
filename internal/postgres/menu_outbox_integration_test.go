//go:build integration

package postgres

import (
	"errors"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/queue"
)

func TestMenuKeyboardIntentIsDurableAndImmutable(t *testing.T) {
	s, ctx := testStore(t)
	draft := queue.Draft{BotID: 123, ChatID: 42, Key: "menu-keyboard-test", Text: "synthetic", ClientMenu: true}
	id, err := s.EnqueueOutbox(ctx, draft)
	if err != nil {
		t.Fatal(err)
	}
	if repeated, err := s.EnqueueOutbox(ctx, draft); err != nil || repeated != id {
		t.Fatal("same menu intent not idempotent")
	}
	draft.ClientMenu = false
	if _, err := s.EnqueueOutbox(ctx, draft); !errors.Is(err, queue.ErrConflict) {
		t.Fatal("menu flag was mutable under same business key")
	}
	messages, err := s.ClaimOutbox(ctx, 123, 1, time.Minute)
	if err != nil || len(messages) != 1 || !messages[0].ClientMenu || messages[0].ID != id {
		t.Fatal("claim lost keyboard intent")
	}
	if err := s.CompleteOutbox(ctx, messages[0].Lease, 99); err != nil {
		t.Fatal(err)
	}
	draft.Key = "plain-keyboard-test"
	if _, err := s.EnqueueOutbox(ctx, draft); err != nil {
		t.Fatal(err)
	}
	messages, err = s.ClaimOutbox(ctx, 123, 1, time.Minute)
	if err != nil || len(messages) != 1 || messages[0].ClientMenu {
		t.Fatal("legacy plain message gained a keyboard")
	}
}
