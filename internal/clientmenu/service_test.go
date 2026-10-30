package clientmenu_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/PlumpAlbert/payplump-bot/internal/clientmenu"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
	"github.com/PlumpAlbert/payplump-bot/templates"
)

// All identities, payment details and remote data in this file are synthetic.
const menuTelegramID int64 = 9007199254740993

const (
	menuNumber = "SYNTHETIC-INVOICE-PRIVATE"
	menuLink   = "https://billing.example.invalid/invoice/synthetic-private-token"
	menuPhone  = "synthetic-payment-phone"
	menuPII    = "synthetic-remote-private-name-and-error"
)

type fakeReader struct {
	customer   invoiceninja.Customer
	invoice    invoiceninja.Invoice
	calls      []string
	onCustomer func(context.Context, string) (invoiceninja.Customer, error)
	onInvoice  func(context.Context, string) (invoiceninja.Invoice, error)
}

var _ clientmenu.Reader = (*fakeReader)(nil)

func (f *fakeReader) GetCustomer(ctx context.Context, id string) (invoiceninja.Customer, error) {
	f.calls = append(f.calls, "customer:"+id)
	if f.onCustomer != nil {
		return f.onCustomer(ctx, id)
	}
	return f.customer, nil
}

func (f *fakeReader) GetInvoice(ctx context.Context, id string) (invoiceninja.Invoice, error) {
	f.calls = append(f.calls, "invoice:"+id)
	if f.onInvoice != nil {
		return f.onInvoice(ctx, id)
	}
	return f.invoice, nil
}

func menuRequest(action clientmenu.Action) clientmenu.Request {
	return clientmenu.Request{BotID: 42, UpdateID: 17, TelegramID: menuTelegramID, ChatID: menuTelegramID, Action: action}
}

func menuSnapshot() clientmenu.Snapshot {
	return clientmenu.Snapshot{
		Known: true, ClientID: 71, CustomerID: "synthetic-local-customer", InvoiceID: "synthetic-local-invoice",
		SUIID: "synthetic-sui-id-never-a-credential", State: "awaiting_payment", DesiredAccess: false,
		PaymentDate: "2024-01-02", NextInvoiceDate: "2024-02-03",
	}
}

func menuConfig() clientmenu.Config {
	return clientmenu.Config{PaymentPhone: menuPhone, PaymentBank: "synthetic-bank", PaymentRecipient: "synthetic-recipient"}
}

func menuReader() *fakeReader {
	s := menuSnapshot()
	return &fakeReader{
		customer: invoiceninja.Customer{ID: s.CustomerID, IDNumber: strconv.FormatInt(menuTelegramID, 10), Name: menuPII, PrivateNotes: menuPII},
		invoice: invoiceninja.Invoice{
			ID: s.InvoiceID, ClientID: s.CustomerID, StatusID: "2", Number: menuNumber,
			Date: "2020-01-02", DueDate: "2020-01-10", Amount: json.Number("1234.50"), Balance: json.Number("1234.50"),
			Invitations: []invoiceninja.Invitation{{ID: "synthetic-invitation", Link: menuLink}}, PrivateNotes: menuPII,
		},
	}
}

func newMenuService(t *testing.T, cfg clientmenu.Config, reader clientmenu.Reader) *clientmenu.Service {
	t.Helper()
	s, err := clientmenu.New(cfg, reader)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func prepareMenu(t *testing.T, s *clientmenu.Service, action clientmenu.Action, snapshot clientmenu.Snapshot) clientmenu.Prepared {
	t.Helper()
	r := menuRequest(action)
	p, err := s.Prepare(context.Background(), r, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if p.Request != r || p.Snapshot != snapshot {
		t.Fatal("Prepare changed authenticated request or commit-time snapshot")
	}
	assertMenuBody(t, p.Text)
	assertNoMenuText(t, p.Text, snapshot.SUIID, menuPII)
	return p
}

func assertMenuBody(t *testing.T, text string) {
	t.Helper()
	if text == "" || !utf8.ValidString(text) || utf8.RuneCountInString(text) > 4096 || strings.ContainsRune(text, '\x00') {
		t.Fatalf("invalid Telegram body: %d runes", utf8.RuneCountInString(text))
	}
}

func assertMenuContains(t *testing.T, text string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("reply does not contain %q: %s", want, text)
		}
	}
}

func assertNoMenuText(t *testing.T, text string, forbidden ...string) {
	t.Helper()
	for _, value := range forbidden {
		if value != "" && strings.Contains(text, value) {
			t.Errorf("reply leaked forbidden synthetic value %q", value)
		}
	}
}

func assertNoInvoiceDisclosure(t *testing.T, text string) {
	t.Helper()
	assertNoMenuText(t, text, menuNumber, menuLink, menuPhone, "synthetic-bank", "synthetic-recipient", menuPII, menuCopy("payment_details", templates.Data{Balance: "1234.5", PaymentPhone: menuPhone}))
}

func assertMenuCalls(t *testing.T, f *fakeReader, wants ...string) {
	t.Helper()
	if len(f.calls) != len(wants) {
		t.Fatalf("remote calls = %v, want %v", f.calls, wants)
	}
	for i := range wants {
		if f.calls[i] != wants[i] {
			t.Fatalf("remote calls = %v, want %v", f.calls, wants)
		}
	}
}

