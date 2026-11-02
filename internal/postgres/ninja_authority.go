package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/PlumpAlbert/payplump-bot/internal/ninjaauthority"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
	"github.com/PlumpAlbert/payplump-bot/templates"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var _ ninjaauthority.Store = (*Store)(nil)

// The dedicated session owns the lock; callbacks intentionally use other pool
// connections. Never return a possibly locked session to the pool.
func (s *Store) WithAuthorityScan(ctx context.Context, bot int64, fn func(context.Context) error) (err error) {
	if bot <= 0 || fn == nil {
		return ninjaauthority.ErrConflict
	}
	c, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	var key int64
	var locked bool
	err = c.QueryRow(ctx, `SELECT hashtextextended('ninja-authority:'||current_schema()||':'||$1::bigint::text,0)`, bot).Scan(&key)
	if err == nil {
		err = c.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&locked)
	}
	if err != nil { // A canceled acquisition may have taken the session lock.
		closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = c.Hijack().Close(closeCtx)
		return err
	}
	if !locked {
		c.Release()
		return nil
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var unlocked bool
		e := c.QueryRow(cleanup, `SELECT pg_advisory_unlock($1)`, key).Scan(&unlocked)
		if e != nil || !unlocked {
			_ = c.Hijack().Close(cleanup)
			if err == nil {
				err = errors.New("authority scan unlock failed")
			}
		} else {
			c.Release()
		}
	}()
	return fn(ctx)
}

type authorityQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func loadAuthorityBindings(ctx context.Context, q authorityQuerier, bot, actorFilter int64) (map[int64]ninjaauthority.Binding, error) {
	rows, err := q.Query(ctx, `
 WITH bindings(actor_id,customer_id,sui_id,name,hash) AS (
 SELECT c.telegram_id,coalesce(c.invoice_ninja_id,''),coalesce(c.sui_id,''),''::text,NULL::bytea FROM clients c
 UNION ALL SELECT c.telegram_id,'',a.sui_id::text,a.name,a.credentials_hash FROM activation_sui_bindings a JOIN clients c ON c.id=a.client_id
 UNION ALL SELECT c.telegram_id,v.customer_id,v.sui_id::text,v.sui_name,v.credentials_hash FROM verified_import_origins v JOIN clients c ON c.id=v.client_id
 UNION ALL SELECT actor_id,coalesce(observation->>'CustomerID',''),coalesce(sui_id::text,''),coalesce(sui_name,''),credentials_hash FROM authority_observations WHERE bot_id=$1)
 SELECT actor_id,customer_id,sui_id,name,hash FROM bindings WHERE ($2::bigint=0 OR actor_id=$2)`, bot, actorFilter)
	if err != nil {
		return nil, err
	}
	out := map[int64]ninjaauthority.Binding{}
	for rows.Next() {
		var actor int64
		var customer, id, name string
		var hash []byte
		if err = rows.Scan(&actor, &customer, &id, &name, &hash); err != nil {
			break
		}
		b := out[actor]
		if customer != "" {
			if b.CustomerID != "" && b.CustomerID != customer {
				err = ninjaauthority.ErrConflict
				break
			}
			b.CustomerID = customer
		}
		if id != "" {
			n, e := strconv.ParseInt(id, 10, 64)
			if e != nil || n <= 0 || strconv.FormatInt(n, 10) != id || (b.SUIID != 0 && b.SUIID != n) {
				err = ninjaauthority.ErrConflict
				break
			}
			b.SUIID = n
		}
		if name != "" {
			if b.Name != "" && b.Name != name {
				err = ninjaauthority.ErrConflict
				break
			}
			b.Name = name
		}
		if hash != nil {
			if len(hash) != 32 {
				err = ninjaauthority.ErrConflict
				break
			}
			var h [32]byte
			copy(h[:], hash)
			if b.CredentialsHash != ([32]byte{}) && b.CredentialsHash != h {
				err = ninjaauthority.ErrConflict
				break
			}
			b.CredentialsHash = h
		}
		out[actor] = b
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if rowErr != nil {
		return nil, rowErr
	}
	// An unconfirmed create still owns its frozen credentials. Never adopt a
	// same-name foreign record while the result of our dispatch is unknown.
	rows, err = q.Query(ctx, `SELECT actor_id,draft,record_id,customer_id FROM authority_create_intents WHERE bot_id=$1 AND ($2::bigint=0 OR actor_id=$2)`, bot, actorFilter)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var actor int64
		var recordID *int64
		var customer string
		var raw []byte
		var d sui.AuthorityCreateDraft
		if err = rows.Scan(&actor, &raw, &recordID, &customer); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		b := out[actor]
		id := b.SUIID
		if recordID != nil {
			id = *recordID
		}
		if b.CustomerID != "" && b.CustomerID != customer {
			return nil, ninjaauthority.ErrConflict
		}
		b.CustomerID = customer
		if (b.SUIID != 0 && b.SUIID != id) || (b.Name != "" && b.Name != d.Name()) || (b.CredentialsHash != ([32]byte{}) && b.CredentialsHash != d.CredentialsHash()) {
			return nil, ninjaauthority.ErrConflict
		}
		b.SUIID = id
		b.Name = d.Name()
		b.CredentialsHash = d.CredentialsHash()
		out[actor] = b
	}
	return out, rows.Err()
}
func (s *Store) LoadAuthorityBindings(ctx context.Context, bot int64) (map[int64]ninjaauthority.Binding, error) {
	return loadAuthorityBindings(ctx, s.pool, bot, 0)
}

