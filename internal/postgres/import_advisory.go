package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/PlumpAlbert/payplump-bot/internal/importadmission"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/templates"
	"github.com/jackc/pgx/v5"
)

func importedBillingPauseAdvisoryKey(stable string, admin int64) string {
	return fmt.Sprintf("import-advisory:%s:billing-paused:%d", stable, admin)
}

func enqueueImportedBillingPauseAdvisory(ctx context.Context, tx pgx.Tx, bot int64, row importadmission.AdmissionRow, admins []int64) error {
	if row.Lifecycle != "active" || len(admins) == 0 {
		return nil
	}
	var recurring invoiceninja.RecurringRecord
	if json.Unmarshal(row.Template, &recurring) != nil {
		return errors.New("invalid imported advisory source")
	}
	if recurring.StatusID != "3" {
		return nil
	}
	// This is a successful-import observation, not an ownership/financial hold.
	// The immutable origin and notice are committed together; replayed origins
	// skip this path, and the stable outbox key also deduplicates administrator IDs.
	text := templates.Render("import/billing_paused", templates.Data{ActorID: row.TelegramID, NextInvoiceDate: row.NextInvoiceDate})
	for _, admin := range admins {
		draft := queue.Draft{BotID: bot, ChatID: admin, Key: importedBillingPauseAdvisoryKey(row.StableHash, admin), Text: text}
		if _, err := enqueueOutbox(ctx, tx, draft); err != nil {
			return errors.New("could not enqueue imported billing advisory")
		}
	}
	return nil
}
