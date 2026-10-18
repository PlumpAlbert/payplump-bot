package outbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
)

type configurationStore struct {
	*fakeStore
	rejected bool
}

func (s *configurationStore) FailConfigurationOutbox(ctx context.Context, lease queue.Lease, code int, cooldown time.Duration, rejected bool) error {
	s.rejected = rejected
	return s.record(ctx, finalization{kind: "protected-fail", lease: lease, code: code, delay: cooldown})
}

type configurationSender struct {
	plain, protected int
	send             func(context.Context, queue.Message) (int64, error)
}

func (s *configurationSender) SendMessage(context.Context, int64, string) (int64, error) {
	s.plain++
	return 1, nil
}
func (s *configurationSender) SendConfiguration(ctx context.Context, m queue.Message) (int64, error) {
	s.protected++
	return s.send(ctx, m)
}
func protectedOutboxMessage() queue.Message {
	return queue.Message{Lease: queue.Lease{ID: 1, Attempt: 1}, BotID: 7, ChatID: 101, Text: queue.ConfigurationIntentBody, ProtectedConfiguration: true}
}
func TestConfigurationDeliveryRequiresBothExplicitCapabilities(t *testing.T) {
	for _, missing := range []string{"sender", "store", "contradictory-menu", "wrong-body"} {
		t.Run(missing, func(t *testing.T) {
			var result finalization
			f := &fakeStore{finalize: func(_ context.Context, v finalization) error { result = v; return nil }}
			store := &configurationStore{fakeStore: f}
			sender := &configurationSender{send: func(context.Context, queue.Message) (int64, error) { t.Fatal("unsupported intent sent"); return 0, nil }}
			m := protectedOutboxMessage()
			var s Sender = sender
			var db Store = store
			switch missing {
			case "sender":
				s = senderFunc(func(context.Context, int64, string) (int64, error) {
					t.Fatal("secret intent silently downgraded")
					return 0, nil
				})
			case "store":
				db = f
			case "contradictory-menu":
				m.ClientMenu = true
			case "wrong-body":
				m.Text = "not an intent"
			}
			testWorker(t, db, s, 1).deliver(context.Background(), m)
			if result.kind != "protected-fail" && !(missing == "store" && result.kind == "fail") {
				t.Fatal("unsupported protected intent was retried or completed")
			}
			if sender.protected != 0 || sender.plain != 0 {
				t.Fatal("unsupported protected intent sent")
			}
		})
	}
}
func TestConfigurationSenderUsesLongerBoundedBudgetWithoutPlainFallback(t *testing.T) {
	var result finalization
	f := &configurationStore{fakeStore: &fakeStore{finalize: func(_ context.Context, v finalization) error { result = v; return nil }}}
	sender := &configurationSender{send: func(ctx context.Context, m queue.Message) (int64, error) {
		checkDeadline(t, ctx, protectedSendTimeout)
		deadline, _ := ctx.Deadline()
		if time.Until(deadline) < 25*time.Second || m.Text != queue.ConfigurationIntentBody {
			t.Fatal("protected preflight lost aggregate budget or typed intent")
		}
		return 33, nil
	}}
	testWorker(t, f, sender, 1).deliver(context.Background(), protectedOutboxMessage())
	if sender.plain != 0 || sender.protected != 1 || result.kind != "complete" || result.messageID != 33 {
		t.Fatal("protected sender routing failed")
	}
}
func TestConfigurationUnknownOutcomesNeverUseOrdinaryAutomaticRetry(t *testing.T) {
	for _, tc := range []struct {
		err      error
		known    bool
		code     int
		cooldown bool
	}{{errors.New("unknown transport outcome"), false, 0, false}, {&telegram.APIError{HTTPStatus: 503, Code: 503}, false, 503, false}, {&telegram.APIError{HTTPStatus: 403, Code: 403, Rejected: true}, true, 403, false}, {&telegram.APIError{HTTPStatus: 429, Code: 429}, false, 429, true}, {&telegram.APIError{HTTPStatus: 200, Code: 429, Rejected: true, RetryAfter: 2 * time.Second}, true, 429, true}, {&telegram.APIError{HTTPStatus: 401, Code: 401, Rejected: true}, true, 401, false}} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			var result finalization
			f := &configurationStore{fakeStore: &fakeStore{finalize: func(_ context.Context, v finalization) error { result = v; return nil }}}
			sender := &configurationSender{send: func(context.Context, queue.Message) (int64, error) { return 0, tc.err }}
			testWorker(t, f, sender, 1).deliver(context.Background(), protectedOutboxMessage())
			if result.kind != "protected-fail" || result.code != tc.code || f.rejected != tc.known || sender.plain != 0 || sender.protected != 1 {
				t.Fatal("protected failure lost unknown/rejection classification")
			}
			if tc.cooldown && (result.delay < time.Minute || result.delay > maxRetryDelay+retryJitter) {
				t.Fatal("Telegram 429 cooldown not persisted conservatively")
			}
			if !tc.cooldown && result.delay != 0 {
				t.Fatal("non-rate-limit failure introduced cooldown")
			}
		})
	}
}
func TestConfigurationShutdownDoesNotRearmUnknownAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &configurationStore{fakeStore: &fakeStore{finalize: func(context.Context, finalization) error {
		t.Fatal("shutdown rearmed or released protected attempt")
		return nil
	}}}
	s := &configurationSender{send: func(context.Context, queue.Message) (int64, error) { cancel(); return 0, context.Canceled }}
	testWorker(t, f, s, 1).deliver(ctx, protectedOutboxMessage())
	if s.protected != 1 || s.plain != 0 {
		t.Fatal("shutdown routing failed")
	}
}
