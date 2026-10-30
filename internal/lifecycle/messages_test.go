package lifecycle

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/billing"
	"github.com/PlumpAlbert/payplump-bot/internal/clientmenu"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/templates"
)

type sentBoundaryMenuReader struct {
	customer invoiceninja.Customer
	invoice  invoiceninja.Invoice
}

func (r sentBoundaryMenuReader) GetCustomer(context.Context, string) (invoiceninja.Customer, error) {
	return r.customer, nil
}

func (r sentBoundaryMenuReader) GetInvoice(context.Context, string) (invoiceninja.Invoice, error) {
	return r.invoice, nil
}

func TestStageOneMenuAndEnforcementUseCanonicalSentDateBoundary(t *testing.T) {
	const sentDate = "2040-01-03"
	const dueDate = "2040-02-01"
	observation := Invoice{
		ID: "synthetic-managed-invoice", Number: "SYNTHETIC-MANAGED", Status: "sent",
		SentDate: sentDate, Amount: "100", Balance: "100", PaidToDate: "0",
	}
	notices := makeNotices(7, nil, Snapshot{ClientID: 1, ChatID: 101}, observation, 0)
	if notices.Client == nil || !strings.Contains(notices.Client.Text, templates.Render("lifecycle/invoice_client", templates.Data{Verb: templates.Render("lifecycle/invoice_issued", templates.Data{}), InvoiceNumber: observation.Number, Balance: "100", SentDate: sentDate})) {
		t.Fatalf("Stage 1 notice omitted sent-date access boundary: %#v", notices.Client)
	}

	reader := sentBoundaryMenuReader{
		customer: invoiceninja.Customer{ID: "customer-1", IDNumber: "101"},
		invoice: invoiceninja.Invoice{
			ID: observation.ID, ClientID: "customer-1", StatusID: "2", Number: observation.Number,
			Date: sentDate, DueDate: dueDate, Amount: json.Number("100"), Balance: json.Number("100"),
			Invitations: []invoiceninja.Invitation{{Link: "https://billing.example.invalid/synthetic"}},
		},
	}
	menu, err := clientmenu.New(clientmenu.Config{}, reader)
	if err != nil {
		t.Fatal(err)
	}
	request := clientmenu.Request{BotID: 7, UpdateID: 1, TelegramID: 101, ChatID: 101, Action: clientmenu.Invoice}
	snapshot := clientmenu.Snapshot{
		Known: true, ClientID: 1, CustomerID: "customer-1", InvoiceID: observation.ID,
		RecurringID: "recurring-1", CanonicalSentDate: sentDate, State: "active", DesiredAccess: true,
	}
	prepared, err := menu.Prepare(context.Background(), request, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prepared.Text, templates.Render("clientmenu/invoice", templates.Data{InvoiceNumber: observation.Number, Amount: "100", Balance: "100", DueDate: dueDate, InvoiceURL: reader.invoice.Invitations[0].Link})) || !strings.Contains(prepared.Text, templates.Render("clientmenu/access_deadline", templates.Data{SentDate: sentDate})) || strings.Contains(prepared.Text, templates.Render("clientmenu/access_deadline", templates.Data{SentDate: dueDate})) {
		t.Fatalf("menu did not separate Invoice Ninja due date from access boundary: %s", prepared.Text)
	}

	// Exercise the same public enforcement validation boundary: it must reject
	// action immediately before the end of sentDate and permit it at the first
	// instant after that full local day.
	deadline, err := billing.PaymentDeadline(sentDate)
	if err != nil {
		t.Fatal(err)
	}
	enforcement := validEnforcementSnapshot()
	enforcement.Invoice.SentDate = sentDate
	if code := validateEnforcementSnapshot(enforcement, 7, []int64{101}, nil, deadline.Add(-time.Nanosecond)); code != ReviewOverdueInvoice {
		t.Fatalf("enforcement crossed sent-date boundary early: review=%d", code)
	}
	if code := validateEnforcementSnapshot(enforcement, 7, []int64{101}, nil, deadline); code != 0 {
		t.Fatalf("enforcement did not become eligible after sent-date boundary: review=%d", code)
	}
}
