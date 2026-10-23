package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/billing"
	"github.com/PlumpAlbert/payplump-bot/internal/configdelivery"
	"github.com/PlumpAlbert/payplump-bot/internal/importorigin"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
	"github.com/jackc/pgx/v5"
)

// readImportedConfigurationEntitlement admits only an untouched active import
// whose originally paid invoice remains the current baseline. It intentionally
// does not synthesize activation or renewal journal state.
func readImportedConfigurationEntitlement(ctx context.Context, tx pgx.Tx, botID, actor int64) (configdelivery.Entitlement, error) {
	var renewal bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM verified_import_origins io JOIN clients c ON c.id=io.client_id
        JOIN subscriptions s ON s.client_id=c.id WHERE io.bot_id=$1 AND c.telegram_id=$2
          AND (io.lifecycle='paused' OR s.current_invoice_id<>io.current_invoice_id OR s.current_invoice_id<>io.paid_anchor_invoice_id))`, botID, actor).Scan(&renewal); err != nil {
		return configdelivery.Entitlement{}, err
	}
	if renewal {
		return readImportedRenewalConfigurationEntitlement(ctx, tx, botID, actor)
	}
	var e configdelivery.Entitlement
	var clientID, telegramID, chatID, originID, originBot int64
	var firstInvoice *string
	var originAdmissionRevision, admissionPermissionRevision, runtimeRevision, configRevision, suiID int64
	var customer, invoice, originInvoice, originPaidAnchor, originLifecycle, recurring, clientSUIID, state, paymentDate, nextDate string
	var desired, confirmationReview bool
	var checkReview, confirmationReviewCode int
	var stableHash, policyHash, scopeHash, credentialHash []byte
	var templateJSON, evidenceJSON, confirmationHash []byte
	var originAmount, originCurrency, originNativeCurrency, originPaymentDate, originNextDate string
	var proofDate, proofNext string
	err := tx.QueryRow(ctx, `SELECT c.id,c.telegram_id,c.chat_id,COALESCE(c.invoice_ninja_id,''),
        COALESCE(s.current_invoice_id,''),s.first_invoice_id,COALESCE(s.recurring_id,''),COALESCE(c.sui_id,''),
        s.state,s.desired_access,COALESCE(s.payment_date::text,''),COALESCE(s.next_invoice_date::text,''),c.configuration_revision,
        io.client_id,io.bot_id,io.admission_revision,ap.revision,rp.revision,io.stable_hash,io.policy_hash,io.source_scope_hash,
        io.sui_id,io.credentials_hash,io.current_invoice_id,io.paid_anchor_invoice_id,io.lifecycle,io.template,io.evidence,
        io.amount::text,io.currency,io.proof->>'currency_id',io.payment_date::text,io.next_invoice_date::text,
        pc.payment_date::text,pc.next_invoice_date::text,pc.proof_hash,pc.review_required,
        p.review_code,pc.review_code
        FROM clients c JOIN subscriptions s ON s.client_id=c.id
        JOIN verified_import_origins io ON io.client_id=c.id AND io.bot_id=$1
        JOIN import_admission_policy ap ON ap.bot_id=io.bot_id AND ap.enabled
        JOIN imported_runtime_permissions rp ON rp.bot_id=io.bot_id AND rp.enabled
        JOIN payment_checks p ON p.client_id=c.id AND p.bot_id=io.bot_id
        JOIN payment_confirmations pc ON pc.invoice_id=s.current_invoice_id AND pc.client_id=c.id AND pc.bot_id=io.bot_id
        WHERE c.telegram_id=$2 AND c.chat_id=$2
          AND io.lifecycle='active' AND io.current_invoice_id=io.paid_anchor_invoice_id
          AND s.current_invoice_id=io.current_invoice_id AND s.first_invoice_id IS NULL
          AND s.state='active' AND s.desired_access
          AND p.review_code=0 AND NOT pc.review_required AND pc.review_code=0
          AND c.invoice_ninja_id=io.customer_id AND s.recurring_id=io.recurring_id
          AND c.sui_id=io.sui_id::text AND io.sui_name=c.telegram_id::text
          AND ap.policy_hash=io.policy_hash AND ap.source_scope_hash=io.source_scope_hash
          AND rp.policy_hash=io.policy_hash AND rp.source_scope_hash=io.source_scope_hash
          AND NOT EXISTS (SELECT 1 FROM operations o WHERE o.client_id=c.id)
          AND NOT EXISTS (SELECT 1 FROM activation_sui_bindings b WHERE b.client_id=c.id)
          AND NOT EXISTS (SELECT 1 FROM payment_confirmations ownpc WHERE ownpc.client_id=c.id AND ownpc.invoice_id<>io.current_invoice_id)
          AND NOT EXISTS (SELECT 1 FROM invoice_lifecycle_invoices li WHERE li.client_id=c.id)
          AND NOT EXISTS (SELECT 1 FROM invoice_lifecycle_checks lc WHERE lc.client_id=c.id AND (lc.bot_id<>io.bot_id OR lc.review_code<>0))`, botID, actor).Scan(
		&clientID, &telegramID, &chatID, &customer, &invoice, &firstInvoice, &recurring, &clientSUIID,
		&state, &desired, &paymentDate, &nextDate, &configRevision,
		&originID, &originBot, &originAdmissionRevision, &admissionPermissionRevision, &runtimeRevision, &stableHash, &policyHash, &scopeHash,
		&suiID, &credentialHash, &originInvoice, &originPaidAnchor, &originLifecycle, &templateJSON, &evidenceJSON,
		&originAmount, &originCurrency, &originNativeCurrency, &originPaymentDate, &originNextDate, &proofDate, &proofNext, &confirmationHash, &confirmationReview, &checkReview, &confirmationReviewCode)
	if errors.Is(err, pgx.ErrNoRows) {
		return e, configdelivery.ErrDenied
	}
	if err != nil {
		return e, err
	}
	if clientID <= 0 || originID != clientID || originBot != botID || telegramID != actor || chatID != actor || !desired || state != "active" ||
		invoice == "" || recurring == "" || customer == "" || firstInvoice != nil || clientSUIID != strconv.FormatInt(suiID, 10) || confirmationReview || checkReview != 0 || confirmationReviewCode != 0 ||
		originInvoice != invoice || originPaidAnchor != invoice || originLifecycle != "active" || paymentDate != proofDate || nextDate != proofNext || paymentDate != originPaymentDate || nextDate != originNextDate || admissionPermissionRevision <= 0 || runtimeRevision <= 0 ||
		originAmount == "" || originCurrency == "" || originNativeCurrency == "" {
		return e, configdelivery.ErrDenied
	}
	stable, okStable := importorigin.ParseHash(stableHash)
	policy, okPolicy := importorigin.ParseHash(policyHash)
	scope, okScope := importorigin.ParseHash(scopeHash)
	credential, okCredential := importorigin.ParseHash(credentialHash)
	if !okStable || !okPolicy || !okScope || !okCredential {
		return e, configdelivery.ErrDenied
	}
	origin := importorigin.Ref{ClientID: clientID, BotID: botID, AdmissionRevision: originAdmissionRevision, RuntimeRevision: runtimeRevision, ConfigurationRevision: configRevision,
		StableHash: stable, PolicyHash: policy, SourceScopeHash: scope}
	if !origin.Valid() {
		return e, configdelivery.ErrDenied
	}
	var template invoiceninja.RecurringRecord
	if json.Unmarshal(templateJSON, &template) != nil || len(template.LineItems) != 1 || template.ID != recurring || template.ClientID != customer {
		return e, configdelivery.ErrDenied
	}
	profile, err := invoiceninja.NewImportedRecurringProfile(template, template.LineItems[0].ProductKey)
	if err != nil {
		return e, configdelivery.ErrDenied
	}
	var evidence struct {
		BotID               int64   `json:"bot_id"`
		CustomerID          string  `json:"customer_id"`
		RecurringID         string  `json:"recurring_id"`
		CurrentInvoiceID    string  `json:"current_invoice_id"`
		PaidAnchorInvoiceID string  `json:"paid_anchor_invoice_id"`
		Lifecycle           string  `json:"lifecycle"`
		SUIID               int64   `json:"sui_id"`
		SUIName             string  `json:"sui_name"`
		InboundIDs          []int64 `json:"inbound_ids"`
		ProofHash           string  `json:"proof_hash"`
		PublicBaseHash      string  `json:"public_base_hash"`
	}
	if json.Unmarshal(evidenceJSON, &evidence) != nil || len(evidence.InboundIDs) == 0 || evidence.ProofHash == "" || evidence.PublicBaseHash == "" ||
		evidence.BotID != botID || evidence.CustomerID != customer || evidence.RecurringID != recurring || evidence.CurrentInvoiceID != invoice ||
		evidence.PaidAnchorInvoiceID != invoice || evidence.Lifecycle != "active" || evidence.SUIID != suiID || evidence.SUIName != strconv.FormatInt(actor, 10) {
		return e, configdelivery.ErrDenied
	}
	seen := make(map[int64]bool, len(evidence.InboundIDs))
	for _, id := range evidence.InboundIDs {
		if id <= 0 || seen[id] {
			return e, configdelivery.ErrDenied
		}
		seen[id] = true
	}
	confirmation, err := readActivationConfirmation(ctx, tx, invoice)
	if err != nil {
		return e, configdelivery.ErrDenied
	}
	proof, err := confirmation.proof()
	normalizedOriginAmount, amountErr := billing.FormatAmount(json.Number(originAmount))
	if err != nil || amountErr != nil || confirmation.clientID != clientID || confirmation.botID != botID || confirmation.customerID != customer || confirmation.paid != paymentDate || confirmation.next != nextDate || proof.InvoiceID != invoice ||
		normalizedOriginAmount != proof.Amount || proof.CurrencyID != originNativeCurrency || proof.PaymentDate != originPaymentDate || proof.NextInvoiceDate != originNextDate {
		return e, configdelivery.ErrDenied
	}
	pinnedProofHash, ok := importorigin.ParseHashHex(evidence.ProofHash)
	if !ok || pinnedProofHash != proof.Hash || len(confirmationHash) != 32 || fmt.Sprintf("%x", confirmationHash) != evidence.ProofHash {
		return e, configdelivery.ErrDenied
	}
	publicBaseHash, ok := importorigin.ParseHashHex(evidence.PublicBaseHash)
	if !ok {
		return e, configdelivery.ErrDenied
	}
	nextSend, err := invoiceninja.ParseRecurringUTC(template.NextSendDatetime)
	var dbNow time.Time
	if err != nil || tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&dbNow) != nil || !nextSend.After(dbNow) {
		return e, configdelivery.ErrDenied
	}
	current := payments.Snapshot{ClientID: clientID, TelegramID: telegramID, ChatID: chatID, BotID: botID, CustomerID: customer,
		InvoiceID: invoice, RecurringID: recurring, SUIID: clientSUIID, State: state, DesiredAccess: desired,
		PaymentDate: paymentDate, NextInvoiceDate: nextDate, ImportedOrigin: origin, ImportedBaselineProofHash: proof.Hash}
	e.Period = configdelivery.ImportedBaselinePeriod
	e.Current = current
	e.CurrentProof = proof
	e.Binding = sui.Binding{ID: suiID, Name: strconv.FormatInt(telegramID, 10), CredentialsHash: credential}
	e.Revision = configRevision
	e.Imported = &configdelivery.ImportedBaseline{Origin: origin, AdmissionPermissionRevision: admissionPermissionRevision, InvoiceID: invoice,
		PaidAnchorInvoiceID: invoice, Lifecycle: "active", ProofHash: proof.Hash, Profile: profile,
		InboundIDs: append([]int64(nil), evidence.InboundIDs...), NextSendUTC: nextSend, PublicBaseHash: publicBaseHash}
	return e, nil
}
