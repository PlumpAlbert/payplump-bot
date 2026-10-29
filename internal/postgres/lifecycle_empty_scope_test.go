package postgres

import (
	"context"
	"testing"
)

// A nil pool proves these calls do not access PostgreSQL for an empty scope.
func TestLifecycleSeedEmptyScopeIsNoop(t *testing.T) {
	store := &Store{}
	for _, allowed := range [][]int64{nil, {}} {
		t.Run("overdue", func(t *testing.T) {
			count, err := store.SeedOverdueOperations(context.Background(), 42, allowed, "2026-10-29", 1000)
			if err != nil || count != 0 {
				t.Fatalf("empty overdue scope: count=%d err=%v", count, err)
			}
		})
		t.Run("renewal", func(t *testing.T) {
			count, err := store.SeedRenewalOperations(context.Background(), 42, allowed, 1000)
			if err != nil || count != 0 {
				t.Fatalf("empty renewal scope: count=%d err=%v", count, err)
			}
		})
	}
}

func TestLifecycleSeedEmptyScopeStillValidatesArguments(t *testing.T) {
	store := &Store{}
	for _, tc := range []struct {
		name  string
		bot   int64
		limit int
	}{
		{"invalid bot", 0, 1000},
		{"invalid limit", 42, 0},
		{"excessive limit", 42, 1001},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.SeedOverdueOperations(context.Background(), tc.bot, nil, "2026-10-29", tc.limit); err == nil {
				t.Fatal("invalid overdue arguments accepted")
			}
			if _, err := store.SeedRenewalOperations(context.Background(), tc.bot, nil, tc.limit); err == nil {
				t.Fatal("invalid renewal arguments accepted")
			}
		})
	}
	if _, err := store.SeedOverdueOperations(context.Background(), 42, nil, "not-a-date", 1000); err == nil {
		t.Fatal("invalid overdue date accepted")
	}
}
