package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/clientmenu"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
	"github.com/PlumpAlbert/payplump-bot/templates"
)

type menuStoreFake struct {
	snapshot               clientmenu.Snapshot
	duplicate              bool
	loads, applies, legacy int
	applyErr               error
	last                   clientmenu.Prepared
}

func (s *menuStoreFake) ApplyTelegramUpdate(context.Context, int64, string, telegram.Update) error {
	s.legacy++
	return nil
}
func (s *menuStoreFake) LoadMenuSnapshot(context.Context, int64, int64, int64) (clientmenu.Snapshot, bool, error) {
	s.loads++
	return s.snapshot, s.duplicate, nil
}
func (s *menuStoreFake) ApplyMenuUpdate(_ context.Context, _ int64, _ string, _ telegram.Update, p clientmenu.Prepared) error {
	s.applies++
	s.last = p
	if s.applyErr == nil {
		s.duplicate = true
	}
	return s.applyErr
}

type menuReaderFake struct{ calls int }

func (r *menuReaderFake) GetCustomer(_ context.Context, id string) (invoiceninja.Customer, error) {
	r.calls++
	return invoiceninja.Customer{ID: id, IDNumber: "42"}, nil
}
func (r *menuReaderFake) GetInvoice(_ context.Context, id string) (invoiceninja.Invoice, error) {
	r.calls++
	return invoiceninja.Invoice{ID: id, ClientID: "owned-customer", Number: "TEST-1", Amount: "100.01", Balance: "50.01", StatusID: "3", Date: "2040-02-29", DueDate: "2040-02-29", Invitations: []invoiceninja.Invitation{{Link: "https://synthetic.invalid/invoice/synthetic"}}}, nil
}
func menuUpdate(id int64, text string) telegram.Update {
	return telegram.Update{ID: id, Message: &telegram.Message{From: &telegram.User{ID: 42, FirstName: "Synthetic"}, Chat: telegram.Chat{ID: 42, Type: "private"}, Text: text, Entities: []telegram.Entity{{Type: "bot_command", Offset: 0, Length: len(text)}}}}
}

func TestMenuGatewayDeduplicatesBeforeRemotePreparation(t *testing.T) {
	store := &menuStoreFake{snapshot: clientmenu.Snapshot{Known: true, ClientID: 1, CustomerID: "owned-customer", InvoiceID: "owned-invoice", State: "overdue"}}
	reader := &menuReaderFake{}
	service, _ := clientmenu.New(clientmenu.Config{PaymentPhone: "synthetic-phone"}, reader)
	gateway := &menuGateway{store, service}
	update := menuUpdate(7, "/invoice")
	if err := gateway.ApplyTelegramUpdate(context.Background(), 77, "SyntheticBot", update); err != nil {
		t.Fatal(err)
	}
	if store.applies != 1 || reader.calls != 2 || store.last.Request.TelegramID != 42 || store.last.Request.ChatID != 42 || !strings.Contains(store.last.Text, "50.01") {
		t.Fatal("actor-scoped invoice preparation failed")
	}
	// Simulate a lost acknowledgement after commit, then changed external state.
	store.snapshot = clientmenu.Snapshot{}
	if err := gateway.ApplyTelegramUpdate(context.Background(), 77, "SyntheticBot", update); err != nil {
		t.Fatal(err)
	}
	if store.applies != 1 || reader.calls != 2 {
		t.Fatal("duplicate repeated remote preparation or enqueue")
	}
}

func TestMenuGatewayLeavesUncommittedUpdateForTelegramRetry(t *testing.T) {
	store := &menuStoreFake{applyErr: clientmenu.ErrStaleSnapshot}
	service, _ := clientmenu.New(clientmenu.Config{}, nil)
	gateway := &menuGateway{store, service}
	update := menuUpdate(8, "/help")
	if err := gateway.ApplyTelegramUpdate(context.Background(), 77, "SyntheticBot", update); !errors.Is(err, clientmenu.ErrStaleSnapshot) {
		t.Fatal("stale binding was acknowledged")
	}
	store.applyErr = nil
	if err := gateway.ApplyTelegramUpdate(context.Background(), 77, "SyntheticBot", update); err != nil || store.applies != 2 {
		t.Fatal("uncommitted update did not retry")
	}
	update.Message.Chat.Type = "group"
	if err := gateway.ApplyTelegramUpdate(context.Background(), 77, "SyntheticBot", update); err != nil || store.legacy != 1 {
		t.Fatal("unsupported input did not use durable ignore path")
	}
}

func TestMenuConfigHonorsExclusionsWithoutRegistration(t *testing.T) {
	for _, key := range []string{"PAYMENT_PHONE", "PAYMENT_BANK", "PAYMENT_RECIPIENT", "INVOICE_NINJA_BASE_URL", "INVOICE_NINJA_API_TOKEN"} {
		t.Setenv(key, "")
	}
	t.Setenv("REGISTRATION_ENABLED", "false")
	t.Setenv("REGISTRATION_EXCLUDED_TELEGRAM_IDS", "42")
	gateway, err := newMenuGateway(&menuStoreFake{})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := gateway.service.Prepare(context.Background(), clientmenu.Request{BotID: 77, UpdateID: 1, TelegramID: 42, ChatID: 42, Action: clientmenu.Home}, clientmenu.Snapshot{})
	if err != nil || prepared.Text != templates.Render("clientmenu/excluded", templates.Data{}) {
		t.Fatal("disabled billing discarded menu exclusion policy")
	}
	for _, value := range []string{"042", "42,42", "0", "42,"} {
		t.Setenv("REGISTRATION_EXCLUDED_TELEGRAM_IDS", value)
		if _, err := newMenuGateway(&menuStoreFake{}); err == nil {
			t.Fatal("invalid exclusions accepted")
		}
	}
}
