//go:build integration

package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
)

type lostCommitResponse struct{ *Store }

func (s lostCommitResponse) ApplyTelegramUpdate(ctx context.Context, id int64, name string, u telegram.Update) error {
	if err := s.Store.ApplyTelegramUpdate(ctx, id, name, u); err != nil {
		return err
	}
	return errors.New("simulated loss of commit acknowledgement")
}

func TestWebhookReplayAfterCommitAndServerRestart(t *testing.T) {
	s, ctx := testStore(t)
	post := func(store telegram.WebhookStore, u telegram.Update) int {
		h, err := telegram.NewWebhookHandler("test-webhook-secret", store)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.Activate(telegram.User{ID: 123, IsBot: true, Username: "PayplumpBot"}); err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(h)
		defer server.Close()
		body, err := json.Marshal(u)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(http.MethodPost, server.URL+telegram.WebhookPath, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(telegram.WebhookSecretHeader, "test-webhook-secret")
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, response.Body)
		return response.StatusCode
	}
	u := privateUpdate(100)
	if status := post(lostCommitResponse{s}, u); status != 503 {
		t.Fatalf("uncertain commit returned %d", status)
	}
	s.pool.Reset()
	u.Message.From.FirstName = "Повтор"
	if status := post(s, u); status != 200 {
		t.Fatalf("replay returned %d", status)
	}
	for _, table := range []string{"clients", "subscriptions", "operations", "telegram_processed_updates"} {
		var count int
		if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("replay changed %s", table)
		}
	}
	var name string
	if err := s.pool.QueryRow(ctx, `SELECT display_name FROM clients`).Scan(&name); err != nil || name != "Клиент" {
		t.Fatal("replayed update repeated business changes")
	}
}

func TestWebhookMigrationPreservesPollingDeduplication(t *testing.T) {
	s, ctx := unmigratedStore(t)
	// Reproduce a database prepared by the prior revision, using its immutable
	// migrations and ledger checksums, then run the new migration normally.
	if _, err := s.pool.Exec(ctx, `CREATE TABLE schema_migrations (version text PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"001_initial.sql", "002_telegram_updates.sql"} {
		data, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(ctx, string(data)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(ctx, `INSERT INTO schema_migrations (version,checksum) VALUES ($1,$2)`, name, fmt.Sprintf("%x", sha256.Sum256(data))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.RegisterPrivateClient(ctx, TelegramIdentity{TelegramID: 4503599627370497, ChatID: 4503599627370497, DisplayName: "Клиент"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO telegram_polling_state (bot_id,next_offset) VALUES (123,101); INSERT INTO telegram_processed_updates (bot_id,update_id) VALUES (123,100)`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	u := privateUpdate(100)
	u.Message.From.FirstName = "Повтор"
	if err := s.ApplyTelegramUpdate(ctx, 123, "PayplumpBot", u); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := s.pool.QueryRow(ctx, `SELECT display_name FROM clients`).Scan(&name); err != nil || name != "Клиент" {
		t.Fatal("migration lost prior deduplication metadata")
	}
	var oldRemoved bool
	if err := s.pool.QueryRow(ctx, `SELECT to_regclass('telegram_polling_state') IS NULL`).Scan(&oldRemoved); err != nil || !oldRemoved {
		t.Fatal("polling table was not migrated")
	}
	// New, lower IDs must be processed even after moving from polling checkpoints.
	if err := s.ApplyTelegramUpdate(ctx, 123, "PayplumpBot", privateUpdate(5)); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM operations`).Scan(&count); err != nil || count != 1 {
		t.Fatal("switching transport duplicated the registration operation")
	}
}
