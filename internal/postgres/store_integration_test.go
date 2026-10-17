//go:build integration

package postgres

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	s, ctx := unmigratedStore(t)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return s, ctx
}

func unmigratedStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	cfg, err := connectionConfig(url, 4)
	if err != nil {
		t.Fatal("integration tests require TEST_DATABASE_URL or PGHOST, PGUSER and PGDATABASE for a disposable database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	admin, err := pgx.ConnectConfig(ctx, cfg.ConnConfig.Copy())
	if err != nil {
		t.Fatal("cannot connect to integration database")
	}
	schema := fmt.Sprintf("test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := admin.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE")
		if err != nil {
			t.Error(err)
		}
		_ = admin.Close(cleanup)
	})
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	// Fail before any migration or destructive test query if a transaction
	// pooler ignores the connection's search_path.
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		var actual string
		if err := conn.QueryRow(ctx, `SELECT current_schema()`).Scan(&actual); err != nil {
			return err
		}
		if actual != schema {
			return fmt.Errorf("integration schema isolation failed: expected %q, got %q; use a direct PostgreSQL connection", schema, actual)
		}
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := &Store{pool: pool}
	t.Cleanup(s.Close)
	return s, ctx
}

func TestConcurrentRegistrationIsDurableAndIdempotent(t *testing.T) {
	s, ctx := testStore(t)
	identity := TelegramIdentity{TelegramID: 4503599627370497, ChatID: 4503599627370497, DisplayName: "Клиент"}
	var wg sync.WaitGroup
	var created atomic.Int32
	var clientID atomic.Int64
	for range 20 {
		wg.Go(func() {
			r, err := s.RegisterPrivateClient(ctx, identity)
			if err != nil {
				t.Error(err)
				return
			}
			if r.Created {
				created.Add(1)
			}
			clientID.CompareAndSwap(0, r.Client.ID)
			if r.Client.ID != clientID.Load() || r.Client.TelegramID != identity.TelegramID {
				t.Error("identity changed across concurrent registrations")
			}
		})
	}
	wg.Wait()
	if created.Load() != 1 {
		t.Fatalf("created %d subscriptions", created.Load())
	}
	for _, table := range []string{"clients", "subscriptions", "operations"} {
		var count int
		if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("%s: %d records", table, count)
		}
	}
	var state string
	var access bool
	if err := s.pool.QueryRow(ctx, `SELECT state, desired_access FROM subscriptions`).Scan(&state, &access); err != nil {
		t.Fatal(err)
	}
	if state != "awaiting_payment" || access {
		t.Fatal("registration granted access before payment")
	}
	// Completed work must not be recreated by another /start, even after reconnect.
	if _, err := s.pool.Exec(ctx, `UPDATE operations SET status = 'completed', completed_at = now()`); err != nil {
		t.Fatal(err)
	}
	s.pool.Reset()
	identity.DisplayName = "Новое имя"
	r, err := s.RegisterPrivateClient(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	if r.Created || r.Client.ID != clientID.Load() || r.Client.DisplayName != identity.DisplayName {
		t.Fatalf("repeat registration: %+v", r)
	}
	var completed int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM operations WHERE status = 'completed'`).Scan(&completed); err != nil {
		t.Fatal(err)
	}
	if completed != 1 {
		t.Fatal("completed operation was lost or recreated")
	}
}

func TestRegistrationRollsBackWhenEnqueueFails(t *testing.T) {
	s, ctx := testStore(t)
	if _, err := s.pool.Exec(ctx, `ALTER TABLE operations ADD CONSTRAINT simulate_failure CHECK (false)`); err != nil {
		t.Fatal(err)
	}
	_, err := s.RegisterPrivateClient(ctx, TelegramIdentity{TelegramID: 42, ChatID: 42})
	if err == nil {
		t.Fatal("enqueue failure ignored")
	}
	for _, table := range []string{"clients", "subscriptions", "operations"} {
		var count int
		if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("partial registration left in %s", table)
		}
	}
}

func TestMigrationsRepeatAndDetectDrift(t *testing.T) {
	s, ctx := testStore(t)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if err := s.Migrate(ctx); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if _, err := s.pool.Exec(ctx, `UPDATE schema_migrations SET checksum = 'corrupted'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err == nil {
		t.Fatal("modified migration accepted")
	}
}

func TestMigrationsRejectUnknownVersion(t *testing.T) {
	s, ctx := testStore(t)
	if _, err := s.pool.Exec(ctx, `INSERT INTO schema_migrations (version, checksum) VALUES ('999_future.sql', 'unknown')`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err == nil {
		t.Fatal("newer schema accepted by older binary")
	}
}
