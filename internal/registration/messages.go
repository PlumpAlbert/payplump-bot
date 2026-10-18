package registration

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
)

var decimalPattern = regexp.MustCompile(`^([0-9]+)(?:\.([0-9]+))?(?:[eE]([+-]?[0-9]+))?$`)

// Expand a bounded JSON decimal exactly. No float, currency conversion or
// rounding is involved; the amount always comes from the concrete invoice.
func displayAmount(value json.Number) (string, error) {
	raw := value.String()
	if len(raw) > 64 {
		return "", errors.New("unsupported invoice amount")
	}
	parts := decimalPattern.FindStringSubmatch(raw)
	if parts == nil || len(parts[1]) > 1 && parts[1][0] == '0' {
		return "", errors.New("unsupported invoice amount")
	}
	exponent := 0
	if parts[3] != "" {
		var err error
		exponent, err = strconv.Atoi(parts[3])
		if err != nil || exponent < -32 || exponent > 32 {
			return "", errors.New("unsupported invoice amount")
		}
	}
	digits := parts[1] + parts[2]
	point := len(parts[1]) + exponent
	var whole, fraction string
	switch {
	case point <= 0:
		whole = "0"
		fraction = strings.Repeat("0", -point) + digits
	case point >= len(digits):
		whole = digits + strings.Repeat("0", point-len(digits))
	default:
		whole, fraction = digits[:point], digits[point:]
	}
	whole = strings.TrimLeft(whole, "0")
	if whole == "" {
		whole = "0"
	}
	fraction = strings.TrimRight(fraction, "0")
	if fraction != "" {
		return whole + "." + fraction, nil
	}
	return whole, nil
}

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
