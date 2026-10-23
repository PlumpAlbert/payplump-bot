//go:build integration

package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/billing"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/lifecycle"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

type renewalLoopback struct {
	mu                                                  sync.Mutex
	recurring                                           invoiceninja.RecurringRecord
	panel                                               json.RawMessage
	invoiceDate, invoiceNext                            string
	customerID, invoiceID, marker, invoiceNumber        string
	telegramID                                          int64
	expectedPageSize                                    int
	paymentID, allocationID                             string
	pauseCalls, scheduleCalls, resumeCalls, panelWrites int
	unknownPause                                        bool
	verifyBeforeWrite                                   func(string) error
}

func (r *renewalLoopback) customer() string {
	if r.customerID != "" {
		return r.customerID
	}
	return "customer-synthetic"
}
func (r *renewalLoopback) invoice() string {
	if r.invoiceID != "" {
		return r.invoiceID
	}
	return "invoice-current"
}
func (r *renewalLoopback) payment() string {
	if r.paymentID != "" {
		return r.paymentID
	}
	return "payment-current"
}
func (r *renewalLoopback) allocation() string {
	if r.allocationID != "" {
		return r.allocationID
	}
	return "allocation-current"
}
func (r *renewalLoopback) actor() int64 {
	if r.telegramID != 0 {
		return r.telegramID
	}
	return 101
}
func (r *renewalLoopback) markerValue() string {
	if r.marker != "" {
		return r.marker
	}
	return "payplump:synthetic_activation_token_123456:recurring_create"
}
func (r *renewalLoopback) number() string {
	if r.invoiceNumber != "" {
		return r.invoiceNumber
	}
	return "SYNTHETIC-CURRENT"
}
func (r *renewalLoopback) pageSize() int {
	if r.expectedPageSize > 0 {
		return r.expectedPageSize
	}
	return 50
}
func (r *renewalLoopback) amount() json.Number {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.recurring.Amount != "" {
		return json.Number(r.recurring.Amount)
	}
	return json.Number("100")
}
func (r *renewalLoopback) panelIdentity() (int64, string, error) {
	var record map[string]json.RawMessage
	if err := json.Unmarshal(r.panel, &record); err != nil {
		return 0, "", err
	}
	var id int64
	var name string
	if err := json.Unmarshal(record["id"], &id); err != nil {
		return 0, "", err
	}
	if err := json.Unmarshal(record["name"], &name); err != nil {
		return 0, "", err
	}
	if id <= 0 || name == "" {
		return 0, "", errors.New("invalid loopback SUI identity")
	}
	return id, name, nil
}

