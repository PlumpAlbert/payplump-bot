//go:build integration

package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/configdelivery"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
)

func configurationBaseline(recurring string, suiID int64, target string) []byte {
	baseline, _ := json.Marshal(struct {
		RecurringID      string      `json:"recurring_id"`
		StatusID         string      `json:"status_id"`
		NextSendDate     string      `json:"next_send_date"`
		Amount           json.Number `json:"amount"`
		NextSendDatetime string      `json:"next_send_datetime"`
		SUIID            int64       `json:"sui_id"`
		SUIEnabled       bool        `json:"sui_enabled"`
	}{recurring, "2", target, json.Number("125.25"), target + "T12:00:00Z", suiID, true})
	return baseline
}

func seedConfigurationRenewal(t *testing.T, s *Store, ctx context.Context, clientID int64) int64 {
	t.Helper()
	var customer, recurring string
	var suiID int64
	if err := s.pool.QueryRow(ctx, `SELECT c.invoice_ninja_id,s.recurring_id,c.sui_id::bigint FROM clients c JOIN subscriptions s ON s.client_id=c.id WHERE c.id=$1`, clientID).Scan(&customer, &recurring, &suiID); err != nil {
		t.Fatal(err)
	}
	invoice, paid, next := "configuration-current-invoice", "2039-02-01", "2039-03-01"
	evidence := payments.Evidence{InvoiceID: invoice, CustomerID: customer, Amount: "125.25", CurrencyID: "1", PaymentDate: paid, NextInvoiceDate: next,
		Allocations: []payments.Allocation{{PaymentID: "configuration-payment", AllocationID: "configuration-allocation", Amount: "125.25", Date: paid}}}
	raw, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(raw)
	configExec(t, s, ctx, `INSERT INTO payment_confirmations(invoice_id,client_id,bot_id,customer_id,invoice_number,amount,currency_id,payment_date,next_invoice_date,proof_hash,evidence)
		VALUES($1,$2,123,$3,'renewal-number',125.25,'1',$4::date,$5::date,$6,$7::jsonb)`, invoice, clientID, customer, paid, next, hash[:], string(raw))
	configExec(t, s, ctx, `INSERT INTO invoice_lifecycle_invoices(invoice_id,client_id,bot_id,customer_id,recurring_id,invoice_number,status,sent_date,amount,balance,paid_to_date,notice_state)
		VALUES($1,$2,123,$3,$4,'renewal-number','paid',$5::date,125.25,0,125.25,'settled')`, invoice, clientID, customer, recurring, paid)
	configExec(t, s, ctx, `UPDATE subscriptions SET current_invoice_id=$2,payment_date=$3::date,next_invoice_date=$4::date WHERE client_id=$1`, clientID, invoice, paid, next)
	var operationID int64
	if err := s.pool.QueryRow(ctx, `INSERT INTO operations(client_id,operation_key,kind,status,completed_at) VALUES($1,$2,'invoice_paid_renewal','completed',clock_timestamp()) RETURNING id`, clientID, "configuration-renewal:"+invoice).Scan(&operationID); err != nil {
		t.Fatal(err)
	}
	configExec(t, s, ctx, `INSERT INTO invoice_renewal_actions(operation_id,client_id,bot_id,invoice_id) VALUES($1,$2,123,$3)`, operationID, clientID, invoice)
	baseline := configurationBaseline(recurring, suiID, next)
	configExec(t, s, ctx, `UPDATE invoice_renewal_actions SET plan_frozen_at=clock_timestamp(),target_date=$2::date,requires_pause=false,requires_schedule=false,requires_enable=false,requires_resume=false,baseline=$3::jsonb WHERE operation_id=$1`, operationID, next, string(baseline))
	configExec(t, s, ctx, `UPDATE invoice_renewal_actions SET recurring_amount=125.25,recurring_next_send_datetime=$2::timestamptz,outcome='completed',finished_at=clock_timestamp() WHERE operation_id=$1`, operationID, next+"T12:00:00Z")
	configExec(t, s, ctx, `UPDATE payment_checks SET review_code=0 WHERE client_id=$1`, clientID)
	return operationID
}

