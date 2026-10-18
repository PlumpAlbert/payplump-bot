package clientmenu

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/PlumpAlbert/payplump-bot/internal/billing"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
)

var ErrStaleSnapshot = errors.New("client menu snapshot changed")

type Config struct {
	PaymentPhone, PaymentBank, PaymentRecipient string
	ExcludedTelegramIDs                         []int64
	ConfigurationEnabled                        bool
	ConfigurationPolicyRevision                 int64
	ConfigurationPolicyHash                     [32]byte
	ConfigurationAllowedTelegramIDs             []int64
}

func (c Config) Validate() error {
	for _, item := range []struct {
		s string
		n int
	}{{c.PaymentPhone, 128}, {c.PaymentBank, 128}, {c.PaymentRecipient, 256}} {
		if !text(item.s, item.n) {
			return errors.New("invalid client menu payment settings")
		}
	}
	if len(c.ExcludedTelegramIDs) > 1000 {
		return errors.New("too many client menu exclusions")
	}
	seen := map[int64]bool{}
	for _, id := range c.ExcludedTelegramIDs {
		if id <= 0 || seen[id] {
			return errors.New("invalid client menu exclusions")
		}
		seen[id] = true
	}
	if c.ConfigurationEnabled {
		if c.ConfigurationPolicyRevision < 1 || c.ConfigurationPolicyHash == ([32]byte{}) || len(c.ConfigurationAllowedTelegramIDs) < 1 || len(c.ConfigurationAllowedTelegramIDs) > 20 {
			return errors.New("invalid client menu configuration policy")
		}
		allowed := map[int64]bool{}
		for _, id := range c.ConfigurationAllowedTelegramIDs {
			if id <= 0 || allowed[id] {
				return errors.New("invalid client menu configuration allowlist")
			}
			allowed[id] = true
		}
	}
	return nil
}

type Service struct {
	cfg    Config
	reader Reader
}

func New(cfg Config, reader Reader) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg.ExcludedTelegramIDs = append([]int64(nil), cfg.ExcludedTelegramIDs...)
	cfg.ConfigurationAllowedTelegramIDs = slices.Clone(cfg.ConfigurationAllowedTelegramIDs)
	return &Service{cfg, reader}, nil
}

const unavailable = "Не удалось безопасно получить текущий счёт. Попробуйте позже или обратитесь к администратору. Новый счёт не создавался."
const instructions = `Инструкция подключения

1. Установите подходящий клиент sing-box из официального источника: https://sing-box.sagernet.org/clients/
2. После подтверждения полной оплаты откройте «Моя конфигурация». Персональная подписка выдаётся в формате JSON.
3. В клиенте добавьте удалённый профиль по полученной ссылке и обновите его. Названия пунктов зависят от приложения и версии.
4. Выберите профиль и включите подключение.

Не публикуйте персональную ссылку. После продления или восстановления доступа прежнюю подписку не нужно удалять и импортировать заново. Если ваш клиент не поддерживает такой способ добавления, уточните настройку у администратора.

Персональная подписка доступна только при отдельно включённой выдаче и после проверки оплаты и доступа. Если выдача недоступна, обратитесь к администратору.`

