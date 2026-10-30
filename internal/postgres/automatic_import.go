package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/templates"
	"github.com/jackc/pgx/v5"
)

// AutomaticImportScanStart uses the same database clock as issue.updated_at.
// Application clock skew must never release a newer scan's registration hold.
func (s *Store) AutomaticImportScanStart(ctx context.Context) (time.Time, error) {
	var started time.Time
	if err := s.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&started); err != nil {
		return time.Time{}, errors.New("automatic import scan clock unavailable")
	}
	return started.UTC(), nil
}

func (s *Store) AutomaticImportKnownActors(ctx context.Context) (map[int64]bool, error) {
	rows, e := s.pool.Query(ctx, `SELECT telegram_id FROM clients ORDER BY telegram_id LIMIT 10001`)
	if e != nil {
		return nil, errors.New("known import identities unavailable")
	}
	defer rows.Close()
	ids := map[int64]bool{}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) != nil {
			return nil, errors.New("known import identities unavailable")
		}
		ids[id] = true
	}
	if rows.Err() != nil || len(ids) > 10000 {
		return nil, errors.New("known import identities incomplete")
	}
	return ids, nil
}

func (s *Store) ResolveAutomaticImportIssue(ctx context.Context, bot int64, key string, started time.Time) error {
	if bot <= 0 || started.IsZero() {
		return errors.New("invalid import issue resolution")
	}
	_, e := s.pool.Exec(ctx, `UPDATE automatic_import_issues SET resolved=true,updated_at=clock_timestamp() WHERE bot_id=$1 AND candidate_key=$2 AND NOT resolved AND updated_at<$3`, bot, key, started)
	if e != nil {
		return errors.New("could not resolve automatic import diagnostic")
	}
	return nil
}

// ReconcileAutomaticImportIssues releases obsolete diagnostics only after a
// complete source inventory. Never clear an issue observed by a newer scan.
func (s *Store) ReconcileAutomaticImportIssues(ctx context.Context, bot int64, active []string, started time.Time) error {
	if bot <= 0 || started.IsZero() || len(active) > 2001 {
		return errors.New("invalid import issue reconciliation")
	}
	for _, key := range active {
		if _, err := importDigest(key); err != nil {
			return err
		}
	}
	if active == nil {
		active = []string{}
	}
	_, err := s.pool.Exec(ctx, `UPDATE automatic_import_issues SET resolved=true,updated_at=clock_timestamp()
		WHERE bot_id=$1 AND NOT resolved AND updated_at<$3 AND NOT (candidate_key=ANY($2::text[]))`, bot, active, started)
	if err != nil {
		return errors.New("could not reconcile automatic import diagnostics")
	}
	return nil
}

