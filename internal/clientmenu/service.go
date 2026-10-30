package clientmenu

import (
	"context"
	"errors"
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
	"github.com/PlumpAlbert/payplump-bot/templates"
)

var ErrStaleSnapshot = errors.New("client menu snapshot changed")

type Config struct {
	PaymentPhone, PaymentBank, PaymentRecipient string
	ExcludedTelegramIDs                         []int64
	ConfigurationEnabled                        bool
	ConfigurationManagedClients                 bool
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
		if c.ConfigurationPolicyRevision < 1 || c.ConfigurationPolicyHash == ([32]byte{}) || (!c.ConfigurationManagedClients && (len(c.ConfigurationAllowedTelegramIDs) < 1 || len(c.ConfigurationAllowedTelegramIDs) > 20)) {
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

var unavailable = templates.Render("clientmenu/invoice_unavailable", templates.Data{})
var instructions = templates.Render("clientmenu/instructions", templates.Data{})

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
			result.Text = templates.Render("clientmenu/excluded", templates.Data{})
			return result, nil
		}
	}
	if !snapshot.Known && r.Action != Start {
		result.Text = templates.Render("clientmenu/register_first", templates.Data{})
		return result, nil
	}
	switch r.Action {
	case Start, Home:
		if !snapshot.Known {
			result.Text = templates.Render("clientmenu/welcome", templates.Data{})
		} else {
			result.Text = templates.Render("clientmenu/home", templates.Data{StatusText: statusText(snapshot)})
		}
		result.Text += templates.Render("clientmenu/actions", templates.Data{})
	case Status:
		result.Text = statusText(snapshot)
	case Configuration:
		result.Text = ConfigurationUnavailableText
		if s.cfg.ConfigurationEnabled {
			if (s.cfg.ConfigurationManagedClients || slices.Contains(s.cfg.ConfigurationAllowedTelegramIDs, r.TelegramID)) && snapshot.Known && snapshot.State == "active" && snapshot.DesiredAccess && !snapshot.PaymentReview && snapshot.SUIID != "" && snapshot.RecurringID != "" {
				result.ProtectedConfiguration = true
				result.ConfigurationPolicyRevision = s.cfg.ConfigurationPolicyRevision
				result.ConfigurationPolicyHash = s.cfg.ConfigurationPolicyHash
				result.Text = queue.ConfigurationIntentBody
			}
		}
	case Invoice:
		if snapshot.CustomerID == "" || snapshot.InvoiceID == "" {
			result.Text = templates.Render("clientmenu/invoice_pending", templates.Data{})
			break
		}
		result.Text = s.invoice(ctx, r, snapshot)
	}
	return result, nil
}

func statusText(s Snapshot) string {
	state := map[string]string{"awaiting_payment": templates.Render("clientmenu/state_awaiting_payment", templates.Data{}), "active": templates.Render("clientmenu/state_active", templates.Data{}), "overdue": templates.Render("clientmenu/state_overdue", templates.Data{})}[s.State]
	if state == "" {
		state = templates.Render("clientmenu/state_unknown", templates.Data{})
	}
	if s.State == "awaiting_payment" && date(s.PaymentDate) {
		state = templates.Render("clientmenu/state_setting_up", templates.Data{})
	}
	if s.PaymentReview {
		state = templates.Render("clientmenu/state_payment_review", templates.Data{})
	}
	message := templates.Render("clientmenu/status", templates.Data{State: state})
	if s.DesiredAccess {
		message += templates.Render("clientmenu/access_requested", templates.Data{})
	} else {
		message += templates.Render("clientmenu/access_denied", templates.Data{})
	}
	if date(s.PaymentDate) {
		message += templates.Render("clientmenu/payment_date", templates.Data{PaymentDate: s.PaymentDate})
	}
	if date(s.NextInvoiceDate) {
		message += templates.Render("clientmenu/next_invoice_date", templates.Data{NextInvoiceDate: s.NextInvoiceDate})
	}
	return message + templates.Render("clientmenu/status_footer", templates.Data{})
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
		return templates.Render("clientmenu/invoice_draft", templates.Data{})
	}
	if invoice.StatusID == "5" || invoice.StatusID == "6" {
		return templates.Render("clientmenu/invoice_cancelled", templates.Data{})
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
	message := templates.Render("clientmenu/invoice", templates.Data{InvoiceNumber: invoice.Number, Amount: amount, Balance: balance, DueDate: invoice.DueDate, InvoiceURL: link})
	if local.RecurringID != "" && date(local.CanonicalSentDate) {
		message += templates.Render("clientmenu/access_deadline", templates.Data{SentDate: local.CanonicalSentDate})
	}
	if invoice.StatusID == "4" && balance == "0" {
		return message + templates.Render("clientmenu/invoice_paid", templates.Data{})
	}
	if invoice.StatusID != "2" && invoice.StatusID != "3" {
		return unavailable
	}
	if balance == "0" {
		return message + templates.Render("clientmenu/payment_unverified", templates.Data{})
	}
	if s.cfg.PaymentPhone == "" {
		return message + templates.Render("clientmenu/payment_details_missing", templates.Data{})
	}
	message += templates.Render("clientmenu/payment_details", templates.Data{Balance: balance, PaymentPhone: s.cfg.PaymentPhone})
	if s.cfg.PaymentBank != "" {
		message += templates.Render("clientmenu/payment_bank", templates.Data{PaymentBank: s.cfg.PaymentBank})
	}
	if s.cfg.PaymentRecipient != "" {
		message += templates.Render("clientmenu/payment_recipient", templates.Data{PaymentRecipient: s.cfg.PaymentRecipient})
	}
	return message + templates.Render("clientmenu/invoice_footer", templates.Data{})
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
