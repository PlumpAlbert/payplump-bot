package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/importplan"
	"github.com/PlumpAlbert/payplump-bot/internal/postgres"
)

const syntheticManifest = `{"records":[{"telegram_id":"123","invoice_customer_id":"customer-A","invoice_id_number":"123","current_invoice_id":"synthetic-invoice-A","invoice_status":"paid","recurring_id":"recurring-A","sui_id":"sui-A","sui_name":"synthetic-private","config_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","period_start":"2025-01-01","period_end":"2025-02-01","currency":"USD","lifecycle":"active","source_version":"synthetic-1","pinned_provenance":true,"excluded":false,"complete_proof":true,"unpaid_invoices":0,"amount":"12.34"}]}`

func privateManifest(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "input.json")
	if err := os.WriteFile(p, []byte(syntheticManifest), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}
func TestImportPlanCommandSafeSyntheticDryRun(t *testing.T) {
	path := privateManifest(t)
	var out bytes.Buffer
	if err := importPlanCommand([]string{"--file", path}, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, `"candidates":1`) || !strings.Contains(got, `"plan_hash"`) {
		t.Fatalf("unexpected summary %s", got)
	}
	for _, secret := range []string{"customer-A", "synthetic-private", "sui-A", strings.Repeat("a", 64)} {
		if strings.Contains(got, secret) {
			t.Fatalf("dry-run leaked %q", secret)
		}
	}
}
func TestImportPlanRejectsNonPrivateMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input.json")
	if err := os.WriteFile(path, []byte(syntheticManifest), 0644); err != nil {
		t.Fatal(err)
	}
	if err := importPlanCommand([]string{"--file", path}, &bytes.Buffer{}); err == nil {
		t.Fatal("accepted non-private source file")
	}
}
func TestImportStageRequiresExplicitApplyAndValidHashBeforeDB(t *testing.T) {
	path := privateManifest(t)
	if err := importStageCommand([]string{"--file", path, "--expected-hash", strings.Repeat("0", 64), "--apply"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "hash") {
		t.Fatalf("expected pre-DB hash rejection, got %v", err)
	}
	if err := importStageCommand([]string{"--file", path, "--expected-hash", strings.Repeat("0", 64)}, &bytes.Buffer{}); err == nil {
		t.Fatal("stage without --apply accepted")
	}
}
func TestRecoveryReportRejectsInvalidLocalOperationBeforeDB(t *testing.T) {
	for _, args := range [][]string{{"--operation", "external-42"}, {"--operation", "0"}, {"--unknown"}} {
		if err := recoveryReportCommand(args, &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted args %v", args)
		}
	}
}

type fakeRecovery struct {
	report postgres.RecoveryReport
	closed bool
}

func (f *fakeRecovery) RecoverySnapshot(_ context.Context, id int64) (postgres.RecoveryReport, error) {
	if f.report.Operation == nil || f.report.Operation.LocalID != id {
		return postgres.RecoveryReport{}, errors.New("unexpected local id")
	}
	return f.report, nil
}
func (f *fakeRecovery) Close() { f.closed = true }
func TestRecoveryReportCommandSerializesSafeLocalReport(t *testing.T) {
	op := &postgres.RecoveryOperation{LocalID: 42, Kind: "register_client", Status: "running", Attempt: 3, ReviewCode: 17}
	f := &fakeRecovery{report: postgres.RecoveryReport{FeatureVersion: "009_protected_configuration.sql", Counts: []postgres.RecoveryCount{{Kind: "register_client", Outcome: "running", Count: 1}}, Operation: op, Guard: postgres.RecoveryGuard{Epoch: 9}}}
	var out bytes.Buffer
	err := recoveryReportWith([]string{"--operation", "42"}, &out, func(context.Context, string, int32) (recoveryReader, error) { return f, nil })
	if err != nil {
		t.Fatal(err)
	}
	if !f.closed || !strings.Contains(out.String(), `"local_operation_id":42`) || !strings.Contains(out.String(), `"review_code":17`) {
		t.Fatalf("unexpected report %s", out.String())
	}
	for _, secret := range []string{"chat_id", "telegram_id", "url", "payload"} {
		if strings.Contains(out.String(), secret) {
			t.Fatalf("report leaked prohibited field %q", secret)
		}
	}
}

type fakeStager struct {
	called, closed bool
	plan           importplan.Plan
	hash           string
	repeated       bool
}

func (f *fakeStager) StageImport(_ context.Context, p importplan.Plan, h string) (bool, error) {
	f.called = true
	f.plan = p
	f.hash = h
	return f.repeated, nil
}
func (f *fakeStager) Close() { f.closed = true }
func TestImportStageCommandStagesExplicitSyntheticPlan(t *testing.T) {
	path := privateManifest(t)
	m, err := importplan.Decode(strings.NewReader(syntheticManifest))
	if err != nil {
		t.Fatal(err)
	}
	p := importplan.Analyze(m)
	f := &fakeStager{}
	var out bytes.Buffer
	err = importStageWith([]string{"--file", path, "--expected-hash", p.Hash, "--apply"}, &out, func(context.Context, string, int32) (importStager, error) { return f, nil })
	if err != nil {
		t.Fatal(err)
	}
	if !f.called || !f.closed || f.hash != p.Hash || f.plan.Imported != 1 || !strings.Contains(out.String(), `"state":"staged"`) {
		t.Fatalf("stage failed: %+v %s", f, out.String())
	}
	for _, secret := range []string{"customer-A", "synthetic-private", "sui-A"} {
		if strings.Contains(out.String(), secret) {
			t.Fatalf("stage output leaked %q", secret)
		}
	}
}
func TestImportStageIdempotentResultIsReported(t *testing.T) {
	path := privateManifest(t)
	m, _ := importplan.Decode(strings.NewReader(syntheticManifest))
	p := importplan.Analyze(m)
	f := &fakeStager{repeated: true}
	var out bytes.Buffer
	err := importStageWith([]string{"--file", path, "--expected-hash", p.Hash, "--apply"}, &out, func(context.Context, string, int32) (importStager, error) { return f, nil })
	if err != nil || !strings.Contains(out.String(), `"state":"already_staged"`) {
		t.Fatalf("idempotent result missing: %v %s", err, out.String())
	}
}