func TestMenuRequestRequiresVerifiedPrivateIdentity(t *testing.T) {
	for _, a := range []clientmenu.Action{clientmenu.Start, clientmenu.Home, clientmenu.Invoice, clientmenu.Status, clientmenu.Configuration, clientmenu.Help} {
		if !a.Valid() || !menuRequest(a).Valid() {
			t.Errorf("valid private actor/action rejected: %s", a)
		}
	}
	for _, a := range []clientmenu.Action{"", "unknown", "/invoice", "invoice synthetic-other-id", "start@foreign_bot"} {
		if a.Valid() || menuRequest(a).Valid() {
			t.Errorf("unparsed text accepted as an action: %q", a)
		}
		f := menuReader()
		p, err := newMenuService(t, menuConfig(), f).Prepare(context.Background(), menuRequest(a), menuSnapshot())
		if err == nil || p.Text != "" {
			t.Error("unparsed action produced a reply")
		}
		assertMenuCalls(t, f)
	}
	for name, change := range map[string]func(*clientmenu.Request){
		"zero bot":               func(r *clientmenu.Request) { r.BotID = 0 },
		"negative bot":           func(r *clientmenu.Request) { r.BotID = -1 },
		"negative update":        func(r *clientmenu.Request) { r.UpdateID = -1 },
		"zero actor":             func(r *clientmenu.Request) { r.TelegramID = 0 },
		"negative actor":         func(r *clientmenu.Request) { r.TelegramID = -1; r.ChatID = -1 },
		"group":                  func(r *clientmenu.Request) { r.ChatID = -10042 },
		"different private chat": func(r *clientmenu.Request) { r.ChatID++ },
		"missing chat":           func(r *clientmenu.Request) { r.ChatID = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			r := menuRequest(clientmenu.Invoice)
			change(&r)
			if r.Valid() {
				t.Fatal("invalid private identity accepted")
			}
			f := menuReader()
			p, err := newMenuService(t, menuConfig(), f).Prepare(context.Background(), r, menuSnapshot())
			if err == nil || p.Text != "" || p.Request != r || p.Snapshot != menuSnapshot() {
				t.Fatal("invalid request produced a reply or changed its evidence")
			}
			assertMenuCalls(t, f)
		})
	}
	r := menuRequest(clientmenu.Start)
	r.UpdateID = 0
	if !r.Valid() || r.Key() != "client-menu:42:0" {
		t.Fatal("update zero must be a valid, bot-scoped idempotency key")
	}
	r.BotID, r.UpdateID, r.TelegramID, r.ChatID = math.MaxInt64, math.MaxInt64, math.MaxInt64, math.MaxInt64
	if !r.Valid() {
		t.Fatal("valid int64 Telegram identity lost precision")
	}

	// The parser supplies identity, not arbitrary customer/invoice IDs or text.
	typ := reflect.TypeOf(clientmenu.Request{})
	allowed := map[string]bool{"BotID": true, "UpdateID": true, "TelegramID": true, "ChatID": true, "Action": true}
	if typ.NumField() != len(allowed) {
		t.Fatal("Request acquired an untrusted text/external-ID input")
	}
	for i := 0; i < typ.NumField(); i++ {
		if !allowed[typ.Field(i).Name] {
			t.Errorf("unexpected Request input %s", typ.Field(i).Name)
		}
	}
}

func TestMenuKnownAndUnknownActionsAreLocal(t *testing.T) {
	for _, known := range []bool{false, true} {
		for _, action := range []clientmenu.Action{clientmenu.Start, clientmenu.Home, clientmenu.Status, clientmenu.Configuration, clientmenu.Help} {
			t.Run(fmt.Sprintf("known=%v/%s", known, action), func(t *testing.T) {
				f := menuReader()
				snapshot := menuSnapshot()
				snapshot.Known = known
				p := prepareMenu(t, newMenuService(t, menuConfig(), f), action, snapshot)
				assertMenuCalls(t, f)
				assertNoInvoiceDisclosure(t, p.Text)
				assertNoMenuText(t, p.Text, snapshot.CustomerID, snapshot.InvoiceID, "<b>", "</", "**", "```", "[Инструкция](")
				switch {
				case action == clientmenu.Help:
					assertMenuContains(t, p.Text, menuCopy("instructions"))
				case !known && action != clientmenu.Start:
					assertMenuContains(t, p.Text, "/start", "/help")
				case !known:
					assertMenuContains(t, p.Text, menuCopy("welcome"), menuCopy("actions"))
				case action == clientmenu.Start || action == clientmenu.Home:
					assertMenuContains(t, p.Text, menuCopy("actions"), menuCopy("status_footer"))
				case action == clientmenu.Status:
					assertMenuContains(t, p.Text, menuCopy("access_denied"), menuCopy("payment_date", templates.Data{PaymentDate: snapshot.PaymentDate}), menuCopy("next_invoice_date", templates.Data{NextInvoiceDate: snapshot.NextInvoiceDate}))
				case action == clientmenu.Configuration:
					assertMenuContains(t, p.Text, menuCopy("configuration_unavailable"))
				}
			})
		}
	}
	f := menuReader()
	snapshot := menuSnapshot()
	snapshot.Known = false
	p := prepareMenu(t, newMenuService(t, menuConfig(), f), clientmenu.Invoice, snapshot)
	assertMenuContains(t, p.Text, "/start", "/help")
	assertNoInvoiceDisclosure(t, p.Text)
	assertMenuCalls(t, f)
}

