//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/registration"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type registrationQueryTrace struct {
	start func(context.Context, pgx.TraceQueryStartData)
}

func (t *registrationQueryTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	t.start(ctx, data)
	return ctx
}
func (*registrationQueryTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestRegistrationPermissionOverridesAsyncPooledCommit(t *testing.T) {
	s, ctx := testStore(t)
	op := seedRegistrationWork(t, s, ctx, 42001)
	seedRegistrationFlow(t, s, ctx, op.ID)

	var mu sync.Mutex
	var statements []string
	trace := &registrationQueryTrace{start: func(_ context.Context, data pgx.TraceQueryStartData) {
		// Do not issue recursive queries from a pgx trace callback.
		mu.Lock()
		defer mu.Unlock()
		statements = append(statements, strings.TrimSpace(strings.ToLower(data.SQL)))
	}}
	cfg := s.pool.Config()
	cfg.MaxConns, cfg.MinConns = 1, 0
	cfg.ConnConfig.RuntimeParams["synchronous_commit"] = "off"
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `SET synchronous_commit = off`); err != nil {
		t.Fatal(err)
	}
	assertAsync := func() {
		t.Helper()
		var setting string
		if err := pool.QueryRow(ctx, `SELECT current_setting('synchronous_commit')`).Scan(&setting); err != nil || setting != "off" {
			t.Fatalf("fixture did not retain session async setting: setting=%s err=%v", setting, err)
		}
	}
	assertAsync()
	payload := json.RawMessage(`{"customer":"synthetic"}`)
	if granted, err := (&Store{pool: pool}).PrepareRegistrationWrite(ctx, op.Lease, registration.CustomerCreate, payload); err != nil || !granted {
		t.Fatalf("durable permission granted=%v err=%v", granted, err)
	}
	assertAsync() // SET LOCAL must not leak into the next pooled transaction.

	mu.Lock()
	defer mu.Unlock()
	set, reservation, commit := -1, -1, -1
	for i, sql := range statements {
		switch {
		case sql == "set local synchronous_commit = on":
			set = i
		case strings.HasPrefix(sql, "insert into operation_steps"):
			reservation = i
		case sql == "commit":
			commit = i
		}
	}
	if set < 0 || reservation <= set || commit <= reservation {
		t.Fatal("acknowledged permission without a preceding local durable-commit override")
	}
}
