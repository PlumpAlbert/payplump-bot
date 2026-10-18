package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/PlumpAlbert/payplump-bot/internal/broadcast"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
	"github.com/jackc/pgx/v5"
)

var (
	ErrStalePolicy    = broadcast.ErrStalePolicy
	ErrInvalidCommand = broadcast.ErrInvalidCommand
	ErrExpired        = broadcast.ErrExpired
)

const broadcastPolicyLock = `SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || ':payplump-broadcast-policy',0))`

func (s *Store) InstallBroadcastPolicy(ctx context.Context, botID int64, p broadcast.Policy) error {
	if botID <= 0 || p.Validate() != nil {
		return ErrStalePolicy
	}
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		return errors.New("broadcast policy storage unavailable")
	}
	defer rollback(tx)
	if _, e = tx.Exec(ctx, broadcastPolicyLock); e != nil {
		return errors.New("broadcast policy storage unavailable")
	}
	if _, e = tx.Exec(ctx, `INSERT INTO telegram_bots(bot_id) VALUES($1) ON CONFLICT DO NOTHING`, botID); e != nil {
		return errors.New("broadcast policy storage unavailable")
	}
	if _, e = tx.Exec(ctx, `SELECT bot_id FROM telegram_bots WHERE bot_id=$1 FOR NO KEY UPDATE`, botID); e != nil {
		return errors.New("broadcast policy storage unavailable")
	}
	old, found, e := loadBroadcastPolicy(ctx, tx, botID)
	if e != nil {
		return errors.New("broadcast policy storage unavailable")
	}
	if found && !p.Enabled {
		if !old.Enabled {
			return tx.Commit(ctx)
		}
		if old.Revision == math.MaxInt64 {
			return ErrStalePolicy
		}
		_, e = tx.Exec(ctx, `UPDATE broadcast_policies SET revision=revision+1,enabled=false WHERE bot_id=$1`, botID)
	} else if found && old.Enabled && p.Revision == old.Revision && p.Equal(old) {
		return tx.Commit(ctx)
	} else if found && p.Revision <= old.Revision {
		return ErrStalePolicy
	} else {
		ids := append([]int64{}, p.AdminIDs...)
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		_, e = tx.Exec(ctx, `INSERT INTO broadcast_policies(bot_id,revision,policy_hash,enabled,max_recipients,admin_ids) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(bot_id) DO UPDATE SET revision=excluded.revision,policy_hash=excluded.policy_hash,enabled=excluded.enabled,max_recipients=excluded.max_recipients,admin_ids=excluded.admin_ids`, botID, p.Revision, p.Hash[:], p.Enabled, p.MaxRecipients, ids)
	}
	if e != nil {
		return errors.New("broadcast policy storage unavailable")
	}
	if _, e = tx.Exec(ctx, `SET LOCAL synchronous_commit=on`); e != nil {
		return errors.New("broadcast policy storage unavailable")
	}
	if tx.Commit(ctx) != nil {
		return errors.New("broadcast policy storage unavailable")
	}
	return nil
}
func loadBroadcastPolicy(ctx context.Context, tx pgx.Tx, bot int64) (broadcast.Policy, bool, error) {
	var p broadcast.Policy
	var hash []byte
	var ids []int64
	e := tx.QueryRow(ctx, `SELECT revision,policy_hash,enabled,max_recipients,admin_ids FROM broadcast_policies WHERE bot_id=$1`, bot).Scan(&p.Revision, &hash, &p.Enabled, &p.MaxRecipients, &ids)
	if errors.Is(e, pgx.ErrNoRows) {
		return p, false, nil
	}
	if e != nil {
		return p, false, e
	}
	if len(hash) != len(p.Hash) || len(ids) > 1000 {
		return p, false, errors.New("invalid stored broadcast policy")
	}
	for i, id := range ids {
		if id <= 0 || (i > 0 && ids[i-1] >= id) {
			return p, false, errors.New("invalid stored broadcast policy")
		}
	}
	copy(p.Hash[:], hash)
	p.AdminIDs = ids
	if p.Enabled && p.Validate() != nil {
		return p, false, errors.New("invalid stored broadcast policy")
	}
	if !p.Enabled && (p.Revision < 0 || p.MaxRecipients < 0 || p.MaxRecipients > 10000) {
		return p, false, errors.New("invalid stored broadcast policy")
	}
	return p, true, nil
}
func (s *Store) DisableBroadcastPolicies(ctx context.Context) error {
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		return errors.New("broadcast policy storage unavailable")
	}
	defer rollback(tx)
	if _, e = tx.Exec(ctx, broadcastPolicyLock); e != nil {
		return errors.New("broadcast policy storage unavailable")
	}
	rows, e := tx.Query(ctx, `SELECT bot_id FROM broadcast_policies WHERE enabled ORDER BY bot_id`)
	if e != nil {
		return errors.New("broadcast policy storage unavailable")
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) != nil {
			rows.Close()
			return errors.New("broadcast policy storage unavailable")
		}
		ids = append(ids, id)
	}
	if e = rows.Err(); e != nil {
		rows.Close()
		return errors.New("broadcast policy storage unavailable")
	}
	rows.Close()
	for _, id := range ids {
		if _, e = tx.Exec(ctx, `SELECT bot_id FROM telegram_bots WHERE bot_id=$1 FOR NO KEY UPDATE`, id); e != nil {
			return errors.New("broadcast policy storage unavailable")
		}
	}
	if _, e = tx.Exec(ctx, `SET LOCAL synchronous_commit=on`); e != nil {
		return errors.New("broadcast policy storage unavailable")
	}
	var overflow bool
	if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM broadcast_policies WHERE enabled AND revision=$1)`, int64(math.MaxInt64)).Scan(&overflow); e != nil || overflow {
		return ErrStalePolicy
	}
	if _, e = tx.Exec(ctx, `UPDATE broadcast_policies SET revision=revision+1,enabled=false WHERE enabled`); e != nil {
		return errors.New("broadcast policy storage unavailable")
	}
	if tx.Commit(ctx) != nil {
		return errors.New("broadcast policy storage unavailable")
	}
	return nil
}

// Membership is recorded only for a known private client and never creates identity.
func recordBroadcastMembership(ctx context.Context, tx pgx.Tx, bot, actor int64) error {
	_, e := tx.Exec(ctx, `INSERT INTO broadcast_memberships(bot_id,client_id,actor_id) SELECT $1,id,telegram_id FROM clients WHERE telegram_id=$2 AND chat_id=$2 ON CONFLICT(bot_id,client_id) DO NOTHING`, bot, actor)
	return e
}

func (s *Store) ApplyBroadcastUpdate(ctx context.Context, botID int64, username string, u telegram.Update, p broadcast.Policy) (bool, error) {
	if botID <= 0 || u.ID < 0 {
		return false, ErrInvalidCommand
	}
	admins := map[int64]struct{}{}
	for _, id := range p.AdminIDs {
		admins[id] = struct{}{}
	}
	identity, _, ok := broadcast.ParseCommand(u, username, admins)
	if !ok {
		return false, nil
	}
	// Parser-recognized updates are durably deduplicated with their safe private response.
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		return true, errors.New("broadcast update unavailable")
	}
	defer rollback(tx)
	if _, e = tx.Exec(ctx, `INSERT INTO telegram_bots(bot_id) VALUES($1) ON CONFLICT DO NOTHING`, botID); e != nil {
		return true, errors.New("broadcast update unavailable")
	}
	if _, e = tx.Exec(ctx, `SELECT bot_id FROM telegram_bots WHERE bot_id=$1 FOR NO KEY UPDATE`, botID); e != nil {
		return true, errors.New("broadcast update unavailable")
	}
	tag, e := tx.Exec(ctx, `INSERT INTO telegram_processed_updates(bot_id,update_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, botID, u.ID)
	if e != nil {
		return true, errors.New("broadcast update unavailable")
	}
	if tag.RowsAffected() == 0 {
		return true, tx.Commit(ctx)
	}
	current, found, policyErr := loadBroadcastPolicy(ctx, tx, botID)
	text := "Команда рассылки не выполнена."
	if policyErr == nil && found {
		admins := make(map[int64]struct{}, len(current.AdminIDs))
		for _, id := range current.AdminIDs {
			admins[id] = struct{}{}
		}
		if actualIdentity, actualCmd, recognized := broadcast.ParseCommand(u, username, admins); recognized && actualIdentity.TelegramID == identity.TelegramID {
			text, e = s.applyBroadcastAction(ctx, tx, botID, actualIdentity, actualCmd, p)
			if e != nil {
				return true, errors.New("broadcast update unavailable")
			}
		} else {
			text = "Доступ к команде рассылки изменился или команда неверна."
		}
	}
	_, e = enqueueOutbox(ctx, tx, queue.Draft{BotID: botID, ChatID: identity.ChatID, Key: fmt.Sprintf("broadcast-admin:%d:%d", botID, u.ID), Text: text})
	if e != nil {
		return true, errors.New("broadcast update unavailable")
	}
	if tx.Commit(ctx) != nil {
		return true, errors.New("broadcast update unavailable")
	}
	return true, nil
}
func containsAdmin(p broadcast.Policy, id int64) bool {
	for _, v := range p.AdminIDs {
		if v == id {
			return p.Enabled
		}
	}
	return false
}
func (s *Store) ProcessBroadcastBatch(ctx context.Context, botID int64, supplied broadcast.Policy, batch int) (result broadcast.Progress, err error) {
	defer func() {
		if err != nil {
			result = broadcast.Progress{}
		}
	}()
	if botID <= 0 || batch < 1 || batch > 50 {
		return broadcast.Progress{}, ErrInvalidCommand
	}
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		return broadcast.Progress{}, errors.New("broadcast processing unavailable")
	}
	defer rollback(tx)
	var lock int64
	e = tx.QueryRow(ctx, `SELECT bot_id FROM telegram_bots WHERE bot_id=$1 FOR NO KEY UPDATE SKIP LOCKED`, botID).Scan(&lock)
	if errors.Is(e, pgx.ErrNoRows) {
		return broadcast.Progress{}, tx.Commit(ctx)
	}
	if e != nil {
		return broadcast.Progress{}, errors.New("broadcast processing unavailable")
	}
	current, found, e := loadBroadcastPolicy(ctx, tx, botID)
	if e != nil {
		return broadcast.Progress{}, errors.New("broadcast processing unavailable")
	}
	if !found || !current.Enabled || supplied.Validate() != nil || !current.Equal(supplied) {
		return broadcast.Progress{}, ErrStalePolicy
	}
	var progress broadcast.Progress
	tag, e := tx.Exec(ctx, `UPDATE broadcast_deliveries d SET status='skipped' FROM broadcasts b WHERE d.job_id=b.id AND b.bot_id=$1 AND b.state='pending' AND d.status='pending' AND (b.policy_revision<>$2 OR b.policy_hash<>$3 OR NOT (b.creator_id=ANY($4::bigint[])))`, botID, current.Revision, current.Hash[:], current.AdminIDs)
	if e != nil {
		return progress, errors.New("broadcast processing unavailable")
	}
	progress.Skipped += int(tag.RowsAffected())
	_, e = tx.Exec(ctx, `UPDATE broadcasts b SET state='cancelled' WHERE b.bot_id=$1 AND b.state='pending' AND (b.policy_revision<>$2 OR b.policy_hash<>$3 OR NOT (b.creator_id=ANY($4::bigint[])))`, botID, current.Revision, current.Hash[:], current.AdminIDs)
	if e != nil {
		return progress, errors.New("broadcast processing unavailable")
	}
	rows, e := tx.Query(ctx, `SELECT b.id,d.client_id,d.actor_id,d.chat_id FROM broadcasts b JOIN broadcast_deliveries d ON d.job_id=b.id WHERE b.bot_id=$1 AND b.state='pending' AND b.policy_revision=$2 AND b.policy_hash=$3 AND b.creator_id=ANY($4::bigint[]) AND d.status='pending' ORDER BY b.id,d.client_id FOR UPDATE OF d SKIP LOCKED LIMIT $5`, botID, current.Revision, current.Hash[:], current.AdminIDs, batch)
	if e != nil {
		return progress, errors.New("broadcast processing unavailable")
	}
	type item struct{ job, client, actor, chat int64 }
	items := make([]item, 0, batch)
	for rows.Next() {
		var x item
		if rows.Scan(&x.job, &x.client, &x.actor, &x.chat) != nil {
			rows.Close()
			return progress, errors.New("broadcast processing unavailable")
		}
		items = append(items, x)
	}
	if e = rows.Err(); e != nil {
		rows.Close()
		return progress, errors.New("broadcast processing unavailable")
	}
	rows.Close()
	clientIDs := make([]int64, 0, len(items))
	for _, x := range items {
		clientIDs = append(clientIDs, x.client)
	}
	sort.Slice(clientIDs, func(i, j int) bool { return clientIDs[i] < clientIDs[j] })
	for _, id := range clientIDs {
		if _, e = tx.Exec(ctx, `SELECT id FROM clients WHERE id=$1 FOR NO KEY UPDATE`, id); e != nil {
			return progress, errors.New("broadcast processing unavailable")
		}
	}
	for _, x := range items {
		var eligible bool
		e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM clients c JOIN broadcast_memberships m ON m.client_id=c.id WHERE c.id=$1 AND c.telegram_id=$2 AND c.chat_id=$3 AND m.bot_id=$4 AND m.actor_id=$2)`, x.client, x.actor, x.chat, botID).Scan(&eligible)
		if e != nil {
			return progress, errors.New("broadcast processing unavailable")
		}
		if !eligible {
			tag, err := tx.Exec(ctx, `UPDATE broadcast_deliveries SET status='skipped' WHERE job_id=$1 AND client_id=$2 AND status='pending'`, x.job, x.client)
			if err != nil {
				return progress, errors.New("broadcast processing unavailable")
			}
			progress.Skipped += int(tag.RowsAffected())
			continue
		}
		var body string
		e = tx.QueryRow(ctx, `SELECT d.body FROM broadcasts b JOIN broadcast_drafts d ON d.id=b.draft_id AND d.state='confirmed' WHERE b.id=$1`, x.job).Scan(&body)
		if e != nil {
			return progress, errors.New("broadcast processing unavailable")
		}
		id, e := enqueueOutbox(ctx, tx, queue.Draft{BotID: botID, ChatID: x.chat, Key: fmt.Sprintf("broadcast:%d:%d", x.job, x.client), Text: body})
		if e != nil {
			return progress, errors.New("broadcast processing unavailable")
		}
		tag, e := tx.Exec(ctx, `UPDATE broadcast_deliveries SET status='enqueued',outbox_id=$3 WHERE job_id=$1 AND client_id=$2 AND status='pending'`, x.job, x.client, id)
		if e != nil || tag.RowsAffected() != 1 {
			return progress, errors.New("broadcast processing unavailable")
		}
		progress.Enqueued++
	}
	if e = refreshBroadcastJobs(ctx, tx, botID, current, &progress); e != nil {
		return broadcast.Progress{}, errors.New("broadcast processing unavailable")
	}
	if e = tx.Commit(ctx); e != nil {
		return broadcast.Progress{}, errors.New("broadcast processing unavailable")
	}
	return progress, nil
}

func refreshBroadcastJobs(ctx context.Context, tx pgx.Tx, bot int64, p broadcast.Policy, progress *broadcast.Progress) error {
	rows, err := tx.Query(ctx, `SELECT b.id,b.creator_id FROM broadcasts b WHERE b.bot_id=$1 AND b.state='pending' AND b.policy_revision=$2 AND b.policy_hash=$3 AND b.creator_id=ANY($4::bigint[]) AND NOT EXISTS(SELECT 1 FROM broadcast_deliveries d WHERE d.job_id=b.id AND d.status='pending') AND NOT EXISTS(SELECT 1 FROM broadcast_deliveries d JOIN outbox o ON o.id=d.outbox_id WHERE d.job_id=b.id AND d.status='enqueued' AND o.status IN ('pending','running')) ORDER BY b.id LIMIT 50 FOR UPDATE OF b`, bot, p.Revision, p.Hash[:], p.AdminIDs)
	if err != nil {
		return err
	}
	type job struct{ id, creator int64 }
	jobs := []job{}
	for rows.Next() {
		var j job
		if err = rows.Scan(&j.id, &j.creator); err != nil {
			rows.Close()
			return err
		}
		jobs = append(jobs, j)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, j := range jobs {
		var pending, enqueued, sent, failed, skipped int
		err = tx.QueryRow(ctx, `SELECT count(*) FILTER(WHERE d.status='pending'),count(*) FILTER(WHERE d.status='enqueued'),count(*) FILTER(WHERE o.status='sent'),count(*) FILTER(WHERE o.status='failed'),count(*) FILTER(WHERE d.status='skipped') FROM broadcast_deliveries d LEFT JOIN outbox o ON o.id=d.outbox_id WHERE d.job_id=$1`, j.id).Scan(&pending, &enqueued, &sent, &failed, &skipped)
		if err != nil {
			return err
		}
		if pending > 0 || enqueued != sent+failed {
			continue
		}
		text := fmt.Sprintf("Рассылка #%d завершена. Передано в отправку %d, доставлено %d, ошибок %d, пропущено %d.", j.id, enqueued, sent, failed, skipped)
		outboxID, err := enqueueOutbox(ctx, tx, queue.Draft{BotID: bot, ChatID: j.creator, Key: fmt.Sprintf("broadcast-summary:%d", j.id), Text: text})
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE broadcasts SET state='done',summary_outbox_id=$2 WHERE id=$1 AND state='pending' AND summary_outbox_id IS NULL`, j.id, outboxID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errors.New("broadcast job changed")
		}
		progress.Completed++
	}
	return nil
}