func TestMenuStatusAndConfigurationNeverClaimLiveSUIAccess(t *testing.T) {
	for _, state := range []string{"awaiting_payment", "active", "overdue", "synthetic-unknown-state"} {
		for _, access := range []bool{false, true} {
			snapshot := menuSnapshot()
			snapshot.State, snapshot.DesiredAccess = state, access
			f := menuReader()
			s := newMenuService(t, menuConfig(), f)
			status := prepareMenu(t, s, clientmenu.Status, snapshot).Text
			if access {
				assertMenuContains(t, status, menuCopy("access_requested"))
			} else {
				assertMenuContains(t, status, menuCopy("access_denied"))
			}
			if state == "synthetic-unknown-state" {
				assertMenuContains(t, status, menuCopy("state_unknown"))
				assertNoMenuText(t, status, state)
			}
			configuration := prepareMenu(t, s, clientmenu.Configuration, snapshot).Text
			assertMenuContains(t, configuration, menuCopy("configuration_unavailable"))
			assertNoMenuText(t, configuration, "https://", "ss://", "vmess://", "vless://", "trojan://", "password", "token=")
			assertMenuCalls(t, f)
		}
	}
	snapshot := menuSnapshot()
	snapshot.PaymentDate, snapshot.NextInvoiceDate = menuPII, "2024-02-30"
	p := prepareMenu(t, newMenuService(t, menuConfig(), nil), clientmenu.Status, snapshot)
	assertNoMenuText(t, p.Text, snapshot.PaymentDate, snapshot.NextInvoiceDate)
}

func TestMenuHelpIsCompiledStaticAndAvailableToEveryone(t *testing.T) {
	var want string
	for _, known := range []bool{false, true} {
		for _, excluded := range []bool{false, true} {
			cfg := menuConfig()
			if excluded {
				cfg.ExcludedTelegramIDs = []int64{menuTelegramID}
			}
			f := menuReader()
			snapshot := menuSnapshot()
			snapshot.Known = known
			p := prepareMenu(t, newMenuService(t, cfg, f), clientmenu.Help, snapshot)
			if want == "" {
				want = p.Text
			}
			if p.Text != want {
				t.Fatal("static help depends on actor, exclusion or remote state")
			}
			assertNoInvoiceDisclosure(t, p.Text)
			assertMenuCalls(t, f)
		}
	}
}

func TestMenuExclusionsDoNotDependOnBillingOrReaderConfiguration(t *testing.T) {
	// Empty payment configuration and nil Reader model disabled/unconfigured billing.
	for _, configured := range []bool{false, true} {
		for _, known := range []bool{false, true} {
			for _, action := range []clientmenu.Action{clientmenu.Start, clientmenu.Home, clientmenu.Invoice, clientmenu.Status, clientmenu.Configuration} {
				cfg := clientmenu.Config{ExcludedTelegramIDs: []int64{menuTelegramID}}
				f := menuReader()
				var reader clientmenu.Reader
				if configured {
					cfg.PaymentPhone = menuPhone
					reader = f
				}
				snapshot := menuSnapshot()
				snapshot.Known = known
				p := prepareMenu(t, newMenuService(t, cfg, reader), action, snapshot)
				assertMenuContains(t, p.Text, menuCopy("excluded"))
				assertNoInvoiceDisclosure(t, p.Text)
				assertMenuCalls(t, f)
			}
		}
	}
}

func TestMenuNewClonesExclusions(t *testing.T) {
	for _, initiallyExcluded := range []bool{false, true} {
		cfg := menuConfig()
		cfg.ExcludedTelegramIDs = []int64{menuTelegramID + 1}
		if initiallyExcluded {
			cfg.ExcludedTelegramIDs[0] = menuTelegramID
		}
		f := menuReader()
		s := newMenuService(t, cfg, f)
		if initiallyExcluded {
			cfg.ExcludedTelegramIDs[0] = menuTelegramID + 1
		} else {
			cfg.ExcludedTelegramIDs[0] = menuTelegramID
		}
		p := prepareMenu(t, s, clientmenu.Invoice, menuSnapshot())
		if initiallyExcluded {
			assertMenuContains(t, p.Text, menuCopy("excluded"))
			assertMenuCalls(t, f)
		} else {
			assertMenuContains(t, p.Text, menuNumber)
			assertMenuCalls(t, f, "customer:"+menuSnapshot().CustomerID, "invoice:"+menuSnapshot().InvoiceID)
		}
	}
}

func TestMenuInvoiceMissingReaderOrBindingsIsSafe(t *testing.T) {
	for _, missing := range []string{"reader", "customer", "invoice", "both"} {
		t.Run(missing, func(t *testing.T) {
			snapshot := menuSnapshot()
			f := menuReader()
			var reader clientmenu.Reader = f
			switch missing {
			case "reader":
				reader = nil
			case "customer":
				snapshot.CustomerID = ""
			case "invoice":
				snapshot.InvoiceID = ""
			case "both":
				snapshot.CustomerID, snapshot.InvoiceID = "", ""
			}
			p := prepareMenu(t, newMenuService(t, menuConfig(), reader), clientmenu.Invoice, snapshot)
			want := "invoice_pending"
			if missing == "reader" {
				want = "invoice_unavailable"
			}
			assertMenuContains(t, p.Text, menuCopy(want))
			assertNoInvoiceDisclosure(t, p.Text)
			assertMenuCalls(t, f)
		})
	}
}

