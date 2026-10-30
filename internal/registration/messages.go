package registration

import (
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"github.com/PlumpAlbert/payplump-bot/internal/billing"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/templates"
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
	text := templates.Render("registration/invoice", templates.Data{InvoiceNumber: invoice.Number, Amount: amount, DueDate: invoice.DueDate, PaymentPhone: cfg.PaymentPhone})
	if cfg.PaymentBank != "" {
		text += templates.Render("registration/payment_bank", templates.Data{PaymentBank: cfg.PaymentBank})
	}
	if cfg.PaymentRecipient != "" {
		text += templates.Render("registration/payment_recipient", templates.Data{PaymentRecipient: cfg.PaymentRecipient})
	}
	text += templates.Render("registration/invoice_footer", templates.Data{InvoiceURL: link})
	messages := []queue.Draft{{BotID: s.BotID, ChatID: s.ChatID, Key: "registration:" + s.Token + ":invoice:client", Text: text}}
	name := s.DisplayName
	if strings.TrimSpace(name) == "" {
		name = templates.Render("registration/default_name", templates.Data{})
	}
	adminText := templates.Render("registration/invoice_admin", templates.Data{DisplayName: name, ActorID: strconv.FormatInt(s.TelegramID, 10), InvoiceNumber: invoice.Number, Amount: amount, DueDate: invoice.DueDate, InvoiceURL: link})
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
		ReviewExistingCustomer: templates.Render("registration/review_existing_customer", templates.Data{}),
		ReviewCustomerConflict: templates.Render("registration/review_customer_conflict", templates.Data{}),
		ReviewInvoiceConflict:  templates.Render("registration/review_invoice_conflict", templates.Data{}),
		ReviewUnknownWrite:     templates.Render("registration/review_unknown_write", templates.Data{}),
		ReviewInvoiceState:     templates.Render("registration/review_invoice_state", templates.Data{}),
		ReviewContract:         templates.Render("registration/review_contract", templates.Data{}),
		ReviewLocalAssociation: templates.Render("registration/review_local_association", templates.Data{}),
		ReviewExcludedUser:     templates.Render("registration/review_excluded", templates.Data{}),
	}[code]
	if reason == "" {
		reason = templates.Render("registration/review_default", templates.Data{})
	}
	text := templates.Render("registration/review_admin", templates.Data{OperationID: strconv.FormatInt(operationID, 10), ReviewCode: strconv.Itoa(code), ActorID: strconv.FormatInt(s.TelegramID, 10), Reason: reason})
	var messages []queue.Draft
	if s.ChatID > 0 {
		messages = append(messages, queue.Draft{BotID: s.BotID, ChatID: s.ChatID, Key: "registration:" + s.Token + ":review:client", Text: templates.Render("registration/review_client", templates.Data{})})
	}
	for _, id := range admins {
		messages = append(messages, queue.Draft{BotID: s.BotID, ChatID: id, Key: "registration:" + s.Token + ":review:" + strconv.Itoa(code) + ":admin:" + strconv.FormatInt(id, 10), Text: text})
	}
	return messages
}
