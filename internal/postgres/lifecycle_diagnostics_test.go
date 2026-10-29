package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestLifecycleStageErrorKeepsSafeDatabaseCategory(t *testing.T) {
	for _, code := range []string{"42P01", "42501", "08006", "40001", "23514"} {
		cause := &pgconn.PgError{Code: code, Message: "private-message", Detail: "private-detail", Where: "private-query", SchemaName: "private-schema", TableName: "private-table"}
		err := invoiceLifecycleStageError("seed paid renewal actions", cause)
		if !strings.Contains(err.Error(), "SQLSTATE "+code) {
			t.Fatal("safe database category lost")
		}
		for _, secret := range []string{"private-message", "private-detail", "private-query", "private-schema", "private-table"} {
			if strings.Contains(err.Error(), secret) {
				t.Fatal("private database details exposed")
			}
		}
	}
	for _, code := range []string{"", "private", "42P01\nprivate"} {
		err := invoiceLifecycleStageError("local stage", &pgconn.PgError{Code: code, Detail: "private"})
		if err.Error() != "local stage" {
			t.Fatal("invalid database category exposed")
		}
	}
	for _, sentinel := range []error{queue.ErrConflict, queue.ErrLeaseLost, context.Canceled, context.DeadlineExceeded} {
		err := invoiceLifecycleStageError("local stage", errors.Join(sentinel, errors.New("private-message")))
		if !errors.Is(err, sentinel) || strings.Contains(err.Error(), "private-message") {
			t.Fatal("sentinel or redaction contract changed")
		}
	}
	if err := invoiceLifecycleStageError("local stage", &pgconn.PgError{Code: "23505", Detail: "private"}); !errors.Is(err, queue.ErrConflict) {
		t.Fatal("uniqueness conflict classification lost")
	}
}
