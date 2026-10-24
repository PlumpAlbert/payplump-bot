// Package managedscope selects application-owned client actors. Selection is
// not entitlement: stores still validate bot, provenance, bindings and payment.
package managedscope

import (
	"context"
	"errors"
	"slices"
)

// Reader returns only private client identities associated with this bot.
type Reader interface {
	ManagedTelegramIDs(context.Context, int64) ([]int64, error)
}

func Allows(actor int64, managed bool, allowed, excluded []int64) bool {
	return actor > 0 && (managed || slices.Contains(allowed, actor)) && !slices.Contains(excluded, actor)
}

func ForActor(actor int64, managed bool, allowed, excluded []int64) []int64 {
	if !Allows(actor, managed, allowed, excluded) {
		return []int64{}
	}
	if managed {
		return []int64{actor}
	}
	return Filter(allowed, excluded)
}

func Filter(ids, excluded []int64) []int64 {
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id > 0 && !slices.Contains(excluded, id) {
			out = append(out, id)
		}
	}
	return out
}

func Load(ctx context.Context, store any, bot int64, managed bool, allowed, excluded []int64) ([]int64, error) {
	if !managed {
		return Filter(allowed, excluded), nil
	}
	reader, ok := store.(Reader)
	if !ok {
		return nil, errors.New("managed client scope reader unavailable")
	}
	ids, err := reader.ManagedTelegramIDs(ctx, bot)
	if err != nil {
		return nil, err
	}
	return Filter(ids, excluded), nil
}
