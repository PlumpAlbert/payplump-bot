package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// SyncImportedEffectPermission changes only the separate imported effects gate.
func (s *Store) SyncImportedEffectPermission(ctx context.Context, botID, revision int64, policyHash, sourceScopeHash string, enabled bool) error {
	ph, err := importDigest(policyHash)
	if err != nil {
		return errors.New("invalid imported effect permission")
	}
	sh, err := importDigest(sourceScopeHash)
	if err != nil || botID <= 0 || revision <= 0 {
		return errors.New("invalid imported effect permission")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return errors.New("could not begin imported effect permission update")
	}
	defer rollback(tx)
	if _, err = tx.Exec(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		return errors.New("could not require durable imported effect permission")
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || ':import-effect-permission:' || $1::bigint::text,0))`, botID); err != nil {
		return errors.New("could not serialize imported effect permission update")
	}
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM telegram_bots WHERE bot_id=$1)`, botID).Scan(&exists); err != nil || !exists {
		return errors.New("imported effect permission bot is unavailable")
	}
	var currentRevision int64
	var currentPolicy, currentScope []byte
	var currentEnabled bool
	err = tx.QueryRow(ctx, `SELECT revision,policy_hash,source_scope_hash,enabled FROM imported_effect_permissions WHERE bot_id=$1 FOR UPDATE`, botID).Scan(&currentRevision, &currentPolicy, &currentScope, &currentEnabled)
	if errors.Is(err, pgx.ErrNoRows) {
		_, err = tx.Exec(ctx, `INSERT INTO imported_effect_permissions(bot_id,revision,policy_hash,source_scope_hash,enabled) VALUES($1,$2,$3,$4,$5)`, botID, revision, ph, sh, enabled)
	} else if err != nil {
		return errors.New("could not read imported effect permission")
	} else {
		if revision < currentRevision || (revision == currentRevision && (!equalImportBytes(ph, currentPolicy) || !equalImportBytes(sh, currentScope) || (!currentEnabled && enabled))) {
			return errors.New("imported effect permission revision conflict")
		}
		if _, err = tx.Exec(ctx, `UPDATE imported_effect_permissions SET revision=$2,policy_hash=$3,source_scope_hash=$4,enabled=$5,updated_at=clock_timestamp() WHERE bot_id=$1`, botID, revision, ph, sh, enabled); err != nil {
			return errors.New("could not update imported effect permission")
		}
	}
	if err != nil {
		return errors.New("could not save imported effect permission")
	}
	if err = tx.Commit(ctx); err != nil {
		return errors.New("could not commit imported effect permission")
	}
	return nil
}
