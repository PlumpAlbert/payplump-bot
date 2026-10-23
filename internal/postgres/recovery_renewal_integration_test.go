//go:build integration

package postgres

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/lifecycle"
)

func TestRecoverySnapshotDetects016AndReportsRenewalGuardAndReceipt(t *testing.T) {
	s, ctx := invoiceLifecycleTestStore(t)
	old, _, clientID, botID := seedOverdueOperation(t, s, ctx, []int64{101})
	if _, err := s.pool.Exec(ctx, `UPDATE invoice_overdue_actions SET outcome='completed',finished_at=clock_timestamp() WHERE operation_id=$1`, old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE operations SET status='completed',locked_until=NULL,completed_at=clock_timestamp() WHERE id=$1`, old.ID); err != nil {
		t.Fatal(err)
	}
	var operationID int64
	if err := s.pool.QueryRow(ctx, `INSERT INTO operations(client_id,operation_key,kind,status) VALUES($1,'synthetic-recovery-renewal',$2,'pending') RETURNING id`, clientID, lifecycle.RenewalOperationKind).Scan(&operationID); err != nil {
		t.Fatal(err)
	}
	target := time.Now().In(time.Local).AddDate(0, 1, 0).Format(time.DateOnly)
	baseline := json.RawMessage(`{"recurring_id":"recurring-synthetic","status_id":"3","next_send_date":"2040-02-01","amount":100,"next_send_datetime":"2040-02-01T08:00:00Z","sui_id":71,"sui_enabled":false}`)
	if _, err := s.pool.Exec(ctx, `INSERT INTO invoice_renewal_actions(operation_id,client_id,bot_id,invoice_id,plan_frozen_at,target_date,requires_pause,requires_schedule,requires_enable,requires_resume,baseline)
        VALUES($1,$2,$3,'invoice-current',clock_timestamp(),$4::date,false,true,false,true,$5::jsonb)`, operationID, clientID, botID, target, string(baseline)); err != nil {
		t.Fatal(err)
	}
	claims, err := s.ClaimOperations(ctx, []string{lifecycle.RenewalOperationKind}, 1, time.Minute)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim renewal: claims=%d err=%v", len(claims), err)
	}
	request := json.RawMessage(`{"synthetic":"PRIVATE_RENEWAL_REQUEST"}`)
	receipt := json.RawMessage(`{"Amount":100,"NextSendDatetime":"2040-02-01T08:00:00Z"}`)
	if _, err := s.pool.Exec(ctx, `INSERT INTO invoice_renewal_steps(operation_id,step,request,dispatch_attempt,external_id,receipt,confirmed_at)
        VALUES($1,'schedule_recurring',$2::jsonb,1,'recurring-synthetic',$3::jsonb,clock_timestamp())`, operationID, string(request), string(receipt)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE activation_write_guard SET epoch=epoch+1,operation_id=$1,step='schedule_recurring' WHERE id=1`, operationID); err != nil {
		t.Fatal(err)
	}
	report, err := s.RecoverySnapshot(ctx, operationID)
	if err != nil {
		t.Fatal(err)
	}
	if report.Operation == nil || report.Operation.Kind != lifecycle.RenewalOperationKind || recoveryCount(report, lifecycle.RenewalOperationKind, "running") != 1 {
		t.Fatalf("016 renewal operation not recognized: %+v", report.Operation)
	}
	if report.Guard.OperationID == nil || *report.Guard.OperationID != operationID || report.Guard.Step == nil || *report.Guard.Step != "schedule_recurring" || report.UnresolvedKnownReceipts != 1 {
		t.Fatalf("016 receipt/guard owner lost: guard=%+v receipts=%d", report.Guard, report.UnresolvedKnownReceipts)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"invoice-current", "recurring-synthetic", "PRIVATE_RENEWAL_REQUEST", "2040-02-01T08:00:00Z"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("recovery report leaked private renewal data %q", private)
		}
	}
}
