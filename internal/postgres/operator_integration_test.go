//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/importplan"
)

func TestOperatorRecoveryAndImportStagingAreInert(t *testing.T) {
	s, _ := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	_, err := s.pool.Exec(ctx, `INSERT INTO clients(telegram_id,chat_id,display_name) VALUES(700001,700001,'synthetic-private-marker')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO operations(client_id,operation_key,kind) SELECT id,'synthetic-private-operation-key','synthetic_operator_test' FROM clients WHERE telegram_id=700001`)
	if err != nil {
		t.Fatal(err)
	}
	var localID int64
	if err = s.pool.QueryRow(ctx, `SELECT id FROM operations WHERE operation_key='synthetic-private-operation-key'`).Scan(&localID); err != nil {
		t.Fatal(err)
	}
	report, err := s.RecoverySnapshot(ctx, localID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "synthetic-private-marker") || strings.Contains(string(encoded), "synthetic-private-operation-key") {
		t.Fatal("recovery output leaked private fixture")
	}
	if report.Operation == nil || report.Operation.LocalID != localID {
		t.Fatal("local operation detail missing")
	}
	if report.FeatureVersion != "009_protected_configuration.sql" {
		t.Fatalf("unexpected schema feature: %s", report.FeatureVersion)
	}
	r := importplan.Record{TelegramID: "123", InvoiceCustomerID: "synthetic-customer-A", InvoiceNumber: "123", CurrentInvoiceID: "synthetic-invoice-A", InvoiceStatus: "paid", RecurringID: "synthetic-recurring-A", SUIID: "synthetic-sui-A", SUIName: "synthetic-name", ConfigHash: strings.Repeat("a", 64), PeriodStart: "2025-01-01", PeriodEnd: "2025-02-01", Currency: "USD", Lifecycle: "active", SourceVersion: "synthetic-1", PinnedProvenance: true, CompleteProof: true, Amount: "1.00"}
	p := importplan.Analyze(importplan.Manifest{Records: []importplan.Record{r}})
	if p.Imported != 1 {
		t.Fatalf("candidate rejected: %+v", p.Reasons)
	}
	repeated, err := s.StageImport(ctx, p, p.Hash)
	if err != nil || repeated {
		t.Fatalf("first stage: repeated=%v err=%v", repeated, err)
	}
	repeated, err = s.StageImport(ctx, p, p.Hash)
	if err != nil || !repeated {
		t.Fatalf("idempotent stage: repeated=%v err=%v", repeated, err)
	}
	var clients, operations, outbox, bindings int
	for table, dst := range map[string]*int{"clients": &clients, "operations": &operations, "outbox": &outbox, "activation_sui_bindings": &bindings} {
		if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	if clients != 1 || operations != 1 || outbox != 0 || bindings != 0 {
		t.Fatalf("staging conferred authority: clients=%d operations=%d outbox=%d bindings=%d", clients, operations, outbox, bindings)
	}
	var raw []byte
	if err := s.pool.QueryRow(ctx, `SELECT candidate_facts FROM import_candidates_staging WHERE plan_hash=decode($1,'hex')`, p.Hash).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var saved importplan.Record
	if err := json.Unmarshal(raw, &saved); err != nil || saved.TelegramID != r.TelegramID {
		t.Fatal("candidate facts were not preserved")
	}
	for _, table := range []string{"subscriptions", "payment_confirmations", "activation_flows", "external_write_intents", "configuration_dispatches"} {
		var n int
		if e := s.pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); e != nil || n != 0 {
			t.Fatal("inert staging changed application authority tables")
		}
	}
	if _, err := s.RecoverySnapshot(ctx, 999999); err == nil {
		t.Fatal("missing operation was reported")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE import_batches SET total_records=total_records WHERE plan_hash=decode($1,'hex')`, p.Hash); err == nil {
		t.Fatal("import batch update was allowed")
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM import_candidates_staging WHERE plan_hash=decode($1,'hex')`, p.Hash); err == nil {
		t.Fatal("candidate deletion was allowed")
	}
	forged := p
	forged.Candidates = append([]importplan.Candidate(nil), p.Candidates...)
	forged.Candidates[0].Facts.SUIName = "forged-private-fact"
	if _, err := s.StageImport(ctx, forged, p.Hash); err == nil {
		t.Fatal("mutated candidate was accepted")
	}

	changed := r
	changed.Amount = "13.00"
	mismatched := importplan.Analyze(importplan.Manifest{Records: []importplan.Record{changed}})
	if _, err := s.StageImport(ctx, mismatched, p.Hash); err == nil {
		t.Fatal("hash mismatch was accepted")
	}
	var mismatchRows int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM import_batches WHERE plan_hash=decode($1,'hex')`, mismatched.Hash).Scan(&mismatchRows); err != nil || mismatchRows != 0 {
		t.Fatal("hash mismatch created staging rows")
	}

	second := r
	second.TelegramID, second.InvoiceNumber, second.InvoiceCustomerID = "124", "124", "synthetic-customer-B"
	second.CurrentInvoiceID, second.RecurringID, second.SUIID, second.SUIName = "synthetic-invoice-B", "synthetic-recurring-B", "synthetic-sui-B", "synthetic-name-B"
	excluded := second
	excluded.Excluded = true
	sealed := importplan.Analyze(importplan.Manifest{Records: []importplan.Record{r, excluded}})
	if _, err := s.StageImport(ctx, sealed, sealed.Hash); err != nil {
		t.Fatal("stage partial candidate set")
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO import_candidates_staging(plan_hash,source_index,candidate_facts) SELECT decode($1,'hex'),1,candidate_facts FROM import_candidates_staging WHERE plan_hash=decode($2,'hex')`, sealed.Hash, p.Hash); err == nil {
		t.Fatal("sealed batch admitted an extra candidate")
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO import_batches(plan_hash,total_records,candidate_count) VALUES(decode(repeat('f',64),'hex'),1,1)`); err == nil {
		t.Fatal("incomplete batch committed")
	}
	batch := importplan.Analyze(importplan.Manifest{Records: []importplan.Record{r, second}})
	if _, err := s.pool.Exec(ctx, `CREATE FUNCTION fail_second_candidate() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.source_index=1 THEN RAISE EXCEPTION 'synthetic rollback'; END IF; RETURN NEW; END $$; CREATE TRIGGER fail_second_candidate BEFORE INSERT ON import_candidates_staging FOR EACH ROW EXECUTE FUNCTION fail_second_candidate()`); err != nil {
		t.Fatal(err)
	}
	_, stageErr := s.StageImport(ctx, batch, batch.Hash)
	_, _ = s.pool.Exec(ctx, `DROP TRIGGER fail_second_candidate ON import_candidates_staging; DROP FUNCTION fail_second_candidate()`)
	if stageErr == nil {
		t.Fatal("partial candidate staging unexpectedly succeeded")
	}
	var batches int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM import_batches WHERE plan_hash=decode($1,'hex')`, batch.Hash).Scan(&batches); err != nil || batches != 0 {
		t.Fatal("partial staging transaction was not rolled back")
	}
}
