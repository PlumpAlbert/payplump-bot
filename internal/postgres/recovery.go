package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

type RecoveryReport struct {
	FeatureVersion            string             `json:"feature_version"`
	Counts                    []RecoveryCount    `json:"counts_by_kind_outcome"`
	Operation                 *RecoveryOperation `json:"operation,omitempty"`
	Guard                     RecoveryGuard      `json:"activation_write_guard"`
	ProtectedUnknown          []ProtectedUnknown `json:"protected_configuration_unknown"`
	ProtectedUnknownTotal     int64              `json:"protected_configuration_unknown_total"`
	ProtectedUnknownTruncated bool               `json:"protected_configuration_unknown_truncated"`
	UnresolvedKnownReceipts   int                `json:"unresolved_known_receipts"`
}
type RecoveryCount struct {
	Kind    string `json:"kind"`
	Outcome string `json:"outcome"`
	Count   int64  `json:"count"`
}
type RecoveryOperation struct {
	LocalID    int64  `json:"local_operation_id"`
	Kind       string `json:"kind"`
	Status     string `json:"status"`
	Attempt    int    `json:"attempt"`
	ReviewCode int    `json:"review_code"`
}
type RecoveryGuard struct {
	Epoch       int64   `json:"epoch"`
	OperationID *int64  `json:"owner_operation_id"`
	Step        *string `json:"owner_step"`
}
type ProtectedUnknown struct {
	LocalOutboxID int64 `json:"local_outbox_id"`
	Attempt       int   `json:"attempt"`
}

