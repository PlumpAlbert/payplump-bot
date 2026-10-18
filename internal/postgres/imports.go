package postgres

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"

	"github.com/PlumpAlbert/payplump-bot/internal/importplan"
	"github.com/jackc/pgx/v5"
)

// StageImport persists private candidate facts only. It does not create clients,
// bindings, receipts, operations, outbox rows, or any capability to activate.
func (s *Store) StageImport(ctx context.Context, p importplan.Plan, expectedHash string) (bool, error) {
	if err := p.ValidateHash(expectedHash); err != nil {
		return false, err
	}
	hash, _ := hex.DecodeString(p.Hash)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, errors.New("could not begin import staging")
	}
	defer rollback(tx)
	lockKey := int64(binary.BigEndian.Uint64(hash[:8]))
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
		return false, errors.New("could not serialize import staging")
	}
	if _, err = tx.Exec(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		return false, errors.New("could not require durable import staging")
	}
	var total, count int
	err = tx.QueryRow(ctx, `SELECT total_records,candidate_count FROM import_batches WHERE plan_hash=$1 FOR UPDATE`, hash).Scan(&total, &count)
	if err == nil {
		if total != p.Total || count != len(p.Candidates) {
			return false, errors.New("staging hash conflict")
		}
		rows, e := tx.Query(ctx, `SELECT source_index,candidate_facts FROM import_candidates_staging WHERE plan_hash=$1 ORDER BY source_index`, hash)
		if e != nil {
			return false, errors.New("could not verify existing staged plan")
		}
		existing := map[int][]byte{}
		for rows.Next() {
			var i int
			var raw []byte
			if e = rows.Scan(&i, &raw); e != nil {
				rows.Close()
				return false, errors.New("could not verify existing staged plan")
			}
			existing[i] = raw
		}
		rows.Close()
		if rows.Err() != nil {
			return false, errors.New("could not verify existing staged plan")
		}
		if len(existing) != len(p.Candidates) {
			return false, errors.New("staging hash conflict")
		}
		for _, candidate := range p.Candidates {
			raw, ok := existing[candidate.Index]
			if !ok {
				return false, errors.New("staging hash conflict")
			}
			actual, decodeErr := importplan.DecodePrivateRecord(raw)
			if decodeErr != nil || actual != candidate.Facts {
				return false, errors.New("staging hash conflict")
			}
		}
		if err = tx.Commit(ctx); err != nil {
			return false, errors.New("could not confirm existing staged plan")
		}
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, errors.New("could not check staged plan")
	}
	if _, err = tx.Exec(ctx, `INSERT INTO import_batches(plan_hash,total_records,candidate_count) VALUES($1,$2,$3)`, hash, p.Total, len(p.Candidates)); err != nil {
		return false, errors.New("could not stage import batch")
	}
	for _, candidate := range p.Candidates {
		facts, e := candidate.Facts.PrivateJSON()
		if e != nil {
			return false, errors.New("could not encode staged candidate")
		}
		if _, err = tx.Exec(ctx, `INSERT INTO import_candidates_staging(plan_hash,source_index,candidate_facts) VALUES($1,$2,$3::jsonb)`, hash, candidate.Index, facts); err != nil {
			return false, errors.New("could not stage import candidates")
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return false, errors.New("could not commit import staging")
	}
	return false, nil
}
