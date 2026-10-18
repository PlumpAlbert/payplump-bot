package registration

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"github.com/PlumpAlbert/payplump-bot/internal/billing"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
)

func displayAmount(value json.Number) (string, error) { return billing.FormatAmount(value) }

func unissued(invoice invoiceninja.Invoice) bool {
	amount, err := displayAmount(invoice.Amount)
	if err != nil || amount == "0" {
		return false
	}
	balance, err := displayAmount(invoice.Balance)
	return err == nil && balance == "0"
}

func unpaid(invoice invoiceninja.Invoice) bool {
	amount, err := displayAmount(invoice.Amount)
	if err != nil || amount == "0" {
		return false
	}
	balance, err := displayAmount(invoice.Balance)
	return err == nil && amount == balance
}

func invoiceMessages(s Snapshot, invoice invoiceninja.Invoice, cfg Config, admins []int64) ([]queue.Draft, error) {
	amount, err := displayAmount(invoice.Amount)
	if err != nil || amount == "0" || !validText(invoice.Number, 128) || invoice.Number == "" {
		return nil, ErrReviewRequired
	}
	var link string
	for _, invitation := range invoice.Invitations {
		if validInvoiceLink(invitation.Link) {
			link = invitation.Link
			break
		}
	}
	if link == "" {
		return nil, errors.New("invoice has no verified public invitation link")
	}
	text := fmt.Sprintf("Счёт №%s\nСумма по счёту: %s\nОплатите до %s включительно.\nПеревод по телефону: %s", invoice.Number, amount, invoice.DueDate, cfg.PaymentPhone)
	if cfg.PaymentBank != "" {
		text += "\nБанк: " + cfg.PaymentBank
	}
	if cfg.PaymentRecipient != "" {
		text += "\nПолучатель: " + cfg.PaymentRecipient
	}
	text += "\nОткрыть счёт: " + link + "\nДоступ будет выдан после подтверждения полной оплаты."
	messages := []queue.Draft{{BotID: s.BotID, ChatID: s.ChatID, Key: "registration:" + s.Token + ":invoice:client", Text: text}}
	name := s.DisplayName
	if strings.TrimSpace(name) == "" {
		name = "Клиент Telegram"
	}
	adminText := fmt.Sprintf("Регистрация: %s (Telegram ID %d).\nВыставлен счёт №%s, сумма %s, срок %s.\n%s", name, s.TelegramID, invoice.Number, amount, invoice.DueDate, link)
	for _, id := range admins {
		messages = append(messages, queue.Draft{BotID: s.BotID, ChatID: id, Key: "registration:" + s.Token + ":invoice:admin:" + strconv.FormatInt(id, 10), Text: adminText})
	}
	for _, message := range messages {
		if err := message.Validate(); err != nil {
			return nil, errors.New("invalid registration notification")
		}
	}
	return messages, nil
}

func validInvoiceLink(raw string) bool {
	if raw == "" || len(raw) > 2048 || strings.IndexFunc(raw, unicode.IsControl) >= 0 || strings.TrimSpace(raw) != raw {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Fragment == "" && u.Opaque == ""
}

func reviewMessages(s Snapshot, operationID int64, code int, admins []int64) []queue.Draft {
	if s.BotID <= 0 || s.Token == "" {
		return nil
	}
	reason := map[int]string{
		ReviewExistingCustomer: "Существующая запись Invoice Ninja требует контролируемого импорта; повторный первый счёт не создаётся.",
		ReviewCustomerConflict: "Найдено неоднозначное или изменённое соответствие клиента.",
		ReviewInvoiceConflict:  "Найдены несовместимые данные или другая история счетов.",
		ReviewUnknownWrite:     "Результат записи не подтверждён. Автоматический повтор POST запрещён; проверьте Invoice Ninja и журнал задания.",
		ReviewInvoiceState:     "Состояние или дата счёта требуют проверки. Автоматическое перевыставление остановлено.",
		ReviewContract:         "Параметры или контракт Invoice Ninja требуют проверки.",
		ReviewLocalAssociation: "У клиента уже есть другое состояние подписки или внешние соответствия.",
		ReviewExcludedUser:     "Пользователь исключён владельцем из автоматического биллинга. Внешние записи не менялись.",
	}[code]
	if reason == "" {
		reason = "Регистрация требует ручной проверки."
	}
	text := fmt.Sprintf("Регистрация, задание #%d, код %d.\nTelegram ID %d.\n%s", operationID, code, s.TelegramID, reason)
	var messages []queue.Draft
	if s.ChatID > 0 {
		messages = append(messages, queue.Draft{BotID: s.BotID, ChatID: s.ChatID, Key: "registration:" + s.Token + ":review:client", Text: "Для продолжения регистрации нужна проверка администратора. Ваши текущие настройки доступа не изменялись."})
	}
	for _, id := range admins {
		messages = append(messages, queue.Draft{BotID: s.BotID, ChatID: id, Key: "registration:" + s.Token + ":review:" + strconv.Itoa(code) + ":admin:" + strconv.FormatInt(id, 10), Text: text})
	}
	return messages
}