// RecordAutomaticImportIssue commits the diagnostic and private admin notices
// together. The key/facts are hashes, and callers supply only static reason
// codes and a validated numeric actor, never raw provider descriptions.
func (s *Store) RecordAutomaticImportIssue(ctx context.Context, bot, actor int64, key, reason, facts string, admins []int64) (bool, error) {
	if bot <= 0 || actor < 0 || len(admins) > 99 {
		return false, errors.New("invalid import diagnostic")
	}
	if _, e := importDigest(key); e != nil {
		return false, e
	}
	if _, e := importDigest(facts); e != nil {
		return false, e
	}
	explanation, ok := importIssueExplanation[reason]
	if !ok {
		return false, errors.New("invalid import diagnostic reason")
	}
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		return false, errors.New("import diagnostic storage unavailable")
	}
	defer rollback(tx)
	// Match incoming Telegram updates' bot-before-diagnostic lock order.
	if _, e = tx.Exec(ctx, `SELECT bot_id FROM telegram_bots WHERE bot_id=$1 FOR NO KEY UPDATE`, bot); e != nil {
		return false, e
	}
	var oldReason, oldFacts string
	var generation int64
	var resolved bool
	e = tx.QueryRow(ctx, `SELECT reason,facts_hash,generation,resolved FROM automatic_import_issues WHERE bot_id=$1 AND candidate_key=$2 FOR UPDATE`, bot, key).Scan(&oldReason, &oldFacts, &generation, &resolved)
	if e == nil && !resolved && oldReason == reason && oldFacts == facts {
		// A repeated observation is not a new notification, but it must protect
		// this hold from reconciliation by an older concurrent inventory.
		if _, e = tx.Exec(ctx, `UPDATE automatic_import_issues SET updated_at=clock_timestamp() WHERE bot_id=$1 AND candidate_key=$2`, bot, key); e != nil {
			return false, errors.New("could not refresh import diagnostic observation")
		}
		if _, e = tx.Exec(ctx, `SET LOCAL synchronous_commit=on`); e != nil {
			return false, e
		}
		return false, tx.Commit(ctx)
	}
	if e != nil && !errors.Is(e, pgx.ErrNoRows) {
		return false, errors.New("could not read import diagnostic")
	}
	if generation == math.MaxInt64 {
		return false, errors.New("import diagnostic generation exhausted")
	}
	generation++
	_, e = tx.Exec(ctx, `INSERT INTO automatic_import_issues(bot_id,candidate_key,actor_id,reason,facts_hash,generation) VALUES($1,$2,NULLIF($3::bigint,0::bigint),$4,$5,$6) ON CONFLICT(bot_id,candidate_key) DO UPDATE SET actor_id=excluded.actor_id,reason=excluded.reason,facts_hash=excluded.facts_hash,generation=excluded.generation,resolved=false,updated_at=clock_timestamp()`, bot, key, actor, reason, facts, generation)
	if e != nil {
		return false, errors.New("could not save import diagnostic")
	}
	subject := templates.Render("import/subject_general", templates.Data{})
	if actor > 0 {
		subject = templates.Render("import/subject_actor", templates.Data{ActorID: strconv.FormatInt(actor, 10)})
	} else if reason != "source_failure" {
		subject = templates.Render("import/subject_candidate", templates.Data{DiagnosticCode: key[:12]})
	}
	text := templates.Render("import/issue_admin", templates.Data{Subject: subject, Reason: explanation, DiagnosticCode: key[:12]})
	for _, admin := range admins {
		if _, e = enqueueOutbox(ctx, tx, queue.Draft{BotID: bot, ChatID: admin, Key: fmt.Sprintf("auto-import:%s:%d:%d", key, generation, admin), Text: text}); e != nil {
			return false, errors.New("could not enqueue import diagnostic")
		}
	}
	if _, e = tx.Exec(ctx, `SET LOCAL synchronous_commit=on`); e != nil {
		return false, e
	}
	if tx.Commit(ctx) != nil {
		return false, errors.New("could not commit import diagnostic")
	}
	return true, nil
}

var importIssueExplanation = map[string]string{
	"unsupported_profile":          templates.Render("import/unsupported_profile", templates.Data{}),
	"ambiguous_source":             templates.Render("import/ambiguous_source", templates.Data{}),
	"source_failure":               templates.Render("import/source_failure", templates.Data{}),
	"payment_unverified":           templates.Render("import/payment_unverified", templates.Data{}),
	"unpaid_debt":                  templates.Render("import/unpaid_debt", templates.Data{}),
	"active_period_expired":        templates.Render("import/active_period_expired", templates.Data{}),
	"source_changed":               templates.Render("import/source_changed", templates.Data{}),
	"excluded":                     templates.Render("import/excluded", templates.Data{}),
	"invalid_policy":               templates.Render("import/invalid_policy", templates.Data{}),
	"paused_history_not_supported": templates.Render("import/paused_history_not_supported", templates.Data{}),
	"historical_anchor_limit":      templates.Render("import/historical_anchor_limit", templates.Data{}),
	"admission_conflict":           templates.Render("import/admission_conflict", templates.Data{}),
}

func rejectAutomaticImportRegistration(ctx context.Context, tx pgx.Tx, bot, actor, updateID int64) (bool, error) {
	var blocked bool
	if e := tx.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM clients WHERE telegram_id=$2) AND EXISTS(SELECT 1 FROM automatic_import_issues WHERE bot_id=$1 AND (actor_id=$2 OR (actor_id IS NULL AND reason='source_failure')) AND NOT resolved)`, bot, actor).Scan(&blocked); e != nil {
		return false, e
	}
	if !blocked {
		return false, nil
	}
	_, e := enqueueOutbox(ctx, tx, queue.Draft{BotID: bot, ChatID: actor, Key: fmt.Sprintf("auto-import-registration:%d", updateID), Text: templates.Render("import/registration_blocked", templates.Data{})})
	return true, e
}