func TestMenuInvoiceChecksCustomerBeforeReadingOnlyTheBoundInvoice(t *testing.T) {
	changes := []struct {
		name   string
		change func(*invoiceninja.Customer)
	}{
		{"foreign ID", func(c *invoiceninja.Customer) { c.ID = "synthetic-foreign-customer" }},
		{"missing ID", func(c *invoiceninja.Customer) { c.ID = "" }},
		{"foreign actor", func(c *invoiceninja.Customer) { c.IDNumber = "42" }},
		{"missing actor", func(c *invoiceninja.Customer) { c.IDNumber = "" }},
		{"leading zero", func(c *invoiceninja.Customer) { c.IDNumber = "0" + c.IDNumber }},
		{"plus prefix", func(c *invoiceninja.Customer) { c.IDNumber = "+" + c.IDNumber }},
		{"whitespace", func(c *invoiceninja.Customer) { c.IDNumber += " " }},
		{"exponent", func(c *invoiceninja.Customer) { c.IDNumber = "9.007199254740993e15" }},
		{"rounded float", func(c *invoiceninja.Customer) { c.IDNumber = "9007199254740992" }},
		{"wrong identity field", func(c *invoiceninja.Customer) { c.CustomValue1 = c.IDNumber; c.Number = c.IDNumber; c.IDNumber = "" }},
		{"deleted", func(c *invoiceninja.Customer) { c.IsDeleted = true }},
		{"archived", func(c *invoiceninja.Customer) { c.ArchivedAt = 1 }},
		{"negative archive", func(c *invoiceninja.Customer) { c.ArchivedAt = -1 }},
	}
	for _, tt := range changes {
		t.Run(tt.name, func(t *testing.T) {
			f := menuReader()
			tt.change(&f.customer)
			p := prepareMenu(t, newMenuService(t, menuConfig(), f), clientmenu.Invoice, menuSnapshot())
			assertNoInvoiceDisclosure(t, p.Text)
			assertMenuCalls(t, f, "customer:"+menuSnapshot().CustomerID)
		})
	}
	f := menuReader()
	p := prepareMenu(t, newMenuService(t, menuConfig(), f), clientmenu.Invoice, menuSnapshot())
	assertMenuContains(t, p.Text, menuNumber, menuLink, menuPhone)
	assertNoMenuText(t, p.Text, f.customer.ID, f.invoice.ID)
	assertMenuCalls(t, f, "customer:"+menuSnapshot().CustomerID, "invoice:"+menuSnapshot().InvoiceID)
}

func TestMenuInvoiceMalformedOrForeignDataFailsClosed(t *testing.T) {
	changes := []struct {
		name   string
		change func(*invoiceninja.Invoice)
	}{
		{"foreign invoice", func(i *invoiceninja.Invoice) { i.ID = "synthetic-foreign-invoice" }},
		{"missing invoice ID", func(i *invoiceninja.Invoice) { i.ID = "" }},
		{"foreign customer", func(i *invoiceninja.Invoice) { i.ClientID = "synthetic-foreign-customer" }},
		{"missing customer", func(i *invoiceninja.Invoice) { i.ClientID = "" }},
		{"deleted", func(i *invoiceninja.Invoice) { i.IsDeleted = true }},
		{"archived", func(i *invoiceninja.Invoice) { i.ArchivedAt = 1 }},
		{"negative archive", func(i *invoiceninja.Invoice) { i.ArchivedAt = -1 }},
		{"unknown status", func(i *invoiceninja.Invoice) { i.StatusID = "99" }},
		{"missing status", func(i *invoiceninja.Invoice) { i.StatusID = "" }},
		{"noncanonical status", func(i *invoiceninja.Invoice) { i.StatusID = "02" }},
		{"paid with positive balance", func(i *invoiceninja.Invoice) { i.StatusID = "4" }},
		{"missing number", func(i *invoiceninja.Invoice) { i.Number = "" }},
		{"long number", func(i *invoiceninja.Invoice) { i.Number = strings.Repeat("я", 129) }},
		{"number controls", func(i *invoiceninja.Invoice) { i.Number += "\n" }},
		{"number invalid UTF8", func(i *invoiceninja.Invoice) { i.Number += string([]byte{0xff}) }},
		{"missing amount", func(i *invoiceninja.Invoice) { i.Amount = "" }},
		{"negative amount", func(i *invoiceninja.Invoice) { i.Amount = "-1234.50" }},
		{"negative balance", func(i *invoiceninja.Invoice) { i.Balance = "-1" }},
		{"negative zero", func(i *invoiceninja.Invoice) { i.Balance = "-0" }},
		{"zero amount", func(i *invoiceninja.Invoice) { i.Amount = "0"; i.Balance = "0" }},
		{"exponent zero amount", func(i *invoiceninja.Invoice) { i.Amount = "0.000e2"; i.Balance = "0" }},
		{"balance exceeds amount", func(i *invoiceninja.Invoice) { i.Balance = "1234.500000000000000001" }},
		{"precise balance exceeds amount", func(i *invoiceninja.Invoice) {
			i.Amount = "9007199254740993.000000000000000001"
			i.Balance = "9007199254740993.000000000000000002"
		}},
		{"NaN", func(i *invoiceninja.Invoice) { i.Amount = "NaN" }},
		{"infinity", func(i *invoiceninja.Invoice) { i.Balance = "Infinity" }},
		{"leading zero amount", func(i *invoiceninja.Invoice) { i.Amount = "01234.50" }},
		{"missing balance", func(i *invoiceninja.Invoice) { i.Balance = "" }},
		{"huge exponent", func(i *invoiceninja.Invoice) { i.Amount = "1e9999" }},
		{"unbounded amount", func(i *invoiceninja.Invoice) { i.Amount = json.Number(strings.Repeat("9", 65)) }},
		{"missing due date", func(i *invoiceninja.Invoice) { i.DueDate = "" }},
		{"malformed due date", func(i *invoiceninja.Invoice) { i.DueDate = "2024-02-30" }},
		{"noncanonical due date", func(i *invoiceninja.Invoice) { i.DueDate = "2024-2-3" }},
		{"due timestamp", func(i *invoiceninja.Invoice) { i.DueDate = "2024-02-03T00:00:00Z" }},
		{"zero year", func(i *invoiceninja.Invoice) { i.DueDate = "0000-01-01" }},
		{"malformed invoice date", func(i *invoiceninja.Invoice) { i.Date = "2024-02-30" }},
		{"missing invoice date", func(i *invoiceninja.Invoice) { i.Date = "" }},
		{"due before invoice date", func(i *invoiceninja.Invoice) { i.Date = "2020-01-11" }},
		{"noncanonical invoice date", func(i *invoiceninja.Invoice) { i.Date = "2020-1-2" }},
		{"invoice timestamp", func(i *invoiceninja.Invoice) { i.Date = "2020-01-02T00:00:00Z" }},
		{"invoice zero year", func(i *invoiceninja.Invoice) { i.Date = "0000-01-01" }},
		{"no invitation", func(i *invoiceninja.Invoice) { i.Invitations = nil }},
	}
	for _, tt := range changes {
		t.Run(tt.name, func(t *testing.T) {
			f := menuReader()
			tt.change(&f.invoice)
			p := prepareMenu(t, newMenuService(t, menuConfig(), f), clientmenu.Invoice, menuSnapshot())
			assertNoInvoiceDisclosure(t, p.Text)
			assertMenuContains(t, p.Text, menuCopy("invoice_unavailable"))
			assertMenuCalls(t, f, "customer:"+menuSnapshot().CustomerID, "invoice:"+menuSnapshot().InvoiceID)
		})
	}
}

