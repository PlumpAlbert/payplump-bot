package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/billing"
	"github.com/PlumpAlbert/payplump-bot/internal/configdelivery"
	"github.com/PlumpAlbert/payplump-bot/internal/importorigin"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/lifecycle"
	"github.com/PlumpAlbert/payplump-bot/internal/payments"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
	"github.com/jackc/pgx/v5"
)

// Imported renewal authorization keeps the immutable origin separate from the
// current paid cycle. The current cycle is independently proven by payment
// confirmations and the completed native renewal journal.
func readImportedRenewalConfigurationEntitlement(ctx context.Context, tx pgx.Tx, botID, actor int64) (configdelivery.Entitlement, error) {
	var e configdelivery.Entitlement
	var clientID, telegramID, chatID, originID, originBot, originAdmission, admissionRevision, runtimeRevision, configRevision, suiID int64
	var customer, invoice, originInvoice, anchor, lifecycle, recurring, clientSUIID, state, paymentDate, nextDate string
	var desired bool
	var reviewCode int
	var stableHash, policyHash, scopeHash, credentialHash, originalProofHash, originProofJSON []byte
	var templateJSON, evidenceJSON []byte
	var originAmount, originCurrency, originNativeCurrency, originPaymentDate, originNextDate string
	var importedActionCount int
	err := tx.QueryRow(ctx, `SELECT c.id,c.telegram_id,c.chat_id,COALESCE(c.invoice_ninja_id,''),
        COALESCE(s.current_invoice_id,''),COALESCE(s.recurring_id,''),COALESCE(c.sui_id,''),s.state,s.desired_access,
        COALESCE(s.payment_date::text,''),COALESCE(s.next_invoice_date::text,''),c.configuration_revision,
        io.client_id,io.bot_id,io.admission_revision,ap.revision,rp.revision,io.stable_hash,io.policy_hash,io.source_scope_hash,
        io.sui_id,io.credentials_hash,io.current_invoice_id,io.paid_anchor_invoice_id,io.lifecycle,io.template,io.evidence,
        io.amount::text,io.currency,io.proof->>'currency_id',io.payment_date::text,io.next_invoice_date::text,io.evidence->>'proof_hash',io.proof,
        p.review_code,
        (SELECT count(*) FROM invoice_renewal_actions a WHERE a.client_id=c.id AND a.bot_id=io.bot_id
          AND a.invoice_id=s.current_invoice_id AND a.outcome='completed' AND a.review_code=0
          AND a.imported_origin_client_id=io.client_id AND a.imported_admission_permission_revision>0
          AND a.imported_runtime_revision>0 AND a.imported_effect_revision>0
          AND EXISTS(SELECT 1 FROM operations o WHERE o.id=a.operation_id AND o.kind='invoice_paid_renewal' AND o.status='completed'))
        FROM clients c JOIN subscriptions s ON s.client_id=c.id
        JOIN verified_import_origins io ON io.client_id=c.id AND io.bot_id=$1
        JOIN import_admission_policy ap ON ap.bot_id=io.bot_id AND ap.enabled
        JOIN imported_runtime_permissions rp ON rp.bot_id=io.bot_id AND rp.enabled
        JOIN payment_checks p ON p.client_id=c.id AND p.bot_id=io.bot_id
        WHERE c.telegram_id=$2 AND c.chat_id=$2 AND s.first_invoice_id IS NULL
          AND s.state='active' AND s.desired_access AND p.review_code=0
          AND c.invoice_ninja_id=io.customer_id AND s.recurring_id=io.recurring_id
          AND c.sui_id=io.sui_id::text AND io.sui_name=c.telegram_id::text
          AND ap.policy_hash=io.policy_hash AND ap.source_scope_hash=io.source_scope_hash
          AND rp.policy_hash=io.policy_hash AND rp.source_scope_hash=io.source_scope_hash
          AND NOT EXISTS(SELECT 1 FROM operations o WHERE o.client_id=c.id AND o.kind='activate_first_subscription')
          AND NOT EXISTS(SELECT 1 FROM activation_sui_bindings b WHERE b.client_id=c.id)
          AND NOT EXISTS(SELECT 1 FROM invoice_lifecycle_checks lc WHERE lc.client_id=c.id AND (lc.bot_id<>io.bot_id OR lc.review_code<>0))`, botID, actor).Scan(
		&clientID, &telegramID, &chatID, &customer, &invoice, &recurring, &clientSUIID, &state, &desired, &paymentDate, &nextDate, &configRevision,
		&originID, &originBot, &originAdmission, &admissionRevision, &runtimeRevision, &stableHash, &policyHash, &scopeHash, &suiID, &credentialHash,
		&originInvoice, &anchor, &lifecycle, &templateJSON, &evidenceJSON, &originAmount, &originCurrency, &originNativeCurrency, &originPaymentDate, &originNextDate,
		&originalProofHash, &originProofJSON, &reviewCode, &importedActionCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return e, configdelivery.ErrDenied
	}
	if err != nil {
		return e, err
	}
	if clientID <= 0 || originID != clientID || originBot != botID || telegramID != actor || chatID != actor || !desired || state != "active" ||
		invoice == "" || recurring == "" || customer == "" || clientSUIID != strconv.FormatInt(suiID, 10) || reviewCode != 0 ||
		originInvoice == "" || anchor == "" || (lifecycle != "active" && lifecycle != "paused") || admissionRevision <= 0 || runtimeRevision <= 0 || importedActionCount != 1 ||
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
	origin := importorigin.Ref{ClientID: clientID, BotID: botID, AdmissionRevision: originAdmission, RuntimeRevision: runtimeRevision, ConfigurationRevision: configRevision, StableHash: stable, PolicyHash: policy, SourceScopeHash: scope}
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
		InboundIDs          []int64         `json:"inbound_ids"`
		ProofHash           string          `json:"proof_hash"`
		PublicBaseHash      string          `json:"public_base_hash"`
		BotID               int64           `json:"bot_id"`
		CustomerID          string          `json:"customer_id"`
		RecurringID         string          `json:"recurring_id"`
		CurrentInvoiceID    string          `json:"current_invoice_id"`
		PaidAnchorInvoiceID string          `json:"paid_anchor_invoice_id"`
		Lifecycle           string          `json:"lifecycle"`
		DebtAmount          string          `json:"debt_amount"`
		CurrentInvoice      json.RawMessage `json:"current_invoice"`
		SUIID               int64           `json:"sui_id"`
		SUIName             string          `json:"sui_name"`
	}
	if json.Unmarshal(evidenceJSON, &evidence) != nil || len(evidence.InboundIDs) == 0 || evidence.ProofHash == "" || evidence.PublicBaseHash == "" || evidence.BotID != botID || evidence.CustomerID != customer || evidence.RecurringID != recurring || evidence.CurrentInvoiceID != originInvoice || evidence.PaidAnchorInvoiceID != anchor || evidence.Lifecycle != lifecycle || evidence.SUIID != suiID || evidence.SUIName != strconv.FormatInt(actor, 10) {
		return e, configdelivery.ErrDenied
	}
	seen := map[int64]bool{}
	for _, id := range evidence.InboundIDs {
		if id <= 0 || seen[id] {
			return e, configdelivery.ErrDenied
		}
		seen[id] = true
	}
	originalHash, ok := importorigin.ParseHashHex(evidence.ProofHash)
	publicBaseHash, okBase := importorigin.ParseHashHex(evidence.PublicBaseHash)
	if !ok || !okBase || fmt.Sprintf("%x", originalHash) != string(originalProofHash) {
		return e, configdelivery.ErrDenied
	}
	var originalEvidence payments.Evidence
	originalEvidenceErr := json.Unmarshal(originProofJSON, &originalEvidence)
	canonicalOriginal, canonicalErr := json.Marshal(originalEvidence)
	if originalEvidenceErr != nil || canonicalErr != nil ||
		originalEvidence.InvoiceID != anchor || originalEvidence.CustomerID != customer || originalEvidence.CurrencyID != originNativeCurrency ||
		originalEvidence.PaymentDate != originPaymentDate || originalEvidence.NextInvoiceDate != originNextDate ||
		fmt.Sprintf("%x", sha256.Sum256(canonicalOriginal)) != evidence.ProofHash {
		return e, configdelivery.ErrDenied
	}
	originalAmount, amountErr := billing.FormatAmount(json.Number(originalEvidence.Amount))
	frozenOriginAmount, frozenAmountErr := billing.FormatAmount(json.Number(originAmount))
	if amountErr != nil || frozenAmountErr != nil || originalAmount != frozenOriginAmount {
		return e, configdelivery.ErrDenied
	}
	if lifecycle == "active" && originInvoice != anchor || lifecycle == "paused" && (originInvoice == anchor || originInvoice == "" || anchor == "") {
		return e, configdelivery.ErrDenied
	}
	if lifecycle == "paused" {
		var frozenInvoice invoiceninja.Invoice
		debt, debtErr := billing.FormatAmount(json.Number(evidence.DebtAmount))
		invoiceAmount, invoiceAmountErr := billing.FormatAmount(json.Number(originAmount))
		if evidence.DebtAmount == "" || json.Unmarshal(evidence.CurrentInvoice, &frozenInvoice) != nil || debtErr != nil || invoiceAmountErr != nil || debt != invoiceAmount || frozenInvoice.ID != originInvoice || frozenInvoice.ClientID != customer || frozenInvoice.RecurringID != recurring || frozenInvoice.Number == "" {
			return e, configdelivery.ErrDenied
		}
		frozenInvoiceAmount, frozenInvoiceAmountErr := billing.FormatAmount(frozenInvoice.Amount)
		if frozenInvoiceAmountErr != nil || frozenInvoiceAmount != invoiceAmount {
			return e, configdelivery.ErrDenied
		}
	} else if evidence.DebtAmount != "" {
		return e, configdelivery.ErrDenied
	}
	baselineNext, baselineNextErr := invoiceninja.ParseRecurringUTC(template.NextSendDatetime)
	if baselineNextErr != nil && template.NextSendDatetime != "" {
		return e, configdelivery.ErrDenied
	}
	baseline := &configdelivery.ImportedBaseline{Origin: origin, AdmissionPermissionRevision: admissionRevision, InvoiceID: originInvoice,
		PaidAnchorInvoiceID: anchor, Lifecycle: lifecycle, ProofHash: originalHash, Profile: profile, InboundIDs: append([]int64(nil), evidence.InboundIDs...), NextSendUTC: baselineNext, PublicBaseHash: publicBaseHash}
	current, err := readActivationConfirmation(ctx, tx, invoice)
	if err != nil {
		return e, configdelivery.ErrDenied
	}
	proof, err := current.proof()
	if err != nil || current.clientID != clientID || current.botID != botID || current.customerID != customer || current.paid != paymentDate || current.next != nextDate || current.review || current.reviewCode != 0 || proof.InvoiceID != invoice || proof.Hash == originalHash {
		return e, configdelivery.ErrDenied
	}
	if originNativeCurrency != proof.CurrencyID || proof.Amount != frozenOriginAmount {
		return e, configdelivery.ErrDenied
	}
	var audit configdelivery.RenewalAudit
	if invoice != originInvoice {
		if invoice == anchor || (lifecycle != "active" && lifecycle != "paused") {
			return e, configdelivery.ErrDenied
		}
		if lifecycle == "paused" {
			var restored bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM invoice_renewal_actions a JOIN operations o ON o.id=a.operation_id
                WHERE a.client_id=$1 AND a.bot_id=$2 AND a.imported_origin_client_id=$1 AND a.origin_mode='imported_initial_paused'
                  AND a.outcome='completed' AND a.review_code=0 AND o.kind='invoice_paid_renewal' AND o.status='completed')`, clientID, botID).Scan(&restored); err != nil || !restored {
				return e, configdelivery.ErrDenied
			}
		}
		var li struct{ Customer, Recurring, Number, Status, Notice, Amount string }
		err = tx.QueryRow(ctx, `SELECT customer_id,recurring_id,invoice_number,status,notice_state,amount::text FROM invoice_lifecycle_invoices WHERE client_id=$1 AND bot_id=$2 AND invoice_id=$3`, clientID, botID, invoice).Scan(&li.Customer, &li.Recurring, &li.Number, &li.Status, &li.Notice, &li.Amount)
		ledgerAmount, amountErr := billing.FormatAmount(json.Number(li.Amount))
		if err != nil || amountErr != nil || li.Customer != customer || li.Recurring != recurring || li.Status != "paid" || li.Notice != "settled" || li.Number != proof.InvoiceNumber || ledgerAmount != proof.Amount || proof.CurrencyID == "" {
			return e, configdelivery.ErrDenied
		}
		audit, err = readImportedRenewalAudit(ctx, tx, clientID, botID, invoice, recurring, strconv.FormatInt(suiID, 10), proof, profile, evidence.InboundIDs, sui.Binding{ID: suiID, Name: strconv.FormatInt(telegramID, 10), CredentialsHash: credential}, "ledger")
		if err != nil {
			return e, configdelivery.ErrDenied
		}
	} else {
		frozenAmount, amountErr := billing.FormatAmount(json.Number(originAmount))
		var frozenInvoice invoiceninja.Invoice
		if json.Unmarshal(evidence.CurrentInvoice, &frozenInvoice) != nil || invoice != originInvoice || anchor == invoice || lifecycle != "paused" || amountErr != nil || proof.Amount != frozenAmount || proof.InvoiceNumber != frozenInvoice.Number {
			return e, configdelivery.ErrDenied
		}
		audit, err = readImportedRenewalAudit(ctx, tx, clientID, botID, invoice, recurring, strconv.FormatInt(suiID, 10), proof, profile, evidence.InboundIDs, sui.Binding{ID: suiID, Name: strconv.FormatInt(telegramID, 10), CredentialsHash: credential}, "imported_initial_paused")
		if err != nil {
			return e, configdelivery.ErrDenied
		}
	}
	if audit.Amount != frozenOriginAmount {
		return e, configdelivery.ErrDenied
	}
	e.Imported = baseline
	e.Period = configdelivery.RenewalPeriod
	e.Revision = configRevision
	e.Current = payments.Snapshot{ClientID: clientID, TelegramID: telegramID, ChatID: chatID, BotID: botID, CustomerID: customer, InvoiceID: invoice, RecurringID: recurring, SUIID: clientSUIID, State: state, DesiredAccess: desired, PaymentDate: paymentDate, NextInvoiceDate: nextDate, ImportedOrigin: origin, ImportedBaselineProofHash: [32]byte{}}
	e.CurrentProof = proof
	e.Binding = sui.Binding{ID: suiID, Name: strconv.FormatInt(telegramID, 10), CredentialsHash: credential}
	e.Renewal = audit
	if e.Validate() != nil {
		return configdelivery.Entitlement{}, configdelivery.ErrDenied
	}
	return e, nil
}

// The initial restoration audit is separate from ledger settlement evidence: it
// is authorized by the immutable paused origin and a completed native receipt.
func readImportedRenewalAudit(ctx context.Context, tx pgx.Tx, clientID, botID int64, invoiceID, recurringID, suiID string, proof payments.Proof, profile invoiceninja.ImportedRecurringProfile, inboundIDs []int64, binding sui.Binding, mode string) (configdelivery.RenewalAudit, error) {
	if mode != "ledger" && mode != "imported_initial_paused" {
		return configdelivery.RenewalAudit{}, configdelivery.ErrDenied
	}
	var audit configdelivery.RenewalAudit
	var target, amount string
	var next, planFrozenAt time.Time
	var baseline []byte
	var flags [4]bool
	err := tx.QueryRow(ctx, `SELECT a.operation_id,a.target_date::text,a.requires_pause,a.requires_schedule,a.requires_enable,a.requires_resume,a.baseline::text,a.recurring_amount::text,a.recurring_next_send_datetime,a.plan_frozen_at
		FROM invoice_renewal_actions a JOIN operations o ON o.id=a.operation_id
		WHERE a.client_id=$1 AND a.bot_id=$2 AND a.invoice_id=$3 AND a.origin_mode=$4 AND a.outcome='completed' AND a.review_code=0
		AND a.imported_origin_client_id=$1 AND a.imported_admission_permission_revision>0 AND a.imported_runtime_revision>0 AND a.imported_effect_revision>0
		AND o.kind='invoice_paid_renewal' AND o.status='completed'
		AND a.imported_origin_client_id=a.client_id AND a.imported_admission_permission_revision>0 AND a.imported_runtime_revision>0 AND a.imported_effect_revision>0
		AND EXISTS(SELECT 1 FROM verified_import_origins io WHERE io.client_id=a.client_id AND io.bot_id=a.bot_id
		  AND ((a.origin_mode='imported_initial_paused' AND io.lifecycle='paused' AND io.current_invoice_id=a.invoice_id AND io.paid_anchor_invoice_id<>io.current_invoice_id)
		    OR (a.origin_mode='ledger' AND a.invoice_id<>io.current_invoice_id AND a.invoice_id<>io.paid_anchor_invoice_id
		      AND (io.lifecycle='active' OR (io.lifecycle='paused' AND EXISTS(SELECT 1 FROM invoice_renewal_actions initial JOIN operations initial_op ON initial_op.id=initial.operation_id
		        WHERE initial.client_id=io.client_id AND initial.bot_id=io.bot_id AND initial.imported_origin_client_id=io.client_id
		          AND initial.origin_mode='imported_initial_paused' AND initial.outcome='completed' AND initial.review_code=0 AND initial_op.kind='invoice_paid_renewal' AND initial_op.status='completed'))))))`, clientID, botID, invoiceID, mode).
		Scan(&audit.OperationID, &target, &flags[0], &flags[1], &flags[2], &flags[3], &baseline, &amount, &next, &planFrozenAt)
	if err != nil || audit.OperationID <= 0 || target != proof.NextInvoiceDate || len(baseline) == 0 || planFrozenAt.IsZero() {
		return audit, configdelivery.ErrDenied
	}
	audit.OriginMode = mode
	audit.TargetDate, audit.RequiresPause, audit.RequiresSchedule, audit.RequiresEnable, audit.RequiresResume = target, flags[0], flags[1], flags[2], flags[3]
	parsed := next.UTC()
	if parsed.IsZero() {
		return audit, configdelivery.ErrDenied
	}
	audit.Amount, _, err = activationNormalize(amount, parsed)
	if err != nil {
		return audit, configdelivery.ErrDenied
	}
	audit.NextSendUTC = parsed
	audit.PlanHash = sha256.Sum256(baseline)
	var frozen struct {
		RecurringID      string      `json:"recurring_id"`
		StatusID         string      `json:"status_id"`
		NextSendDate     string      `json:"next_send_date"`
		Amount           json.Number `json:"amount"`
		NextSendDatetime string      `json:"next_send_datetime"`
		SUIID            int64       `json:"sui_id"`
		SUIEnabled       bool        `json:"sui_enabled"`
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(baseline, &fields) != nil || len(fields) != 7 {
		return audit, configdelivery.ErrDenied
	}
	for _, key := range []string{"recurring_id", "status_id", "next_send_date", "amount", "next_send_datetime", "sui_id", "sui_enabled"} {
		if fields[key] == nil {
			return audit, configdelivery.ErrDenied
		}
	}
	if json.Unmarshal(baseline, &frozen) != nil || frozen.RecurringID != recurringID || fmt.Sprint(frozen.SUIID) != suiID || (frozen.StatusID != "2" && frozen.StatusID != "3") || frozen.SUIEnabled == flags[2] {
		return audit, configdelivery.ErrDenied
	}
	oldDateOK := validRenewalDate(frozen.NextSendDate)
	oldAmount, oldUTC, oldErr := activation.NormalizeRecurringFacts(frozen.Amount, frozen.NextSendDatetime)
	var oldInstant time.Time
	if oldErr == nil {
		oldInstant, oldErr = time.Parse(time.RFC3339Nano, oldUTC)
	}
	finalAmount, finalUTC, finalErr := activation.NormalizeRecurringFacts(json.Number(audit.Amount), audit.NextSendUTC.Format(time.RFC3339Nano))
	var now time.Time
	if tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now) != nil {
		return audit, configdelivery.ErrDenied
	}
	oldPastAtFreeze := oldErr == nil && (!oldInstant.After(planFrozenAt) || frozen.NextSendDate <= planFrozenAt.In(time.Local).Format(time.DateOnly))
	shouldSchedule := frozen.NextSendDate != target || mode == "imported_initial_paused" && (oldErr != nil || !oldInstant.After(planFrozenAt))
	if finalErr != nil || finalAmount != audit.Amount || finalUTC != parsed.Format(time.RFC3339Nano) ||
		(mode == "ledger" && (!oldDateOK || oldErr != nil || oldPastAtFreeze)) ||
		(mode == "imported_initial_paused" && ((!oldDateOK && frozen.NextSendDate != "") || (oldErr != nil && frozen.NextSendDatetime != "") || ((!oldDateOK || oldErr != nil || frozen.NextSendDate == "" || frozen.NextSendDatetime == "" || oldPastAtFreeze) && !shouldSchedule))) ||
		flags[1] != shouldSchedule || flags[0] != (shouldSchedule && frozen.StatusID == "2") || flags[3] != (shouldSchedule || flags[2] || frozen.StatusID == "3") ||
		(mode == "imported_initial_paused" && (flags[0] || !flags[2] || !flags[3] || frozen.StatusID != "3" || frozen.SUIEnabled)) {
		return audit, configdelivery.ErrDenied
	}
	if !parsed.After(now) || target <= now.In(time.Local).Format(time.DateOnly) {
		return audit, configdelivery.ErrDenied
	}
	planned := []struct {
		name     string
		required bool
	}{{string(lifecycle.RenewalPause), flags[0]}, {string(lifecycle.RenewalSchedule), flags[1]}, {string(lifecycle.RenewalEnable), flags[2]}, {string(lifecycle.RenewalResume), flags[3]}}
	rows, err := tx.Query(ctx, `SELECT step,request::text,COALESCE(external_id,''),COALESCE(receipt::text,''),dispatch_attempt,confirmed_at IS NOT NULL FROM invoice_renewal_steps WHERE operation_id=$1 ORDER BY step`, audit.OperationID)
	if err != nil {
		return audit, err
	}
	defer rows.Close()
	seen := [4]bool{}
	for rows.Next() {
		var step, request, externalID, receipt string
		var attempt int
		var confirmed bool
		if rows.Scan(&step, &request, &externalID, &receipt, &attempt, &confirmed) != nil {
			return audit, configdelivery.ErrDenied
		}
		idx := -1
		for i, p := range planned {
			if p.name == step {
				idx = i
				break
			}
		}
		if idx < 0 || seen[idx] || !planned[idx].required || !confirmed || attempt < 1 || externalID == "" || (idx == 2 && externalID != suiID) || (idx != 2 && externalID != recurringID) {
			return audit, configdelivery.ErrDenied
		}
		switch idx {
		case 0:
			var d invoiceninja.ImportedPauseDraft
			if json.Unmarshal([]byte(request), &d) != nil || d.ProfileHash() != profile.Hash() || receipt != "" {
				return audit, configdelivery.ErrDenied
			}
		case 1:
			var d invoiceninja.ImportedScheduleDraft
			var wire struct {
				Date string `json:"date"`
			}
			var rec invoiceninja.RecurringReceipt
			if json.Unmarshal([]byte(request), &d) != nil || d.ProfileHash() != profile.Hash() || json.Unmarshal([]byte(request), &wire) != nil || wire.Date != target || decodeImportedRecurringReceipt([]byte(receipt), &rec) != nil {
				return audit, configdelivery.ErrDenied
			}
			ra, rt, re := activation.NormalizeRecurringFacts(rec.Amount, rec.NextSendDatetime)
			if re != nil || ra != finalAmount || rt != finalUTC {
				return audit, configdelivery.ErrDenied
			}
		case 2:
			var d sui.EnableDraft
			var wire struct {
				Before  json.RawMessage `json:"before"`
				Binding sui.Binding     `json:"binding"`
			}
			if json.Unmarshal([]byte(request), &d) != nil || json.Unmarshal([]byte(request), &wire) != nil || wire.Binding.ID <= 0 || len(wire.Before) == 0 {
				return audit, configdelivery.ErrDenied
			}
			before, e := sui.ParseClientRecord(wire.Before)
			if e != nil || wire.Binding != binding || d.Binding() != binding || before.Enabled() != frozen.SUIEnabled || frozen.SUIEnabled || sui.ValidateImportedRecordBinding(before, binding, inboundIDs) != nil {
				return audit, configdelivery.ErrDenied
			}
		case 3:
			var d invoiceninja.ImportedResumeDraft
			var wire struct {
				Receipt invoiceninja.RecurringReceipt `json:"receipt"`
			}
			if json.Unmarshal([]byte(request), &d) != nil || d.ProfileHash() != profile.Hash() || json.Unmarshal([]byte(request), &wire) != nil || receipt != "" {
				return audit, configdelivery.ErrDenied
			}
			resumeAmount, resumeUTC, resumeErr := activation.NormalizeRecurringFacts(wire.Receipt.Amount, wire.Receipt.NextSendDatetime)
			if resumeErr != nil || resumeAmount != finalAmount || resumeUTC != finalUTC {
				return audit, configdelivery.ErrDenied
			}
		}
		seen[idx] = true
		audit.StepIDs[idx] = externalID
		audit.StepHashes[idx] = sha256.Sum256([]byte(request + "\x00" + externalID + "\x00" + receipt))
	}
	if rows.Err() != nil {
		return audit, rows.Err()
	}
	for i, p := range planned {
		if seen[i] != p.required {
			return audit, configdelivery.ErrDenied
		}
	}
	if !flags[1] && (oldErr != nil || !oldDateOK || oldAmount != finalAmount || oldUTC != finalUTC || frozen.NextSendDate != target) {
		return audit, configdelivery.ErrDenied
	}
	return audit, nil
}

func decodeImportedRecurringReceipt(raw []byte, receipt *invoiceninja.RecurringReceipt) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return configdelivery.ErrDenied
	}
	seen := map[string]bool{}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		token, err = decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] || (key != "Amount" && key != "NextSendDatetime") {
			return configdelivery.ErrDenied
		}
		seen[key] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return configdelivery.ErrDenied
		}
		fields[key] = value
	}
	if len(seen) != 2 {
		return configdelivery.ErrDenied
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return configdelivery.ErrDenied
	}
	var extra any
	if err = decoder.Decode(&extra); err != io.EOF {
		return configdelivery.ErrDenied
	}
	encoded, err := json.Marshal(fields)
	if err != nil || json.Unmarshal(encoded, receipt) != nil {
		return configdelivery.ErrDenied
	}
	return nil
}

func activationNormalize(amount string, instant time.Time) (string, time.Time, error) {
	canonicalAmount, canonicalTime, err := activation.NormalizeRecurringFacts(json.Number(amount), instant.Format(time.RFC3339Nano))
	if err != nil {
		return "", time.Time{}, err
	}
	parsed, err := time.Parse(time.RFC3339Nano, canonicalTime)
	return canonicalAmount, parsed, err
}