func authorityError(err error) error {
	var p *pgconn.PgError
	if errors.As(err, &p) && (p.Code == "23505" || p.Code == "23514") {
		return ninjaauthority.ErrConflict
	}
	return err
}

func (s *Store) SaveAuthorityObservation(ctx context.Context, bot int64, o ninjaauthority.Observation) error {
	if bot <= 0 || o.Actor <= 0 || o.CustomerID == "" || len(o.CustomerID) > 256 || len(o.RecurringID) > 256 || len(o.InvoiceID) > 256 || o.ObservedAt.IsZero() {
		return ninjaauthority.ErrConflict
	}
	if o.Binding.CustomerID != "" && o.Binding.CustomerID != o.CustomerID {
		return ninjaauthority.ErrConflict
	}
	if o.Binding.SUIID < 0 || (o.Binding.SUIID > 0 && (o.Binding.Name == "" || o.Binding.CredentialsHash == ([32]byte{}))) {
		return ninjaauthority.ErrConflict
	}
	if o.NextInvoiceDate != "" {
		d, e := time.Parse("2006-01-02", o.NextInvoiceDate)
		if e != nil || d.Format("2006-01-02") != o.NextInvoiceDate {
			return ninjaauthority.ErrConflict
		}
	}
	for _, id := range o.InboundIDs {
		if id <= 0 {
			return ninjaauthority.ErrConflict
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	// Match the outbox bot-before-client lock order for atomic source notices.
	if _, err = tx.Exec(ctx, `SELECT bot_id FROM telegram_bots WHERE bot_id=$1 FOR NO KEY UPDATE`, bot); err != nil {
		return err
	}
	// Serialize identity adoption with other authority writes and registration.
	_, err = tx.Exec(ctx, `INSERT INTO clients(telegram_id,chat_id,display_name) VALUES($1,$1,'') ON CONFLICT(telegram_id) DO NOTHING`, o.Actor)
	if err != nil {
		return authorityError(err)
	}
	var client int64
	var customer string
	err = tx.QueryRow(ctx, `SELECT id,coalesce(invoice_ninja_id,'') FROM clients WHERE telegram_id=$1 FOR UPDATE`, o.Actor).Scan(&client, &customer)
	if err != nil {
		return err
	}
	if err = checkAuthorityHistoricalScope(ctx, tx, bot, o.Actor, o.ScopeHash); err != nil {
		return err
	}
	var pendingOwn bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM subscriptions s WHERE s.client_id=$1 AND s.first_invoice_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM verified_import_origins v WHERE v.client_id=$1) AND NOT EXISTS(SELECT 1 FROM operations op WHERE op.client_id=$1 AND op.kind='activate_first_subscription' AND op.status='completed'))`, client).Scan(&pendingOwn)
	if err != nil {
		return err
	}
	if pendingOwn {
		return ninjaauthority.ErrConflict
	}
	var persistedScope string
	var previousRaw []byte
	err = tx.QueryRow(ctx, `SELECT coalesce(observation->>'ScopeHash',''),observation FROM authority_observations WHERE bot_id=$1 AND client_id=$2`, bot, client).Scan(&persistedScope, &previousRaw)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err == nil && persistedScope != o.ScopeHash {
		return ninjaauthority.ErrConflict
	}
	var intentConflict bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM authority_create_intents WHERE bot_id=$1 AND actor_id=$2 AND (scope_hash<>$3 OR customer_id<>$4))`, bot, o.Actor, o.ScopeHash, o.CustomerID).Scan(&intentConflict)
	if err != nil {
		return err
	}
	if intentConflict {
		return ninjaauthority.ErrConflict
	}
	bindings, err := loadAuthorityBindings(ctx, tx, bot, o.Actor)
	if err != nil {
		return err
	}
	old := bindings[o.Actor]
	if customer != "" && customer != o.CustomerID || old.CustomerID != "" && old.CustomerID != o.CustomerID {
		return ninjaauthority.ErrConflict
	}
	if old.SUIID != 0 && old.SUIID != o.Binding.SUIID || old.Name != "" && old.Name != o.Binding.Name || old.CredentialsHash != ([32]byte{}) && old.CredentialsHash != o.Binding.CredentialsHash {
		return ninjaauthority.ErrConflict
	}
	var suiID any
	var suiText any
	var name any
	var hash any
	if o.Binding.Name != "" {
		name = o.Binding.Name
	}
	if o.Binding.CredentialsHash != ([32]byte{}) {
		hash = o.Binding.CredentialsHash[:]
	}
	if o.Binding.SUIID > 0 {
		suiID = o.Binding.SUIID
		suiText = strconv.FormatInt(o.Binding.SUIID, 10)
		name = o.Binding.Name
		hash = o.Binding.CredentialsHash[:]
		_, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(imported_sui_lock_key($1))`, o.Binding.SUIID)
		if err != nil {
			return err
		}
		var conflict bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM activation_sui_bindings WHERE client_id<>$1 AND (sui_id=$2 OR name=$3) UNION ALL SELECT 1 FROM verified_import_origins WHERE client_id<>$1 AND (sui_id=$2 OR sui_name=$3) UNION ALL SELECT 1 FROM authority_observations WHERE client_id<>$1 AND (sui_id=$2 OR sui_name=$3))`, client, o.Binding.SUIID, o.Binding.Name).Scan(&conflict)
		if err != nil {
			return err
		}
		if conflict {
			return ninjaauthority.ErrConflict
		}
	}
	_, err = tx.Exec(ctx, `UPDATE clients SET invoice_ninja_id=$2,sui_id=$3 WHERE id=$1`, client, o.CustomerID, suiText)
	if err != nil {
		return authorityError(err)
	}
	state := "awaiting_payment"
	if o.StateKnown {
		state = "overdue"
		if o.Enabled {
			state = "active"
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO subscriptions(client_id,state,desired_access,current_invoice_id,recurring_id,next_invoice_date) VALUES($1,$2,$3,NULLIF($4,''),NULLIF($5,''),NULLIF($6,'')::date) ON CONFLICT(client_id) DO UPDATE SET state=excluded.state,desired_access=excluded.desired_access,current_invoice_id=excluded.current_invoice_id,recurring_id=excluded.recurring_id,next_invoice_date=excluded.next_invoice_date`, client, state, o.StateKnown && o.Enabled, o.InvoiceID, o.RecurringID, o.NextInvoiceDate)
	if err != nil {
		return authorityError(err)
	}
	raw, err := json.Marshal(o)
	if err != nil {
		return err
	}
	h := o.Hash()
	var revision int64
	err = tx.QueryRow(ctx, `INSERT INTO authority_observations(bot_id,client_id,actor_id,observation,observation_hash,source_hash,sui_id,sui_name,credentials_hash,observed_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(bot_id,client_id) DO UPDATE SET observation=excluded.observation,observation_hash=excluded.observation_hash,source_hash=excluded.source_hash,sui_id=excluded.sui_id,sui_name=excluded.sui_name,credentials_hash=excluded.credentials_hash,observed_at=excluded.observed_at,revision=authority_observations.revision+1 RETURNING revision`, bot, client, o.Actor, raw, h[:], o.SourceHash[:], suiID, name, hash, o.ObservedAt).Scan(&revision)
	if err != nil {
		return authorityError(err)
	}
	if err = enqueueAuthorityChanges(ctx, tx, bot, client, revision, previousRaw, o); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE automatic_import_issues SET resolved=true,updated_at=clock_timestamp() WHERE bot_id=$1 AND actor_id=$2 AND NOT resolved`, bot, o.Actor)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) PrepareAuthorityCreate(ctx context.Context, bot int64, o ninjaauthority.Observation, d sui.AuthorityCreateDraft) (ninjaauthority.CreateIntent, error) {
	var out ninjaauthority.CreateIntent
	if bot <= 0 || o.Actor <= 0 {
		return out, ninjaauthority.ErrConflict
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return out, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer rollback(tx)
	if err = authorityCreateGuard(ctx, tx, o.Actor); err != nil {
		return out, err
	}
	if err = checkAuthorityHistoricalScope(ctx, tx, bot, o.Actor, o.ScopeHash); err != nil {
		return out, err
	}
	var conflict bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM authority_observations WHERE bot_id=$1 AND actor_id=$2 AND (coalesce(observation->>'ScopeHash','')<>$3 OR coalesce(observation->>'CustomerID','')<>$4))`, bot, o.Actor, o.ScopeHash, o.CustomerID).Scan(&conflict)
	if err != nil {
		return out, err
	}
	if conflict {
		return out, ninjaauthority.ErrConflict
	}
	bindings, err := loadAuthorityBindings(ctx, tx, bot, o.Actor)
	if err != nil {
		return out, err
	}
	binding := bindings[o.Actor]
	if (binding.CustomerID != "" && binding.CustomerID != o.CustomerID) || (binding.Name != "" && binding.Name != d.Name()) || (binding.SUIID > 0 && binding.Name == "") {
		return out, ninjaauthority.ErrConflict
	}
	_, err = tx.Exec(ctx, `INSERT INTO authority_create_intents(bot_id,actor_id,draft,source_hash,scope_hash,customer_id) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(bot_id,actor_id) DO NOTHING`, bot, o.Actor, raw, o.SourceHash[:], o.ScopeHash, o.CustomerID)
	if err != nil {
		return out, err
	}
	var stored, hash []byte
	var scope, customer string
	err = tx.QueryRow(ctx, `SELECT draft,source_hash,dispatched,completed,scope_hash,customer_id FROM authority_create_intents WHERE bot_id=$1 AND actor_id=$2 FOR UPDATE`, bot, o.Actor).Scan(&stored, &hash, &out.Dispatched, &out.Completed, &scope, &customer)
	if err != nil {
		return out, err
	}
	if err = json.Unmarshal(stored, &out.Draft); err != nil {
		return out, err
	}
	copy(out.SourceHash[:], hash)
	// Fresh proposals may have newly generated credentials: always reuse the
	// persisted credentials, but never replace its identity/source/access intent.
	if customer != o.CustomerID || scope != o.ScopeHash || out.SourceHash != o.SourceHash || out.Draft.Name() != d.Name() || out.Draft.Enabled() != d.Enabled() || !slices.Equal(out.Draft.InboundIDs(), d.InboundIDs()) {
		return out, ninjaauthority.ErrConflict
	}
	_, err = tx.Exec(ctx, `SET LOCAL synchronous_commit=on`)
	if err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}
func (s *Store) AuthorizeAuthorityCreate(ctx context.Context, bot, actor int64, hash [32]byte) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if err = authorityCreateGuard(ctx, tx, actor); err != nil {
		return err
	}
	var scope string
	if err = tx.QueryRow(ctx, `SELECT scope_hash FROM authority_create_intents WHERE bot_id=$1 AND actor_id=$2 FOR UPDATE`, bot, actor).Scan(&scope); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ninjaauthority.ErrUnknown
		}
		return err
	}
	if err = checkAuthorityHistoricalScope(ctx, tx, bot, actor, scope); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE authority_create_intents SET dispatched=true WHERE bot_id=$1 AND actor_id=$2 AND source_hash=$3 AND NOT dispatched AND NOT completed`, bot, actor, hash[:])
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ninjaauthority.ErrUnknown
	}
	return tx.Commit(ctx)
}
func (s *Store) CompleteAuthorityCreate(ctx context.Context, bot, actor int64, r sui.ClientRecord) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	var raw []byte
	var completed bool
	var id *int64
	var d sui.AuthorityCreateDraft
	err = tx.QueryRow(ctx, `SELECT draft,completed,record_id FROM authority_create_intents WHERE bot_id=$1 AND actor_id=$2 FOR UPDATE`, bot, actor).Scan(&raw, &completed, &id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ninjaauthority.ErrUnknown
	}
	if err != nil {
		return err
	}
	if err = json.Unmarshal(raw, &d); err != nil {
		return err
	}
	if r.ID() <= 0 || r.Name() != d.Name() || r.Enabled() != d.Enabled() || r.CredentialsHash() != d.CredentialsHash() || !slices.Equal(r.InboundIDs(), d.InboundIDs()) || (completed && *id != r.ID()) {
		return ninjaauthority.ErrConflict
	}
	if completed {
		return nil
	}
	// SaveAuthorityObservation on the next pass links the verified returned ID.
	// LoadAuthorityBindings includes this completed intent even before that pass.
	_, err = tx.Exec(ctx, `UPDATE authority_create_intents SET completed=true,record_id=$3 WHERE bot_id=$1 AND actor_id=$2`, bot, actor, r.ID())
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