func TestMenuInvoiceRejectsUnsafeInvitationLinks(t *testing.T) {
	for _, link := range []string{
		"", "http://billing.example.invalid/private", "//billing.example.invalid/private", "/private", "javascript:synthetic-private", "https:opaque",
		"https:///missing-host", "https://user:synthetic-secret@billing.example.invalid/private", menuLink + "#private", menuLink + "\n",
		" " + menuLink, menuLink + "%zz", "https://billing.example.invalid/" + strings.Repeat("x", 2049),
	} {
		t.Run(fmt.Sprintf("link-%d", len(link))+"/"+strconv.Itoa(strings.Count(link, "/")), func(t *testing.T) {
			f := menuReader()
			f.invoice.Invitations = []invoiceninja.Invitation{{Link: link}}
			p := prepareMenu(t, newMenuService(t, menuConfig(), f), clientmenu.Invoice, menuSnapshot())
			assertNoInvoiceDisclosure(t, p.Text)
			assertNoMenuText(t, p.Text, link)
		})
	}
}

func TestMenuInvoiceStatusPaymentAndOverdueSemantics(t *testing.T) {
	for _, tt := range []struct {
		name, status, balance string
		wants                 []string
		payment               bool
	}{
		{"old unpaid", "2", "1234.50", []string{"2020-01-10", menuCopy("payment_details", templates.Data{Balance: "1234.5", PaymentPhone: menuPhone}), menuCopy("invoice_footer")}, true},
		{"partially paid", "3", "234.125", []string{menuInvoiceCopy("1234.5", "234.125", "2020-01-10"), menuCopy("payment_details", templates.Data{Balance: "234.125", PaymentPhone: menuPhone})}, true},
		{"Ninja paid only", "4", "0", []string{menuCopy("invoice_paid")}, false},
		{"sent zero is not payment proof", "2", "0", []string{menuCopy("payment_unverified")}, false},
		{"partial zero is not payment proof", "3", "0e2", []string{menuCopy("payment_unverified")}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := menuReader()
			f.invoice.StatusID, f.invoice.Balance = tt.status, json.Number(tt.balance)
			snapshot := menuSnapshot()
			snapshot.State, snapshot.DesiredAccess = "overdue", false
			p := prepareMenu(t, newMenuService(t, menuConfig(), f), clientmenu.Invoice, snapshot)
			assertMenuContains(t, p.Text, menuNumber, menuLink)
			assertMenuContains(t, p.Text, tt.wants...)
			if tt.payment {
				assertMenuContains(t, p.Text, menuPhone, "synthetic-bank", "synthetic-recipient", menuCopy("invoice_footer"))
			} else {
				assertNoMenuText(t, p.Text, menuPhone, menuCopy("payment_details", templates.Data{Balance: "0", PaymentPhone: menuPhone}))
			}
			if tt.status == "3" {
				assertNoMenuText(t, p.Text, menuCopy("payment_details", templates.Data{Balance: "1234.5", PaymentPhone: menuPhone}))
			}
			assertMenuCalls(t, f, "customer:"+snapshot.CustomerID, "invoice:"+snapshot.InvoiceID)
		})
	}
	for _, tt := range []struct{ status, phrase string }{{"1", menuCopy("invoice_draft")}, {"5", menuCopy("invoice_cancelled")}, {"6", menuCopy("invoice_cancelled")}} {
		t.Run("not-payable-"+tt.status, func(t *testing.T) {
			f := menuReader()
			f.invoice.StatusID = tt.status
			p := prepareMenu(t, newMenuService(t, menuConfig(), f), clientmenu.Invoice, menuSnapshot())
			assertMenuContains(t, p.Text, tt.phrase)
			assertNoInvoiceDisclosure(t, p.Text)
		})
	}
}

