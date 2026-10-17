package telegram

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

type fakeAPI struct {
	me      func(context.Context) (User, error)
	updates func(context.Context, int64) ([]Update, error)
}

func (f fakeAPI) GetMe(ctx context.Context) (User, error) {
	if f.me != nil {
		return f.me(ctx)
	}
	return User{ID: 123, IsBot: true, Username: "PayplumpBot"}, nil
}
func (f fakeAPI) GetUpdates(ctx context.Context, offset int64) ([]Update, error) {
	return f.updates(ctx, offset)
}

type fakeStore struct {
	offset int64
	apply  func(context.Context, int64, string, Update) error
	lookup func(context.Context, int64) (int64, error)
}

func (f *fakeStore) TelegramOffset(ctx context.Context, botID int64) (int64, error) {
	if f.lookup != nil {
		return f.lookup(ctx, botID)
	}
	return f.offset, nil
}
func (f *fakeStore) ApplyTelegramUpdate(ctx context.Context, botID int64, username string, u Update) error {
	if f.apply != nil {
		if err := f.apply(ctx, botID, username, u); err != nil {
			return err
		}
	}
	f.offset = u.ID + 1
	return nil
}

func TestPollerNeverAcknowledgesFailedUpdate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &fakeStore{}
	failed := false
	var applied, offsets []int64
	store.apply = func(ctx context.Context, botID int64, name string, u Update) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("database operation missing deadline")
		}
		if botID != 123 || name != "PayplumpBot" {
			t.Error("wrong bot identity")
		}
		if u.ID == 10 && !failed {
			failed = true
			return errors.New("database temporarily unavailable")
		}
		applied = append(applied, u.ID)
		return nil
	}
	api := fakeAPI{updates: func(ctx context.Context, offset int64) ([]Update, error) {
		offsets = append(offsets, offset)
		switch len(offsets) {
		case 1:
			return []Update{{ID: 9}, {ID: 10}}, nil
		case 2:
			return []Update{{ID: 10}}, nil
		default:
			cancel()
			return nil, ctx.Err()
		}
	}}
	p := NewPoller(api, store, nil)
	p.wait = func(context.Context, time.Duration) error { return nil }
	if err := p.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(offsets, []int64{0, 10, 11}) || !reflect.DeepEqual(applied, []int64{9, 10}) {
		t.Fatalf("offsets %v, applied %v", offsets, applied)
	}
	// After restart, a new poller reads the committed checkpoint before fetching.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	api.updates = func(ctx context.Context, offset int64) ([]Update, error) {
		if offset != 11 {
			t.Error("restart did not resume persisted offset")
		}
		cancel2()
		return nil, ctx.Err()
	}
	if err := NewPoller(api, store, nil).Run(ctx2); err != nil {
		t.Fatal(err)
	}
}

func TestPollerHonorsRetryAfterAndRecovers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	meCalls, updateCalls := 0, 0
	api := fakeAPI{
		me: func(context.Context) (User, error) {
			meCalls++
			if meCalls == 1 {
				return User{}, &APIError{HTTPStatus: 429, Code: 429, RetryAfter: 17 * time.Second}
			}
			return User{ID: 123, Username: "PayplumpBot"}, nil
		},
		updates: func(ctx context.Context, _ int64) ([]Update, error) {
			updateCalls++
			if updateCalls == 1 {
				return nil, &APIError{HTTPStatus: 429, Code: 429, RetryAfter: 7 * time.Second}
			}
			cancel()
			return nil, ctx.Err()
		},
	}
	var delays []time.Duration
	p := NewPoller(api, &fakeStore{}, nil)
	p.wait = func(_ context.Context, delay time.Duration) error { delays = append(delays, delay); return nil }
	if err := p.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(delays, []time.Duration{17 * time.Second, 7 * time.Second}) {
		t.Fatalf("retry delays: %v", delays)
	}
}

func TestPollerStopsOnPermanentErrors(t *testing.T) {
	for _, code := range []int{401, 403, 409} {
		apiErr := &APIError{HTTPStatus: code, Code: code}
		api := fakeAPI{updates: func(context.Context, int64) ([]Update, error) { return nil, apiErr }}
		p := NewPoller(api, &fakeStore{}, nil)
		if err := p.Run(context.Background()); !errors.Is(err, apiErr) {
			t.Fatalf("code %d: %v", code, err)
		}
		api.me = func(context.Context) (User, error) { return User{}, apiErr }
		if err := NewPoller(api, &fakeStore{}, nil).Run(context.Background()); !errors.Is(err, apiErr) {
			t.Fatalf("getMe code %d: %v", code, err)
		}
	}
}

func TestPollerRetriesOffsetReadAndCancelsBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lookupCalls := 0
	store := &fakeStore{lookup: func(context.Context, int64) (int64, error) { lookupCalls++; return 0, errors.New("offline") }}
	p := NewPoller(fakeAPI{}, store, nil)
	p.wait = func(ctx context.Context, _ time.Duration) error { cancel(); return waitForRetry(ctx, time.Hour) }
	if err := p.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if lookupCalls != 1 {
		t.Fatal("continued querying during cancelled backoff")
	}
}
