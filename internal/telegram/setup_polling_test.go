package telegram

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

type setupFake struct {
	calls             []string
	identity          User
	getErr, deleteErr error
}

func (f *setupFake) GetMe(context.Context) (User, error) {
	f.calls = append(f.calls, "getMe")
	return f.identity, f.getErr
}
func (f *setupFake) DeleteWebhook(context.Context) error {
	f.calls = append(f.calls, "deleteWebhook")
	return f.deleteErr
}
func TestConfigurePollingVerifiesIdentityThenPreservesQueue(t *testing.T) {
	f := &setupFake{identity: User{ID: 9, IsBot: true, Username: "bot"}}
	u, err := configurePolling(context.Background(), f, nil, func(context.Context, time.Duration) error { t.Fatal("unexpected retry"); return nil })
	if err != nil || u.ID != 9 || !reflect.DeepEqual(f.calls, []string{"getMe", "deleteWebhook"}) {
		t.Fatalf("user=%+v calls=%v err=%v", u, f.calls, err)
	}
}
func TestConfigurePollingPermanentAndCancellationAnd429(t *testing.T) {
	t.Run("permanent", func(t *testing.T) {
		f := &setupFake{getErr: &APIError{HTTPStatus: 401, Code: 401}}
		_, err := configurePolling(context.Background(), f, nil, func(context.Context, time.Duration) error { t.Fatal("retried permanent error"); return nil })
		var ae *APIError
		if !errors.As(err, &ae) || !reflect.DeepEqual(f.calls, []string{"getMe"}) {
			t.Fatalf("calls=%v err=%v", f.calls, err)
		}
	})
	t.Run("429", func(t *testing.T) {
		f := &setupFake{identity: User{ID: 9, IsBot: true, Username: "bot"}, deleteErr: &APIError{HTTPStatus: 429, Code: 429, RetryAfter: 17 * time.Second}}
		n := 0
		_, err := configurePolling(context.Background(), f, nil, func(_ context.Context, d time.Duration) error {
			n++
			if d != 17*time.Second {
				t.Fatalf("retry delay %v", d)
			}
			f.deleteErr = nil
			return nil
		})
		if err != nil || n != 1 || len(f.calls) != 3 {
			t.Fatalf("calls=%v retries=%d err=%v", f.calls, n, err)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		f := &setupFake{getErr: errors.New("transient")}
		_, err := configurePolling(ctx, f, nil, func(context.Context, time.Duration) error { cancel(); return ctx.Err() })
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("%v", err)
		}
	})
}