func TestManagedRecurringInvoiceUsesCanonicalSentDateForAccessBoundary(t *testing.T) {
	f := menuReader()
	f.invoice.Date, f.invoice.DueDate = "2040-01-03", "2040-02-01"
	snapshot := menuSnapshot()
	snapshot.RecurringID = "recurring-synthetic"
	snapshot.CanonicalSentDate = "2040-01-03"
	p := prepareMenu(t, newMenuService(t, menuConfig(), f), clientmenu.Invoice, snapshot)
	assertMenuContains(t, p.Text,
		menuInvoiceCopy("1234.5", "1234.5", "2040-02-01"),
		menuCopy("access_deadline", templates.Data{SentDate: "2040-01-03"}))
	assertNoMenuText(t, p.Text, menuCopy("access_deadline", templates.Data{SentDate: "2040-02-01"}))

	// A first/ordinary invoice has no lifecycle-owned sent date and gets no
	// invented access boundary from its remote due date or invoice date.
	snapshot.RecurringID, snapshot.CanonicalSentDate = "", ""
	p = prepareMenu(t, newMenuService(t, menuConfig(), f), clientmenu.Invoice, snapshot)
	assertMenuContains(t, p.Text, menuInvoiceCopy("1234.5", "1234.5", "2040-02-01"))
	assertNoMenuText(t, p.Text, menuCopy("access_deadline", templates.Data{SentDate: "2040-01-03"}), "2040-01-03")
}

func TestMenuInvoiceAmountsAreExactDecimalsNotFloats(t *testing.T) {
	for _, tt := range []struct{ amount, balance, wantAmount, wantBalance string }{
		{"9007199254740993.010000000000000001", "9007199254740993.010000000000000001", "9007199254740993.010000000000000001", "9007199254740993.010000000000000001"},
		{"1.234500e3", "2.34125e2", "1234.5", "234.125"},
		{"1e32", "1e-32", "100000000000000000000000000000000", "0.00000000000000000000000000000001"},
		{"99999999999999999999999999999999", "0.000000000000000001", "99999999999999999999999999999999", "0.000000000000000001"},
	} {
		t.Run(tt.amount+"/"+tt.balance, func(t *testing.T) {
			f := menuReader()
			f.invoice.Amount, f.invoice.Balance, f.invoice.StatusID = json.Number(tt.amount), json.Number(tt.balance), "3"
			p := prepareMenu(t, newMenuService(t, menuConfig(), f), clientmenu.Invoice, menuSnapshot())
			assertMenuContains(t, p.Text, tt.wantAmount, tt.wantBalance, menuInvoiceCopy(tt.wantAmount, tt.wantBalance, "2020-01-10"), menuCopy("payment_details", templates.Data{Balance: tt.wantBalance, PaymentPhone: menuPhone}))
		})
	}
}

func TestMenuPaymentSettingsAreOptionalWithoutInventedData(t *testing.T) {
	for _, cfg := range []clientmenu.Config{
		{}, {PaymentBank: "synthetic-bank", PaymentRecipient: "synthetic-recipient"}, {PaymentPhone: menuPhone},
	} {
		p := prepareMenu(t, newMenuService(t, cfg, menuReader()), clientmenu.Invoice, menuSnapshot())
		assertMenuContains(t, p.Text, menuNumber, menuLink)
		if cfg.PaymentPhone == "" {
			assertMenuContains(t, p.Text, menuCopy("payment_details_missing"))
			assertNoMenuText(t, p.Text, menuPhone, cfg.PaymentBank, cfg.PaymentRecipient, menuCopy("payment_bank", templates.Data{PaymentBank: "synthetic-bank"}), menuCopy("payment_recipient", templates.Data{PaymentRecipient: "synthetic-recipient"}))
		} else {
			assertMenuContains(t, p.Text, menuCopy("payment_details", templates.Data{Balance: "1234.5", PaymentPhone: menuPhone}))
			assertNoMenuText(t, p.Text, menuCopy("payment_bank", templates.Data{PaymentBank: "synthetic-bank"}), menuCopy("payment_recipient", templates.Data{PaymentRecipient: "synthetic-recipient"}))
		}
	}
}