func TestInvoicePaidRenewalRealStoreAdaptersLoopbackSuccessAndUnknown(t *testing.T) {
	for _, tc := range []struct {
		name           string
		unknown        bool
		paidRace       bool
		overdueRestore bool
	}{{name: "confirmed schedule and resume"}, {name: "unknown pause is pinned", unknown: true}, {name: "paid after owned overdue pause", paidRace: true}, {name: "late overdue schedule enable resume", overdueRestore: true}} {
		t.Run(tc.name, func(t *testing.T) {
			s, ctx := invoiceLifecycleTestStore(t)
			clientID, botID := seedInvoiceLifecycleAccount(t, s, ctx, false)
			if tc.paidRace {
				seedRenewalPaidRaceAfterOverduePause(t, s, ctx, clientID, botID)
			}
			if tc.overdueRestore {
				seedRenewalFullOverduePrefix(t, s, ctx, clientID, botID)
			}
			state, target := prepareRenewalDatabaseFixture(t, s, ctx, clientID, botID)
			if tc.paidRace || tc.overdueRestore {
				state.recurring.StatusID = "3"
				state.recurring.NextSendDatetime = time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
			}
			if tc.overdueRestore {
				prepareRenewalOverdueExternalState(t, s, ctx, state, clientID)
			}
			assertRenewalActionInvoiceBinding(t, s, ctx, clientID, botID, tc.paidRace, tc.overdueRestore)
			state.unknownPause = tc.unknown
			state.verifyBeforeWrite = func(step string) error {
				auditCtx, stop := context.WithTimeout(ctx, time.Second)
				defer stop()
				var opID, owner, guardEpoch int64
				var outcome, opStatus, guardStep, requestText string
				var attempt, dispatchAttempt int
				var confirmed, pauseRequired, scheduleRequired, enableRequired bool
				var pauseConfirmed, scheduleConfirmed, enableConfirmed bool
				var requestHash []byte
				err := s.pool.QueryRow(auditCtx, `SELECT a.operation_id,a.outcome,o.status,o.attempts,g.operation_id,COALESCE(g.step,''),g.epoch,
                    st.request::text,st.dispatch_attempt,st.confirmed_at IS NOT NULL,i.request_hash,
                    a.requires_pause,a.requires_schedule,a.requires_enable,
                    COALESCE(p.confirmed_at IS NOT NULL,false),COALESCE(sc.confirmed_at IS NOT NULL,false),COALESCE(en.confirmed_at IS NOT NULL,false)
                    FROM invoice_renewal_actions a JOIN operations o ON o.id=a.operation_id
                    JOIN invoice_renewal_steps st ON st.operation_id=a.operation_id AND st.step=$2
                    JOIN external_write_intents i ON i.operation_id=st.operation_id AND i.step=st.step AND i.origin='lifecycle'
                    CROSS JOIN activation_write_guard g
                    LEFT JOIN invoice_renewal_steps p ON p.operation_id=a.operation_id AND p.step='pause_recurring'
                    LEFT JOIN invoice_renewal_steps sc ON sc.operation_id=a.operation_id AND sc.step='schedule_recurring'
                    LEFT JOIN invoice_renewal_steps en ON en.operation_id=a.operation_id AND en.step='enable_sui'
                    WHERE g.id=1 AND a.client_id=$1 AND a.invoice_id='invoice-current' AND a.outcome='pending' AND a.plan_frozen_at IS NOT NULL`, clientID, step).Scan(
					&opID, &outcome, &opStatus, &attempt, &owner, &guardStep, &guardEpoch, &requestText, &dispatchAttempt, &confirmed, &requestHash,
					&pauseRequired, &scheduleRequired, &enableRequired, &pauseConfirmed, &scheduleConfirmed, &enableConfirmed)
				if err != nil {
					return err
				}
				hash := sha256.Sum256([]byte(requestText))
				expectedEpoch := int64(1)
				switch step {
				case string(lifecycle.RenewalSchedule):
					if pauseRequired {
						expectedEpoch++
					}
				case string(lifecycle.RenewalEnable):
					if pauseRequired {
						expectedEpoch++
					}
					if scheduleRequired {
						expectedEpoch++
					}
				case string(lifecycle.RenewalResume):
					if pauseRequired {
						expectedEpoch++
					}
					if scheduleRequired {
						expectedEpoch++
					}
					if enableRequired {
						expectedEpoch++
					}
				}
				if outcome != "pending" || opStatus != "running" || attempt != dispatchAttempt || confirmed || owner != opID || guardStep != step || guardEpoch != expectedEpoch || !bytes.Equal(hash[:], requestHash) {
					return errors.New("renewal write lacks a committed immutable intent and shared guard")
				}
				switch step {
				case string(lifecycle.RenewalSchedule):
					if pauseRequired && !pauseConfirmed {
						return errors.New("schedule write preceded required pause")
					}
				case string(lifecycle.RenewalEnable):
					if scheduleRequired && !scheduleConfirmed {
						return errors.New("enable write preceded schedule receipt")
					}
				case string(lifecycle.RenewalResume):
					if scheduleRequired && !scheduleConfirmed || enableRequired && !enableConfirmed {
						return errors.New("resume write preceded required receipts")
					}
				}
				return nil
			}
			ninja := httptest.NewServer(state.ninjaHandler())
			defer ninja.Close()
			panelServer := httptest.NewServer(state.panelHandler())
			defer panelServer.Close()
			cycle, err := invoiceninja.NewCycleReader(ninja.URL, "synthetic-ninja-token", 2*time.Second, 50, 20)
			if err != nil {
				t.Fatal(err)
			}
			settlements, err := invoiceninja.NewSettlementClient(ninja.URL, "synthetic-ninja-token", 2*time.Second, 50, 20)
			if err != nil {
				t.Fatal(err)
			}
			recurring, err := invoiceninja.NewRecurringClient(ninja.URL, "synthetic-ninja-token", 2*time.Second, 50, 20)
			if err != nil {
				t.Fatal(err)
			}
			panel, err := sui.NewManagedClient(panelServer.URL, "synthetic-panel-token", "1.6.4", 2*time.Second, 1000)
			if err != nil {
				t.Fatal(err)
			}
			cfg := lifecycle.RenewalConfig{Enabled: true, ExclusiveWriter: true, BaseURL: ninja.URL, Token: "synthetic-ninja-token", InvoiceNinjaVersion: "5.13.47", SUIBaseURL: panelServer.URL, SUIToken: "synthetic-panel-token", SUIVersion: "1.6.4", AllowedTelegramIDs: []int64{101}, Workers: 1, MaxAccounts: 1000, PollInterval: 100 * time.Millisecond, HTTPTimeout: 2 * time.Second}
			worker, err := lifecycle.NewRenewalWorker(s, cycle, settlements, recurring, panel, botID, []int64{9001}, "1", cfg, nil)
			if err != nil {
				t.Fatal(err)
			}
			service, cancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- worker.Run(service) }()
			wantOutcome := "completed"
			if tc.unknown {
				wantOutcome = "review"
			}
			deadline := time.Now().Add(15 * time.Second)
			var outcome string
			for time.Now().Before(deadline) {
				if err := s.pool.QueryRow(ctx, `SELECT outcome FROM invoice_renewal_actions WHERE client_id=$1 AND invoice_id='invoice-current'`, clientID).Scan(&outcome); err == nil && outcome == wantOutcome {
					break
				}
				time.Sleep(25 * time.Millisecond)
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("renewal worker stopped: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("renewal worker did not join")
			}
			var reviewCode int
			var operationStatus string
			var planFrozen bool
			if err := s.pool.QueryRow(ctx, `SELECT a.review_code,o.status,a.plan_frozen_at IS NOT NULL FROM invoice_renewal_actions a JOIN operations o ON o.id=a.operation_id WHERE a.client_id=$1 AND a.invoice_id='invoice-current'`, clientID).Scan(&reviewCode, &operationStatus, &planFrozen); err != nil {
				t.Fatalf("read renewal completion state: %v", err)
			}
			if outcome != wantOutcome || operationStatus != "completed" || !planFrozen {
				t.Fatalf("renewal outcome=%q want=%q review_code=%d operation=%q plan_frozen=%v", outcome, wantOutcome, reviewCode, operationStatus, planFrozen)
			}
			var noticeCount, clientNoticeCount, secretNoticeCount int
			if err := s.pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE chat_id=101),count(*) FILTER(WHERE body LIKE '%synthetic-ninja-token%' OR body LIKE '%synthetic-panel-token%') FROM outbox WHERE bot_id=$1 AND message_key LIKE 'renewal:%'`, botID).Scan(&noticeCount, &clientNoticeCount, &secretNoticeCount); err != nil {
				t.Fatal(err)
			}
			wantNotices, wantClientNotices := 2, 1
			if tc.unknown {
				wantNotices, wantClientNotices = 1, 0
			}
			if noticeCount != wantNotices || clientNoticeCount != wantClientNotices || secretNoticeCount != 0 {
				t.Fatalf("renewal notices=%d client=%d secret-bearing=%d want=%d/%d/0", noticeCount, clientNoticeCount, secretNoticeCount, wantNotices, wantClientNotices)
			}
			var guardOwner int64
			var guardStep string
			if err := s.pool.QueryRow(ctx, `SELECT COALESCE(operation_id,0),COALESCE(step,'') FROM activation_write_guard WHERE id=1`).Scan(&guardOwner, &guardStep); err != nil {
				t.Fatal(err)
			}
			state.mu.Lock()
			pause, schedule, resume, panelWrites := state.pauseCalls, state.scheduleCalls, state.resumeCalls, state.panelWrites
			state.mu.Unlock()
			if tc.unknown {
				if pause != 1 || schedule != 0 || resume != 0 || panelWrites != 0 || guardOwner == 0 || guardStep != "pause_recurring" {
					t.Fatalf("unknown action replay or guard release: writes=%d/%d/%d panel=%d owner=%d step=%q", pause, schedule, resume, panelWrites, guardOwner, guardStep)
				}
				var unconfirmed int
				if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM invoice_renewal_steps WHERE operation_id=(SELECT operation_id FROM invoice_renewal_actions WHERE client_id=$1 AND invoice_id='invoice-current') AND confirmed_at IS NULL`, clientID).Scan(&unconfirmed); err != nil || unconfirmed != 1 {
					t.Fatalf("unknown intent unconfirmed=%d err=%v", unconfirmed, err)
				}
			} else {
				wantPause, wantPanelWrites := 1, 0
				if tc.paidRace || tc.overdueRestore {
					wantPause = 0
				}
				if tc.overdueRestore {
					wantPanelWrites = 1
				}
				if pause != wantPause || schedule != 1 || resume != 1 || panelWrites != wantPanelWrites || guardOwner != 0 || guardStep != "" {
					t.Fatalf("success actions/guard: writes=%d/%d/%d want_pause=%d panel=%d owner=%d step=%q", pause, schedule, resume, wantPause, panelWrites, guardOwner, guardStep)
				}
				var status, next string
				var desired bool
				if err := s.pool.QueryRow(ctx, `SELECT state,desired_access,next_invoice_date::text FROM subscriptions WHERE client_id=$1`, clientID).Scan(&status, &desired, &next); err != nil {
					t.Fatal(err)
				}
				if status != "active" || !desired || next != target {
					t.Fatalf("subscription=%s desired=%v next=%s want=%s", status, desired, next, target)
				}
				if tc.paidRace || tc.overdueRestore {
					var pauseRequired, scheduleRequired, enableRequired, resumeRequired bool
					if err := s.pool.QueryRow(ctx, `SELECT requires_pause,requires_schedule,requires_enable,requires_resume FROM invoice_renewal_actions WHERE client_id=$1 AND invoice_id='invoice-current'`, clientID).Scan(&pauseRequired, &scheduleRequired, &enableRequired, &resumeRequired); err != nil {
						t.Fatal(err)
					}
					if pauseRequired || !scheduleRequired || enableRequired != tc.overdueRestore || !resumeRequired {
						t.Fatalf("frozen plan flags pause=%v schedule=%v enable=%v resume=%v", pauseRequired, scheduleRequired, enableRequired, resumeRequired)
					}
				}
				var intents, confirmedSteps, scheduleReceipts int
				wantIntents := 3
				if tc.paidRace {
					wantIntents = 2
				}
				if err := s.pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE confirmed_at IS NOT NULL),count(*) FILTER(WHERE step='schedule_recurring' AND receipt IS NOT NULL) FROM invoice_renewal_steps WHERE operation_id=(SELECT operation_id FROM invoice_renewal_actions WHERE client_id=$1 AND invoice_id='invoice-current')`, clientID).Scan(&intents, &confirmedSteps, &scheduleReceipts); err != nil || intents != wantIntents || confirmedSteps != wantIntents || scheduleReceipts != 1 {
					t.Fatalf("lifecycle steps=%d confirmed=%d schedule_receipts=%d want=%d/1 err=%v", intents, confirmedSteps, scheduleReceipts, wantIntents, err)
				}
			}
			service, cancel = context.WithCancel(ctx)
			restartDone := make(chan error, 1)
			go func() { restartDone <- worker.Run(service) }()
			time.Sleep(250 * time.Millisecond)
			cancel()
			select {
			case err := <-restartDone:
				if err != nil {
					t.Fatalf("restarted renewal worker stopped: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("restarted renewal worker did not join")
			}
			state.mu.Lock()
			defer state.mu.Unlock()
			if state.pauseCalls != pause || state.scheduleCalls != schedule || state.resumeCalls != resume || state.panelWrites != panelWrites {
				t.Fatalf("restart replayed writes: before=%d/%d/%d/%d after=%d/%d/%d/%d", pause, schedule, resume, panelWrites, state.pauseCalls, state.scheduleCalls, state.resumeCalls, state.panelWrites)
			}
		})
	}
}

func seedRenewalPaidRaceAfterOverduePause(t *testing.T, s *Store, ctx context.Context, clientID, botID int64) {
	t.Helper()
	yesterday := time.Now().In(time.Local).AddDate(0, 0, -1).Format(time.DateOnly)
	if _, err := s.pool.Exec(ctx, `UPDATE invoice_lifecycle_invoices SET sent_date=$2::date WHERE client_id=$1 AND invoice_id='invoice-current'`, clientID, yesterday); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO invoice_lifecycle_checks(client_id,bot_id) VALUES($1,$2) ON CONFLICT(client_id) DO UPDATE SET review_code=0`, clientID, botID); err != nil {
		t.Fatal(err)
	}
	if count, err := s.SeedOverdueOperations(ctx, botID, []int64{101}, time.Now().In(time.Local).Format(time.DateOnly), 10); err != nil || count != 1 {
		t.Fatalf("seed prior overdue action count=%d err=%v", count, err)
	}
	claims, err := s.ClaimOperations(ctx, []string{lifecycle.OverdueOperationKind}, 1, time.Minute)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim prior overdue action count=%d err=%v", len(claims), err)
	}
	claim := claims[0]
	if _, err = s.pool.Exec(ctx, `INSERT INTO invoice_overdue_steps(operation_id,step,request,dispatch_attempt,external_id,confirmed_at) VALUES($1,'pause_recurring',$2::jsonb,$3,'recurring-synthetic',clock_timestamp())`, claim.ID, `{"synthetic":"owned pause receipt"}`, claim.Attempt); err != nil {
		t.Fatalf("record prior confirmed overdue pause: %v", err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE invoice_overdue_actions SET outcome='review',review_code=$2,finished_at=clock_timestamp() WHERE operation_id=$1 AND outcome='pending'`, claim.ID, lifecycle.ReviewOverduePayment); err != nil {
		t.Fatalf("freeze prior paid-overdue review: %v", err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE operations SET status='completed',locked_until=NULL,completed_at=clock_timestamp(),last_error_code=$2 WHERE id=$1 AND status='running'`, claim.ID, lifecycle.ReviewOverduePayment); err != nil {
		t.Fatalf("complete prior overdue operation: %v", err)
	}
}

