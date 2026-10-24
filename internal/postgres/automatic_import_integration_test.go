//go:build integration

package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/broadcast"
	"github.com/PlumpAlbert/payplump-bot/internal/configdelivery"
)

func TestAutomaticImportIssueOutboxDeduplicatesAndRearmsOnChange(t *testing.T) {
	s, ctx := activationTestStore(t)
	const bot, actor = int64(9001), int64(1<<40 + 42001)
	if _, err := s.pool.Exec(ctx, `INSERT INTO telegram_bots(bot_id) VALUES($1)`, bot); err != nil {
		t.Fatal(err)
	}
	key, facts := strings.Repeat("a", 64), strings.Repeat("b", 64)
	admins := []int64{9002, 9003}
	changed, err := s.RecordAutomaticImportIssue(ctx, bot, actor, key, "ambiguous_source", facts, admins)
	if err != nil || !changed {
		t.Fatalf("first issue changed=%v err=%v", changed, err)
	}
	changed, err = s.RecordAutomaticImportIssue(ctx, bot, actor, key, "ambiguous_source", facts, admins)
	if err != nil || changed {
		t.Fatalf("identical issue was not deduplicated: changed=%v err=%v", changed, err)
	}
	assertIssueState(t, s, ctx, bot, key, actor, "ambiguous_source", facts, 1, false, 2)

	newFacts := strings.Repeat("c", 64)
	changed, err = s.RecordAutomaticImportIssue(ctx, bot, actor, key, "payment_unverified", newFacts, admins)
	if err != nil || !changed {
		t.Fatalf("changed diagnosis did not advance: changed=%v err=%v", changed, err)
	}
	assertIssueState(t, s, ctx, bot, key, actor, "payment_unverified", newFacts, 2, false, 4)
	var body string
	if err := s.pool.QueryRow(ctx, `SELECT body FROM outbox WHERE bot_id=$1 ORDER BY id LIMIT 1`, bot).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, facts) || strings.Contains(body, newFacts) || strings.Contains(body, "synthetic") {
		t.Fatalf("notification leaked private facts: %q", body)
	}

	resolvedAt, err := s.AutomaticImportScanStart(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveAutomaticImportIssue(ctx, bot, key, resolvedAt); err != nil {
		t.Fatal(err)
	}
	assertIssueState(t, s, ctx, bot, key, actor, "payment_unverified", newFacts, 2, true, 4)
	changed, err = s.RecordAutomaticImportIssue(ctx, bot, 0, key, "source_failure", newFacts, admins)
	if err != nil || !changed {
		t.Fatalf("reappearing global issue did not create a new generation: changed=%v err=%v", changed, err)
	}
	assertIssueState(t, s, ctx, bot, key, 0, "source_failure", newFacts, 3, false, 6)
}

