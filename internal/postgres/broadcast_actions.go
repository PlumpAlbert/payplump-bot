package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/PlumpAlbert/payplump-bot/internal/broadcast"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
	"github.com/jackc/pgx/v5"
)

func tokenHash(s string) ([32]byte, bool) {
	var zero [32]byte
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) != 24 || base64.RawURLEncoding.EncodeToString(b) != s {
		return zero, false
	}
	// Hash the canonical encoded token, identically at creation and presentation.
	return sha256.Sum256([]byte(s)), true
}
func policyContains(p broadcast.Policy, id int64) bool {
	for _, v := range p.AdminIDs {
		if v == id {
			return true
		}
	}
	return false
}
func (s *Store) applyBroadcastAction(ctx context.Context, tx pgx.Tx, bot int64, identity telegram.Identity, cmd broadcast.Command, supplied broadcast.Policy) (string, error) {
	current, found, err := loadBroadcastPolicy(ctx, tx, bot)
	if err != nil {
		return "", err
	}
	if !found || !current.Enabled || !current.Equal(supplied) || !policyContains(current, identity.TelegramID) {
		return "Настройки рассылки изменились или доступ отключён. Обратитесь к владельцу бота.", nil
	}
	if err = recordBroadcastMembership(ctx, tx, bot, identity.TelegramID); err != nil {
		return "", err
	}
	switch cmd.Name {
	case "/broadcast":
		if cmd.Argument == "" {
			return "Создайте текст командой /broadcast_text <текст>. Перед отправкой появится предпросмотр и подтверждение.", nil
		}
	case "/broadcast_text":
		body := cmd.Argument
		if broadcast.ValidateBody(body) != nil {
			return "Текст должен содержать от 1 до 3000 байт. Переводы строк и табуляция разрешены.", nil
		}
		tok, e := broadcast.NewToken()
		if e != nil {
			return "", e
		}
		h, ok := tokenHash(tok)
		if !ok {
			return "", broadcast.ErrInvalidCommand
		}
		if _, e = tx.Exec(ctx, `UPDATE broadcast_drafts SET state='cancelled' WHERE bot_id=$1 AND actor_id=$2 AND state IN ('text','audience')`, bot, identity.TelegramID); e != nil {
			return "", e
		}
		if _, e = tx.Exec(ctx, `INSERT INTO broadcast_drafts(bot_id,actor_id,chat_id,policy_revision,policy_hash,body,token_hash,state,expires_at) VALUES($1,$2,$2,$3,$4,$5,$6,'text',clock_timestamp()+interval '15 minutes')`, bot, identity.TelegramID, current.Revision, current.Hash[:], body, h[:]); e != nil {
			return "", e
		}
		return "Предпросмотр рассылки:\n\n" + body + "\n\nТокен: " + tok + "\nДля заморозки получателей: /broadcast_all " + tok, nil
	case "/broadcast_all":
		h, ok := tokenHash(cmd.Argument)
		if ok {
			return freezeBroadcastAudience(ctx, tx, bot, identity.TelegramID, current, h)
		}
	case "/broadcast_confirm":
		h, ok := tokenHash(cmd.Argument)
		if ok {
			return confirmBroadcast(ctx, tx, bot, identity.TelegramID, current, h)
		}
	case "/broadcast_cancel":
		h, ok := tokenHash(cmd.Argument)
		if !ok {
			break
		}
		tag, e := tx.Exec(ctx, `UPDATE broadcast_drafts SET state='cancelled' WHERE bot_id=$1 AND actor_id=$2 AND token_hash=$3 AND state IN ('text','audience') AND expires_at>clock_timestamp()`, bot, identity.TelegramID, h[:])
		if e != nil {
			return "", e
		}
		if tag.RowsAffected() == 0 {
			return "Черновик не найден или срок его действия истёк.", nil
		}
		return "Черновик рассылки отменён.", nil
	case "/broadcast_status":
		h, ok := tokenHash(cmd.Argument)
		if ok {
			return broadcastStatus(ctx, tx, bot, identity.TelegramID, h)
		}
	}
	return "Команда или токен указаны неверно. Проверьте формат и попробуйте снова.", nil
}

type draftSnapshot struct {
	id    int64
	state string
	valid bool
	rev   int64
	hash  []byte
}

