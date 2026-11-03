//go:build integration

package postgres

import (
	"errors"
	"github.com/PlumpAlbert/payplump-bot/internal/ninjaauthority"
	"testing"
	"time"
)

func TestAuthoritySourceFailurePublicDetailsAndStableDedupe(t *testing.T) {
	s, ctx := authorityFixture(t)
	key := "synthetic-scope:source:sui"
	d := ninjaauthority.SourceFailure{Service: "sui", Operation: "inventory", Category: "http", HTTPStatus: 401}
	notify := func() {
		t.Helper()
		if err := s.AuthoritySourceFailure(ctx, 9123, key, 0, d, []int64{99}); err != nil {
			t.Fatal(err)
		}
	}
	notify()
	notify()
	paymentSQL(t, s, ctx, `SELECT count(*)=1 AND bool_and(body LIKE '%S-UI%' AND body LIKE '%inventory%' AND body LIKE '%401%' AND body LIKE '%'||$1||'%') FROM outbox`, ninjaauthority.NoticeCode(key))
	d.HTTPStatus = 403
	notify()
	d.Category = "timeout"
	d.HTTPStatus = 0
	notify()
	notify()
	paymentSQL(t, s, ctx, `SELECT (SELECT count(*)=3 FROM outbox) AND (SELECT generation=3 FROM authority_notices)`)
	if err := s.ReconcileAuthorityNotices(ctx, 9123, nil, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	notify()
	paymentSQL(t, s, ctx, `SELECT (SELECT count(*)=4 FROM outbox) AND (SELECT generation=4 AND NOT resolved FROM authority_notices)`)
	d.Category = "PRIVATE_PASSWORD"
	if err := s.AuthoritySourceFailure(ctx, 9123, key, 0, d, []int64{99}); !errors.Is(err, ninjaauthority.ErrConflict) {
		t.Fatal("unsafe diagnostic accepted")
	}
	paymentSQL(t, s, ctx, `SELECT count(*)=4 AND NOT bool_or(body LIKE '%PRIVATE%' OR body LIKE '%synthetic-scope%') FROM outbox`)
}
