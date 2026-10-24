package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"
)

var ErrServicePolicyRevoked = errors.New("service policy was revoked; explicit operator review is required")

// A restart never re-enables a durable revocation. A changed enabled policy
// advances monotonically, leaving old requests bound to their original version.
func servicePolicyRevision(found, enabled bool, revision int64, same bool) (int64, error) {
	if !found {
		return 1, nil
	}
	if !enabled {
		return 0, ErrServicePolicyRevoked
	}
	if revision < 1 {
		return 0, errors.New("invalid stored service policy revision")
	}
	if same {
		return revision, nil
	}
	if revision == math.MaxInt64 {
		return 0, errors.New("service policy revision exhausted")
	}
	return revision + 1, nil
}

// InstallServiceImportPolicies preserves the three independent durable gates.
// Partial grants may persist on failure, just as in explicit operator setup;
// no worker is started until all three succeed. Unknown write guards are never
// touched here. Existing immutable origins cannot silently change policy.
func (s *Store) InstallServiceImportPolicies(ctx context.Context, botID int64, policyHash, scopeHash string) (int64, int64, int64, error) {
	ph, e := importDigest(policyHash)
	if e != nil {
		return 0, 0, 0, e
	}
	sh, e := importDigest(scopeHash)
	if e != nil || botID <= 0 {
		return 0, 0, 0, errors.New("invalid service import policy")
	}
	var mismatch bool
	e = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM verified_import_origins WHERE bot_id=$1 AND (policy_hash<>$2 OR source_scope_hash<>$3))`, botID, ph, sh).Scan(&mismatch)
	if e != nil {
		return 0, 0, 0, errors.New("could not inspect existing import policy")
	}
	if mismatch {
		return 0, 0, 0, errors.New("existing imported clients use a different policy; explicit verified migration is required")
	}
	a, e := s.installServiceImportGate(ctx, botID, "admission", ph, sh)
	if e != nil {
		return 0, 0, 0, e
	}
	r, e := s.installServiceImportGate(ctx, botID, "read", ph, sh)
	if e != nil {
		return a, 0, 0, e
	}
	w, e := s.installServiceImportGate(ctx, botID, "effects", ph, sh)
	return a, r, w, e
}

func (s *Store) installServiceImportGate(ctx context.Context, botID int64, gate string, ph, sh []byte) (int64, error) {
	var table, lock, global string
	switch gate {
	case "admission":
		table = "import_admission_policy"
		lock = "import-admission-policy:"
	case "read":
		table = "imported_runtime_permissions"
		lock = "import-runtime-permission:"
		global = "import-runtime-permissions-global"
	case "effects":
		table = "imported_effect_permissions"
		lock = "import-effect-permission:"
		global = "import-effect-permissions-global"
	default:
		return 0, errors.New("invalid service import gate")
	}
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		return 0, errors.New("service import policy unavailable")
	}
	defer rollback(tx)
	if global != "" {
		if _, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || ':' || $1,0))`, global); e != nil {
			return 0, errors.New("could not serialize service import gate")
		}
	}
	if _, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || ':' || $1 || $2::bigint::text,0))`, lock, botID); e != nil {
		return 0, errors.New("could not serialize service import policy")
	}
	var rev int64
	var enabled bool
	var oldPH, oldSH []byte
	e = tx.QueryRow(ctx, fmt.Sprintf(`SELECT revision,enabled,policy_hash,source_scope_hash FROM %s WHERE bot_id=$1 FOR UPDATE`, table), botID).Scan(&rev, &enabled, &oldPH, &oldSH)
	found := e == nil
	if e != nil && !errors.Is(e, pgx.ErrNoRows) {
		return 0, errors.New("could not read service import policy")
	}
	if gate == "admission" {
		var mismatch bool
		e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM verified_import_origins WHERE bot_id=$1 AND (policy_hash<>$2 OR source_scope_hash<>$3))`, botID, ph, sh).Scan(&mismatch)
		if e != nil || mismatch {
			return 0, errors.New("existing import origin requires explicit verified policy migration")
		}
	}
	rev, e = servicePolicyRevision(found, enabled, rev, equalImportBytes(ph, oldPH) && equalImportBytes(sh, oldSH))
	if e != nil {
		return 0, e
	}
	if _, e = tx.Exec(ctx, `SET LOCAL synchronous_commit=on`); e != nil {
		return 0, errors.New("could not require durable service import policy")
	}
	_, e = tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s(bot_id,revision,policy_hash,source_scope_hash,enabled) VALUES($1,$2,$3,$4,true) ON CONFLICT(bot_id) DO UPDATE SET revision=excluded.revision,policy_hash=excluded.policy_hash,source_scope_hash=excluded.source_scope_hash,enabled=true,updated_at=clock_timestamp()`, table), botID, rev, ph, sh)
	if e != nil {
		return 0, errors.New("could not save service import policy")
	}
	if tx.Commit(ctx) != nil {
		return 0, errors.New("could not commit service import policy")
	}
	return rev, nil
}