// RecoverySnapshot uses a read-only transaction and never attempts lease recovery.
func (s *Store) RecoverySnapshot(ctx context.Context, operationID int64) (RecoveryReport, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return RecoveryReport{}, errors.New("could not begin recovery report")
	}
	defer rollback(tx)
	var present int
	err = tx.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name=ANY($1::text[])`, []string{"operations", "operation_steps", "registration_flows", "activation_write_guard", "activation_steps", "outbox", "schema_migrations"}).Scan(&present)
	if err != nil || present != 7 {
		return RecoveryReport{}, errors.New("recovery report requires a recognized database schema")
	}
	var latest string
	if err = tx.QueryRow(ctx, `SELECT version FROM schema_migrations WHERE version='009_protected_configuration.sql'`).Scan(&latest); err != nil {
		return RecoveryReport{}, errors.New("recovery report requires schema feature 009")
	}
	r := RecoveryReport{FeatureVersion: latest, Counts: []RecoveryCount{}, ProtectedUnknown: []ProtectedUnknown{}}
	var has015 bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version='015_invoice_overdue_enforcement.sql')`).Scan(&has015); err != nil {
		return r, errors.New("could not inspect recovery schema features")
	}
	knownKinds := "('register_client','activate_first_subscription')"
	if has015 {
		knownKinds = "('register_client','activate_first_subscription','invoice_overdue_enforcement')"
	}
	var has016Migration bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version='016_invoice_paid_renewal.sql')`).Scan(&has016Migration); err != nil {
		return r, errors.New("could not inspect recovery schema features")
	}
	var renewalTables int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name=ANY($1::text[])`, []string{"invoice_renewal_actions", "invoice_renewal_steps"}).Scan(&renewalTables); err != nil {
		return r, errors.New("could not inspect recovery schema features")
	}
	has016 := has016Migration || renewalTables > 0
	if has016 && (!has016Migration || renewalTables != 2 || !has015) {
		return r, errors.New("recovery report requires a complete schema feature 016")
	}
	if has016 {
		knownKinds = "('register_client','activate_first_subscription','invoice_overdue_enforcement','invoice_paid_renewal')"
	}
	rows, err := tx.Query(ctx, `SELECT CASE WHEN kind IN `+knownKinds+` THEN kind ELSE 'other' END,
                       CASE WHEN status IN ('pending','running','completed','failed') THEN status ELSE 'other' END,
                       count(*) FROM operations GROUP BY 1,2 ORDER BY 1,2`)
	if err != nil {
		return r, errors.New("could not read operation counts")
	}
	for rows.Next() {
		var x RecoveryCount
		if err = rows.Scan(&x.Kind, &x.Outcome, &x.Count); err != nil {
			rows.Close()
			return r, errors.New("could not read operation counts")
		}
		r.Counts = append(r.Counts, x)
	}
	rows.Close()
	if rows.Err() != nil {
		return r, errors.New("could not read operation counts")
	}
	if operationID > 0 {
		var op RecoveryOperation
		operationQuery := `SELECT o.id,CASE WHEN o.kind IN ` + knownKinds + ` THEN o.kind ELSE 'other' END,
			CASE WHEN o.status IN ('pending','running','completed','failed') THEN o.status ELSE 'other' END,
			o.attempts,COALESCE(r.review_code,a.review_code,0) FROM operations o
			LEFT JOIN registration_flows r ON r.operation_id=o.id
			LEFT JOIN activation_flows a ON a.operation_id=o.id WHERE o.id=$1`
		if has015 {
			operationQuery = `SELECT o.id,CASE WHEN o.kind IN ` + knownKinds + ` THEN o.kind ELSE 'other' END,
				CASE WHEN o.status IN ('pending','running','completed','failed') THEN o.status ELSE 'other' END,
				o.attempts,COALESCE(r.review_code,a.review_code,v.review_code,0) FROM operations o
				LEFT JOIN registration_flows r ON r.operation_id=o.id
				LEFT JOIN activation_flows a ON a.operation_id=o.id
				LEFT JOIN invoice_overdue_actions v ON v.operation_id=o.id WHERE o.id=$1`
		}
		if has016 {
			operationQuery = `SELECT o.id,CASE WHEN o.kind IN ` + knownKinds + ` THEN o.kind ELSE 'other' END,
				CASE WHEN o.status IN ('pending','running','completed','failed') THEN o.status ELSE 'other' END,
				o.attempts,COALESCE(r.review_code,a.review_code,v.review_code,n.review_code,0) FROM operations o
				LEFT JOIN registration_flows r ON r.operation_id=o.id
				LEFT JOIN activation_flows a ON a.operation_id=o.id
				LEFT JOIN invoice_overdue_actions v ON v.operation_id=o.id
				LEFT JOIN invoice_renewal_actions n ON n.operation_id=o.id WHERE o.id=$1`
		}
		err = tx.QueryRow(ctx, operationQuery, operationID).Scan(&op.LocalID, &op.Kind, &op.Status, &op.Attempt, &op.ReviewCode)
		if err != nil {
			return r, errors.New("requested local operation was not found")
		}
		r.Operation = &op
	}
	var guard RecoveryGuard
	guardSteps := "'sui_create','recurring_create','sui_enable','recurring_start','customer_create','invoice_create','invoice_issue','renewal_create','renewal_start','cancel','pause','resume','notify'"
	if has015 {
		guardSteps += ",'pause_recurring','disable_sui','close_sessions'"
	}
	if has016 {
		guardSteps += ",'schedule_recurring','enable_sui','resume_recurring'"
	}
	err = tx.QueryRow(ctx, `SELECT epoch,operation_id,
		CASE WHEN step IS NULL THEN NULL WHEN step IN (`+guardSteps+`) THEN step ELSE 'other' END
		FROM activation_write_guard WHERE id=1`).Scan(&guard.Epoch, &guard.OperationID, &guard.Step)
	if err != nil {
		return r, errors.New("could not read activation guard")
	}
	r.Guard = guard
	has010 := false
	if e := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version='010_shared_external_dispatch.sql')`).Scan(&has010); e != nil {
		return r, errors.New("could not inspect recovery schema features")
	}
	if has010 {
		if e := tx.QueryRow(ctx, `SELECT count(*) FROM configuration_dispatches WHERE outcome='unknown'`).Scan(&r.ProtectedUnknownTotal); e != nil {
			return r, errors.New("could not count protected delivery status")
		}
	}
	if has010 {
		rows, err = tx.Query(ctx, `SELECT o.id,d.attempt FROM configuration_dispatches d JOIN outbox o ON o.id=d.outbox_id WHERE d.outcome='unknown' ORDER BY o.id LIMIT 251`)
		if err != nil {
			return r, errors.New("could not read protected delivery status")
		}
		for rows.Next() {
			var x ProtectedUnknown
			if err = rows.Scan(&x.LocalOutboxID, &x.Attempt); err != nil {
				rows.Close()
				return r, errors.New("could not read protected delivery status")
			}
			if len(r.ProtectedUnknown) < 250 {
				r.ProtectedUnknown = append(r.ProtectedUnknown, x)
			}
		}
		rows.Close()
		if rows.Err() != nil {
			return r, errors.New("could not read protected delivery status")
		}
	}
	r.ProtectedUnknownTruncated = r.ProtectedUnknownTotal > int64(len(r.ProtectedUnknown))
	receiptsQuery := `SELECT count(*) FROM (
		SELECT s.operation_id FROM operation_steps s JOIN operations o ON o.id=s.operation_id WHERE s.external_id IS NOT NULL AND o.status <> 'completed'
		UNION ALL
		SELECT s.operation_id FROM activation_steps s JOIN operations o ON o.id=s.operation_id WHERE s.external_id IS NOT NULL AND o.status <> 'completed'
	) known_receipts`
	if has015 {
		receiptsQuery = `SELECT count(*) FROM (
			SELECT s.operation_id FROM operation_steps s JOIN operations o ON o.id=s.operation_id WHERE s.external_id IS NOT NULL AND o.status <> 'completed'
			UNION ALL
			SELECT s.operation_id FROM activation_steps s JOIN operations o ON o.id=s.operation_id WHERE s.external_id IS NOT NULL AND o.status <> 'completed'
			UNION ALL
			SELECT s.operation_id FROM invoice_overdue_steps s JOIN operations o ON o.id=s.operation_id WHERE s.external_id IS NOT NULL AND o.status <> 'completed'
		) known_receipts`
	}
	if has016 {
		receiptsQuery = `SELECT count(*) FROM (
			SELECT s.operation_id FROM operation_steps s JOIN operations o ON o.id=s.operation_id WHERE s.external_id IS NOT NULL AND o.status <> 'completed'
			UNION ALL
			SELECT s.operation_id FROM activation_steps s JOIN operations o ON o.id=s.operation_id WHERE s.external_id IS NOT NULL AND o.status <> 'completed'
			UNION ALL
			SELECT s.operation_id FROM invoice_overdue_steps s JOIN operations o ON o.id=s.operation_id WHERE s.external_id IS NOT NULL AND o.status <> 'completed'
			UNION ALL
			SELECT s.operation_id FROM invoice_renewal_steps s JOIN operations o ON o.id=s.operation_id WHERE s.external_id IS NOT NULL AND o.status <> 'completed'
		) known_receipts`
	}
	if err = tx.QueryRow(ctx, receiptsQuery).Scan(&r.UnresolvedKnownReceipts); err != nil {
		return r, errors.New("could not count unresolved receipts")
	}
	if err = tx.Commit(ctx); err != nil {
		return r, fmt.Errorf("could not finish recovery report")
	}
	return r, nil
}