func TestMenuConfigValidationBoundsAndControls(t *testing.T) {
	ids := make([]int64, 1000)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	maxCfg := clientmenu.Config{PaymentPhone: strings.Repeat("я", 128), PaymentBank: strings.Repeat("я", 128), PaymentRecipient: strings.Repeat("я", 256), ExcludedTelegramIDs: ids}
	for _, cfg := range []clientmenu.Config{{}, menuConfig(), maxCfg, {ExcludedTelegramIDs: []int64{math.MaxInt64}}} {
		if err := cfg.Validate(); err != nil {
			t.Fatalf("valid config rejected: %v", err)
		}
		newMenuService(t, cfg, nil)
	}
	setters := []struct {
		name string
		max  int
		set  func(*clientmenu.Config, string)
	}{
		{"phone", 128, func(c *clientmenu.Config, v string) { c.PaymentPhone = v }},
		{"bank", 128, func(c *clientmenu.Config, v string) { c.PaymentBank = v }},
		{"recipient", 256, func(c *clientmenu.Config, v string) { c.PaymentRecipient = v }},
	}
	for _, field := range setters {
		for index, invalid := range []string{strings.Repeat("я", field.max+1), " leading", "trailing ", "\n", "\r", "\t", "\x00", "\x7f", "\u0085", string([]byte{0xff})} {
			t.Run(fmt.Sprintf("%s/%d", field.name, index), func(t *testing.T) {
				cfg := menuConfig()
				field.set(&cfg, invalid)
				assertInvalidMenuConfig(t, cfg)
			})
		}
	}
	for _, excluded := range [][]int64{{0}, {-1}, {1, 1}, append(append([]int64(nil), ids...), 1001)} {
		cfg := menuConfig()
		cfg.ExcludedTelegramIDs = excluded
		assertInvalidMenuConfig(t, cfg)
	}
}

func assertInvalidMenuConfig(t *testing.T, cfg clientmenu.Config) {
	t.Helper()
	if err := cfg.Validate(); err == nil {
		t.Error("Config.Validate accepted unsafe settings")
	}
	if s, err := clientmenu.New(cfg, nil); err == nil || s != nil {
		t.Error("New bypassed config validation")
	}
}

func TestMenuReplyFitsTelegramAtEveryValidFieldLimit(t *testing.T) {
	cfg := clientmenu.Config{PaymentPhone: strings.Repeat("я", 128), PaymentBank: strings.Repeat("я", 128), PaymentRecipient: strings.Repeat("я", 256)}
	f := menuReader()
	f.invoice.Number = strings.Repeat("я", 128)
	prefix := "https://billing.example.invalid/"
	f.invoice.Invitations = []invoiceninja.Invitation{{Link: prefix + strings.Repeat("x", 2048-len(prefix))}}
	f.invoice.Amount = json.Number(strings.Repeat("9", 60) + "e+32") // 64-byte accepted numeric lexeme.
	f.invoice.Balance = f.invoice.Amount
	for _, action := range []clientmenu.Action{clientmenu.Start, clientmenu.Home, clientmenu.Invoice, clientmenu.Status, clientmenu.Configuration, clientmenu.Help} {
		p := prepareMenu(t, newMenuService(t, cfg, f), action, menuSnapshot())
		if action == clientmenu.Invoice {
			assertMenuContains(t, p.Text, f.invoice.Number, f.invoice.Invitations[0].Link, cfg.PaymentRecipient)
		}
	}
}

