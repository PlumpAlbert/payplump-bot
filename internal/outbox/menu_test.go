package outbox

import (
	"context"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/queue"
)

type menuTestSender struct{ plain, menu int }

func (s *menuTestSender) SendMessage(context.Context, int64, string) (int64, error) {
	s.plain++
	return 101, nil
}
func (s *menuTestSender) SendMenuMessage(context.Context, int64, string) (int64, error) {
	s.menu++
	return 102, nil
}

func TestDurableMenuFlagSelectsSenderCapability(t *testing.T) {
	for _, menu := range []bool{false, true} {
		var result finalization
		store := &fakeStore{finalize: func(ctx context.Context, f finalization) error {
			checkDeadline(t, ctx, finalizeTimeout)
			result = f
			return nil
		}}
		sender := &menuTestSender{}
		worker := testWorker(t, store, sender, 1)
		worker.deliver(context.Background(), queue.Message{Lease: queue.Lease{ID: 42, Attempt: 3}, BotID: 7, ChatID: 42, Text: "synthetic", ClientMenu: menu})
		if result.kind != "complete" || result.lease.Attempt != 3 {
			t.Fatal("message not finalized with its fence")
		}
		if menu && (sender.menu != 1 || sender.plain != 0 || result.messageID != 102) || !menu && (sender.plain != 1 || sender.menu != 0 || result.messageID != 101) {
			t.Fatal("keyboard intent was lost or applied to a plain message")
		}
	}
}

func TestUnsupportedMenuSenderFailsWithoutSendingPlainText(t *testing.T) {
	called := false
	var result finalization
	store := &fakeStore{finalize: func(_ context.Context, f finalization) error { result = f; return nil }}
	worker := testWorker(t, store, senderFunc(func(context.Context, int64, string) (int64, error) { called = true; return 1, nil }), 1)
	worker.deliver(context.Background(), queue.Message{Lease: queue.Lease{ID: 42, Attempt: 1}, BotID: 7, ChatID: 42, Text: "synthetic", ClientMenu: true})
	if called || result.kind != "fail" {
		t.Fatal("unsupported sender silently discarded durable markup")
	}
}