func TestConfigurationRenewalRequestAndFinalAuthorizationUseCurrentPeriod(t *testing.T) {
	s, ctx, clientID, policy := configFixture(t)
	operationID := seedConfigurationRenewal(t, s, ctx, clientID)
	if err := s.InstallConfigurationPolicy(ctx, 123, policy); err != nil {
		t.Fatal(err)
	}
	m := configRequest(t, s, ctx, 901, policy)
	view := configView(t, s, ctx, m)
	if !view.Matches() || view.Entitlement.Period != configdelivery.RenewalPeriod || view.Entitlement.Activation.Proof.InvoiceID == view.Entitlement.CurrentProof.InvoiceID ||
		view.Request.InvoiceID != "configuration-current-invoice" || view.Request.RenewalID != operationID || view.Request.Period != configdelivery.RenewalPeriod {
		t.Fatal("renewal request did not preserve historical activation and explicit current proof")
	}
	var mode string
	var renewalID int64
	if err := s.pool.QueryRow(ctx, `SELECT period_kind,renewal_operation_id FROM configuration_requests WHERE outbox_id=$1`, m.ID).Scan(&mode, &renewalID); err != nil || mode != "renewal" || renewalID != operationID {
		t.Fatal("renewal mode/operation were not durably frozen")
	}
	permit := configAuthorize(t, s, ctx, m, configdelivery.Secret)
	if permit.Kind != configdelivery.Secret {
		t.Fatal("fresh matching renewal was not authorized")
	}
	configFinish(t, s, ctx, m)
}

func TestConfigurationRenewalMutationFencesLoadedRequest(t *testing.T) {
	s, ctx, clientID, policy := configFixture(t)
	operationID := seedConfigurationRenewal(t, s, ctx, clientID)
	if err := s.InstallConfigurationPolicy(ctx, 123, policy); err != nil {
		t.Fatal(err)
	}
	m := configRequest(t, s, ctx, 902, policy)
	view := configView(t, s, ctx, m)
	if !view.Matches() || view.Request.RenewalID != operationID {
		t.Fatal("renewal fixture did not load")
	}
	configExec(t, s, ctx, `UPDATE invoice_renewal_actions SET review_code=5302,outcome='review',finished_at=clock_timestamp() WHERE operation_id=$1`, operationID)
	permit, err := s.AuthorizeConfiguration(ctx, m, view, configdelivery.Decision{Kind: configdelivery.Secret})
	if err == nil || permit.Kind == configdelivery.Secret {
		t.Fatal("reviewed renewal retained an old secret permit")
	}
	configAssert(t, s, ctx, `SELECT configuration_revision>0 FROM clients WHERE id=$1`, clientID)
}

func TestConfigurationRenewalChangedPaymentTargetsCurrentConfirmation(t *testing.T) {
	s, ctx, clientID, policy := configFixture(t)
	seedConfigurationRenewal(t, s, ctx, clientID)
	if err := s.InstallConfigurationPolicy(ctx, 123, policy); err != nil {
		t.Fatal(err)
	}
	m := configRequest(t, s, ctx, 904, policy)
	view := configView(t, s, ctx, m)
	permit, err := s.AuthorizeConfiguration(ctx, m, view, configdelivery.Decision{Kind: configdelivery.Denial, Reason: configdelivery.DeniedPayment, PaymentChanged: true})
	if err != nil || permit.Kind != configdelivery.Denial {
		t.Fatal("changed renewal proof was not downgraded")
	}
	configAssert(t, s, ctx, `SELECT review_required AND review_code=2007 FROM payment_confirmations WHERE invoice_id='configuration-current-invoice'`)
	configAssert(t, s, ctx, `SELECT NOT review_required AND review_code=0 FROM payment_confirmations WHERE invoice_id=(SELECT first_invoice_id FROM subscriptions WHERE client_id=$1)`, clientID)
}

