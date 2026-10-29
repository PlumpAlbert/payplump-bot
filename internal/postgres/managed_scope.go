package postgres

import (
	"context"
	"errors"
)

// ManagedTelegramIDs is a selection aid, never write authority. Downstream
// stores independently recheck immutable provenance, identity and entitlement.
func (s *Store) ManagedTelegramIDs(ctx context.Context, botID int64) ([]int64, error) {
	if botID <= 0 {
		return nil, errors.New("invalid managed bot identity")
	}
	rows, err := s.pool.Query(ctx, `SELECT c.telegram_id FROM clients c WHERE c.chat_id=c.telegram_id AND (
 EXISTS(SELECT 1 FROM operations o JOIN registration_flows f ON f.operation_id=o.id WHERE o.client_id=c.id AND f.bot_id=$1)
 OR EXISTS(SELECT 1 FROM verified_import_origins io WHERE io.client_id=c.id AND io.bot_id=$1)
 ) ORDER BY c.telegram_id LIMIT 1001`, botID)
	if err != nil {
		return nil, invoiceLifecycleStageError("managed client scope unavailable", err)
	}
	defer rows.Close()
	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, invoiceLifecycleStageError("invalid managed client scope", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, invoiceLifecycleStageError("managed client scope incomplete", err)
	}
	if len(ids) > 1000 {
		return nil, errors.New("managed client scope exceeds 1000 clients")
	}
	return ids, nil
}
