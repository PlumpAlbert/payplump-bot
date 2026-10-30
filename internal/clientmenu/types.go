// Package clientmenu provides read-only, actor-scoped client replies. It never
// creates billing records or grants access. Configuration requests enqueue only
// a nonsensitive protected intent; its separate dispatcher resolves credentials.
package clientmenu

import (
	"context"
	"fmt"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/templates"
)

type Action string

const (
	Start         Action = "start"
	Home          Action = "menu"
	Invoice       Action = "invoice"
	Status        Action = "status"
	Configuration Action = "config"
	Help          Action = "help"
)

var (
	InvoiceLabel       = templates.Render("clientmenu/label_invoice", templates.Data{})
	StatusLabel        = templates.Render("clientmenu/label_status", templates.Data{})
	ConfigurationLabel = templates.Render("clientmenu/label_configuration", templates.Data{})
	HelpLabel          = templates.Render("clientmenu/label_help", templates.Data{})
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
	RecurringID                  string
	ConfigurationRevision        int64
	State                        string
	DesiredAccess                bool
	PaymentReview                bool
	PaymentDate, NextInvoiceDate string
	CanonicalSentDate            string
}

// ConfigurationUnavailableText is compiled public copy, not request data: an
// ordinary outbox message must never accept a personal URL from prepared text.
var ConfigurationUnavailableText = templates.Render("clientmenu/configuration_unavailable", templates.Data{})

type Prepared struct {
	Request                     Request
	Snapshot                    Snapshot
	Text                        string
	ProtectedConfiguration      bool
	ConfigurationPolicyRevision int64
	ConfigurationPolicyHash     [32]byte
}
type Reader interface {
	GetCustomer(context.Context, string) (invoiceninja.Customer, error)
	GetInvoice(context.Context, string) (invoiceninja.Invoice, error)
}