func seedRenewalFullOverduePrefix(t *testing.T, s *Store, ctx context.Context, clientID, botID int64) {
	t.Helper()
	yesterday := time.Now().In(time.Local).AddDate(0, 0, -1).Format(time.DateOnly)
	if _, err := s.pool.Exec(ctx, `UPDATE invoice_lifecycle_invoices SET sent_date=$2::date WHERE client_id=$1 AND invoice_id='invoice-current'`, clientID, yesterday); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO invoice_lifecycle_checks(client_id,bot_id) VALUES($1,$2) ON CONFLICT(client_id) DO UPDATE SET review_code=0`, clientID, botID); err != nil {
		t.Fatal(err)
	}
	if count, err := s.SeedOverdueOperations(ctx, botID, []int64{101}, time.Now().In(time.Local).Format(time.DateOnly), 10); err != nil || count != 1 {
		t.Fatalf("seed full overdue action count=%d err=%v", count, err)
	}
	claims, err := s.ClaimOperations(ctx, []string{lifecycle.OverdueOperationKind}, 1, time.Minute)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim full overdue action count=%d err=%v", len(claims), err)
	}
	claim := claims[0]
	for _, step := range []struct{ name, external string }{{"pause_recurring", "recurring-synthetic"}, {"disable_sui", "71"}} {
		if _, err = s.pool.Exec(ctx, `INSERT INTO invoice_overdue_steps(operation_id,step,request,dispatch_attempt,external_id,confirmed_at) VALUES($1,$2,$3::jsonb,$4,$5,clock_timestamp())`, claim.ID, step.name, `{"synthetic":"owned overdue receipt"}`, claim.Attempt, step.external); err != nil {
			t.Fatalf("record prior overdue %s receipt: %v", step.name, err)
		}
	}
	if _, err = s.pool.Exec(ctx, `UPDATE invoice_overdue_actions SET outcome='completed',finished_at=clock_timestamp() WHERE operation_id=$1 AND outcome='pending'`, claim.ID); err != nil {
		t.Fatalf("complete prior overdue action: %v", err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE operations SET status='completed',locked_until=NULL,completed_at=clock_timestamp() WHERE id=$1 AND status='running'`, claim.ID); err != nil {
		t.Fatalf("complete prior overdue operation: %v", err)
	}
}

