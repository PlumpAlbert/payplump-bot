//go:build integration

package postgres

import (
	"errors"
	"strings"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/ninjaauthority"
)

func TestAuthorityHonorsHistoricalSourceScopeBeforeFirstAdoption(t *testing.T) {
	s, ctx := authorityFixture(t)
	o := authorityObservation(68000)
	d := authorityDraft(t, "68000", true)
	paymentExec(t, s, ctx, `INSERT INTO import_admission_policy(bot_id,revision,policy_hash,source_scope_hash,enabled) VALUES(9123,1,decode($1,'hex'),decode($2,'hex'),false)`, strings.Repeat("c", 64), strings.Repeat("b", 64))
	if err := s.SaveAuthorityObservation(ctx, 9123, o); !errors.Is(err, ninjaauthority.ErrConflict) {
		t.Fatal("historical source scope was overwritten")
	}
	if _, err := s.PrepareAuthorityCreate(ctx, 9123, o, d); !errors.Is(err, ninjaauthority.ErrConflict) {
		t.Fatal("creation ignored historical source scope")
	}
	paymentSQL(t, s, ctx, `SELECT NOT EXISTS(SELECT 1 FROM authority_observations) AND NOT EXISTS(SELECT 1 FROM authority_create_intents) AND NOT EXISTS(SELECT 1 FROM clients WHERE telegram_id=68000)`)
	o.ScopeHash = strings.Repeat("b", 64)
	if err := s.SaveAuthorityObservation(ctx, 9123, o); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareAuthorityCreate(ctx, 9123, o, d); err != nil {
		t.Fatal(err)
	}
	// A new scope permission installed after preparation must still stop dispatch.
	paymentExec(t, s, ctx, `UPDATE import_admission_policy SET revision=2,source_scope_hash=decode($1,'hex') WHERE bot_id=9123`, strings.Repeat("d", 64))
	if err := s.AuthorizeAuthorityCreate(ctx, 9123, o.Actor, o.SourceHash); !errors.Is(err, ninjaauthority.ErrConflict) {
		t.Fatal("scope drift authorized frozen create")
	}
	paymentSQL(t, s, ctx, `SELECT NOT dispatched FROM authority_create_intents`)
}
