package postgres

import (
	"context"
	"strings"
	"testing"
)

func TestOpenRejectsInvalidConfiguration(t *testing.T) {
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