func prepareRenewalOverdueExternalState(t *testing.T, s *Store, ctx context.Context, state *renewalLoopback, clientID int64) {
	t.Helper()
	if _, err := s.pool.Exec(ctx, `UPDATE subscriptions SET state='overdue',desired_access=false WHERE client_id=$1`, clientID); err != nil {
		t.Fatal(err)
	}
	var panel map[string]any
	if err := json.Unmarshal(state.panel, &panel); err != nil {
		t.Fatal(err)
	}
	panel["enable"] = false
	state.panel, _ = json.Marshal(panel)
	record, err := sui.ParseClientRecord(state.panel)
	if err != nil {
		t.Fatal(err)
	}
	hash := record.CredentialsHash()
	if _, err = s.pool.Exec(ctx, `UPDATE activation_sui_bindings SET credentials_hash=$2 WHERE client_id=$1`, clientID, hash[:]); err != nil {
		t.Fatal(err)
	}
}

func assertRenewalActionInvoiceBinding(t *testing.T, s *Store, ctx context.Context, clientID, botID int64, wantPaidRace, wantOverdueRestore bool) {
	t.Helper()
	allowed := []int64{101}
	if _, err := s.SeedRenewalOperations(ctx, botID, allowed, 10); err != nil {
		t.Fatalf("seed renewal diagnostic action: %v", err)
	}
	claims, err := s.ClaimOperations(ctx, []string{lifecycle.RenewalOperationKind}, 1, time.Minute)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim renewal diagnostic action: count=%d err=%v", len(claims), err)
	}
	claim := claims[0]
	snapshot, err := s.LoadRenewalSnapshot(ctx, claim, botID)
	if err != nil {
		t.Fatalf("load initial managed renewal snapshot: %v", err)
	}
	wantState, wantDesired := "active", true
	if wantOverdueRestore {
		wantState, wantDesired = "overdue", false
	}
	if snapshot.ClientID != clientID || snapshot.InvoiceID != "invoice-current" || snapshot.CurrentInvoiceID != snapshot.InvoiceID || snapshot.FirstInvoiceID != "invoice-first" || !snapshot.ManagedProvenance || snapshot.Proof.Validate() != nil || snapshot.Proof.InvoiceID != snapshot.InvoiceID || snapshot.RecurringID != "recurring-synthetic" || snapshot.SUIBinding.ID != 71 || snapshot.SUIBinding.Name == "" || snapshot.OverdueReviewPayment != wantPaidRace || wantPaidRace && (!snapshot.OverdueSteps["pause_recurring"] || snapshot.OverdueSteps["disable_sui"] || snapshot.OverduePending) || wantOverdueRestore && (!snapshot.OverdueSteps["pause_recurring"] || !snapshot.OverdueSteps["disable_sui"] || snapshot.OverduePending) || snapshot.State != wantState || snapshot.DesiredAccess != wantDesired {
		t.Fatalf("invalid initial managed renewal snapshot: client=%d action_invoice=%q current_invoice=%q first_invoice=%q managed=%v proof_invoice=%q recurring=%q sui_id=%d", snapshot.ClientID, snapshot.InvoiceID, snapshot.CurrentInvoiceID, snapshot.FirstInvoiceID, snapshot.ManagedProvenance, snapshot.Proof.InvoiceID, snapshot.RecurringID, snapshot.SUIBinding.ID)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE subscriptions SET current_invoice_id=first_invoice_id WHERE client_id=$1`, clientID); err != nil {
		t.Fatal(err)
	}
	changed, err := s.LoadRenewalSnapshot(ctx, claim, botID)
	if err != nil {
		t.Fatalf("load renewal snapshot after current-invoice change: %v", err)
	}
	if changed.InvoiceID != "invoice-current" || changed.CurrentInvoiceID != "invoice-first" || changed.Proof.InvoiceID != "invoice-current" {
		t.Fatalf("renewal action rebound after current-invoice change: action=%q current=%q proof=%q", changed.InvoiceID, changed.CurrentInvoiceID, changed.Proof.InvoiceID)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE subscriptions SET current_invoice_id='invoice-current' WHERE client_id=$1`, clientID); err != nil {
		t.Fatal(err)
	}
	if err = s.RetryOperation(ctx, claim.Lease, 0, 0); err != nil {
		t.Fatalf("release diagnostic renewal claim: %v", err)
	}
}

