package activation

import (
	"fmt"
	"strconv"

	"github.com/PlumpAlbert/payplump-bot/internal/queue"
)

func reviewMessages(botID, operationID int64, admins []int64) []queue.Draft {
	// Both key and text are immutable across reason changes/retries. Partial
	// external results can persist; never promise that access was unchanged.
	text := fmt.Sprintf("Активация (задание %d) остановлена для ручной проверки. Часть изменений во внешних системах могла сохраниться. Автоматические записи остановлены; проверьте журнал и внешние системы.", operationID)
	messages := make([]queue.Draft, 0, len(admins))
	for _, id := range admins {
		messages = append(messages, queue.Draft{BotID: botID, ChatID: id, Key: "activation:" + strconv.FormatInt(operationID, 10) + ":review:admin:" + strconv.FormatInt(id, 10), Text: text})
	}
	return messages
}

func completionMessages(s Snapshot, operationID int64, admins []int64) []queue.Draft {
	prefix := "activation:" + strconv.FormatInt(operationID, 10) + ":complete:"
	text := "Учётная запись и расписание подписки настроены. Следующая дата счёта: " + s.Proof.NextInvoiceDate + ". Запросить персональную подписку: /config. Выдача требует отдельных проверок. Работоспособность подключения не проверена."
	messages := []queue.Draft{{BotID: s.Account.BotID, ChatID: s.Account.ChatID, Key: prefix + "client", Text: text}}
	adminText := fmt.Sprintf("Активация (задание %d), Telegram ID %d: учётная запись и расписание настроены. Следующая дата счёта: %s. Выдача через /config требует отдельного включения и проверок; работоспособность подключения не проверена.", operationID, s.Account.TelegramID, s.Proof.NextInvoiceDate)
	for _, id := range admins {
		messages = append(messages, queue.Draft{BotID: s.Account.BotID, ChatID: id, Key: prefix + "admin:" + strconv.FormatInt(id, 10), Text: adminText})
	}
	return messages
}
