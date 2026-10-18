package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/configdelivery"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestConfigurationInvalidInputsNeverTouchDatabase(t *testing.T) {
	s, ctx := &Store{}, context.Background()
	hash := sha256.Sum256([]byte("synthetic policy"))
	for _, p := range []configdelivery.Policy{{Enabled: true}, {Enabled: true, Revision: 1}, {Enabled: true, Revision: -1, Hash: hash}, {Revision: 1}, {Hash: hash}} {
		if err := s.InstallConfigurationPolicy(ctx, 123, p); !errors.Is(err, configdelivery.ErrStalePolicy) {
			t.Fatal("accepted invalid policy")
		}
	}
	if err := s.InstallConfigurationPolicy(ctx, 0, configdelivery.Policy{}); err == nil {
		t.Fatal("accepted missing bot")
	}
	base := queue.Message{Lease: queue.Lease{ID: 1, Attempt: 1}, BotID: 123, ChatID: 456, Text: queue.ConfigurationIntentBody, ProtectedConfiguration: true}
	invalid := []queue.Message{{}, base, base, base, base, base, base}
	invalid[1].ProtectedConfiguration = false
	invalid[2].ClientMenu = true
	invalid[3].Text = "untrusted"
	invalid[4].Attempt = 0
	invalid[5].ChatID = 0
	invalid[6].BotID = 0
	for _, m := range invalid {
		if _, err := s.LoadConfiguration(ctx, m); !errors.Is(err, queue.ErrLeaseLost) {
			t.Fatal("accepted invalid protected message")
		}
		if p, err := s.AuthorizeConfiguration(ctx, m, configdelivery.View{}, configdelivery.Decision{Kind: configdelivery.Secret}); !errors.Is(err, queue.ErrLeaseLost) || p.Kind != "" {
			t.Fatal("granted invalid message")
		}
	}
	for _, draft := range []queue.Draft{{ProtectedConfiguration: true}, {Text: queue.ConfigurationIntentBody}} {
		if _, err := s.EnqueueOutbox(ctx, draft); err == nil {
			t.Fatal("generic public enqueue accepted protected intent")
		}
		if _, err := enqueueOutbox(ctx, nil, draft); err == nil {
			t.Fatal("generic transactional enqueue accepted protected intent")
		}
	}
	if _, err := s.AuthorizeConfiguration(ctx, base, configdelivery.View{}, configdelivery.Decision{}); err == nil {
		t.Fatal("accepted invalid decision")
	}
	for _, args := range []struct {
		code     int
		cooldown time.Duration
	}{{-1, 0}, {0, -1}, {1 << 32, 0}} {
		if err := s.FailConfigurationOutbox(ctx, base.Lease, args.code, args.cooldown, true); err == nil {
			t.Fatal("accepted invalid numeric failure")
		}
	}
}

func TestConfigurationSafeErrorsAndImmutableEquality(t *testing.T) {
	for _, sentinel := range []error{queue.ErrLeaseLost, queue.ErrConflict, configdelivery.ErrDenied, configdelivery.ErrAlreadyDispatched, configdelivery.ErrStalePolicy, context.Canceled, context.DeadlineExceeded} {
		err := configurationError(errors.Join(sentinel, errors.New("private SQL JSON value")))
		if !errors.Is(err, sentinel) || strings.Contains(err.Error(), "private") {
			t.Fatal("unsafe sentinel handling")
		}
	}
	for _, err := range []error{errors.New("private SQL"), &pgconn.PgError{Code: "23514", Detail: "private binding"}} {
		if strings.Contains(configurationError(err).Error(), "private") {
			t.Fatal("driver details escaped")
		}
	}
	if configurationError(nil) != nil {
		t.Fatal("success became failure")
	}
	now := time.Date(2039, 1, 1, 0, 0, 0, 0, time.UTC)
	a := configdelivery.Request{OutboxID: 1, CreatedAt: now, ExpiresAt: now.Add(5 * time.Minute)}
	b := a
	b.CreatedAt = now.In(time.FixedZone("synthetic", 3600))
	if !sameConfigurationRequest(a, b) {
		t.Fatal("same instant depended on time zone")
	}
	b.ActorID = 1
	if sameConfigurationRequest(a, b) {
		t.Fatal("identity mutation accepted")
	}
	b = a
	b.ExpiresAt = b.ExpiresAt.Add(time.Microsecond)
	if sameConfigurationRequest(a, b) {
		t.Fatal("expiry mutation accepted")
	}
	p := configdelivery.Policy{Enabled: true, Revision: 2, Hash: sha256.Sum256([]byte("synthetic"))}
	r := configdelivery.Request{PolicyRevision: p.Revision, PolicyHash: p.Hash}
	if !configurationPolicyMatches(p, r) {
		t.Fatal("matching policy denied")
	}
	p.Enabled = false
	if configurationPolicyMatches(p, r) {
		t.Fatal("disabled policy accepted")
	}
	p.Enabled = true
	p.Revision++
	if configurationPolicyMatches(p, r) {
		t.Fatal("stale revision accepted")
	}
}
