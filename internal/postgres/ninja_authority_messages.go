package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/PlumpAlbert/payplump-bot/internal/ninjaauthority"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/templates"
	"github.com/jackc/pgx/v5"
)

// Public source-change notices are atomic with the observation. They are not
// payment receipts and contain neither subscription URLs nor credentials.
// Initial adoption is silent; later invoice changes retain the bot's useful
// notifications without running legacy billing/access mutation workers.
func enqueueAuthorityChanges(ctx context.Context, tx pgx.Tx, bot, client, revision int64, previousRaw []byte, o ninjaauthority.Observation) error {
	var previous ninjaauthority.Observation
	if len(previousRaw) > 0 && json.Unmarshal(previousRaw, &previous) != nil {
		return ninjaauthority.ErrConflict
	}
	enqueue := func(kind, text string) error {
		_, err := enqueueOutbox(ctx, tx, queue.Draft{BotID: bot, ChatID: o.Actor, Key: fmt.Sprintf("authority-change:%d:%d:%s", client, revision, kind), Text: text})
		return err
	}
	if len(previousRaw) > 0 && o.InvoiceID != "" && (previous.InvoiceID != o.InvoiceID || previous.InvoiceStatus != o.InvoiceStatus) {
		if err := enqueue("invoice", templates.Render("authority/invoice_client", templates.Data{InvoiceNumber: o.InvoiceNumber, Body: templates.Render("clientmenu/label_invoice", templates.Data{})})); err != nil {
			return err
		}
	}
	if o.StateKnown && o.Enabled && o.SUIEnabled && o.Binding.SUIID > 0 && previous.Binding.SUIID == 0 {
		var ownFirst bool
		err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations op JOIN activation_flows f ON f.operation_id=op.id WHERE op.client_id=$1 AND op.kind='activate_first_subscription' AND op.status='completed' AND f.bot_id=$2 AND f.flow_mode='ninja_authority')`, client, bot).Scan(&ownFirst)
		if err != nil {
			return err
		}
		if ownFirst {
			return enqueue("configuration_ready", templates.Render("authority/configuration_ready", templates.Data{Body: templates.Render("clientmenu/label_configuration", templates.Data{})}))
		}
	}
	return nil
}
