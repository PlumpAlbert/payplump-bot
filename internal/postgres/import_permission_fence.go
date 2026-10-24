package postgres

import (
	"context"
	"errors"
)

// DisableImportedRuntimePermissions durably closes existing runtime gates before
// read-only startup. It never creates rows or changes their policy identity.
func (s *Store) DisableImportedRuntimePermissions(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return errors.New("could not begin imported runtime permission fence")
	}
	defer rollback(tx)
	if _, err = tx.Exec(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		return errors.New("could not require durable imported runtime permission fence")
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || ':import-runtime-permissions-global',0))`); err != nil {
		return errors.New("could not serialize imported runtime permission fence")
	}
	if _, err = tx.Exec(ctx, `UPDATE imported_runtime_permissions SET revision=revision+1,enabled=false,updated_at=clock_timestamp() WHERE enabled`); err != nil {
		return errors.New("could not disable imported runtime permissions")
	}
	if err = tx.Commit(ctx); err != nil {
		return errors.New("could not commit imported runtime permission fence")
	}
	return nil
}

// DisableImportedEffectPermissions durably closes existing effect gates during
// startup, including when read-only runtime permission is later enabled.
func (s *Store) DisableImportedEffectPermissions(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return errors.New("could not begin imported effect permission fence")
	}
	defer rollback(tx)
	if _, err = tx.Exec(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		return errors.New("could not require durable imported effect permission fence")
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || ':import-effect-permissions-global',0))`); err != nil {
		return errors.New("could not serialize imported effect permission fence")
	}
	if _, err = tx.Exec(ctx, `UPDATE imported_effect_permissions SET revision=revision+1,enabled=false,updated_at=clock_timestamp() WHERE enabled`); err != nil {
		return errors.New("could not disable imported effect permissions")
	}
	if err = tx.Commit(ctx); err != nil {
		return errors.New("could not commit imported effect permission fence")
	}
	return nil
}
