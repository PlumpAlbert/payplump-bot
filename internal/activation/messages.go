package activation

import (
	"strconv"

	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/templates"
)

func reviewMessages(botID, operationID int64, admins []int64) []queue.Draft {
	// Both key and text are immutable across reason changes/retries. Partial
	// external results can persist; never promise that access was unchanged.
	text := templates.Render("activation/review_admin", templates.Data{OperationID: strconv.FormatInt(operationID, 10)})
	messages := make([]queue.Draft, 0, len(admins))
	for _, id := range admins {
		messages = append(messages, queue.Draft{BotID: botID, ChatID: id, Key: "activation:" + strconv.FormatInt(operationID, 10) + ":review:admin:" + strconv.FormatInt(id, 10), Text: text})
	}
	return messages
}

func completionMessages(s Snapshot, operationID int64, admins []int64) []queue.Draft {
	prefix := "activation:" + strconv.FormatInt(operationID, 10) + ":complete:"
	text := templates.Render("activation/complete_client", templates.Data{NextInvoiceDate: s.Proof.NextInvoiceDate})
	messages := []queue.Draft{{BotID: s.Account.BotID, ChatID: s.Account.ChatID, Key: prefix + "client", Text: text}}
	adminText := templates.Render("activation/complete_admin", templates.Data{OperationID: strconv.FormatInt(operationID, 10), ActorID: strconv.FormatInt(s.Account.TelegramID, 10), NextInvoiceDate: s.Proof.NextInvoiceDate})
	for _, id := range admins {
		messages = append(messages, queue.Draft{BotID: s.Account.BotID, ChatID: id, Key: prefix + "admin:" + strconv.FormatInt(id, 10), Text: adminText})
	}
	return messages
}
