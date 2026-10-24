//go:build integration

package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/configdelivery"
	"github.com/PlumpAlbert/payplump-bot/internal/lifecycle"
	"github.com/jackc/pgx/v5"
)

const rehearsalManifestLimit = 2 << 20

type rehearsalDigest struct {
	Tables        map[string]string `json:"tables"`
	Sequences     map[string]string `json:"sequences"`
	Schema        string            `json:"schema"`
	ServerVersion int               `json:"server_version"`
	Counts        map[string]int64  `json:"counts"`
}

func TestRestoreRehearsalPrepareSource(t *testing.T) {
	rehearsalMode(t, "prepare")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	dsn := rehearsalURL(t, "RESTORE_REHEARSAL_SOURCE_URL", "payplump_restore_source_")
	targetDSN := rehearsalURL(t, "RESTORE_REHEARSAL_TARGET_URL", "payplump_restore_check_")
	if rehearsalTenant(dsn) != rehearsalTenant(targetDSN) || rehearsalDatabaseName(dsn) == rehearsalDatabaseName(targetDSN) {
		t.Fatal("source and target URL tenant mismatch")
	}
	s := rehearsalOpen(t, ctx, dsn)
	defer s.Close()
	assertRehearsalDatabase(t, ctx, s, rehearsalDatabaseName(dsn))
	if n, err := rehearsalObjects(ctx, s); err != nil || n != 0 {
		t.Fatalf("source database is not fresh: objects=%d err=%v", n, err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// Import admission is deliberately performed before occupying the shared writer guard.
	if _, err := s.pool.Exec(ctx, `INSERT INTO telegram_bots(bot_id) VALUES($1)`, admissionFixtureBotID); err != nil {
		t.Fatal(err)
	}
	prepared := admissionPreparedFixture(t, ctx)
	if err := s.SyncImportAdmissionPolicy(ctx, admissionFixtureBotID, 1, prepared.PolicyHash(), prepared.SourceScopeHash(), true); err != nil {
		t.Fatal(err)
	}
	result, err := s.ApplyVerifiedImport(ctx, prepared)
	if err != nil || result.Admitted != 1 {
		t.Fatalf("verified synthetic import: %+v %v", result, err)
	}

	_, policy := seedConfigurationFixture(t, s, ctx)
	if err := s.InstallConfigurationPolicy(ctx, 123, policy); err != nil {
		t.Fatal(err)
	}
	message := configRequest(t, s, ctx, 88001, policy)
	if permit := configAuthorize(t, s, ctx, message, configdelivery.Secret); permit.Kind != configdelivery.Secret {
		t.Fatal("synthetic protected configuration was not permitted")
	}
	if err := s.FailConfigurationOutbox(ctx, message.Lease, 400, 0, false); err != nil {
		t.Fatal(err)
	}
	var dispatchUnknown bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM configuration_dispatches WHERE outbox_id=$1 AND outcome='unknown')`, message.ID).Scan(&dispatchUnknown); err != nil || !dispatchUnknown {
		t.Fatalf("protected configuration unknown: %v", err)
	}
	var guardUnknown bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM configuration_chat_guards WHERE outbox_id=$1)`, message.ID).Scan(&guardUnknown); err != nil || !guardUnknown {
		t.Fatalf("configuration barrier missing: %v", err)
	}

	seedActivationAccounts(t, s, ctx, 123, int(configActor)+1, 1)
	seedActivationBatch(t, s, ctx, 123, []int64{configActor + 1}, 1)
	var pending bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM operations o JOIN clients c ON c.id=o.client_id WHERE c.telegram_id=$1 AND o.status='pending')`, configActor+1).Scan(&pending); err != nil || !pending {
		t.Fatalf("pending owned activation missing: %v", err)
	}

	claim, snapshot, _, botID := seedOverdueOperation(t, s, ctx, []int64{101})
	today := time.Now().In(time.Local).Format(time.DateOnly)
	request := []byte(`{"before":{"id":"recurring-synthetic"},"synthetic":"restore-pause"}`)
	ok, err := s.AuthorizeOverdueWrite(ctx, claim, botID, snapshot, lifecycle.OverduePauseRecurring, request, today, []int64{101})
	if err != nil || !ok {
		t.Fatalf("authorize known pause: %v %v", ok, err)
	}
	if err := s.ConfirmOverdueStep(ctx, claim, lifecycle.OverduePauseRecurring, snapshot.RecurringID); err != nil {
		t.Fatal(err)
	}
	snapshot, err = s.LoadOverdueSnapshot(ctx, claim, botID)
	if err != nil {
		t.Fatal(err)
	}
	request = []byte(`{"before":{"id":"71"},"binding":{"id":71,"name":"71"},"synthetic":"restore-disable"}`)
	ok, err = s.AuthorizeOverdueWrite(ctx, claim, botID, snapshot, lifecycle.OverdueDisableSUI, request, today, []int64{101})
	if err != nil || !ok {
		t.Fatalf("authorize unknown disable: %v %v", ok, err)
	}
	snapshot, err = s.LoadOverdueSnapshot(ctx, claim, botID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.HoldOverdue(ctx, claim, botID, snapshot, lifecycle.ReviewOverdueUnknownWrite, nil); err != nil {
		t.Fatal(err)
	}
	report, err := s.RecoverySnapshot(ctx, claim.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertRehearsalSemantics(t, ctx, s)
	if report.UnresolvedKnownReceipts < 1 || report.Guard.OperationID == nil || *report.Guard.OperationID != claim.ID || report.Guard.Step == nil || *report.Guard.Step != string(lifecycle.OverdueDisableSUI) || report.ProtectedUnknownTotal < 1 {
		t.Fatalf("synthetic recovery state not represented: %+v", report)
	}
	assertUnknownRehearsalState(t, ctx, s, *report.Guard.OperationID, message.ID)
	digest := rehearsalSnapshot(t, ctx, s)
	rehearsalSchemaFingerprintDetectsDisabledTrigger(t, ctx, s, digest)
	writeRehearsalFile(t, os.Getenv("RESTORE_REHEARSAL_MANIFEST"), digest)
}

func TestRestoreRehearsalVerifyTarget(t *testing.T) {
	mode := rehearsalMode(t, "verify")
	var expected rehearsalDigest
	readRehearsalFile(t, os.Getenv("RESTORE_REHEARSAL_MANIFEST"), &expected)
	dsn := rehearsalURL(t, "RESTORE_REHEARSAL_SOURCE_URL", "payplump_restore_source_")
	target := rehearsalURL(t, "RESTORE_REHEARSAL_TARGET_URL", "payplump_restore_check_")
	if rehearsalTenant(dsn) != rehearsalTenant(target) || rehearsalDatabaseName(dsn) == rehearsalDatabaseName(target) {
		t.Fatal("source and target URL tenant mismatch")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	s := rehearsalOpen(t, ctx, target)
	defer s.Close()
	assertRehearsalDatabase(t, ctx, s, rehearsalDatabaseName(target))
	actual := rehearsalSnapshot(t, ctx, s)
	if !equalRehearsal(expected, actual) {
		t.Fatal("restored database differs from private source manifest")
	}
	before := rehearsalHash(actual)
	report, err := s.RecoverySnapshot(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertRehearsalSemantics(t, ctx, s)
	if report.Guard.OperationID != nil {
		assertUnknownRehearsalState(t, ctx, s, *report.Guard.OperationID, 0)
	}
	if report.Guard.OperationID == nil || report.Guard.Step == nil || *report.Guard.Step != string(lifecycle.OverdueDisableSUI) || report.UnresolvedKnownReceipts < 1 || report.ProtectedUnknownTotal < 1 {
		t.Fatalf("restored recovery barriers missing: %+v", report)
	}
	after := rehearsalSnapshot(t, ctx, s)
	if rehearsalHash(after) != before {
		t.Fatal("read-only recovery inspection changed target")
	}
	writeRehearsalFile(t, os.Getenv("RESTORE_REHEARSAL_REPORT"), map[string]any{"mode": mode, "synthetic": true, "data_sha256": before, "tables": len(actual.Tables), "sequences": len(actual.Sequences), "recovery_verified": true})
}

func rehearsalMode(t *testing.T, want string) string {
	t.Helper()
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		t.Skip("restore rehearsal is CI-only")
	}
	mode := os.Getenv("RESTORE_REHEARSAL_MODE")
	if mode == "" {
		t.Skip("restore rehearsal mode not selected")
	}
	if mode != "prepare" && mode != "verify" {
		t.Fatal("invalid restore rehearsal mode")
	}
	if mode != want {
		t.Skip("restore rehearsal mode not selected")
	}
	return want
}
func rehearsalURL(t *testing.T, key, prefix string) string {
	t.Helper()
	for _, name := range []string{"PGOPTIONS", "PGSERVICE", "PGSERVICEFILE", "PGHOSTADDR"} {
		if os.Getenv(name) != "" {
			t.Fatalf("unsafe PostgreSQL override %s", name)
		}
	}
	raw := os.Getenv(key)
	u, err := url.Parse(raw)
	if err != nil || u == nil {
		t.Fatalf("invalid %s", key)
	}
	password, hasPassword := "", false
	if u.User != nil {
		password, hasPassword = u.User.Password()
	}
	if u.Scheme != "postgres" || u.User == nil || u.User.Username() == "" || !hasPassword || password == "" || u.Host == "" || u.RawQuery != "sslmode=disable" || u.Fragment != "" || u.Opaque != "" || u.RawPath != "" || u.ForceQuery {
		t.Fatalf("invalid %s", key)
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		t.Fatalf("%s must use literal loopback", key)
	}
	if port := u.Port(); port != "" {
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 {
			t.Fatalf("invalid port in %s", key)
		}
	}
	db := strings.TrimPrefix(u.Path, "/")
	if !strings.HasPrefix(db, prefix) || len(db) > 63 || !simpleDatabaseName(db) || strings.TrimPrefix(u.Path, "/") != db || u.EscapedPath() != "/"+db {
		t.Fatalf("invalid rehearsal database name in %s", key)
	}
	return raw
}
func simpleDatabaseName(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	return true
}
func rehearsalDatabaseName(raw string) string {
	u, _ := url.Parse(raw)
	return strings.TrimPrefix(u.Path, "/")
}
func rehearsalTenant(raw string) string {
	u, _ := url.Parse(raw)
	return u.Host + "|" + u.User.String()
}
func rehearsalOpen(t *testing.T, ctx context.Context, raw string) *Store {
	t.Helper()
	u, _ := url.Parse(raw)
	params := u.Query()
	params.Set("search_path", "public")
	u.RawQuery = params.Encode()
	scoped := u.String()
	cfg, err := pgx.ParseConfig(scoped)
	if err != nil {
		t.Fatal("invalid scoped rehearsal database URL")
	}
	check, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal("could not validate scoped rehearsal database")
	}
	var db, schema, path string
	if err = check.QueryRow(ctx, `SELECT current_database(),current_schema(),current_setting('search_path')`).Scan(&db, &schema, &path); err != nil {
		_ = check.Close(ctx)
		t.Fatal(err)
	}
	if db != rehearsalDatabaseName(raw) || schema != "public" || path != "public" {
		_ = check.Close(ctx)
		t.Fatalf("unexpected rehearsal connection target: %q %q %q", db, schema, path)
	}
	if err = check.Close(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, scoped, 4)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err = s.pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::integer`).Scan(&version); err != nil || version < 170000 || version >= 180000 {
		s.Close()
		t.Fatalf("requires PostgreSQL 17: %d %v", version, err)
	}
	return s
}
func assertRehearsalDatabase(t *testing.T, ctx context.Context, s *Store, expectedName string) {
	t.Helper()
	var name, schema, search string
	if err := s.pool.QueryRow(ctx, `SELECT current_database(),current_schema(),current_setting('search_path')`).Scan(&name, &schema, &search); err != nil {
		t.Fatal(err)
	}
	if name != expectedName || schema != "public" || search != "public" {
		t.Fatalf("unexpected rehearsal database/schema: %q %q %q", name, schema, search)
	}
}
func assertRehearsalSemantics(t *testing.T, ctx context.Context, s *Store) {
	t.Helper()
	var importedOK, pendingOK bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM clients c JOIN subscriptions sub ON sub.client_id=c.id JOIN verified_import_origins i ON i.client_id=c.id WHERE c.telegram_id='42' AND sub.first_invoice_id IS NULL AND i.source_facts_hash IS NOT NULL AND NOT EXISTS (SELECT 1 FROM operations o WHERE o.client_id=c.id) AND NOT EXISTS (SELECT 1 FROM payment_checks p WHERE p.client_id=c.id) AND NOT EXISTS (SELECT 1 FROM activation_sui_bindings b WHERE b.client_id=c.id))`).Scan(&importedOK); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM clients c JOIN operations o ON o.client_id=c.id AND o.kind='activate_first_subscription' WHERE c.telegram_id=33001 AND o.status='pending')`).Scan(&pendingOK); err != nil {
		t.Fatal(err)
	}
	if !importedOK || !pendingOK {
		t.Fatalf("imported origin or pending owned activation invariant missing: imported=%v pending=%v", importedOK, pendingOK)
	}
}
func assertUnknownRehearsalState(t *testing.T, ctx context.Context, s *Store, operationID, outboxID int64) {
	t.Helper()
	var guard, steps, dispatch bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM activation_write_guard WHERE operation_id=$1 AND step='disable_sui' AND epoch>0)`, operationID).Scan(&guard); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM invoice_overdue_steps known JOIN invoice_overdue_steps unknown ON unknown.operation_id=known.operation_id JOIN external_write_intents i ON i.operation_id=unknown.operation_id AND i.step=unknown.step WHERE known.operation_id=$1 AND known.step='pause_recurring' AND known.confirmed_at IS NOT NULL AND known.external_id IS NOT NULL AND unknown.step='disable_sui' AND unknown.confirmed_at IS NULL AND unknown.external_id IS NULL AND i.request_hash=sha256(convert_to(unknown.request::text,'UTF8')))`, operationID).Scan(&steps); err != nil {
		t.Fatal(err)
	}
	if outboxID > 0 {
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM configuration_chat_guards g JOIN configuration_dispatches d ON d.outbox_id=g.outbox_id AND d.attempt=g.attempt WHERE g.outbox_id=$1 AND d.outcome='unknown' AND d.kind='secret')`, outboxID).Scan(&dispatch); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM configuration_chat_guards g JOIN configuration_dispatches d ON d.outbox_id=g.outbox_id AND d.attempt=g.attempt WHERE d.outcome='unknown' AND d.kind='secret')`).Scan(&dispatch); err != nil {
			t.Fatal(err)
		}
	}
	if !guard || !steps || !dispatch {
		t.Fatalf("unknown recovery evidence missing: guard=%v steps=%v dispatch=%v", guard, steps, dispatch)
	}
}
func rehearsalObjects(ctx context.Context, s *Store) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN ('r','p','v','m','S','f')`).Scan(&n)
	return n, err
}
func rehearsalSnapshot(t *testing.T, ctx context.Context, s *Store) rehearsalDigest {
	t.Helper()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	d := rehearsalDigest{Tables: map[string]string{}, Sequences: map[string]string{}, Counts: map[string]int64{}}
	if err = tx.QueryRow(ctx, `SELECT current_setting('server_version_num')::integer`).Scan(&d.ServerVersion); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname='public' ORDER BY tablename`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		if err = rows.Scan(&n); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	totalBytes := 0
	for _, table := range tables {
		q := `SELECT row_to_json(x)::text FROM ` + pgx.Identifier{"public", table}.Sanitize() + ` x ORDER BY row_to_json(x)::text`
		rr, e := tx.Query(ctx, q)
		if e != nil {
			t.Fatal(e)
		}
		h := sha256.New()
		var count int64
		for rr.Next() {
			var row string
			if e = rr.Scan(&row); e != nil {
				rr.Close()
				t.Fatal(e)
			}
			count++
			_, _ = h.Write([]byte(row))
			_, _ = h.Write([]byte{0})
			totalBytes += len(row) + 1
			if totalBytes > rehearsalManifestLimit {
				rr.Close()
				t.Fatal("database row snapshot exceeds bound")
			}
		}
		if e = rr.Err(); e != nil {
			rr.Close()
			t.Fatal(e)
		}
		rr.Close()
		d.Tables[table] = hex.EncodeToString(h.Sum(nil))
		d.Counts[table] = count
	}
	if d.Schema, err = rehearsalSchemaHash(ctx, tx, &totalBytes); err != nil {
		t.Fatal(err)
	}
	sr, e := tx.Query(ctx, `SELECT sequencename FROM pg_sequences WHERE schemaname='public' ORDER BY sequencename`)
	if e != nil {
		t.Fatal(e)
	}
	var names []string
	for sr.Next() {
		var name string
		if e = sr.Scan(&name); e != nil {
			sr.Close()
			t.Fatal(e)
		}
		names = append(names, name)
	}
	if e = sr.Err(); e != nil {
		sr.Close()
		t.Fatal(e)
	}
	sr.Close()
	for _, name := range names {
		var last int64
		var called bool
		if e = tx.QueryRow(ctx, `SELECT last_value,is_called FROM `+pgx.Identifier{"public", name}.Sanitize()).Scan(&last, &called); e != nil {
			t.Fatal(e)
		}
		b, _ := json.Marshal([]any{last, called})
		d.Sequences[name] = string(b)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return d
}

