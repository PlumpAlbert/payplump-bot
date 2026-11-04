package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/PlumpAlbert/payplump-bot/internal/ninjaauthority"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/templates"
	"github.com/jackc/pgx/v5"
)

// Public source-change notices are atomic with the observation. They are not
// payment receipts and contain neither subscription URLs nor credentials.
// Initial adoption is silent; later invoice changes retain the bot's useful
// notifications without running legacy billing/access mutation workers.
func enqueueAuthorityChanges(ctx context.Context, tx pgx.Tx, bot, client, revision int64, previousRaw []byte, o ninjaauthority.Observation, admins []int64) error {
	var previous ninjaauthority.Observation
	if len(previousRaw) > 0 && json.Unmarshal(previousRaw, &previous) != nil {
		return ninjaauthority.ErrConflict
	}
	if o.StateKnown && o.Enabled && o.SUIEnabled && o.Binding.SUIID > 0 && (!previous.StateKnown || !previous.Enabled || !previous.SUIEnabled || previous.Binding.SUIID == 0) {
		var ownFirst bool
		err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations op JOIN activation_flows f ON f.operation_id=op.id WHERE op.client_id=$1 AND op.kind='activate_first_subscription' AND op.status='completed' AND f.bot_id=$2 AND f.flow_mode='ninja_authority')`, client, bot).Scan(&ownFirst)
		if err != nil {
			return err
		}
		if ownFirst {
			// First readiness can occur after binding an initially disabled SUI.
			// A stable key suppresses later pause/resume and copy changes as well.
			key := fmt.Sprintf("authority-change:%d:0:configuration_ready", client)
			var sent bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM outbox WHERE bot_id=$1 AND message_key=$2)`, bot, key).Scan(&sent); err != nil {
				return err
			}
			if sent {
				return nil
			}
			return enqueueAuthorityClientAndAdmins(ctx, tx, bot, o.Actor, key,
				templates.Render("authority/configuration_ready", templates.Data{Body: templates.Render("clientmenu/label_configuration", templates.Data{})}),
				templates.Render("authority/configuration_ready_admin", templates.Data{ActorID: strconv.FormatInt(o.Actor, 10)}), admins)
		}
	}
	return nil
}

// Only automatic public business events enter this helper. Client copy and safe
// admin summaries share the observation transaction, but keep distinct role keys.
// admins has been validated, sorted and deduplicated at the save boundary.
func enqueueAuthorityClientAndAdmins(ctx context.Context, tx pgx.Tx, bot, actor int64, key, clientText, adminText string, admins []int64) error {
	if _, err := enqueueOutbox(ctx, tx, queue.Draft{BotID: bot, ChatID: actor, Key: key, Text: clientText}); err != nil {
		return err
	}
	for _, admin := range admins {
		if _, err := enqueueOutbox(ctx, tx, queue.Draft{BotID: bot, ChatID: admin, Key: key + ":admin:" + strconv.FormatInt(admin, 10), Text: adminText}); err != nil {
			return err
		}
	}
	return nil
}
