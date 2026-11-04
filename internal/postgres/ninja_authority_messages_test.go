package postgres

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/ninjaauthority"
	"github.com/PlumpAlbert/payplump-bot/templates"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestAuthorityAdminRecipients(t *testing.T) {
	input := []int64{99, 42, 99, 101, 42}
	got, err := authorityAdminRecipients(input)
	if err != nil || !slices.Equal(got, []int64{42, 99, 101}) || !slices.Equal(input, []int64{99, 42, 99, 101, 42}) {
		t.Fatalf("recipients=%v err=%v input=%v", got, err, input)
	}
	got[0] = 1
	if input[1] != 42 {
		t.Fatal("recipients alias caller configuration")
	}
	if got, err := authorityAdminRecipients(nil); err != nil || len(got) != 0 {
		t.Fatal("offline callers may omit admins")
	}
}

func TestAuthorityInvalidAdminsFailBeforeTransaction(t *testing.T) {
	// A nil pool panics if either wrapper tries to begin a transaction.
	s := &Store{}
	for _, admins := range [][]int64{{0}, {-1}, {99, 0}, {99, -42}} {
		if err := s.SaveAuthorityObservation(context.Background(), 77, ninjaauthority.Observation{}, admins...); !errors.Is(err, ninjaauthority.ErrConflict) {
			t.Fatalf("invalid admins accepted: %v", err)
		}
		if err := s.SaveAuthorityObservationWithInvoices(context.Background(), 77, ninjaauthority.Observation{}, nil, admins...); !errors.Is(err, ninjaauthority.ErrConflict) {
			t.Fatalf("invalid admins accepted with invoices: %v", err)
		}
	}
}

type authorityMessageRow struct{ scan func(...any) error }

func (r authorityMessageRow) Scan(dest ...any) error { return r.scan(dest...) }

type authorityMessageTx struct {
	pgx.Tx
	messages [][]any
	failAt   int
	failure  error
	ownFirst bool
	sent     bool
	queries  int
}

func (tx *authorityMessageTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (tx *authorityMessageTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	return authorityMessageRow{scan: func(dest ...any) error {
		if strings.Contains(sql, "INSERT INTO outbox") {
			tx.messages = append(tx.messages, args)
			if tx.failAt == len(tx.messages) {
				return tx.failure
			}
			*dest[0].(*int64) = int64(len(tx.messages))
			return nil
		}
		tx.queries++
		if strings.Contains(sql, "FROM operations") {
			*dest[0].(*bool) = tx.ownFirst
			return nil
		}
		if strings.Contains(sql, "FROM outbox") {
			*dest[0].(*bool) = tx.sent
			return nil
		}
		return errors.New("unexpected authority message query")
	}}
}

func TestAuthorityRoleFanoutAndFailure(t *testing.T) {
	admins, err := authorityAdminRecipients([]int64{99, 42, 99})
	if err != nil {
		t.Fatal(err)
	}
	tx := &authorityMessageTx{}
	if err := enqueueAuthorityClientAndAdmins(context.Background(), tx, 77, 42, "existing-client-key", "client copy", "safe admin summary", admins); err != nil {
		t.Fatal(err)
	}
	if len(tx.messages) != 3 {
		t.Fatalf("messages=%d", len(tx.messages))
	}
	for i, want := range []struct {
		chat      int64
		key, text string
	}{{42, "existing-client-key", "client copy"}, {42, "existing-client-key:admin:42", "safe admin summary"}, {99, "existing-client-key:admin:99", "safe admin summary"}} {
		m := tx.messages[i]
		if m[0] != int64(77) || m[1] != want.chat || m[2] != want.key || m[3] != want.text || m[5] != false {
			t.Fatalf("message %d = %v", i, m)
		}
	}
	failure := errors.New("synthetic enqueue failure")
	for _, at := range []int{1, 2, 3} {
		tx := &authorityMessageTx{failAt: at, failure: failure}
		if err := enqueueAuthorityClientAndAdmins(context.Background(), tx, 77, 42, "key", "client", "admin", admins); !errors.Is(err, failure) || len(tx.messages) != at {
			t.Fatalf("failure not propagated at %d: %v", at, err)
		}
	}
}

func TestAuthorityReadinessHistoricalKeySuppressesAllRoles(t *testing.T) {
	o := ninjaauthority.Observation{Actor: 42, StateKnown: true, Enabled: true, SUIEnabled: true, Binding: ninjaauthority.Binding{SUIID: 7}}
	for _, tc := range []struct {
		own, sent bool
		want      int
	}{{false, false, 0}, {true, true, 0}, {true, false, 3}} {
		tx := &authorityMessageTx{ownFirst: tc.own, sent: tc.sent}
		if err := enqueueAuthorityChanges(context.Background(), tx, 77, 12, 1, nil, o, []int64{42, 99}); err != nil {
			t.Fatal(err)
		}
		if len(tx.messages) != tc.want {
			t.Fatalf("own=%t historical=%t messages=%d", tc.own, tc.sent, len(tx.messages))
		}
		if tc.want != 0 && tx.messages[0][2] != "authority-change:12:0:configuration_ready" {
			t.Fatal("client stable key changed")
		}
	}
	// No actual readiness transition, even though own activation completed.
	o.SUIEnabled = false
	tx := &authorityMessageTx{ownFirst: true}
	if err := enqueueAuthorityChanges(context.Background(), tx, 77, 12, 1, nil, o, []int64{99}); err != nil || tx.queries != 0 || len(tx.messages) != 0 {
		t.Fatal("not-ready observation emitted notification")
	}
}

func TestAuthorityAdminTemplatesOnlyRenderSafeFields(t *testing.T) {
	data := templates.Data{ActorID: "42", InvoiceNumber: "INV-test", Amount: "123.45", Status: "4", SubscriptionURL: "SECRET_URL", Token: "SECRET_TOKEN", Body: "SECRET_BODY", InvoiceURL: "SECRET_INVOICE"}
	for _, name := range []string{"authority/invoice_created_admin", "authority/invoice_updated_admin", "authority/configuration_ready_admin"} {
		text := templates.Render(name, data)
		if !strings.Contains(text, "42") || strings.Contains(text, "SECRET") {
			t.Fatalf("unsafe summary: %s", text)
		}
		if name == "authority/configuration_ready_admin" {
			if strings.Contains(text, "INV-test") || strings.Contains(text, "123.45") {
				t.Fatal("readiness includes invoice data")
			}
		} else if !strings.Contains(text, "INV-test") || !strings.Contains(text, "123.45") || !strings.Contains(text, "Статус в Invoice Ninja: 4") || strings.Contains(text, "₽") || strings.Contains(text, "оплата подтверждена") {
			t.Fatalf("incorrect invoice summary: %s", text)
		}
	}
}