func rehearsalSchemaFingerprintDetectsDisabledTrigger(t *testing.T, ctx context.Context, s *Store, baseline rehearsalDigest) {
	t.Helper()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	var trigger, state string
	if err = tx.QueryRow(ctx, `SELECT t.tgname,t.tgenabled::text FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relname='verified_import_origins' AND t.tgname='verified_import_origin_immutable' AND NOT t.tgisinternal`).Scan(&trigger, &state); err != nil {
		t.Fatal(err)
	}
	if state != "O" {
		t.Fatalf("immutability trigger is not enabled before probe: %q", state)
	}
	if _, err = tx.Exec(ctx, `ALTER TABLE public.verified_import_origins DISABLE TRIGGER `+pgx.Identifier{trigger}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	bytes := 0
	fingerprint, err := rehearsalSchemaHash(ctx, tx, &bytes)
	if err != nil {
		t.Fatal(err)
	}
	if fingerprint == baseline.Schema {
		t.Fatal("schema fingerprint ignored disabled trigger state")
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	after := rehearsalSnapshot(t, ctx, s)
	if !equalRehearsal(baseline, after) {
		t.Fatal("rolled-back trigger probe changed source snapshot")
	}
}

func rehearsalSchemaHash(ctx context.Context, tx pgx.Tx, totalBytes *int) (string, error) {
	h := sha256.New()
	queries := []string{
		`WITH active_columns AS (SELECT c.oid AS table_oid,c.relname,a.attnum,a.attname,a.atttypid,a.atttypmod,a.attnotnull,a.attgenerated,a.attidentity,pg_get_expr(d.adbin,d.adrelid) AS column_default FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace JOIN pg_attribute a ON a.attrelid=c.oid LEFT JOIN pg_attrdef d ON d.adrelid=c.oid AND d.adnum=a.attnum WHERE n.nspname='public' AND c.relkind IN ('r','p','v','m','f') AND a.attnum>0 AND NOT a.attisdropped) SELECT 'column|'||relname||'|'||row_number() OVER(PARTITION BY table_oid ORDER BY attnum)::text||'|'||attname||'|'||format_type(atttypid,atttypmod)||'|'||attnotnull::text||'|'||attgenerated::text||'|'||attidentity::text||'|'||COALESCE(column_default,'') FROM active_columns ORDER BY relname,attnum`,
		`SELECT 'function|'||p.proname||'|'||pg_get_function_identity_arguments(p.oid)||'|'||pg_get_functiondef(p.oid) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='public' ORDER BY p.proname,pg_get_function_identity_arguments(p.oid)`,
		`SELECT 'trigger|'||c.relname||'|'||t.tgname||'|'||t.tgenabled::text||'|'||pg_get_triggerdef(t.oid,true) FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND NOT t.tgisinternal ORDER BY c.relname,t.tgname`,
		`SELECT 'index|'||c.relname||'|'||i.relname||'|'||pg_get_indexdef(i.oid) FROM pg_index x JOIN pg_class c ON c.oid=x.indrelid JOIN pg_class i ON i.oid=x.indexrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' ORDER BY c.relname,i.relname`,
		`SELECT 'constraint|'||c.relname||'|'||k.conname||'|'||pg_get_constraintdef(k.oid,true) FROM pg_constraint k JOIN pg_class c ON c.oid=k.conrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' ORDER BY c.relname,k.conname`,
	}
	for _, q := range queries {
		rows, err := tx.Query(ctx, q)
		if err != nil {
			return "", err
		}
		for rows.Next() {
			var item string
			if err = rows.Scan(&item); err != nil {
				rows.Close()
				return "", err
			}
			*totalBytes += len(item) + 1
			if *totalBytes > rehearsalManifestLimit {
				rows.Close()
				return "", errors.New("schema snapshot exceeds bound")
			}
			_, _ = h.Write([]byte(item))
			_, _ = h.Write([]byte{0})
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return "", err
		}
		rows.Close()
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func rehearsalHash(d rehearsalDigest) string {
	b, _ := json.Marshal(d)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func equalRehearsal(a, b rehearsalDigest) bool { return rehearsalHash(a) == rehearsalHash(b) }
func writeRehearsalFile(t *testing.T, path string, v any) {
	t.Helper()
	if path == "" || !filepath.IsAbs(path) {
		t.Fatal("missing safe rehearsal output path")
	}
	b, err := json.Marshal(v)
	if err != nil || len(b) > rehearsalManifestLimit {
		t.Fatal("invalid rehearsal output")
	}
	f, err := os.OpenFile(filepath.Clean(path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Chmod(0600); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if _, err = f.Write(b); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
}
func readRehearsalFile(t *testing.T, path string, v any) {
	t.Helper()
	if path == "" || !filepath.IsAbs(path) {
		t.Fatal("manifest path must be absolute")
	}
	info, err := os.Lstat(filepath.Clean(path))
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatal("invalid rehearsal manifest file")
	}
	f, err := os.OpenFile(filepath.Clean(path), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !os.SameFile(info, st) || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || st.Size() > rehearsalManifestLimit {
		t.Fatal("invalid rehearsal manifest file")
	}
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err = dec.Decode(v); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err = dec.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatal("trailing manifest data")
	}
	if digest, ok := v.(*rehearsalDigest); ok {
		if len(digest.Tables) == 0 || len(digest.Sequences) == 0 || digest.Schema == "" || digest.ServerVersion < 170000 || digest.ServerVersion >= 180000 || len(digest.Counts) != len(digest.Tables) {
			t.Fatal("incomplete rehearsal manifest")
		}
		for name, hash := range digest.Tables {
			if !simpleDatabaseName(name) || len(hash) != 64 {
				t.Fatal("invalid table digest")
			}
			if _, e := hex.DecodeString(hash); e != nil {
				t.Fatal("invalid table digest")
			}
		}
	}
}
