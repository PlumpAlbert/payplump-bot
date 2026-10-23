package lifecycle

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/PlumpAlbert/payplump-bot/internal/queue"
)

func makeNotices(botID int64, admins []int64, snapshot Snapshot, invoice Invoice, reviewCode int) Notices {
	identity := invoice.ID
	if identity == "" {
		identity = fmt.Sprintf("client:%d", snapshot.ClientID)
	}
	digest := sha256.Sum256([]byte(identity))
	prefix := "invoice-lifecycle:" + hex.EncodeToString(digest[:])
	if reviewCode != 0 {
		reason := reviewText(reviewCode)
		number := invoice.Number
		if !printable(number, 128) {
			number = "[неизвестен]"
		}
		body := fmt.Sprintf("Нужна ручная проверка нового recurring-счёта. Telegram ID %d, номер счёта %s. %s Автоматизация не меняла счёт или доступ.", snapshot.TelegramID, number, reason)
		out := Notices{}
		for _, id := range sortedIDs(admins) {
			out.Review = append(out.Review, queue.Draft{BotID: botID, ChatID: id, Key: prefix + ":review:admin:" + strconv.FormatInt(id, 10), Text: body})
		}
		return out
	}
	if invoice.Status != "sent" && invoice.Status != "partial" {
		return Notices{}
	}
	verb := "Выставлен новый счёт"
	if invoice.Status == "partial" {
		verb = "Остаток по счёту обновлён"
	}
	body := fmt.Sprintf("%s №%s. Остаток к оплате: %s. Дата фактической отправки: %s. Последний день сохранения доступа при неоплате: %s включительно; со следующего дня доступ могут отключить. Если вы уже оплатили остаток, дождитесь автоматической проверки.", verb, invoice.Number, invoice.Balance, invoice.SentDate, invoice.SentDate)
	out := Notices{Client: &queue.Draft{BotID: botID, ChatID: snapshot.ChatID, Key: prefix + ":issued:client", Text: body}}
	for _, id := range sortedIDs(admins) {
		out.Admins = append(out.Admins, queue.Draft{BotID: botID, ChatID: id, Key: prefix + ":issued:admin:" + strconv.FormatInt(id, 10), Text: fmt.Sprintf("Recurring-счёт %s: Telegram ID %d, номер %s, остаток %s, sent_date %s.", invoice.Status, snapshot.TelegramID, invoice.Number, invoice.Balance, invoice.SentDate)})
	}
	return out
}

func reviewText(code int) string {
	switch code {
	case ReviewAssociation:
		return "Связь клиента, recurring-счёта или private provenance marker не подтверждена."
	case ReviewInvoice:
		return "Канонический статус счёта нельзя безопасно обработать."
	case ReviewSentDate:
		return "В приглашениях нет однозначной фактической даты отправки."
	case ReviewBalance:
		return "Сумма, оплаченная сумма и остаток счёта противоречат друг другу."
	case ReviewCollision:
		return "Обнаружено больше одного нового recurring-счёта."
	case ReviewExcluded:
		return "Аккаунт не входит в разрешённую пилотную область."
	case ReviewPreviousPayment:
		return "Предыдущий счёт или подтверждение его полной оплаты требует ручной проверки."
	default:
		return "Контракт lifecycle-обработки не подтверждён."
	}
}

func sortedIDs(ids []int64) []int64 {
	out := append([]int64(nil), ids...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func printable(value string, max int) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) == value && utf8.RuneCountInString(value) <= max && strings.IndexFunc(value, unicode.IsControl) < 0
}
