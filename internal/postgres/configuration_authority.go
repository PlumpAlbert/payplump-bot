package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/clientmenu"
	"github.com/PlumpAlbert/payplump-bot/internal/configdelivery"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
	"github.com/jackc/pgx/v5"
)

// Presence selects authority before either historical audit origin. Bad or
// disabled observations deny; they never fall back to historical payment.
func readAuthorityConfigurationEntitlement(ctx context.Context, tx pgx.Tx, botID, actor int64) (configdelivery.Entitlement, error) {
	e := configdelivery.Entitlement{Period: configdelivery.AuthorityPeriod, Authority: &configdelivery.AuthorityEntitlement{BotID: botID}}
	a := e.Authority
	var raw, hash []byte
	var customer, panel, recurring, invoice, state, next string
	var desired bool
	err := tx.QueryRow(ctx, `SELECT c.id,c.configuration_revision,ao.revision,ao.observation,ao.observation_hash,ao.observed_at,
 COALESCE(c.invoice_ninja_id,''),COALESCE(c.sui_id,''),COALESCE(s.recurring_id,''),COALESCE(s.current_invoice_id,''),s.state,s.desired_access,COALESCE(s.next_invoice_date::text,'')
 FROM authority_observations ao JOIN clients c ON c.id=ao.client_id JOIN subscriptions s ON s.client_id=c.id
 WHERE ao.bot_id=$1 AND ao.actor_id=$2 AND c.telegram_id=$2 AND c.chat_id=$2`, botID, actor).Scan(
		&a.ClientID, &e.Revision, &a.Revision, &raw, &hash, &a.Observation.ObservedAt, &customer, &panel, &recurring, &invoice, &state, &desired, &next)
	if errors.Is(err, pgx.ErrNoRows) {
		return e, configdelivery.ErrDenied
	}
	if err != nil {
		return e, err
	}
	observedAt := a.Observation.ObservedAt
	if json.Unmarshal(raw, &a.Observation) != nil {
		return e, configdelivery.ErrDenied
	}
	o := a.Observation
	expected := o.Hash()
	if len(hash) != 32 || string(hash) != string(expected[:]) || !observedAt.Equal(o.ObservedAt.Truncate(time.Microsecond)) || o.Actor != actor || o.CustomerID != customer || strconv.FormatInt(o.Binding.SUIID, 10) != panel || o.RecurringID != recurring || o.InvoiceID != invoice || o.NextInvoiceDate != next || state != "active" || !desired {
		return e, configdelivery.ErrDenied
	}
	e.Binding = sui.Binding{ID: o.Binding.SUIID, Name: o.Binding.Name, CredentialsHash: o.Binding.CredentialsHash}
	if e.Validate() != nil {
		return e, configdelivery.ErrDenied
	}
	return e, nil
}

func enqueueAuthorityConfiguration(ctx context.Context, tx pgx.Tx, prepared clientmenu.Prepared, e configdelivery.Entitlement, p configdelivery.Policy, fingerprint [32]byte) error {
	r, a := prepared.Request, e.Authority
	id, err := insertOutbox(ctx, tx, queue.Draft{BotID: r.BotID, ChatID: r.ChatID, Key: r.Key(), Text: queue.ConfigurationIntentBody, ProtectedConfiguration: true})
	if err != nil {
		return err
	}
	hash := a.Observation.Hash()
	_, err = tx.Exec(ctx, `INSERT INTO configuration_requests(outbox_id,bot_id,client_id,actor_id,chat_id,client_revision,policy_revision,policy_hash,entitlement_hash,created_at,expires_at,period_kind,origin_kind,authority_revision,authority_hash)
 SELECT $1,$2,$3,$4,$4,$5,$6,$7,$8,t,t+interval '5 minutes','authority','authority',$9,$10 FROM (SELECT clock_timestamp() t) stamp`, id, r.BotID, a.ClientID, r.TelegramID, e.Revision, p.Revision, p.Hash[:], fingerprint[:], a.Revision, hash[:])
	return err
}
