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
		return nil, errors.New("managed client scope unavailable")
	}
	defer rows.Close()
	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if rows.Scan(&id) != nil {
			return nil, errors.New("invalid managed client scope")
		}
		ids = append(ids, id)
	}
	if rows.Err() != nil || len(ids) > 1000 {
		return nil, errors.New("managed client scope incomplete or exceeds supported inventory bound")
	}
	return ids, nil
}