func findDraft(ctx context.Context, tx pgx.Tx, bot, actor int64, h [32]byte) (draftSnapshot, error) {
	var d draftSnapshot
	err := tx.QueryRow(ctx, `SELECT id,state,expires_at>clock_timestamp(),policy_revision,policy_hash FROM broadcast_drafts WHERE bot_id=$1 AND actor_id=$2 AND chat_id=$2 AND token_hash=$3 FOR UPDATE`, bot, actor, h[:]).Scan(&d.id, &d.state, &d.valid, &d.rev, &d.hash)
	return d, err
}
func freezeBroadcastAudience(ctx context.Context, tx pgx.Tx, bot, actor int64, p broadcast.Policy, h [32]byte) (string, error) {
	d, e := findDraft(ctx, tx, bot, actor, h)
	if errors.Is(e, pgx.ErrNoRows) {
		return "Черновик не найден.", nil
	}
	if e != nil {
		return "", e
	}
	if d.state != "text" || !d.valid || d.rev != p.Revision || !equalBytes(d.hash, p.Hash[:]) {
		return "Черновик истёк или настройки рассылки изменились.", nil
	}
	tok, e := broadcast.NewToken()
	if e != nil {
		return "", e
	}
	nh, ok := tokenHash(tok)
	if !ok {
		return "", broadcast.ErrInvalidCommand
	}
	if _, e = tx.Exec(ctx, `INSERT INTO broadcast_recipients(draft_id,client_id,actor_id,chat_id) SELECT $1,c.id,c.telegram_id,c.chat_id FROM broadcast_memberships m JOIN clients c ON c.id=m.client_id WHERE m.bot_id=$2 AND m.actor_id=c.telegram_id AND c.chat_id=c.telegram_id AND c.telegram_id>0 ORDER BY c.id LIMIT $3`, d.id, bot, p.MaxRecipients+1); e != nil {
		return "", e
	}
	var count int
	if e = tx.QueryRow(ctx, `SELECT count(*) FROM broadcast_recipients WHERE draft_id=$1`, d.id).Scan(&count); e != nil {
		return "", e
	}
	if count > p.MaxRecipients {
		if _, e = tx.Exec(ctx, `DELETE FROM broadcast_recipients WHERE draft_id=$1`, d.id); e != nil {
			return "", e
		}
		return "Число получателей превышает разрешённый предел. Черновик не изменён.", nil
	}
	tag, e := tx.Exec(ctx, `UPDATE broadcast_drafts SET state='audience',token_hash=$2 WHERE id=$1 AND state='text' AND expires_at>clock_timestamp()`, d.id, nh[:])
	if e != nil {
		return "", e
	}
	if tag.RowsAffected() != 1 {
		return "", broadcast.ErrExpired
	}
	return fmt.Sprintf("Получателей: %d. Для подтверждения отправки: /broadcast_confirm %s", count, tok), nil
}
func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func confirmBroadcast(ctx context.Context, tx pgx.Tx, bot, actor int64, p broadcast.Policy, h [32]byte) (string, error) {
	d, e := findDraft(ctx, tx, bot, actor, h)
	if errors.Is(e, pgx.ErrNoRows) {
		return "Черновик не найден.", nil
	}
	if e != nil {
		return "", e
	}
	if d.state == "confirmed" {
		return reportBroadcast(ctx, tx, d.id)
	}
	if d.state != "audience" || !d.valid || d.rev != p.Revision || !equalBytes(d.hash, p.Hash[:]) {
		return "Черновик истёк или настройки рассылки изменились.", nil
	}
	var job int64
	e = tx.QueryRow(ctx, `INSERT INTO broadcasts(draft_id,bot_id,creator_id,policy_revision,policy_hash,state) VALUES($1,$2,$3,$4,$5,'pending') RETURNING id`, d.id, bot, actor, p.Revision, p.Hash[:]).Scan(&job)
	if e != nil {
		return "", e
	}
	if _, e = tx.Exec(ctx, `INSERT INTO broadcast_deliveries(job_id,client_id,actor_id,chat_id,status) SELECT $1,client_id,actor_id,chat_id,'pending' FROM broadcast_recipients WHERE draft_id=$2`, job, d.id); e != nil {
		return "", e
	}
	tag, e := tx.Exec(ctx, `UPDATE broadcast_drafts SET state='confirmed' WHERE id=$1 AND state='audience' AND expires_at>clock_timestamp()`, d.id)
	if e != nil {
		return "", e
	}
	if tag.RowsAffected() != 1 {
		return "", broadcast.ErrExpired
	}
	return reportBroadcast(ctx, tx, d.id)
}
func reportBroadcast(ctx context.Context, tx pgx.Tx, draft int64) (string, error) {
	var pending, enqueued, sent, failed, skipped int
	e := tx.QueryRow(ctx, `SELECT count(*) FILTER(WHERE d.status='pending'),count(*) FILTER(WHERE d.status='enqueued' AND o.status IN ('pending','running')),count(*) FILTER(WHERE o.status='sent'),count(*) FILTER(WHERE o.status='failed'),count(*) FILTER(WHERE d.status='skipped') FROM broadcasts b LEFT JOIN broadcast_deliveries d ON d.job_id=b.id LEFT JOIN outbox o ON o.id=d.outbox_id WHERE b.draft_id=$1`, draft).Scan(&pending, &enqueued, &sent, &failed, &skipped)
	if e != nil {
		return "", e
	}
	return fmt.Sprintf("Статус: ожидают %d, в очереди %d, доставлено %d, ошибок %d, пропущено %d.", pending, enqueued, sent, failed, skipped), nil
}
func broadcastStatus(ctx context.Context, tx pgx.Tx, bot, actor int64, h [32]byte) (string, error) {
	var id int64
	var state string
	e := tx.QueryRow(ctx, `SELECT id,state FROM broadcast_drafts WHERE bot_id=$1 AND actor_id=$2 AND chat_id=$2 AND token_hash=$3`, bot, actor, h[:]).Scan(&id, &state)
	if errors.Is(e, pgx.ErrNoRows) {
		return "Черновик или рассылка не найдены.", nil
	}
	if e != nil {
		return "", e
	}
	if state != "confirmed" {
		return "Статус доступен после подтверждения рассылки.", nil
	}
	return reportBroadcast(ctx, tx, id)
}
