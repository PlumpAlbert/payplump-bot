package postgres

import (
	"context"
	"crypto/sha256"
	"embed"
	"fmt"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Migrate applies embedded migrations in filename order in a transaction.
// The advisory lock serializes concurrent starts. Applied checksums are immutable.
func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migrations: %w", err)
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7245196301)`); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
        version text PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now()
    )`); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	applied := make(map[string]string)
	for rows.Next() {
		var version, checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			rows.Close()
			return err
		}
		applied[version] = checksum
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		data, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		checksum := fmt.Sprintf("%x", sha256.Sum256(data))
		if previous, ok := applied[name]; ok {
			if previous != checksum {
				return fmt.Errorf("migration checksum mismatch: %s", name)
			}
			delete(applied, name)
			continue
		}
		if _, err := tx.Exec(ctx, string(data)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, checksum) VALUES ($1, $2)`, name, checksum); err != nil {
			return err
		}
	}
	if len(applied) != 0 {
		return fmt.Errorf("database contains migrations unknown to this binary")
	}
	return tx.Commit(ctx)
}
