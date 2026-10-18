// Package clientmenu provides read-only, actor-scoped client replies. It never
// creates billing records, grants access or returns subscription credentials.
package clientmenu

import (
	"context"
	"fmt"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
)

type Action string

const (
	Start              Action = "start"
	Home               Action = "menu"
	Invoice            Action = "invoice"
	Status             Action = "status"
	Configuration      Action = "config"
	Help               Action = "help"
	InvoiceLabel              = "Текущий счёт и реквизиты"
	StatusLabel               = "Состояние подписки"
	ConfigurationLabel        = "Моя конфигурация"
	HelpLabel                 = "Инструкция подключения"
)

func (a Action) Valid() bool {
	return a == Start || a == Home || a == Invoice || a == Status || a == Configuration || a == Help
}

// Request identity is derived solely from a verified private Telegram message.
// No invoice/customer/subscription identifier is accepted from message text.
type Request struct {
	BotID, UpdateID, TelegramID, ChatID int64
	Action                              Action
}

func (r Request) Valid() bool {
	return r.BotID > 0 && r.UpdateID >= 0 && r.TelegramID > 0 && r.ChatID == r.TelegramID && r.Action.Valid()
}
func (r Request) Key() string { return fmt.Sprintf("client-menu:%d:%d", r.BotID, r.UpdateID) }

// Snapshot is comparable and is checked again inside the commit transaction.
// Never log it: external bindings and account state are private.
type Snapshot struct {
	Known                        bool
	ClientID                     int64
	CustomerID, InvoiceID, SUIID string
	State                        string
	DesiredAccess                bool
	PaymentReview                bool
	PaymentDate, NextInvoiceDate string
}

type Prepared struct {
	Request  Request
	Snapshot Snapshot
	Text     string
}
type Reader interface {
	GetCustomer(context.Context, string) (invoiceninja.Customer, error)
	GetInvoice(context.Context, string) (invoiceninja.Invoice, error)
}