func TestServiceImportPolicyInstallIsIdempotentAndRevocationIsNotRearmed(t *testing.T) {
	s, ctx := activationTestStore(t)
	const bot = int64(9001)
	if _, err := s.pool.Exec(ctx, `INSERT INTO telegram_bots(bot_id) VALUES($1)`, bot); err != nil {
		t.Fatal(err)
	}
	policyHash, scopeHash := strings.Repeat("a", 64), strings.Repeat("b", 64)
	first, read, effects, err := s.InstallServiceImportPolicies(ctx, bot, policyHash, scopeHash)
	if err != nil || first != 1 || read != 1 || effects != 1 {
		t.Fatalf("initial service import policy: %d/%d/%d err=%v", first, read, effects, err)
	}
	first, read, effects, err = s.InstallServiceImportPolicies(ctx, bot, policyHash, scopeHash)
	if err != nil || first != 1 || read != 1 || effects != 1 {
		t.Fatalf("idempotent service import policy changed revisions: %d/%d/%d err=%v", first, read, effects, err)
	}
	if err := s.DisableImportedEffectPermissions(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.InstallServiceImportPolicies(ctx, bot, policyHash, scopeHash); err == nil {
		t.Fatal("normal service startup silently rearmed revoked import effects")
	}
	var revision int64
	var enabled bool
	if err := s.pool.QueryRow(ctx, `SELECT revision,enabled FROM imported_effect_permissions WHERE bot_id=$1`, bot).Scan(&revision, &enabled); err != nil || revision != 2 || enabled {
		t.Fatalf("revoked effect gate changed after startup: revision=%d enabled=%v err=%v", revision, enabled, err)
	}
}

func TestServiceConfigurationAndBroadcastPoliciesAdvanceAndStayRevoked(t *testing.T) {
	s, ctx := activationTestStore(t)
	const bot = int64(9001)
	configuration := &configdelivery.Policy{Enabled: true, Revision: 1, Hash: [32]byte{1}}
	if err := s.InstallServiceConfigurationPolicy(ctx, bot, configuration); err != nil || configuration.Revision != 1 {
		t.Fatalf("first config policy rev=%d err=%v", configuration.Revision, err)
	}
	if err := s.InstallServiceConfigurationPolicy(ctx, bot, configuration); err != nil || configuration.Revision != 1 {
		t.Fatalf("idempotent config policy rev=%d err=%v", configuration.Revision, err)
	}
	configuration.Hash = [32]byte{2}
	if err := s.InstallServiceConfigurationPolicy(ctx, bot, configuration); err != nil || configuration.Revision != 2 {
		t.Fatalf("changed config policy rev=%d err=%v", configuration.Revision, err)
	}
	if err := s.InstallServiceConfigurationPolicy(ctx, bot, &configdelivery.Policy{}); err != nil {
		t.Fatal(err)
	}
	if err := s.InstallServiceConfigurationPolicy(ctx, bot, &configdelivery.Policy{Enabled: true, Revision: 1, Hash: [32]byte{2}}); !errors.Is(err, ErrServicePolicyRevoked) {
		t.Fatalf("configuration service policy did not reject rearm as revoked: %v", err)
	}
	var cfgRevision int64
	var cfgEnabled bool
	if err := s.pool.QueryRow(ctx, `SELECT revision,enabled FROM configuration_policies WHERE bot_id=$1`, bot).Scan(&cfgRevision, &cfgEnabled); err != nil || cfgRevision != 3 || cfgEnabled {
		t.Fatalf("config revocation state revision=%d enabled=%v err=%v", cfgRevision, cfgEnabled, err)
	}

	broadcastPolicy := (broadcast.Config{Enabled: true, Revision: 1, MaxRecipients: 100, AdminIDs: map[int64]struct{}{9002: {}}}).Policy()
	if err := s.InstallServiceBroadcastPolicy(ctx, bot, &broadcastPolicy); err != nil || broadcastPolicy.Revision != 1 {
		t.Fatalf("first broadcast policy rev=%d err=%v", broadcastPolicy.Revision, err)
	}
	if err := s.InstallServiceBroadcastPolicy(ctx, bot, &broadcastPolicy); err != nil || broadcastPolicy.Revision != 1 {
		t.Fatalf("idempotent broadcast policy rev=%d err=%v", broadcastPolicy.Revision, err)
	}
	broadcastPolicy.MaxRecipients = 101
	broadcastPolicy.Hash = [32]byte{} // Config.Policy is the authoritative hash constructor.
	broadcastPolicy = (broadcast.Config{Enabled: true, Revision: 1, MaxRecipients: 101, AdminIDs: map[int64]struct{}{9002: {}}}).Policy()
	if err := s.InstallServiceBroadcastPolicy(ctx, bot, &broadcastPolicy); err != nil || broadcastPolicy.Revision != 2 {
		t.Fatalf("changed broadcast policy rev=%d err=%v", broadcastPolicy.Revision, err)
	}
	if err := s.InstallServiceBroadcastPolicy(ctx, bot, &broadcast.Policy{}); err != nil {
		t.Fatal(err)
	}
	rearmBroadcast := (broadcast.Config{Enabled: true, Revision: 1, MaxRecipients: 101, AdminIDs: map[int64]struct{}{9002: {}}}).Policy()
	if err := s.InstallServiceBroadcastPolicy(ctx, bot, &rearmBroadcast); !errors.Is(err, ErrServicePolicyRevoked) {
		t.Fatalf("broadcast service policy did not reject rearm as revoked: %v", err)
	}
	var bRevision int64
	var bEnabled bool
	if err := s.pool.QueryRow(ctx, `SELECT revision,enabled FROM broadcast_policies WHERE bot_id=$1`, bot).Scan(&bRevision, &bEnabled); err != nil || bRevision != 3 || bEnabled {
		t.Fatalf("broadcast revocation state revision=%d enabled=%v err=%v", bRevision, bEnabled, err)
	}
}

func TestAutomaticImportIssueReconciliationIsBoundedByCompletedScan(t *testing.T) {
	s, ctx := activationTestStore(t)
	const bot, actor = int64(9001), int64(42001)
	if _, err := s.pool.Exec(ctx, `INSERT INTO telegram_bots(bot_id) VALUES($1)`, bot); err != nil {
		t.Fatal(err)
	}
	active, absent, newer := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	reobserved := strings.Repeat("e", 64)
	equal := strings.Repeat("f", 64)
	facts := strings.Repeat("d", 64)
	for _, key := range []string{active, absent, newer, reobserved, equal} {
		if _, err := s.RecordAutomaticImportIssue(ctx, bot, actor, key, "ambiguous_source", facts, []int64{9002}); err != nil {
			t.Fatal(err)
		}
	}
	started, err := s.AutomaticImportScanStart(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE automatic_import_issues SET updated_at=$3 WHERE bot_id=$1 AND candidate_key=$2`, bot, newer, started.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE automatic_import_issues SET updated_at=$3 WHERE bot_id=$1 AND candidate_key=$2`, bot, equal, started); err != nil {
		t.Fatal(err)
	}
	changed, err := s.RecordAutomaticImportIssue(ctx, bot, actor, reobserved, "ambiguous_source", facts, []int64{9002})
	if err != nil || changed {
		t.Fatal("repeated observation changed notification generation")
	}
	reconciler, ok := any(s).(interface {
		ReconcileAutomaticImportIssues(context.Context, int64, []string, time.Time) error
	})
	if !ok {
		t.Fatal("Store does not expose bounded automatic-import issue reconciliation")
	}
	if err := reconciler.ReconcileAutomaticImportIssues(ctx, bot, []string{active}, started); err != nil {
		t.Fatal(err)
	}
	var activeResolved, absentResolved, newerResolved, reobservedResolved, equalResolved bool
	for key, target := range map[string]*bool{active: &activeResolved, absent: &absentResolved, newer: &newerResolved, reobserved: &reobservedResolved, equal: &equalResolved} {
		if err := s.pool.QueryRow(ctx, `SELECT resolved FROM automatic_import_issues WHERE bot_id=$1 AND candidate_key=$2`, bot, key).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if activeResolved || !absentResolved || newerResolved || reobservedResolved || equalResolved {
		t.Fatalf("reconciliation crossed active/cutoff boundary: active=%v absent=%v newer=%v reobserved=%v equal=%v", activeResolved, absentResolved, newerResolved, reobservedResolved, equalResolved)
	}
	for _, key := range []string{newer, equal, reobserved} {
		if err := s.ResolveAutomaticImportIssue(ctx, bot, key, started); err != nil {
			t.Fatal(err)
		}
		var resolved bool
		if err := s.pool.QueryRow(ctx, `SELECT resolved FROM automatic_import_issues WHERE bot_id=$1 AND candidate_key=$2`, bot, key).Scan(&resolved); err != nil || resolved {
			t.Fatal("old successful scan resolved a newer/equal hold")
		}
	}
}

func TestAutomaticImportRegistrationBlocksGlobalOutageAndOnlyAffectedActor(t *testing.T) {
	s, ctx := activationTestStore(t)
	const bot = int64(9001)
	if _, err := s.pool.Exec(ctx, `INSERT INTO telegram_bots(bot_id) VALUES($1)`, bot); err != nil {
		t.Fatal(err)
	}
	facts := strings.Repeat("b", 64)
	if _, err := s.RecordAutomaticImportIssue(ctx, bot, 77, strings.Repeat("c", 64), "source_failure", facts, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordAutomaticImportIssue(ctx, bot, 88, strings.Repeat("d", 64), "ambiguous_source", facts, []int64{9002}); err != nil {
		t.Fatal(err)
	}
	for _, actor := range []int64{77, 88, 78} {
		update := privateUpdate(actor)
		update.Message.From.ID, update.Message.Chat.ID = actor, actor
		if err := s.ApplyTelegramUpdate(ctx, bot, "PayplumpBot", update); err != nil {
			t.Fatalf("start actor %d: %v", actor, err)
		}
	}
	var clients, blockedNotices int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM clients`).Scan(&clients); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE bot_id=$1 AND chat_id IN (77,78,88)`, bot).Scan(&blockedNotices); err != nil {
		t.Fatal(err)
	}
	var has77, has78, has88 bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM clients WHERE telegram_id=77),EXISTS(SELECT 1 FROM clients WHERE telegram_id=78),EXISTS(SELECT 1 FROM clients WHERE telegram_id=88)`).Scan(&has77, &has78, &has88); err != nil {
		t.Fatal(err)
	}
	if clients != 1 || has77 || !has78 || has88 || blockedNotices != 2 {
		t.Fatalf("registration hold scope incorrect: clients=%d actor77=%v actor78=%v actor88=%v notices=%d", clients, has77, has78, has88, blockedNotices)
	}
	global := strings.Repeat("a", 64)
	if _, err := s.RecordAutomaticImportIssue(ctx, bot, 0, global, "source_failure", facts, []int64{9002}); err != nil {
		t.Fatal(err)
	}
	for _, actor := range []int64{78, 79} {
		update := privateUpdate(actor + 10000)
		update.Message.From.ID, update.Message.Chat.ID = actor, actor
		if err := s.ApplyTelegramUpdate(ctx, bot, "PayplumpBot", update); err != nil {
			t.Fatal(err)
		}
	}
	var newActor, knownDenied bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM clients WHERE telegram_id=79),EXISTS(SELECT 1 FROM outbox WHERE bot_id=$1 AND chat_id=78)`, bot).Scan(&newActor, &knownDenied); err != nil || newActor || knownDenied {
		t.Fatal("global outage must block unknown registration, not an existing private client")
	}
	cutoff, err := s.AutomaticImportScanStart(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveAutomaticImportIssue(ctx, bot, global, cutoff); err != nil {
		t.Fatal(err)
	}
	update := privateUpdate(20079)
	update.Message.From.ID, update.Message.Chat.ID = 79, 79
	if err := s.ApplyTelegramUpdate(ctx, bot, "PayplumpBot", update); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM clients WHERE telegram_id=79)`).Scan(&newActor); err != nil || !newActor {
		t.Fatal("resolved global outage left an unrelated new actor blocked")
	}
}

func TestAutomaticImportIssueRequiresExistingBot(t *testing.T) {
	s, ctx := activationTestStore(t)
	_, err := s.RecordAutomaticImportIssue(ctx, 9001, 42001, strings.Repeat("a", 64), "ambiguous_source", strings.Repeat("b", 64), []int64{9002})
	if err == nil {
		t.Fatal("diagnostic created for nonexistent bot")
	}
	var issues, outbox int
	if e := s.pool.QueryRow(ctx, `SELECT count(*) FROM automatic_import_issues`).Scan(&issues); e != nil {
		t.Fatal(e)
	}
	if e := s.pool.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&outbox); e != nil {
		t.Fatal(e)
	}
	if issues != 0 || outbox != 0 {
		t.Fatalf("failed issue transaction leaked writes: issues=%d outbox=%d", issues, outbox)
	}
}

func assertIssueState(t *testing.T, s *Store, ctx context.Context, bot int64, key string, actor int64, reason, facts string, generation int64, resolved bool, notices int) {
	t.Helper()
	var gotActor *int64
	var gotReason, gotFacts string
	var gotGeneration int64
	var gotResolved bool
	if err := s.pool.QueryRow(ctx, `SELECT actor_id,reason,facts_hash,generation,resolved FROM automatic_import_issues WHERE bot_id=$1 AND candidate_key=$2`, bot, key).Scan(&gotActor, &gotReason, &gotFacts, &gotGeneration, &gotResolved); err != nil {
		t.Fatal(err)
	}
	if gotActor == nil && actor != 0 || gotActor != nil && (actor == 0 || *gotActor != actor) || gotReason != reason || gotFacts != facts || gotGeneration != generation || gotResolved != resolved {
		t.Fatalf("issue state actor=%v reason=%q facts=%q generation=%d resolved=%v", gotActor, gotReason, gotFacts, gotGeneration, gotResolved)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE bot_id=$1`, bot).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != notices {
		t.Fatalf("notice count=%d want=%d", count, notices)
	}
}
