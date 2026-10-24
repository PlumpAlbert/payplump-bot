package managedscope

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type reader struct {
	ids []int64
	err error
	bot int64
}

func (r *reader) ManagedTelegramIDs(_ context.Context, bot int64) ([]int64, error) {
	r.bot = bot
	return r.ids, r.err
}
func TestScopeLoadsOnlyTheRequestedBotAndAppliesExclusions(t *testing.T) {
	r := &reader{ids: []int64{11, 22, 33}}
	got, e := Load(context.Background(), r, 9, true, nil, []int64{22})
	if e != nil || r.bot != 9 || !reflect.DeepEqual(got, []int64{11, 33}) {
		t.Fatal("incorrect managed scope")
	}
}
func TestMissingOrFailedManagedReaderDoesNotFallBackToAllActors(t *testing.T) {
	for _, store := range []any{struct{}{}, &reader{err: errors.New("synthetic")}} {
		if _, e := Load(context.Background(), store, 9, true, nil, nil); e == nil {
			t.Fatal("missing managed scope accepted")
		}
	}
}
func TestActorSelectionNeverReturnsNilForDeniedActor(t *testing.T) {
	for _, actor := range []int64{-1, 0, 22} {
		ids := ForActor(actor, true, nil, []int64{22})
		if ids == nil || len(ids) != 0 {
			t.Fatal("denied actor returned unrestricted nil scope")
		}
	}
	if got := ForActor(11, true, nil, nil); !reflect.DeepEqual(got, []int64{11}) {
		t.Fatal("managed write did not retain exact actor scope")
	}
	if Allows(11, false, []int64{22}, nil) {
		t.Fatal("legacy allowlist broadened")
	}
}
func TestLegacyScopeDoesNotRequireManagedReader(t *testing.T) {
	got, e := Load(context.Background(), struct{}{}, 9, false, []int64{11, 22}, []int64{22})
	if e != nil || !reflect.DeepEqual(got, []int64{11}) {
		t.Fatal("legacy scope changed")
	}
}
