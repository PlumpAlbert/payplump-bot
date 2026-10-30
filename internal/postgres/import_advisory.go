package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/PlumpAlbert/payplump-bot/internal/importadmission"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/jackc/pgx/v5"
)

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
	text := fmt.Sprintf("Клиент Telegram ID %s импортирован.\nВ Invoice Ninja выставление счетов по расписанию на паузе, а доступ S-UI включён.\nОплаченный период подтверждён до %s. При импорте состояния сервисов не менялись.\nПроверьте, нужно ли оставлять выставление счетов на паузе.", row.TelegramID, row.NextInvoiceDate)
	for _, admin := range admins {
		draft := queue.Draft{BotID: bot, ChatID: admin, Key: fmt.Sprintf("import-advisory:%s:billing-paused:%d", row.StableHash, admin), Text: text}
		if _, err := enqueueOutbox(ctx, tx, draft); err != nil {
			return errors.New("could not enqueue imported billing advisory")
		}
	}
	return nil
}