func TestMenuRemoteErrorsAndCancellationDoNotLeakPII(t *testing.T) {
	for _, stage := range []string{"customer", "invoice"} {
		for _, remoteErr := range []error{errors.New(menuPII + " " + menuLink + " " + menuPhone), context.Canceled, context.DeadlineExceeded} {
			t.Run(stage+"/"+remoteErr.Error(), func(t *testing.T) {
				f := menuReader()
				if stage == "customer" {
					f.onCustomer = func(context.Context, string) (invoiceninja.Customer, error) { return f.customer, remoteErr }
				} else {
					f.onInvoice = func(context.Context, string) (invoiceninja.Invoice, error) { return f.invoice, remoteErr }
				}
				p := prepareMenu(t, newMenuService(t, menuConfig(), f), clientmenu.Invoice, menuSnapshot())
				assertNoInvoiceDisclosure(t, p.Text)
				assertNoMenuText(t, p.Text, "context canceled", "context deadline exceeded")
				if stage == "customer" {
					assertMenuCalls(t, f, "customer:"+menuSnapshot().CustomerID)
				} else {
					assertMenuCalls(t, f, "customer:"+menuSnapshot().CustomerID, "invoice:"+menuSnapshot().InvoiceID)
				}
			})
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := menuReader()
	r, snapshot := menuRequest(clientmenu.Invoice), menuSnapshot()
	p, err := newMenuService(t, menuConfig(), f).Prepare(ctx, r, snapshot)
	if !errors.Is(err, context.Canceled) || p.Text != "" || p.Request != r || p.Snapshot != snapshot {
		t.Fatal("already-cancelled request produced output or lost its snapshot")
	}
	assertMenuCalls(t, f)
}

func TestMenuRemoteReadsShareOne1500MillisecondContextBudget(t *testing.T) {
	f := menuReader()
	var firstContext context.Context
	var firstDeadline time.Time
	started := time.Now()
	f.onCustomer = func(ctx context.Context, id string) (invoiceninja.Customer, error) {
		firstContext = ctx
		var ok bool
		firstDeadline, ok = ctx.Deadline()
		if !ok || firstDeadline.After(started.Add(1500*time.Millisecond+25*time.Millisecond)) {
			t.Error("customer read has no bounded 1500 ms budget")
		}
		return f.customer, nil
	}
	f.onInvoice = func(ctx context.Context, id string) (invoiceninja.Invoice, error) {
		deadline, ok := ctx.Deadline()
		if !ok || !deadline.Equal(firstDeadline) {
			t.Error("invoice read restarted the remote context budget")
		}
		return f.invoice, nil
	}
	prepareMenu(t, newMenuService(t, menuConfig(), f), clientmenu.Invoice, menuSnapshot())
	if firstContext == nil || !errors.Is(firstContext.Err(), context.Canceled) {
		t.Fatal("remote context not released after Prepare")
	}

	t.Run("earlier parent deadline and cancellation", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		parentDeadline, _ := ctx.Deadline()
		f := menuReader()
		f.onCustomer = func(remote context.Context, id string) (invoiceninja.Customer, error) {
			deadline, ok := remote.Deadline()
			if !ok || !deadline.Equal(parentDeadline) {
				t.Error("remote budget ignored the shorter parent deadline")
			}
			return f.customer, nil
		}
		f.onInvoice = func(remote context.Context, id string) (invoiceninja.Invoice, error) {
			if deadline, ok := remote.Deadline(); !ok || !deadline.Equal(parentDeadline) {
				t.Error("invoice read restarted the shorter parent budget")
			}
			cancel() // No 1.5-second sleep: verify cancellation through the actual context.
			select {
			case <-remote.Done():
			case <-time.After(time.Second):
				t.Error("parent cancellation did not reach invoice GET")
			}
			return f.invoice, remote.Err()
		}
		r, snapshot := menuRequest(clientmenu.Invoice), menuSnapshot()
		p, err := newMenuService(t, menuConfig(), f).Prepare(ctx, r, snapshot)
		if err != nil || p.Request != r || p.Snapshot != snapshot {
			t.Fatalf("cancellation escaped as a remote error: %v", err)
		}
		assertMenuBody(t, p.Text)
		assertNoInvoiceDisclosure(t, p.Text)
		assertNoMenuText(t, p.Text, "context canceled", "context deadline exceeded")
	})
}

type menuRoundTripFunc func(*http.Request) (*http.Response, error)

func (f menuRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMenuPreparedTextIsRenderedWithoutMarkup(t *testing.T) {
	type expectedMessage struct {
		text string
		menu bool
	}
	wants := make(chan expectedMessage, 2)
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/botsynthetic-menu-token/sendMessage" {
			t.Error("unexpected Telegram renderer request")
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		want := <-wants
		var chatID int64
		var text string
		if json.Unmarshal(body["chat_id"], &chatID) != nil || chatID != menuTelegramID || json.Unmarshal(body["text"], &text) != nil || text != want.text {
			t.Error("renderer changed the private identity or prepared plain text")
		}
		for name := range body {
			if name != "chat_id" && name != "text" && name != "reply_markup" {
				t.Errorf("renderer enabled markup/entities or extra field %s", name)
			}
		}
		if want.menu {
			var markup struct {
				Keyboard [][]string `json:"keyboard"`
			}
			if json.Unmarshal(body["reply_markup"], &markup) != nil || !reflect.DeepEqual(markup.Keyboard, [][]string{
				{clientmenu.InvoiceLabel}, {clientmenu.StatusLabel}, {clientmenu.ConfigurationLabel}, {clientmenu.HelpLabel},
			}) {
				t.Error("renderer keyboard contains something other than static labels")
			}
			assertNoMenuText(t, string(body["reply_markup"]), "https://", "callback_data", "synthetic-local", "synthetic-sui")
		} else if _, ok := body["reply_markup"]; ok {
			t.Error("plain sender unexpectedly added reply markup")
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"result":{"message_id":42,"chat":{"id":%d,"type":"private"}}}`, menuTelegramID)
	}))
	defer s.Close()

	// The Telegram constructor has no endpoint option. In this sequential test,
	// intercept its default transport and route ONLY to this loopback httptest
	// server. Every other destination is rejected; no live API can be contacted.
	original := http.DefaultTransport
	localTransport := http.DefaultTransport.(*http.Transport).Clone()
	localTransport.Proxy = nil
	defer localTransport.CloseIdleConnections()
	target, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	http.DefaultTransport = menuRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "api.telegram.org" || r.URL.Path != "/botsynthetic-menu-token/sendMessage" {
			return nil, errors.New("test blocked non-local or unexpected HTTP request")
		}
		copy := r.Clone(r.Context())
		u := *r.URL
		u.Scheme, u.Host = target.Scheme, target.Host
		copy.URL, copy.Host = &u, target.Host
		return localTransport.RoundTrip(copy)
	})
	defer func() { http.DefaultTransport = original }()

	bot := telegram.NewClient("synthetic-menu-token")
	f := menuReader()
	f.invoice.Number = "<b>synthetic</b> & _literal_ [not-markup]"
	svc := newMenuService(t, menuConfig(), f)
	for _, tt := range []struct {
		action clientmenu.Action
		menu   bool
	}{{clientmenu.Help, false}, {clientmenu.Invoice, true}} {
		p := prepareMenu(t, svc, tt.action, menuSnapshot())
		wants <- expectedMessage{text: p.Text, menu: tt.menu}
		send := bot.SendMessage
		if tt.menu {
			send = bot.SendMenuMessage
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		id, err := send(ctx, menuTelegramID, p.Text)
		cancel()
		if err != nil || id != 42 {
			t.Fatalf("plain-text renderer: id=%d, err=%v", id, err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("renderer made %d local calls, want two", calls.Load())
	}
}

// Expected UI copy follows editable templates; precision, routing and disclosure
// assertions above still pin the intended branch and exact display values.
func menuCopy(id string, values ...templates.Data) string {
	var data templates.Data
	if len(values) > 0 {
		data = values[0]
	}
	return templates.Render("clientmenu/"+id, data)
}
func menuInvoiceCopy(amount, balance, dueDate string) string {
	return menuCopy("invoice", templates.Data{InvoiceNumber: menuNumber, Amount: amount, Balance: balance, DueDate: dueDate, InvoiceURL: menuLink})
}