func prepareRenewalDatabaseFixture(t *testing.T, s *Store, ctx context.Context, clientID, botID int64) (*renewalLoopback, string) {
	t.Helper()
	today := time.Now().In(time.Local).Format(time.DateOnly)
	todayTime, _ := time.Parse(time.DateOnly, today)
	paidDate := todayTime.AddDate(0, -2, 0).Format(time.DateOnly)
	target, err := billing.NextBillingDate(today)
	if err != nil {
		t.Fatal(err)
	}
	token := "synthetic_activation_token_123456"
	marker := activation.Marker(token)
	product := invoiceninja.Product{ID: "product-synthetic", ProductKey: "monthly", Notes: "subscription", Price: "100", TaxRate1: "0", TaxRate2: "0", TaxRate3: "0", TaxID: "0"}
	firstDraft, err := invoiceninja.BuildRecurringDraft("customer-synthetic", paidDate, paidDate, product, false, marker)
	if err != nil {
		t.Fatal(err)
	}
	draftJSON, err := json.Marshal(firstDraft)
	if err != nil {
		t.Fatal(err)
	}
	customer := invoiceninja.SettlementCustomer{ID: "customer-synthetic", IDNumber: "101", CurrencyID: "1"}
	firstInvoice := invoiceninja.SettlementInvoice{ID: "invoice-first", ClientID: "customer-synthetic", Number: "SYNTHETIC-FIRST", Date: paidDate, DueDate: firstDraft.NextSendDate, Amount: "100", Balance: "0", PaidToDate: "100", ExchangeRate: "1", StatusID: "4"}
	firstPay := renewalProofPayment("payment-first", "allocation-first", "invoice-first", paidDate)
	firstLocal := payments.Snapshot{ClientID: clientID, TelegramID: 101, ChatID: 101, BotID: botID, CustomerID: "customer-synthetic", InvoiceID: "invoice-first", FirstInvoiceID: "invoice-first", RecurringID: "recurring-synthetic"}
	firstDecision := payments.Evaluate(firstLocal, customer, firstInvoice, []invoiceninja.SettlementPayment{firstPay}, "1", today)
	if firstDecision.Kind != payments.Verified {
		t.Fatalf("first activation proof fixture invalid: %+v", firstDecision)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE payment_confirmations SET invoice_number=$1,amount=$2::numeric,currency_id=$3,payment_date=$4::date,next_invoice_date=$5::date,proof_hash=$6,evidence=$7::jsonb,review_required=false,review_code=0 WHERE invoice_id='invoice-first'`, firstDecision.Proof.InvoiceNumber, firstDecision.Proof.Amount, firstDecision.Proof.CurrencyID, firstDecision.Proof.PaymentDate, firstDecision.Proof.NextInvoiceDate, firstDecision.Proof.Hash[:], string(firstDecision.Proof.JSON)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE activation_flows SET proof_hash=$1 WHERE invoice_id='invoice-first'`, firstDecision.Proof.Hash[:]); err != nil {
		t.Fatal(err)
	}
	firstDatetime := firstDraft.NextSendDate + "T08:00:00Z"
	for _, step := range []string{"recurring_create", "recurring_start"} {
		if _, err = s.pool.Exec(ctx, `UPDATE activation_steps SET request=$2::jsonb,recurring_amount=100,recurring_datetime=$3 WHERE step=$1 AND operation_id=(SELECT operation_id FROM activation_flows WHERE invoice_id='invoice-first')`, step, string(draftJSON), firstDatetime); err != nil {
			t.Fatal(err)
		}
	}
	currentInvoice := invoiceninja.SettlementInvoice{ID: "invoice-current", ClientID: "customer-synthetic", RecurringID: "recurring-synthetic", Number: "SYNTHETIC-CURRENT", StatusID: "4", Date: today, DueDate: target, Amount: "100", Balance: "0", PaidToDate: "100", ExchangeRate: "1"}
	currentPay := renewalProofPayment("payment-current", "allocation-current", "invoice-current", today)
	currentLocal := payments.Snapshot{ClientID: clientID, TelegramID: 101, ChatID: 101, BotID: botID, CustomerID: "customer-synthetic", InvoiceID: "invoice-current", FirstInvoiceID: "invoice-first", RecurringID: "recurring-synthetic", State: "active", DesiredAccess: true}
	currentDecision := payments.Evaluate(currentLocal, customer, currentInvoice, []invoiceninja.SettlementPayment{currentPay}, "1", today)
	if currentDecision.Kind != payments.Verified {
		t.Fatalf("current renewal proof fixture invalid: %+v", currentDecision)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO payment_confirmations(invoice_id,client_id,bot_id,customer_id,invoice_number,amount,currency_id,payment_date,next_invoice_date,proof_hash,evidence)
        VALUES('invoice-current',$1,$2,$3,$4,$5::numeric,$6,$7::date,$8::date,$9,$10::jsonb)`, clientID, botID, currentDecision.Proof.CustomerID, currentDecision.Proof.InvoiceNumber, currentDecision.Proof.Amount, currentDecision.Proof.CurrencyID, currentDecision.Proof.PaymentDate, currentDecision.Proof.NextInvoiceDate, currentDecision.Proof.Hash[:], string(currentDecision.Proof.JSON)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE invoice_lifecycle_invoices SET invoice_number='SYNTHETIC-CURRENT',status='sent',sent_date=$1::date,amount=100,balance=100,paid_to_date=0,notice_state='issued' WHERE invoice_id='invoice-current'`, todayTime.AddDate(0, 0, -1).Format(time.DateOnly)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE subscriptions SET state='active',desired_access=true,payment_date=$2::date,next_invoice_date=$3::date WHERE client_id=$1`, clientID, today, currentDecision.Proof.NextInvoiceDate); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO invoice_lifecycle_checks(client_id,bot_id) VALUES($1,$2) ON CONFLICT(client_id) DO UPDATE SET review_code=0`, clientID, botID); err != nil {
		t.Fatal(err)
	}
	panelJSON := json.RawMessage(`{"id":71,"enable":true,"name":"pp-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","config":{},"inbounds":[1],"links":[],"volume":0,"expiry":0,"down":1,"up":1,"desc":"","group":"","remark":"","createdAt":1,"onlineAt":0,"delayStart":false,"autoReset":false,"resetDays":0,"nextReset":0,"totalUp":0,"totalDown":0}`)
	panelRecord, err := sui.ParseClientRecord(panelJSON)
	if err != nil {
		t.Fatal(err)
	}
	panelHash := panelRecord.CredentialsHash()
	if _, err = s.pool.Exec(ctx, `UPDATE activation_sui_bindings SET credentials_hash=$2 WHERE client_id=$1`, clientID, panelHash[:]); err != nil {
		t.Fatal(err)
	}
	line := firstDraft.LineItems[0]
	record := invoiceninja.RecurringRecord{ID: "recurring-synthetic", ClientID: "customer-synthetic", StatusID: "2", PrivateNotes: marker, FrequencyID: "5", NextSendDate: todayTime.AddDate(0, 0, 2).Format(time.DateOnly), NextSendDatetime: target + "T08:00:00Z", LastSentDate: paidDate + "T08:00:00Z", DueDateDays: "on_receipt", AutoBill: "off", RemainingCycles: -1, Amount: "100", Balance: "100", ExchangeRate: "1", LineItems: []invoiceninja.InvoiceLine{line}, Discount: "0", TaxRate1: "0", TaxRate2: "0", TaxRate3: "0", CustomSurcharge1: "0", CustomSurcharge2: "0", CustomSurcharge3: "0", CustomSurcharge4: "0", Partial: "0", PaidToDate: "0"}
	return &renewalLoopback{recurring: record, panel: panelJSON, invoiceDate: today, invoiceNext: target}, target
}