func (s *Service) Prepare(ctx context.Context, r Request, snapshot Snapshot) (Prepared, error) {
	result := Prepared{Request: r, Snapshot: snapshot}
	if !r.Valid() {
		return result, errors.New("invalid client menu request")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if r.Action == Help {
		result.Text = instructions
		return result, nil
	}
	for _, id := range s.cfg.ExcludedTelegramIDs {
		if id == r.TelegramID {
			result.Text = "Этот аккаунт исключён владельцем из автоматического биллинга. Настройки доступа не менялись. По вопросам подключения обратитесь к администратору. Инструкция доступна через /help."
			return result, nil
		}
	}
	if !snapshot.Known && r.Action != Start {
		result.Text = "Для регистрации отправьте /start. Общая инструкция подключения доступна через /help."
		return result, nil
	}
	switch r.Action {
	case Start, Home:
		if !snapshot.Known {
			result.Text = "Здравствуйте! Регистрация сохранена. Когда первый счёт будет сформирован, бот пришлёт его отдельно. До подтверждения полной оплаты доступ не выдаётся.\n\n"
		} else {
			result.Text = "Личное меню.\n" + statusText(snapshot) + "\n\n"
		}
		result.Text += "Выберите действие на клавиатуре:\n/invoice — текущий счёт и реквизиты\n/status — состояние подписки\n/config — моя конфигурация\n/help — инструкция подключения"
	case Status:
		result.Text = statusText(snapshot)
	case Configuration:
		result.Text = ConfigurationUnavailableText
		if s.cfg.ConfigurationEnabled {
			if slices.Contains(s.cfg.ConfigurationAllowedTelegramIDs, r.TelegramID) && snapshot.Known && snapshot.State == "active" && snapshot.DesiredAccess && !snapshot.PaymentReview && snapshot.SUIID != "" && snapshot.RecurringID != "" {
				result.ProtectedConfiguration = true
				result.ConfigurationPolicyRevision = s.cfg.ConfigurationPolicyRevision
				result.ConfigurationPolicyHash = s.cfg.ConfigurationPolicyHash
				result.Text = queue.ConfigurationIntentBody
			}
		}
	case Invoice:
		if snapshot.CustomerID == "" || snapshot.InvoiceID == "" {
			result.Text = "Текущий счёт пока не сформирован или регистрация требует проверки. Повторное обращение не создаёт новый счёт. Если ожидание затянулось, обратитесь к администратору."
			break
		}
		result.Text = s.invoice(ctx, r, snapshot)
	}
	return result, nil
}

func statusText(s Snapshot) string {
	state := map[string]string{"awaiting_payment": "ожидается подтверждение первой оплаты", "active": "активна", "overdue": "просрочена"}[s.State]
	if state == "" {
		state = "требует проверки администратора"
	}
	if s.State == "awaiting_payment" && date(s.PaymentDate) {
		state = "оплата проверена; настройка доступа ещё не завершена"
	}
	if s.PaymentReview {
		state = "подтверждение оплаты требует ручной перепроверки"
	}
	message := "Последнее сохранённое состояние подписки: " + state + ".\n"
	if s.DesiredAccess {
		message += "В базе запрошен активный доступ. Это не проверка текущего подключения S-UI.\n"
	} else {
		message += "В базе активный доступ не разрешён.\n"
	}
	if date(s.PaymentDate) {
		message += "Дата подтверждённого платежа в базе: " + s.PaymentDate + ".\n"
	}
	if date(s.NextInvoiceDate) {
		message += "Следующий счёт по сохранённым данным: " + s.NextInvoiceDate + ".\n"
	}
	return message + "Это последнее сохранённое наблюдение; актуальный доступ S-UI здесь не проверяется. Текущий счёт и реквизиты: /invoice."
}

func (s *Service) invoice(ctx context.Context, r Request, local Snapshot) string {
	if s.reader == nil {
		return unavailable
	}
	// Leave most of the webhook deadline to the final database transaction.
	remote, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	customer, err := s.reader.GetCustomer(remote, local.CustomerID)
	if err != nil || customer.ID != local.CustomerID || !customer.Active() || customer.IDNumber != strconv.FormatInt(r.TelegramID, 10) {
		return unavailable
	}
	invoice, err := s.reader.GetInvoice(remote, local.InvoiceID)
	if err != nil || invoice.ID != local.InvoiceID || invoice.ClientID != customer.ID || invoice.IsDeleted || invoice.ArchivedAt != 0 {
		return unavailable
	}
	if invoice.StatusID == "1" {
		return "Текущий счёт ещё не выставлен. Не переводите оплату по черновику; дождитесь сообщения о выставленном счёте."
	}
	if invoice.StatusID == "5" || invoice.StatusID == "6" {
		return "Текущий счёт отменён или сторнирован. Не переводите по нему оплату; обратитесь к администратору."
	}
	amount, aerr := billing.FormatAmount(invoice.Amount)
	balance, berr := billing.FormatAmount(invoice.Balance)
	if aerr != nil || berr != nil || amount == "0" || !text(invoice.Number, 128) || invoice.Number == "" || !date(invoice.Date) || !date(invoice.DueDate) || invoice.DueDate < invoice.Date {
		return unavailable
	}
	a, _ := new(big.Rat).SetString(amount)
	b, _ := new(big.Rat).SetString(balance)
	if a == nil || b == nil || b.Cmp(a) > 0 {
		return unavailable
	}
	var link string
	for _, invitation := range invoice.Invitations {
		if invoiceLink(invitation.Link) {
			link = invitation.Link
			break
		}
	}
	if link == "" {
		return unavailable
	}
	message := fmt.Sprintf("Счёт №%s\nСумма по счёту: %s\nОстаток: %s\nСрок оплаты: %s включительно.\nОткрыть счёт: %s\nСведения на момент запроса. Перед переводом проверьте актуальный остаток по ссылке.", invoice.Number, amount, balance, invoice.DueDate, link)
	if invoice.StatusID == "4" && balance == "0" {
		return message + "\nВ Invoice Ninja счёт отмечен оплаченным. Это не заменяет проверку платежа и само по себе не включает доступ."
	}
	if invoice.StatusID != "2" && invoice.StatusID != "3" {
		return unavailable
	}
	if balance == "0" {
		return message + "\nНулевой остаток сам по себе не подтверждает оплату. Проверку должен завершить администратор."
	}
	if s.cfg.PaymentPhone == "" {
		return message + "\nРеквизиты перевода в боте не настроены. Уточните их у администратора."
	}
	message += "\nСумма к переводу: " + balance + "\nТелефон: " + s.cfg.PaymentPhone
	if s.cfg.PaymentBank != "" {
		message += "\nБанк: " + s.cfg.PaymentBank
	}
	if s.cfg.PaymentRecipient != "" {
		message += "\nПолучатель: " + s.cfg.PaymentRecipient
	}
	return message + "\nПри просрочке этот счёт сохраняется и может быть оплачен позднее. Доступ возможен только после подтверждения полной оплаты."
}

func text(s string, max int) bool {
	return utf8.ValidString(s) && strings.TrimSpace(s) == s && utf8.RuneCountInString(s) <= max && strings.IndexFunc(s, unicode.IsControl) < 0
}
func date(s string) bool {
	d, err := time.Parse(time.DateOnly, s)
	return err == nil && d.Year() > 0 && d.Format(time.DateOnly) == s
}
func invoiceLink(raw string) bool {
	if raw == "" || len(raw) > 2048 || strings.TrimSpace(raw) != raw || strings.IndexFunc(raw, unicode.IsControl) >= 0 {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Fragment == "" && u.Opaque == ""
}