func TestConfigurationRenewalLedgerABAInvalidatesOldView(t *testing.T) {
	s, ctx, clientID, policy := configFixture(t)
	seedConfigurationRenewal(t, s, ctx, clientID)
	if err := s.InstallConfigurationPolicy(ctx, 123, policy); err != nil {
		t.Fatal(err)
	}
	m := configRequest(t, s, ctx, 903, policy)
	view := configView(t, s, ctx, m)
	var revision int64
	if err := s.pool.QueryRow(ctx, `SELECT configuration_revision FROM clients WHERE id=$1`, clientID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	configExec(t, s, ctx, `UPDATE invoice_lifecycle_invoices SET invoice_number='temporary-renewal-number' WHERE invoice_id='configuration-current-invoice'`)
	configExec(t, s, ctx, `UPDATE invoice_lifecycle_invoices SET invoice_number='renewal-number' WHERE invoice_id='configuration-current-invoice'`)
	configAssert(t, s, ctx, `SELECT configuration_revision=$2+2 FROM clients WHERE id=$1`, clientID, revision)
	permit, err := s.AuthorizeConfiguration(ctx, m, view, configdelivery.Decision{Kind: configdelivery.Secret})
	if err != nil || permit.Kind == configdelivery.Secret {
		t.Fatal("ABA-restored renewal ledger retained stale secret permit")
	}
}

func seedNextConfigurationRenewal(t *testing.T, s *Store, ctx context.Context, clientID int64) {
	t.Helper()
	var customer, recurring string
	var suiID int64
	if err := s.pool.QueryRow(ctx, `SELECT c.invoice_ninja_id,s.recurring_id,c.sui_id::bigint FROM clients c JOIN subscriptions s ON s.client_id=c.id WHERE c.id=$1`, clientID).Scan(&customer, &recurring, &suiID); err != nil {
		t.Fatal(err)
	}
	invoice, paid, next := "configuration-next-invoice", "2039-03-01", "2039-04-01"
	evidence := payments.Evidence{InvoiceID: invoice, CustomerID: customer, Amount: "125.25", CurrencyID: "1", PaymentDate: paid, NextInvoiceDate: next,
		Allocations: []payments.Allocation{{PaymentID: "configuration-next-payment", AllocationID: "configuration-next-allocation", Amount: "125.25", Date: paid}}}
	raw, _ := json.Marshal(evidence)
	hash := sha256.Sum256(raw)
	configExec(t, s, ctx, `INSERT INTO payment_confirmations(invoice_id,client_id,bot_id,customer_id,invoice_number,amount,currency_id,payment_date,next_invoice_date,proof_hash,evidence)
		VALUES($1,$2,123,$3,'next-renewal-number',125.25,'1',$4::date,$5::date,$6,$7::jsonb)`, invoice, clientID, customer, paid, next, hash[:], string(raw))
	configExec(t, s, ctx, `INSERT INTO invoice_lifecycle_invoices(invoice_id,client_id,bot_id,customer_id,recurring_id,invoice_number,status,sent_date,amount,balance,paid_to_date,notice_state)
		VALUES($1,$2,123,$3,$4,'next-renewal-number','paid',$5::date,125.25,0,125.25,'settled')`, invoice, clientID, customer, recurring, paid)
	configExec(t, s, ctx, `UPDATE subscriptions SET current_invoice_id=$2,payment_date=$3::date,next_invoice_date=$4::date WHERE client_id=$1`, clientID, invoice, paid, next)
	var operationID int64
	if err := s.pool.QueryRow(ctx, `INSERT INTO operations(client_id,operation_key,kind,status,completed_at) VALUES($1,$2,'invoice_paid_renewal','completed',clock_timestamp()) RETURNING id`, clientID, "configuration-renewal:"+invoice).Scan(&operationID); err != nil {
		t.Fatal(err)
	}
	configExec(t, s, ctx, `INSERT INTO invoice_renewal_actions(operation_id,client_id,bot_id,invoice_id) VALUES($1,$2,123,$3)`, operationID, clientID, invoice)
	baseline := configurationBaseline(recurring, suiID, next)
	configExec(t, s, ctx, `UPDATE invoice_renewal_actions SET plan_frozen_at=clock_timestamp(),target_date=$2::date,requires_pause=false,requires_schedule=false,requires_enable=false,requires_resume=false,baseline=$3::jsonb WHERE operation_id=$1`, operationID, next, string(baseline))
	configExec(t, s, ctx, `UPDATE invoice_renewal_actions SET recurring_amount=125.25,recurring_next_send_datetime=$2::timestamptz,outcome='completed',finished_at=clock_timestamp() WHERE operation_id=$1`, operationID, next+"T12:00:00Z")
}

func TestConfigurationUnknownSecretCannotReplayAfterNewPaidPeriod(t *testing.T) {
	s, ctx, clientID, policy := configFixture(t)
	seedConfigurationRenewal(t, s, ctx, clientID)
	if err := s.InstallConfigurationPolicy(ctx, 123, policy); err != nil {
		t.Fatal(err)
	}
	first := configRequest(t, s, ctx, 905, policy)
	view := configView(t, s, ctx, first)
	permit, err := s.AuthorizeConfiguration(ctx, first, view, configdelivery.Decision{Kind: configdelivery.Secret})
	if err != nil || permit.Kind != configdelivery.Secret {
		t.Fatal("first renewal secret was not reserved")
	}
	if err := s.FailConfigurationOutbox(ctx, first.Lease, 500, 0, false); err != nil {
		t.Fatal("could not persist unknown send")
	}
	seedNextConfigurationRenewal(t, s, ctx, clientID)
	second := configRequest(t, s, ctx, 906, policy)
	secondView := configView(t, s, ctx, second)
	if !secondView.ChatBlocked || secondView.Entitlement.Current.InvoiceID != "configuration-next-invoice" {
		t.Fatal("new paid period did not retain the permanent chat barrier")
	}
	denied, err := s.AuthorizeConfiguration(ctx, second, secondView, configdelivery.Decision{Kind: configdelivery.Denial, Reason: configdelivery.DeniedUnknownDelivery})
	if err != nil || denied.Kind != configdelivery.Denial || denied.Reason != configdelivery.DeniedUnknownDelivery {
		t.Fatal("new paid period replayed an unknown secret send")
	}
}