func renewalProofPayment(paymentID, allocationID, invoiceID, date string) invoiceninja.SettlementPayment {
	return renewalProofPaymentAmount(paymentID, allocationID, invoiceID, date, "100")
}

func renewalProofPaymentAmount(paymentID, allocationID, invoiceID, date, amount string) invoiceninja.SettlementPayment {
	return invoiceninja.SettlementPayment{ID: paymentID, ClientID: "customer-synthetic", Date: date, StatusID: "4", CurrencyID: "1", Amount: json.Number(amount), Refunded: "0", Applied: json.Number(amount), ExchangeRate: "1", IsManual: true, Allocations: []invoiceninja.SettlementAllocation{{ID: allocationID, InvoiceID: invoiceID, PaymentID: paymentID, Amount: json.Number(amount), Refunded: "0"}}}
}

func (r *renewalLoopback) ninjaHandler() http.Handler {
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("X-APP-VERSION", "5.13.47")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/api/v1/ping", func(w http.ResponseWriter, q *http.Request) {
		if q.Method != http.MethodGet || q.URL.RawQuery != "" {
			http.Error(w, "invalid ping request", http.StatusBadRequest)
			return
		}
		write(w, 200, map[string]any{"data": map[string]any{}})
	})
	mux.HandleFunc("/api/v1/clients/", func(w http.ResponseWriter, q *http.Request) {
		customer, actor := r.customer(), r.actor()
		if q.Method != http.MethodGet || q.URL.RawQuery != "" {
			http.Error(w, "invalid client lookup", http.StatusBadRequest)
			return
		}
		if q.URL.Path != "/api/v1/clients/"+customer {
			http.NotFound(w, q)
			return
		}
		write(w, 200, map[string]any{"data": map[string]any{"id": customer, "id_number": strconv.FormatInt(actor, 10), "group_settings_id": "", "settings": map[string]any{"currency_id": "1"}, "is_deleted": false, "archived_at": 0}})
	})
	mux.HandleFunc("/api/v1/invoices/", func(w http.ResponseWriter, q *http.Request) {
		invoice, customer, marker, number, amount := r.invoice(), r.customer(), r.markerValue(), r.number(), r.amount()
		if q.Method != http.MethodGet || q.URL.RawQuery != "" {
			http.Error(w, "invalid invoice lookup", http.StatusBadRequest)
			return
		}
		if q.URL.Path != "/api/v1/invoices/"+invoice {
			http.NotFound(w, q)
			return
		}
		r.mu.Lock()
		date, next, recurringID := r.invoiceDate, r.invoiceNext, r.recurring.ID
		r.mu.Unlock()
		write(w, 200, map[string]any{"data": map[string]any{"id": invoice, "client_id": customer, "recurring_id": recurringID, "number": number, "private_notes": marker, "status_id": "4", "date": date, "due_date": next, "amount": amount, "balance": 0, "paid_to_date": amount, "exchange_rate": 1, "is_deleted": false, "archived_at": 0, "updated_at": 1, "invitations": []any{map[string]any{"id": "invitation-current", "sent_date": date + " 08:00:00"}}}})
	})
	mux.HandleFunc("/api/v1/payments", func(w http.ResponseWriter, q *http.Request) {
		if q.Method != http.MethodGet {
			http.Error(w, "payments requires GET", http.StatusBadRequest)
			return
		}
		want := url.Values{"client_id": {r.customer()}, "include": {"invoices"}, "status": {"active,archived,deleted"}, "sort": {"id|asc"}, "per_page": {strconv.Itoa(r.pageSize())}, "page": {"1"}}
		query := q.URL.Query()
		if q.URL.RawQuery != want.Encode() || len(query) != 6 || len(query["client_id"]) != 1 || query.Get("client_id") != r.customer() {
			http.Error(w, "invalid exact-client payment inventory query", http.StatusBadRequest)
			return
		}
		amount, customer, invoice, paymentID, allocationID, date := r.amount(), r.customer(), r.invoice(), r.payment(), r.allocation(), r.invoiceDate
		p := map[string]any{"id": paymentID, "client_id": customer, "date": date, "status_id": "4", "currency_id": "1", "exchange_currency_id": "", "amount": json.Number(amount), "refunded": 0, "applied": json.Number(amount), "exchange_rate": 1, "is_manual": true, "is_deleted": false, "archived_at": 0, "paymentables": []any{map[string]any{"id": allocationID, "invoice_id": invoice, "payment_id": paymentID, "amount": json.Number(amount), "refunded": 0, "archived_at": 0}}}
		write(w, 200, map[string]any{"data": []any{p}, "meta": map[string]any{"pagination": map[string]any{"total": 1, "count": 1, "per_page": r.pageSize(), "current_page": 1, "total_pages": 1}}})
	})
	mux.HandleFunc("/api/v1/recurring_invoices/", func(w http.ResponseWriter, q *http.Request) {
		if q.URL.Path == "/api/v1/recurring_invoices/bulk" || q.URL.Path != "/api/v1/recurring_invoices/"+r.recurring.ID {
			http.NotFound(w, q)
			return
		}
		if q.URL.RawQuery != "" || q.Method != http.MethodGet && q.Method != http.MethodPut {
			http.Error(w, "invalid recurring entity request", http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if q.Method == http.MethodPut {
			if q.Header.Get("Content-Type") != "application/json" {
				http.Error(w, "recurring schedule requires JSON", http.StatusBadRequest)
				return
			}
			if r.verifyBeforeWrite != nil {
				if err := r.verifyBeforeWrite(string(lifecycle.RenewalSchedule)); err != nil {
					http.Error(w, "missing committed renewal intent", http.StatusConflict)
					return
				}
			}
			expectedDate := r.invoiceNext
			expectedExchange := json.Number(r.recurring.ExchangeRate)
			var body map[string]json.RawMessage
			var bodyDate string
			var bodyExchange json.Number
			if json.NewDecoder(q.Body).Decode(&body) != nil || len(body) != 2 || json.Unmarshal(body["next_send_date"], &bodyDate) != nil || json.Unmarshal(body["exchange_rate"], &bodyExchange) != nil || bodyDate == "" {
				http.Error(w, "invalid recurring schedule payload", http.StatusBadRequest)
				return
			}
			if bodyDate != expectedDate || bodyExchange != expectedExchange {
				http.Error(w, "unexpected recurring schedule facts", http.StatusConflict)
				return
			}
			r.recurring.NextSendDate = bodyDate
			r.recurring.NextSendDatetime = bodyDate + "T08:00:00Z"
			r.scheduleCalls++
		}
		write(w, 200, map[string]any{"data": r.recurring})
	})
	mux.HandleFunc("/api/v1/recurring_invoices/bulk", func(w http.ResponseWriter, q *http.Request) {
		if q.Method != http.MethodPost || q.URL.RawQuery != "" || q.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "recurring bulk requires exact POST JSON", http.StatusBadRequest)
			return
		}
		var fields map[string]json.RawMessage
		if json.NewDecoder(q.Body).Decode(&fields) != nil || len(fields) != 2 || len(fields["ids"]) == 0 || len(fields["action"]) == 0 {
			http.Error(w, "invalid recurring bulk object", http.StatusBadRequest)
			return
		}
		var body struct {
			IDs    []string `json:"ids"`
			Action string   `json:"action"`
		}
		encoded, err := json.Marshal(fields)
		if err != nil || json.Unmarshal(encoded, &body) != nil {
			http.Error(w, "invalid recurring bulk fields", http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if len(body.IDs) != 1 || body.IDs[0] != r.recurring.ID || body.Action != "stop" && body.Action != "start" {
			http.Error(w, "unexpected recurring bulk payload", http.StatusConflict)
			return
		}
		switch body.Action {
		case "stop":
			if r.verifyBeforeWrite != nil {
				if err := r.verifyBeforeWrite(string(lifecycle.RenewalPause)); err != nil {
					http.Error(w, "missing committed renewal intent", http.StatusConflict)
					return
				}
			}
			r.pauseCalls++
			r.recurring.StatusID = "3"
			if r.unknownPause {
				write(w, 503, map[string]any{"error": "synthetic unknown"})
				return
			}
		case "start":
			if r.verifyBeforeWrite != nil {
				if err := r.verifyBeforeWrite(string(lifecycle.RenewalResume)); err != nil {
					http.Error(w, "missing committed renewal intent", http.StatusConflict)
					return
				}
			}
			r.resumeCalls++
			r.recurring.StatusID = "2"
		}
		write(w, 200, map[string]any{"data": []invoiceninja.RecurringRecord{r.recurring}})
	})
	return mux
}
func TestRenewalLoopbackSUIRejectsCreateAndNonPOSTWithoutDatabase(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		action string
		want   int
	}{{"create action", http.MethodPost, "new", http.StatusBadRequest}, {"non-POST method", http.MethodGet, "edit", http.StatusMethodNotAllowed}} {
		t.Run(tc.name, func(t *testing.T) {
			writes := 0
			state := &renewalLoopback{
				panel:             json.RawMessage(`{"id":71,"enable":false}`),
				verifyBeforeWrite: func(string) error { writes++; return nil },
			}
			form := url.Values{"object": {"clients"}, "action": {tc.action}, "data": {`{"id":71,"enable":true}`}}
			request := httptest.NewRequest(tc.method, "/apiv2/save", strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			recorder := httptest.NewRecorder()
			state.panelHandler().ServeHTTP(recorder, request)
			if recorder.Code != tc.want || writes != 0 || state.panelWrites != 0 {
				t.Fatalf("status=%d writes=%d panelWrites=%d want_status=%d", recorder.Code, writes, state.panelWrites, tc.want)
			}
		})
	}
}

func TestRenewalLoopbackRejectsUnscopedSettlementRequestsWithoutDatabase(t *testing.T) {
	valid := url.Values{"client_id": {"customer-synthetic"}, "include": {"invoices"}, "status": {"active,archived,deleted"}, "sort": {"id|asc"}, "per_page": {"50"}, "page": {"1"}}
	missing := valid.Clone()
	missing.Del("client_id")
	other := valid.Clone()
	other.Set("client_id", "customer-other")
	duplicate := valid.Encode() + "&client_id=customer-synthetic"
	cases := []struct {
		name, method, path string
		want               int
	}{
		{"payments wrong method", http.MethodPost, "/api/v1/payments?" + valid.Encode(), http.StatusBadRequest},
		{"payments missing client", http.MethodGet, "/api/v1/payments?" + missing.Encode(), http.StatusBadRequest},
		{"payments other client", http.MethodGet, "/api/v1/payments?" + other.Encode(), http.StatusBadRequest},
		{"payments duplicate client", http.MethodGet, "/api/v1/payments?" + duplicate, http.StatusBadRequest},
		{"client query", http.MethodGet, "/api/v1/clients/customer-synthetic?include=all", http.StatusBadRequest},
		{"invoice method", http.MethodPost, "/api/v1/invoices/invoice-current", http.StatusBadRequest},
		{"ping query", http.MethodGet, "/api/v1/ping?probe=1", http.StatusBadRequest},
	}
	state := &renewalLoopback{}
	handler := state.ninjaHandler()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status=%d want=%d", rec.Code, tc.want)
			}
		})
	}
	panel := &renewalLoopback{panel: json.RawMessage(`{"id":71,"enable":true,"name":"pp-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)}
	req := httptest.NewRequest(http.MethodGet, "/apiv2/clients?id=72", nil)
	rec := httptest.NewRecorder()
	panel.panelHandler().ServeHTTP(rec, req)
	var response struct {
		Obj struct {
			Clients []map[string]any `json:"clients"`
		} `json:"obj"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &response) != nil || len(response.Obj.Clients) != 0 {
		t.Fatalf("unknown SUI id was not empty: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func (r *renewalLoopback) panelHandler() http.Handler {
	mux := http.NewServeMux()
	envelope := func(w http.ResponseWriter, obj map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "obj": obj})
	}
	mux.HandleFunc("/apiv2/clients", func(w http.ResponseWriter, q *http.Request) {
		if q.Method != http.MethodGet {
			http.Error(w, "SUI clients requires GET", http.StatusMethodNotAllowed)
			return
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		var record map[string]any
		if err := json.Unmarshal(r.panel, &record); err != nil {
			http.Error(w, "invalid SUI record", http.StatusInternalServerError)
			return
		}
		id, name, identityErr := r.panelIdentity()
		if identityErr != nil {
			http.Error(w, "invalid SUI identity", http.StatusInternalServerError)
			return
		}
		if q.URL.RawQuery == "" {
			envelope(w, map[string]any{"clients": []any{map[string]any{"id": id, "name": name}}})
			return
		}
		query := q.URL.Query()
		if len(query) != 1 || len(query["id"]) != 1 {
			http.Error(w, "unexpected SUI client query", http.StatusBadRequest)
			return
		}
		requested := query.Get("id")
		if requested != strconv.FormatInt(id, 10) {
			envelope(w, map[string]any{"clients": []any{}})
			return
		}
		if q.URL.RawQuery != (url.Values{"id": {requested}}).Encode() {
			http.Error(w, "noncanonical SUI client query", http.StatusBadRequest)
			return
		}
		envelope(w, map[string]any{"clients": []any{record}})
	})
	mux.HandleFunc("/apiv2/inbounds", func(w http.ResponseWriter, q *http.Request) {
		if q.Method != http.MethodGet || q.URL.RawQuery != "" {
			http.Error(w, "invalid SUI inbounds request", http.StatusBadRequest)
			return
		}
		envelope(w, map[string]any{"inbounds": []any{map[string]any{"id": 1, "type": "vmess"}}})
	})
	mux.HandleFunc("/apiv2/status", func(w http.ResponseWriter, q *http.Request) {
		if q.Method != http.MethodGet || q.URL.RawQuery != "r=sys" {
			http.Error(w, "invalid SUI status request", http.StatusBadRequest)
			return
		}
		envelope(w, map[string]any{"sys": map[string]any{"appVersion": "1.6.4"}})
	})
	mux.HandleFunc("/apiv2/save", func(w http.ResponseWriter, q *http.Request) {
		if q.Method != http.MethodPost {
			http.Error(w, "S-UI save requires POST", http.StatusMethodNotAllowed)
			return
		}
		if q.URL.RawQuery != "" || q.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			http.Error(w, "invalid S-UI save request", http.StatusBadRequest)
			return
		}
		if err := q.ParseForm(); err != nil {
			http.Error(w, "invalid S-UI save form", http.StatusBadRequest)
			return
		}
		if len(q.PostForm) != 3 || len(q.PostForm["object"]) != 1 || len(q.PostForm["action"]) != 1 || len(q.PostForm["data"]) != 1 || q.PostForm.Get("object") != "clients" || q.PostForm.Get("action") != "edit" || q.PostForm.Get("data") == "" {
			http.Error(w, "invalid S-UI edit form", http.StatusBadRequest)
			return
		}
		if r.verifyBeforeWrite != nil {
			if err := r.verifyBeforeWrite(string(lifecycle.RenewalEnable)); err != nil {
				http.Error(w, "missing committed renewal intent", http.StatusConflict)
				return
			}
		}
		var record map[string]any
		id, _, identityErr := r.panelIdentity()
		if identityErr != nil {
			http.Error(w, "invalid SUI identity", http.StatusInternalServerError)
			return
		}
		if json.Unmarshal([]byte(q.PostForm.Get("data")), &record) != nil || record["id"] != float64(id) || record["enable"] != true {
			http.Error(w, "invalid S-UI enable payload", http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		var before map[string]any
		if json.Unmarshal(r.panel, &before) != nil {
			r.mu.Unlock()
			http.Error(w, "invalid S-UI baseline", http.StatusInternalServerError)
			return
		}
		before["enable"] = record["enable"]
		if !reflect.DeepEqual(before, record) {
			r.mu.Unlock()
			http.Error(w, "S-UI fields changed beyond enable", http.StatusConflict)
			return
		}
		raw, _ := json.Marshal(record)
		r.panel = raw
		r.panelWrites++
		r.mu.Unlock()
		envelope(w, map[string]any{"ok": true})
	})
	return mux
}