var authorityControl = regexp.MustCompile(`[\x00-\x1f\x7f]`)
var authorityReason = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func authorityKey(key string) bool {
	return len(key) > 0 && len(key) <= 256 && utf8.ValidString(key) && !authorityControl.MatchString(key)
}
func (s *Store) AuthorityNotice(ctx context.Context, bot int64, key string, actor int64, reason, facts string, admins []int64) error {
	if bot <= 0 || actor < 0 || !authorityKey(key) || !authorityKey(facts) || !authorityReason.MatchString(reason) || len(admins) > 1000 {
		return ninjaauthority.ErrConflict
	}
	for _, id := range admins {
		if id <= 0 {
			return ninjaauthority.ErrConflict
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	// Bot row serializes concurrent first insertion and matches outbox lock order.
	if _, err = tx.Exec(ctx, `SELECT bot_id FROM telegram_bots WHERE bot_id=$1 FOR NO KEY UPDATE`, bot); err != nil {
		return err
	}
	var previousActor int64
	var previousReason, previousFacts string
	var resolved bool
	err = tx.QueryRow(ctx, `SELECT actor_id,reason,facts_hash,resolved FROM authority_notices WHERE bot_id=$1 AND notice_key=$2 FOR UPDATE`, bot, key).Scan(&previousActor, &previousReason, &previousFacts, &resolved)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err == nil && !resolved && previousActor == actor && previousReason == reason && previousFacts == facts {
		_, err = tx.Exec(ctx, `UPDATE authority_notices SET updated_at=clock_timestamp() WHERE bot_id=$1 AND notice_key=$2`, bot, key)
		if err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	var generation int64
	err = tx.QueryRow(ctx, `INSERT INTO authority_notices(bot_id,notice_key,actor_id,reason,facts_hash,generation) VALUES($1,$2,$3,$4,$5,1) ON CONFLICT(bot_id,notice_key) DO UPDATE SET actor_id=excluded.actor_id,reason=excluded.reason,facts_hash=excluded.facts_hash,resolved=false,updated_at=clock_timestamp(),generation=authority_notices.generation+CASE WHEN authority_notices.resolved OR authority_notices.reason<>excluded.reason OR authority_notices.facts_hash<>excluded.facts_hash OR authority_notices.actor_id<>excluded.actor_id THEN 1 ELSE 0 END RETURNING generation`, bot, key, actor, reason, facts).Scan(&generation)
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(key))
	text := templates.Render("authority/notice", templates.Data{ActorID: strconv.FormatInt(actor, 10), Reason: reason, DiagnosticCode: fmt.Sprintf("%x", digest[:6])})
	for _, admin := range admins {
		_, err = enqueueOutbox(ctx, tx, queue.Draft{BotID: bot, ChatID: admin, Key: fmt.Sprintf("authority:%x:%d:%d", digest, generation, admin), Text: text})
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
func (s *Store) ReconcileAuthorityNotices(ctx context.Context, bot int64, active []string, started time.Time) error {
	if bot <= 0 || started.IsZero() {
		return ninjaauthority.ErrConflict
	}
	for _, key := range active {
		if !authorityKey(key) {
			return ninjaauthority.ErrConflict
		}
	}
	if active == nil {
		active = []string{}
	}
	_, err := s.pool.Exec(ctx, `UPDATE authority_notices SET resolved=true,updated_at=clock_timestamp() WHERE bot_id=$1 AND NOT resolved AND updated_at<$3 AND NOT(notice_key=ANY($2::text[]))`, bot, active, started)
	return err
}

// Scope established before authority adoption must not be silently replaced
// when endpoints/tokens change. Revocation retains this historical anchor.
func checkAuthorityHistoricalScope(ctx context.Context, tx pgx.Tx, bot, actor int64, scope string) error {
	var conflict bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(
 SELECT 1 FROM import_admission_policy WHERE bot_id=$1 AND encode(source_scope_hash,'hex')<>$3
 UNION ALL SELECT 1 FROM verified_import_origins v JOIN clients c ON c.id=v.client_id WHERE c.telegram_id=$2 AND (v.bot_id<>$1 OR encode(v.source_scope_hash,'hex')<>$3))`, bot, actor, scope).Scan(&conflict)
	if err != nil {
		return err
	}
	if conflict {
		return ninjaauthority.ErrConflict
	}
	return nil
}

// Old senders may still POST after losing their worker lease. Never release
// their non-expiring guard or turn an unresolved create into a fresh create.
func authorityCreateGuard(ctx context.Context, tx pgx.Tx, actor int64) error {
	guard, err := lockActivationGuard(ctx, tx)
	if err != nil {
		return err
	}
	if guard.operationID != nil {
		return ninjaauthority.ErrUnknown
	}
	var pendingOwn bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM clients c JOIN subscriptions s ON s.client_id=c.id WHERE c.telegram_id=$1 AND s.first_invoice_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM verified_import_origins v WHERE v.client_id=c.id) AND NOT EXISTS(SELECT 1 FROM operations op WHERE op.client_id=c.id AND op.kind='activate_first_subscription' AND op.status='completed'))`, actor).Scan(&pendingOwn)
	if err != nil {
		return err
	}
	if pendingOwn {
		return ninjaauthority.ErrConflict
	}
	var unknown bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM external_write_intents e JOIN operations o ON o.id=e.operation_id JOIN clients c ON c.id=o.client_id WHERE c.telegram_id=$1 AND e.step='sui_create' AND NOT EXISTS(SELECT 1 FROM activation_sui_bindings a WHERE a.client_id=c.id))`, actor).Scan(&unknown)
	if err != nil {
		return err
	}
	if unknown {
		return ninjaauthority.ErrUnknown
	}
	return nil
}
