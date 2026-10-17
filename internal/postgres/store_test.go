package postgres

import (
	"context"
	"strings"
	"testing"
)

func TestOpenRejectsInvalidConfiguration(t *testing.T) {
	t.Setenv("PGHOST", "")
	t.Setenv("PGUSER", "")
	t.Setenv("PGDATABASE", "")
	for _, tc := range []struct {
		url string
		max int32
	}{
		{"", 4}, {"postgres://localhost/db", 0}, {"postgres://localhost/db", 65},
		{"postgres://user:do-not-log-this@host:invalid/db", 4},
	} {
		s, err := Open(context.Background(), tc.url, tc.max)
		if err == nil {
			s.Close()
			t.Fatal("invalid configuration accepted")
		}
		if strings.Contains(err.Error(), "do-not-log-this") {
			t.Fatal("credential leaked in error")
		}
	}
}

func TestConnectionFromEnvironment(t *testing.T) {
	t.Setenv("PGSERVICE", "")
	t.Setenv("PGHOST", "database.internal")
	t.Setenv("PGPORT", "15432")
	t.Setenv("PGUSER", "test-user")
	t.Setenv("PGDATABASE", "test-database")
	t.Setenv("PGPASSWORD", "test-only:@/ %'\"$\\password")
	t.Setenv("PGSSLMODE", "require")
	cfg, err := connectionConfig("", 3)
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.ConnConfig
	if c.Host != "database.internal" || c.Port != 15432 || c.User != "test-user" || c.Database != "test-database" || c.Password != "test-only:@/ %'\"$\\password" {
		t.Fatal("connection parameters were not preserved")
	}
	if cfg.MaxConns != 3 || c.TLSConfig == nil {
		t.Fatal("pool size or TLS setting lost")
	}
	// A configured URL remains supported and overrides the corresponding env.
	cfg, err = connectionConfig("postgres://other-user:other-password@other-host:25432/other-db?sslmode=disable", 4)
	if err != nil {
		t.Fatal(err)
	}
	c = cfg.ConnConfig
	if c.Host != "other-host" || c.Port != 25432 || c.User != "other-user" || c.Database != "other-db" || c.Password != "other-password" || c.TLSConfig != nil {
		t.Fatal("explicit DATABASE_URL fields did not take precedence")
	}
	t.Setenv("PGPORT", "not-a-port")
	if _, err := connectionConfig("", 4); err == nil || strings.Contains(err.Error(), "test-only") {
		t.Fatal("invalid environment must return a sanitized error")
	}
}

func TestConnectionPreservesSchemaOptions(t *testing.T) {
	// CI smoke tests use a run-specific schema without changing DATABASE_URL.
	t.Setenv("PGOPTIONS", "-c search_path=ci_smoke_test")
	cfg, err := connectionConfig("postgres://test-user@localhost/test-db?sslmode=disable", 4)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConnConfig.RuntimeParams["options"] != "-c search_path=ci_smoke_test" {
		t.Fatal("PGOPTIONS schema isolation was lost")
	}
}

func TestConnectionRequiresExplicitTarget(t *testing.T) {
	for _, missing := range []string{"PGHOST", "PGUSER", "PGDATABASE"} {
		t.Run(missing, func(t *testing.T) {
			t.Setenv("PGHOST", "database.internal")
			t.Setenv("PGUSER", "test-user")
			t.Setenv("PGDATABASE", "test-database")
			t.Setenv(missing, "")
			if _, err := connectionConfig("", 4); err == nil {
				t.Fatal("missing target silently defaulted")
			}
		})
	}
}

func TestRegistrationRejectsNonPrivateIdentity(t *testing.T) {
	// Validation must happen before any database operation.
	s := &Store{}
	for _, identity := range []TelegramIdentity{
		{TelegramID: 0, ChatID: 0}, {TelegramID: -1, ChatID: -1},
		{TelegramID: 1, ChatID: -10}, {TelegramID: 1, ChatID: 2},
		{TelegramID: 1, ChatID: 1, DisplayName: strings.Repeat("я", 257)},
		{TelegramID: 1, ChatID: 1, DisplayName: string([]byte{0xff})},
	} {
		if _, err := s.RegisterPrivateClient(context.Background(), identity); err == nil {
			t.Fatal("invalid identity accepted")
		}
	}
}
