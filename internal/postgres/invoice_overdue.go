package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/importorigin"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/lifecycle"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func (s *Store) SeedOverdueOperations(ctx context.Context, botID int64, allowed []int64, today string, limit int) (count int, err error) {
	defer func() { err = invoiceLifecycleStageError("seed invoice overdue actions", err) }()
	if botID <= 0 || !lifecycleIDs(allowed) || len(allowed) == 0 || limit < 1 || limit > 1000 {
		return 0, errors.New("invalid invoice overdue seed bounds")
	}
	if parsed, parseErr := time.Parse(time.DateOnly, today); parseErr != nil || parsed.Format(time.DateOnly) != today {
		return 0, errors.New("invalid invoice overdue local date")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, err
	}
	defer rollback(tx)
	if err := prepareLifecycleBot(ctx, tx, botID); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `SELECT c.id, v.invoice_id, COALESCE(io.client_id,0),
        CASE WHEN io.client_id IS NULL THEN 0 ELSE ap.revision END,
        CASE WHEN io.client_id IS NULL THEN 0 ELSE rp.revision END,
        CASE WHEN io.client_id IS NULL THEN 0 ELSE ep.revision END
        FROM clients c JOIN subscriptions s ON s.client_id = c.id
        JOIN invoice_lifecycle_invoices v ON v.client_id = c.id AND v.invoice_id = s.current_invoice_id
        LEFT JOIN verified_import_origins io ON io.client_id=c.id AND io.bot_id=$1
        LEFT JOIN imported_runtime_permissions rp ON rp.bot_id=io.bot_id AND rp.enabled AND rp.policy_hash=io.policy_hash AND rp.source_scope_hash=io.source_scope_hash
        LEFT JOIN import_admission_policy ap ON ap.bot_id=io.bot_id AND ap.enabled AND ap.policy_hash=io.policy_hash AND ap.source_scope_hash=io.source_scope_hash
        LEFT JOIN imported_effect_permissions ep ON ep.bot_id=io.bot_id AND ep.enabled AND ep.policy_hash=io.policy_hash AND ep.source_scope_hash=io.source_scope_hash
        WHERE ((`+invoiceLifecycleEligible+` AND io.client_id IS NULL)
          OR (io.client_id=c.id AND rp.bot_id IS NOT NULL AND ap.bot_id IS NOT NULL AND ep.bot_id IS NOT NULL
            AND c.telegram_id=ANY($2::bigint[]) AND s.state='active' AND s.desired_access=true
            AND c.chat_id=c.telegram_id AND c.invoice_ninja_id=io.customer_id AND s.first_invoice_id IS NULL
            AND s.current_invoice_id IS NOT NULL AND s.recurring_id=io.recurring_id AND c.sui_id=io.sui_id::text AND io.sui_name=c.telegram_id::text))
          AND v.bot_id=$1 AND v.customer_id=c.invoice_ninja_id AND v.recurring_id=s.recurring_id
          AND v.invoice_id IS DISTINCT FROM s.first_invoice_id AND v.status='sent' AND v.sent_date<$3::date
          AND NOT EXISTS (SELECT 1 FROM invoice_overdue_actions a WHERE a.client_id=c.id AND a.invoice_id=v.invoice_id)
        ORDER BY v.sent_date,c.id LIMIT $4 FOR NO KEY UPDATE OF c SKIP LOCKED`, botID, allowed, today, limit)
	if err != nil {
		return 0, err
	}
	type candidate struct {
		clientID                    int64
		invoiceID                   string
		originID                    int64
		admissionPermissionRevision int64
		runtimeRevision             int64
		effectRevision              int64
	}
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
		var c candidate
		err := row.Scan(&c.clientID, &c.invoiceID, &c.originID, &c.admissionPermissionRevision, &c.runtimeRevision, &c.effectRevision)
		return c, err
	})
	if err != nil {
		return 0, err
	}
	if len(candidates) == 0 {
		return 0, tx.Commit(ctx)
	}
	clientIDs := make([]int64, 0, len(candidates))
	for _, c := range candidates {
		clientIDs = append(clientIDs, c.clientID)
	}
	if err := lockPaymentSubscriptions(ctx, tx, clientIDs); err != nil {
		return 0, err
	}
	for _, c := range candidates {
		keyHash := sha256.Sum256([]byte(c.invoiceID))
		key := "invoice-overdue:" + hex.EncodeToString(keyHash[:])
		var operationID int64
		err := tx.QueryRow(ctx, `INSERT INTO operations (client_id, operation_key, kind, status)
            VALUES ($1, $2, $3, 'pending') ON CONFLICT (operation_key) DO NOTHING RETURNING id`, c.clientID, key, lifecycle.OverdueOperationKind).Scan(&operationID)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO invoice_overdue_actions (operation_id, client_id, bot_id, invoice_id, imported_origin_client_id, imported_admission_permission_revision, imported_runtime_revision, imported_effect_revision)
            VALUES ($1, $2, $3, $4, NULLIF($5,0), NULLIF($6,0), NULLIF($7,0), NULLIF($8,0))`, operationID, c.clientID, botID, c.invoiceID, c.originID, c.admissionPermissionRevision, c.runtimeRevision, c.effectRevision); err != nil {
			return 0, err
		}
		count++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) LoadOverdueSnapshot(ctx context.Context, claim queue.Operation, botID int64) (snapshot lifecycle.EnforcementSnapshot, err error) {
	defer func() { err = invoiceLifecycleStageError("load invoice overdue snapshot", err) }()
	if claim.ID <= 0 || claim.Attempt <= 0 || claim.ClientID <= 0 || claim.Kind != lifecycle.OverdueOperationKind || botID <= 0 {
		return snapshot, queue.ErrLeaseLost
	}
	snapshot, err = readOverdueSnapshot(ctx, s.pool, claim, botID)
	if err != nil {
		return lifecycle.EnforcementSnapshot{}, err
	}
	return snapshot, nil
}

type overdueSnapshotReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func readOverdueSnapshot(ctx context.Context, reader overdueSnapshotReader, claim queue.Operation, botID int64) (lifecycle.EnforcementSnapshot, error) {
	var snapshot lifecycle.EnforcementSnapshot
	var recurringRequest, credentialHash []byte
	var actionOutcome string
	var actionReview int
	var suiID int64
	var paymentCheckReview, paymentConfirmationReview bool
	var hasOwnedRecurringJournal bool
	var importedOriginID, importedBotID, importedAdmissionRevision int64
	var importedAdmissionPermissionRevision, importedRuntimeRevision, importedEffectRevision int64
	var currentAdmissionPermissionRevision, currentRuntimeRevision, currentEffectRevision int64
	var currentConfigurationRevision int64
	var importedStable, importedPolicy, importedScope, importedCredential []byte
	var importedSUIID int64
	var importedSUIName, importedPrivateNotes string
	var importedTemplate, importedEvidence []byte
	var importedAllowed bool
	err := reader.QueryRow(ctx, `SELECT c.id, c.telegram_id, c.chat_id, a.bot_id,
        COALESCE(c.invoice_ninja_id, ''), COALESCE(s.first_invoice_id, ''), COALESCE(s.current_invoice_id, ''),
        COALESCE(s.recurring_id, ''), COALESCE(c.sui_id, ''), COALESCE(registration.token, ''), COALESCE(activation.token, ''),
        s.state, s.desired_access, v.invoice_id, v.invoice_number, v.status, v.sent_date::text,
        v.amount::text, v.balance::text, v.paid_to_date::text,
        COALESCE(recurring.request, '{}'::jsonb)::text, (recurring.request IS NOT NULL),
        COALESCE(binding.sui_id, 0), COALESCE(binding.name, ''), COALESCE(binding.credentials_hash, ''::bytea),
        (((a.imported_origin_client_id IS NULL) AND (`+invoiceLifecycleManaged+`)) OR
          (io.client_id=a.client_id AND io.bot_id=a.bot_id AND c.chat_id=c.telegram_id AND c.invoice_ninja_id=io.customer_id
           AND s.first_invoice_id IS NULL AND s.current_invoice_id IS NOT NULL AND s.current_invoice_id<>'' AND s.recurring_id=io.recurring_id
           AND c.sui_id=io.sui_id::text AND io.sui_name=c.telegram_id::text AND s.state IN ('active','overdue'))), p.review_code <> 0,
        (p.locked_until IS NOT NULL AND p.locked_until > clock_timestamp()),
        EXISTS (SELECT 1 FROM payment_confirmations pc WHERE pc.invoice_id = a.invoice_id),
        EXISTS (SELECT 1 FROM payment_confirmations pc WHERE pc.invoice_id = a.invoice_id AND (pc.review_required OR pc.review_code <> 0)),
        (lc.client_id IS NULL OR lc.review_code <> 0), g.epoch, (g.operation_id IS NOT NULL),
        COALESCE(g.operation_id, 0), COALESCE(g.step, ''), a.outcome, a.review_code,
        COALESCE(a.imported_origin_client_id,0), COALESCE(a.imported_admission_permission_revision,0), COALESCE(a.imported_runtime_revision,0), COALESCE(a.imported_effect_revision,0), c.configuration_revision,
        COALESCE(io.bot_id,0), COALESCE(io.admission_revision,0), COALESCE(io.stable_hash,''::bytea), COALESCE(io.policy_hash,''::bytea), COALESCE(io.source_scope_hash,''::bytea),
        COALESCE(io.template,'{}'::jsonb)::text, COALESCE(io.evidence,'{}'::jsonb)::text,
        COALESCE(io.sui_id,0), COALESCE(io.sui_name,''), COALESCE(io.credentials_hash,''::bytea), COALESCE(io.template->>'private_notes',''),
        COALESCE(ap.revision,0), COALESCE(rp.revision,0), COALESCE(ep.revision,0),
        (io.client_id IS NOT NULL AND COALESCE(ap.enabled,false) AND COALESCE(rp.enabled,false) AND COALESCE(ep.enabled,false)
          AND rp.policy_hash=io.policy_hash AND rp.source_scope_hash=io.source_scope_hash
          AND ap.policy_hash=io.policy_hash AND ap.source_scope_hash=io.source_scope_hash
          AND ap.revision=a.imported_admission_permission_revision
          AND ep.policy_hash=io.policy_hash AND ep.source_scope_hash=io.source_scope_hash
          AND rp.revision=a.imported_runtime_revision AND ep.revision=a.imported_effect_revision)
        FROM invoice_overdue_actions a JOIN operations o ON o.id = a.operation_id
        JOIN clients c ON c.id = a.client_id JOIN subscriptions s ON s.client_id = c.id
        JOIN invoice_lifecycle_invoices v ON v.client_id = a.client_id AND v.invoice_id = a.invoice_id
        JOIN payment_checks p ON p.client_id = c.id AND p.bot_id = a.bot_id
        LEFT JOIN invoice_lifecycle_checks lc ON lc.client_id = c.id AND lc.bot_id = a.bot_id
        LEFT JOIN LATERAL (
            SELECT rf.token FROM operations ro JOIN registration_flows rf ON rf.operation_id = ro.id
            WHERE ro.client_id = c.id AND ro.kind = 'register_client' AND ro.status = 'completed' AND rf.bot_id = a.bot_id
            ORDER BY ro.id DESC LIMIT 1
        ) registration ON true
        LEFT JOIN LATERAL (
            SELECT af.token, af.operation_id FROM operations ao JOIN activation_flows af ON af.operation_id = ao.id
            WHERE ao.client_id = c.id AND ao.kind = 'activate_first_subscription' AND ao.status = 'completed' AND af.bot_id = a.bot_id
            ORDER BY ao.id DESC LIMIT 1
        ) activation ON true
        LEFT JOIN activation_steps recurring ON recurring.operation_id = activation.operation_id AND recurring.step = 'recurring_create' AND recurring.confirmed_at IS NOT NULL
        LEFT JOIN activation_sui_bindings binding ON binding.client_id = c.id AND binding.bot_id = a.bot_id AND binding.sui_id::text = c.sui_id
        LEFT JOIN verified_import_origins io ON io.client_id=a.imported_origin_client_id
        LEFT JOIN import_admission_policy ap ON ap.bot_id=io.bot_id
        LEFT JOIN imported_runtime_permissions rp ON rp.bot_id=io.bot_id
        LEFT JOIN imported_effect_permissions ep ON ep.bot_id=io.bot_id
        CROSS JOIN activation_write_guard g
        WHERE g.id = 1 AND a.operation_id = $2 AND a.bot_id = $1 AND a.client_id = $3
          AND a.outcome = 'pending' AND a.review_code = 0
          AND o.kind = $4 AND o.status = 'running' AND o.attempts = $5 AND o.locked_until > clock_timestamp()`,
		botID, claim.ID, claim.ClientID, lifecycle.OverdueOperationKind, claim.Attempt).Scan(
		&snapshot.ClientID, &snapshot.TelegramID, &snapshot.ChatID, &snapshot.BotID,
		&snapshot.CustomerID, &snapshot.FirstInvoiceID, &snapshot.CurrentInvoiceID,
		&snapshot.RecurringID, &snapshot.SUIID, &snapshot.RegistrationToken, &snapshot.ActivationToken,
		&snapshot.State, &snapshot.DesiredAccess, &snapshot.Invoice.ID, &snapshot.Invoice.Number, &snapshot.Invoice.Status,
		&snapshot.Invoice.SentDate, &snapshot.Invoice.Amount, &snapshot.Invoice.Balance, &snapshot.Invoice.PaidToDate,
		&recurringRequest, &hasOwnedRecurringJournal, &suiID, &snapshot.SUIBinding.Name, &credentialHash, &snapshot.ManagedProvenance,
		&paymentCheckReview, &snapshot.PaymentLocked, &snapshot.PaymentConfirmed, &paymentConfirmationReview,
		&snapshot.LifecycleReview, &snapshot.WriteEpoch, &snapshot.WriterBusy, &snapshot.WriterOperationID,
		&snapshot.WriterStep, &actionOutcome, &actionReview,
		&importedOriginID, &importedAdmissionPermissionRevision, &importedRuntimeRevision, &importedEffectRevision, &currentConfigurationRevision,
		&importedBotID, &importedAdmissionRevision, &importedStable, &importedPolicy, &importedScope,
		&importedTemplate, &importedEvidence, &importedSUIID, &importedSUIName, &importedCredential, &importedPrivateNotes,
		&currentAdmissionPermissionRevision, &currentRuntimeRevision, &currentEffectRevision, &importedAllowed)
	if errors.Is(err, pgx.ErrNoRows) {
		return lifecycle.EnforcementSnapshot{}, queue.ErrLeaseLost
	}
	if err != nil {
		return lifecycle.EnforcementSnapshot{}, err
	}
	if actionOutcome != "pending" || actionReview != 0 {
		return lifecycle.EnforcementSnapshot{}, queue.ErrConflict
	}
	snapshot.InvoiceID = snapshot.Invoice.ID
	snapshot.PaymentReview = paymentCheckReview || paymentConfirmationReview
	if importedOriginID != 0 {
		stable, okStable := importorigin.ParseHash(importedStable)
		policy, okPolicy := importorigin.ParseHash(importedPolicy)
		scope, okScope := importorigin.ParseHash(importedScope)
		credential, okCredential := importorigin.ParseHash(importedCredential)
		if !okStable || !okPolicy || !okScope || !okCredential || importedOriginID != snapshot.ClientID || importedBotID != botID || importedAdmissionRevision <= 0 || importedAdmissionPermissionRevision <= 0 || importedRuntimeRevision <= 0 || importedEffectRevision <= 0 || importedSUIID <= 0 || importedSUIName == "" || len(importedEvidence) == 0 || len(importedTemplate) == 0 {
			return lifecycle.EnforcementSnapshot{}, queue.ErrConflict
		}
		var template invoiceninja.RecurringRecord
		if json.Unmarshal(importedTemplate, &template) != nil || len(template.LineItems) != 1 || template.ID != snapshot.RecurringID || template.ClientID != snapshot.CustomerID || importedPrivateNotes != template.PrivateNotes {
			return lifecycle.EnforcementSnapshot{}, queue.ErrConflict
		}
		profile, profileErr := invoiceninja.NewImportedRecurringProfile(template, template.LineItems[0].ProductKey)
		if profileErr != nil {
			return lifecycle.EnforcementSnapshot{}, queue.ErrConflict
		}
		var evidence struct {
			InboundIDs []int64 `json:"inbound_ids"`
		}
		if json.Unmarshal(importedEvidence, &evidence) != nil || len(evidence.InboundIDs) == 0 {
			return lifecycle.EnforcementSnapshot{}, queue.ErrConflict
		}
		seen := map[int64]bool{}
		for _, id := range evidence.InboundIDs {
			if id <= 0 || seen[id] {
				return lifecycle.EnforcementSnapshot{}, queue.ErrConflict
			}
			seen[id] = true
		}
		snapshot.ImportedOrigin = importorigin.Ref{ClientID: importedOriginID, BotID: importedBotID, AdmissionRevision: importedAdmissionRevision, RuntimeRevision: importedRuntimeRevision, ConfigurationRevision: currentConfigurationRevision, StableHash: stable, PolicyHash: policy, SourceScopeHash: scope}
		if !snapshot.ImportedOrigin.Valid() {
			return lifecycle.EnforcementSnapshot{}, queue.ErrConflict
		}
		snapshot.ImportedAdmissionPermissionRevision = importedAdmissionPermissionRevision
		snapshot.ImportedEffectsRevision = importedEffectRevision
		snapshot.ImportedEffectsAllowed = importedAllowed
		snapshot.ImportedRecurringProfile = profile
		snapshot.ImportedPrivateNotes = importedPrivateNotes
		snapshot.ImportedInboundIDs = append([]int64(nil), evidence.InboundIDs...)
		snapshot.SUIBinding = sui.Binding{ID: importedSUIID, Name: importedSUIName, CredentialsHash: credential}
		if suiID != 0 || hasOwnedRecurringJournal {
			return lifecycle.EnforcementSnapshot{}, queue.ErrConflict
		}
	} else {
		if len(credentialHash) != 32 || len(recurringRequest) == 0 || json.Unmarshal(recurringRequest, &snapshot.RecurringDraft) != nil {
			return lifecycle.EnforcementSnapshot{}, queue.ErrConflict
		}
		snapshot.SUIBinding.ID = suiID
		copy(snapshot.SUIBinding.CredentialsHash[:], credentialHash)
	}
	snapshot.Steps = make(map[lifecycle.EnforcementStep]lifecycle.OverdueStep, 3)
	rows, err := reader.Query(ctx, `SELECT step, request::text, COALESCE(external_id, ''), dispatch_attempt, confirmed_at IS NOT NULL
        FROM invoice_overdue_steps WHERE operation_id = $1 ORDER BY step`, claim.ID)
	if err != nil {
		return lifecycle.EnforcementSnapshot{}, err
	}
	for rows.Next() {
		var stepName string
		var raw []byte
		var value lifecycle.OverdueStep
		if err := rows.Scan(&stepName, &raw, &value.ExternalID, &value.DispatchAttempt, &value.Confirmed); err != nil {
			rows.Close()
			return lifecycle.EnforcementSnapshot{}, err
		}
		step := lifecycle.EnforcementStep(stepName)
		if !step.Valid() {
			rows.Close()
			return lifecycle.EnforcementSnapshot{}, queue.ErrConflict
		}
		value.Request = append([]byte(nil), raw...)
		snapshot.Steps[step] = value
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return lifecycle.EnforcementSnapshot{}, err
	}
	rows.Close()
	return snapshot, nil
}

func lockImportedEffectPermissions(ctx context.Context, tx pgx.Tx, clientID, botID int64) error {
	var originBot int64
	if err := tx.QueryRow(ctx, `SELECT bot_id FROM verified_import_origins WHERE client_id=$1`, clientID).Scan(&originBot); err != nil || originBot != botID {
		return queue.ErrConflict
	}
	for _, query := range []string{
		`SELECT bot_id FROM import_admission_policy WHERE bot_id=$1 FOR SHARE`,
		`SELECT bot_id FROM imported_runtime_permissions WHERE bot_id=$1 FOR SHARE`,
		`SELECT bot_id FROM imported_effect_permissions WHERE bot_id=$1 FOR SHARE`,
	} {
		var lockedBot int64
		err := tx.QueryRow(ctx, query, botID).Scan(&lockedBot)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	return nil
}

func validateImportedOverdueRequest(raw json.RawMessage, step lifecycle.EnforcementStep, snapshot lifecycle.EnforcementSnapshot) error {
	if step == lifecycle.OverduePauseRecurring {
		var draft invoiceninja.ImportedPauseDraft
		if json.Unmarshal(raw, &draft) != nil || draft.ProfileHash() != snapshot.ImportedRecurringProfile.Hash() {
			return queue.ErrConflict
		}
		return nil
	}
	var wire struct {
		Before  json.RawMessage `json:"before"`
		Binding sui.Binding     `json:"binding"`
	}
	if json.Unmarshal(raw, &wire) != nil || len(wire.Before) == 0 || wire.Binding != snapshot.SUIBinding {
		return queue.ErrConflict
	}
	before, err := sui.ParseClientRecord(wire.Before)
	if err != nil || sui.ValidateImportedRecordBinding(before, snapshot.SUIBinding, snapshot.ImportedInboundIDs) != nil {
		return queue.ErrConflict
	}
	if step == lifecycle.OverdueDisableSUI {
		var draft sui.DisableDraft
		if json.Unmarshal(raw, &draft) != nil || draft.Binding() != snapshot.SUIBinding {
			return queue.ErrConflict
		}
	} else {
		var draft sui.CloseSessionsDraft
		if json.Unmarshal(raw, &draft) != nil || draft.Binding() != snapshot.SUIBinding {
			return queue.ErrConflict
		}
	}
	return nil
}

func (s *Store) AuthorizeOverdueWrite(ctx context.Context, claim queue.Operation, botID int64, expected lifecycle.EnforcementSnapshot, step lifecycle.EnforcementStep, request json.RawMessage, today string, allowed []int64) (authorized bool, err error) {
	defer func() { err = invoiceLifecycleStageError("authorize invoice overdue write", err) }()
	if !step.Valid() || len(request) == 0 || len(request) > 4<<20 || !json.Valid(request) || !lifecycleIDs(allowed) {
		return false, errors.New("invalid invoice overdue intent")
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(request, &object) != nil || len(object) == 0 {
		return false, errors.New("invoice overdue intent must be a JSON object")
	}
	if parsed, parseErr := time.Parse(time.DateOnly, today); parseErr != nil || parsed.Format(time.DateOnly) != today {
		return false, errors.New("invalid invoice overdue local date")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, err
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `SET LOCAL synchronous_commit = on`); err != nil {
		return false, err
	}
	clientID, invoiceID, err := lockOverdueClaim(ctx, tx, claim, botID)
	if err != nil {
		return false, err
	}
	if expected.ImportedOrigin.Valid() {
		if err := lockImportedEffectPermissions(ctx, tx, expected.ImportedOrigin.ClientID, botID); err != nil {
			return false, err
		}
	}
	actual, err := readOverdueSnapshot(ctx, tx, claim, botID)
	if err != nil {
		return false, err
	}
	if clientID != expected.ClientID || invoiceID != expected.InvoiceID || !reflect.DeepEqual(actual, expected) {
		return false, queue.ErrConflict
	}
	if !actual.ManagedProvenance || !containsID(allowed, actual.TelegramID) || actual.PaymentConfirmed || actual.PaymentReview || actual.PaymentLocked || actual.LifecycleReview || actual.WriterBusy {
		return false, lifecycle.ErrOverdueNotUnpaid
	}
	if actual.Invoice.Status != "sent" || actual.Invoice.PaidToDate != "0" || actual.Invoice.Balance != actual.Invoice.Amount || actual.CurrentInvoiceID != actual.InvoiceID || (actual.ImportedOrigin.Valid() && actual.FirstInvoiceID != "") || (!actual.ImportedOrigin.Valid() && actual.FirstInvoiceID == actual.InvoiceID) || !overdueDatePassed(actual.Invoice.SentDate, today) {
		return false, lifecycle.ErrOverdueNotUnpaid
	}
	if actual.ImportedOrigin.Valid() {
		if !actual.ImportedEffectsAllowed || actual.ImportedAdmissionPermissionRevision != expected.ImportedAdmissionPermissionRevision || actual.ImportedOrigin.RuntimeRevision != expected.ImportedOrigin.RuntimeRevision || actual.ImportedEffectsRevision != expected.ImportedEffectsRevision || actual.ImportedOrigin.ClientID != actual.ClientID || actual.ImportedOrigin.BotID != botID || actual.RegistrationToken != "" || actual.ActivationToken != "" || actual.ImportedRecurringProfile.Hash() == "" || actual.RecurringDraft.ClientID != "" || actual.SUIBinding.ID <= 0 || strconv.FormatInt(actual.SUIBinding.ID, 10) != actual.SUIID {
			return false, queue.ErrConflict
		}
		if err := validateImportedOverdueRequest(request, step, actual); err != nil {
			return false, queue.ErrConflict
		}
	} else if actual.RegistrationToken == "" || actual.ActivationToken == "" || actual.RecurringDraft.ClientID != actual.CustomerID || actual.RecurringDraft.PrivateNotes != activation.Marker(actual.ActivationToken) || invoiceninja.ValidateRecurringDraft(actual.RecurringDraft) != nil || actual.SUIBinding.ID <= 0 || strconv.FormatInt(actual.SUIBinding.ID, 10) != actual.SUIID || actual.SUIBinding.Name == "" {
		return false, queue.ErrConflict
	}
	next, pending, complete, valid := lifecycle.NextOverdueStep(actual.Steps)
	if !valid {
		return false, queue.ErrConflict
	}
	if pending || complete || next != step {
		return false, lifecycle.ErrOverdueIntentExists
	}
	if step == lifecycle.OverduePauseRecurring || step == lifecycle.OverdueDisableSUI {
		if actual.State != "active" || !actual.DesiredAccess {
			return false, queue.ErrConflict
		}
	} else if actual.State != "overdue" || actual.DesiredAccess {
		return false, queue.ErrConflict
	}
	var epoch int64
	var ownerID int64
	var ownerStep string
	err = tx.QueryRow(ctx, `SELECT epoch, COALESCE(operation_id, 0), COALESCE(step, '')
        FROM activation_write_guard WHERE id = 1 FOR UPDATE`).Scan(&epoch, &ownerID, &ownerStep)
	if err != nil {
		return false, err
	}
	if ownerID != 0 {
		return false, lifecycle.ErrOverdueGuardBusy
	}
	if epoch != expected.WriteEpoch {
		return false, queue.ErrConflict
	}
	_, err = tx.Exec(ctx, `INSERT INTO invoice_overdue_steps (operation_id, step, request, dispatch_attempt)
        VALUES ($1, $2, $3::jsonb, $4)`, claim.ID, string(step), string(request), claim.Attempt)
	if err != nil {
		if isUniqueViolation(err) {
			return false, lifecycle.ErrOverdueIntentExists
		}
		return false, err
	}
	tag, err := tx.Exec(ctx, `UPDATE activation_write_guard SET operation_id = $1, step = $2, epoch = epoch + 1
        WHERE id = 1 AND operation_id IS NULL AND epoch = $3`, claim.ID, string(step), epoch)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() != 1 {
		return false, lifecycle.ErrOverdueGuardBusy
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) ConfirmOverdueStep(ctx context.Context, claim queue.Operation, step lifecycle.EnforcementStep, externalID string) (err error) {
	defer func() { err = invoiceLifecycleStageError("confirm invoice overdue write", err) }()
	if !step.Valid() || externalID == "" || len(externalID) > 256 {
		return errors.New("invalid invoice overdue confirmation")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer rollback(tx)
	var botID int64
	if err := tx.QueryRow(ctx, `SELECT bot_id FROM invoice_overdue_actions WHERE operation_id = $1`, claim.ID).Scan(&botID); errors.Is(err, pgx.ErrNoRows) {
		return queue.ErrLeaseLost
	} else if err != nil {
		return err
	}
	clientID, _, err := lockOverdueClaim(ctx, tx, claim, botID)
	if err != nil {
		return err
	}
	actual, err := readOverdueSnapshot(ctx, tx, claim, botID)
	if err != nil {
		return err
	}
	if step == lifecycle.OverduePauseRecurring {
		if externalID != actual.RecurringID {
			return queue.ErrConflict
		}
	} else if externalID != actual.SUIID {
		return queue.ErrConflict
	}
	next, pending, _, valid := lifecycle.NextOverdueStep(actual.Steps)
	if !valid || !pending || next != step {
		return queue.ErrConflict
	}
	var ownerID int64
	var ownerStep string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(operation_id, 0), COALESCE(step, '') FROM activation_write_guard WHERE id = 1 FOR UPDATE`).Scan(&ownerID, &ownerStep); err != nil {
		return err
	}
	if ownerID != claim.ID || ownerStep != string(step) {
		return queue.ErrConflict
	}
	var dispatchAttempt int
	var confirmed bool
	if err := tx.QueryRow(ctx, `SELECT dispatch_attempt, confirmed_at IS NOT NULL FROM invoice_overdue_steps
        WHERE operation_id = $1 AND step = $2 FOR UPDATE`, claim.ID, string(step)).Scan(&dispatchAttempt, &confirmed); errors.Is(err, pgx.ErrNoRows) {
		return queue.ErrConflict
	} else if err != nil {
		return err
	}
	if dispatchAttempt != claim.Attempt || confirmed {
		return queue.ErrConflict
	}
	tag, err := tx.Exec(ctx, `UPDATE invoice_overdue_steps SET external_id = $3, confirmed_at = clock_timestamp()
        WHERE operation_id = $1 AND step = $2 AND confirmed_at IS NULL`, claim.ID, string(step), externalID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrConflict
	}
	if step == lifecycle.OverdueDisableSUI {
		if _, err := tx.Exec(ctx, `UPDATE subscriptions SET state = 'overdue', desired_access = false
            WHERE client_id = $1 AND state = 'active' AND desired_access = true`, clientID); err != nil {
			return err
		}
	}
	tag, err = tx.Exec(ctx, `UPDATE activation_write_guard SET operation_id = NULL, step = NULL
        WHERE id = 1 AND operation_id = $1 AND step = $2`, claim.ID, string(step))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrConflict
	}
	return tx.Commit(ctx)
}

func (s *Store) HoldOverdue(ctx context.Context, claim queue.Operation, botID int64, expected lifecycle.EnforcementSnapshot, reviewCode int, drafts []queue.Draft) (err error) {
	defer func() { err = invoiceLifecycleStageError("hold invoice overdue action", err) }()
	if botID <= 0 || reviewCode <= 0 {
		return errors.New("invalid invoice overdue review")
	}
	if len(drafts) == 0 || len(drafts) > 99 {
		return errors.New("invoice overdue review requires administrator notices")
	}
	for _, draft := range drafts {
		if draft.BotID != botID || draft.Validate() != nil {
			return errors.New("invalid invoice overdue review notice")
		}
	}
	return s.finishOverdueOperation(ctx, claim, botID, expected, "review", reviewCode, drafts)
}

func (s *Store) FinishPaidOverdue(ctx context.Context, claim queue.Operation, botID int64, expected lifecycle.EnforcementSnapshot) (err error) {
	defer func() { err = invoiceLifecycleStageError("finish paid invoice overdue action", err) }()
	return s.finishOverdueOperation(ctx, claim, botID, expected, "paid", 0, nil)
}

func (s *Store) FinishOverdue(ctx context.Context, claim queue.Operation, botID int64, expected lifecycle.EnforcementSnapshot, drafts []queue.Draft) (err error) {
	defer func() { err = invoiceLifecycleStageError("finish invoice overdue action", err) }()
	if err := validateOverdueDrafts(drafts, botID); err != nil {
		return err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer rollback(tx)
	if err := prepareOutboxBots(ctx, tx, drafts); err != nil {
		return err
	}
	clientID, invoiceID, err := lockOverdueClaim(ctx, tx, claim, botID)
	if err != nil {
		return err
	}
	actual, err := readOverdueSnapshot(ctx, tx, claim, botID)
	if err != nil {
		return err
	}
	if clientID != expected.ClientID || invoiceID != expected.InvoiceID {
		return queue.ErrConflict
	}
	if actual.PaymentLocked {
		return lifecycle.ErrOverduePaymentLease
	}
	if !sameOverdueCompletionFacts(actual, expected) || actual.PaymentConfirmed || actual.PaymentReview || actual.LifecycleReview {
		return queue.ErrConflict
	}
	_, pending, complete, valid := lifecycle.NextOverdueStep(actual.Steps)
	if !valid || pending || !complete {
		return queue.ErrConflict
	}
	if actual.State != "overdue" || actual.DesiredAccess || actual.CurrentInvoiceID != actual.InvoiceID || !actual.ManagedProvenance {
		return queue.ErrConflict
	}
	if err := completeOverdueOperation(ctx, tx, claim, "completed", 0); err != nil {
		return err
	}
	for _, draft := range drafts {
		if _, err := enqueueOutbox(ctx, tx, draft); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) finishOverdueOperation(ctx context.Context, claim queue.Operation, botID int64, expected lifecycle.EnforcementSnapshot, outcome string, reviewCode int, drafts []queue.Draft) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer rollback(tx)
	if err := prepareOutboxBots(ctx, tx, drafts); err != nil {
		return err
	}
	clientID, invoiceID, err := lockOverdueClaim(ctx, tx, claim, botID)
	if err != nil {
		return err
	}
	actual, err := readOverdueSnapshot(ctx, tx, claim, botID)
	if err != nil {
		return err
	}
	if clientID != expected.ClientID || invoiceID != expected.InvoiceID || actual.ClientID != expected.ClientID || actual.InvoiceID != expected.InvoiceID {
		return queue.ErrConflict
	}
	if outcome == "paid" && len(actual.Steps) != 0 {
		return queue.ErrConflict
	}
	if outcome == "review" && !reflect.DeepEqual(actual.Steps, expected.Steps) {
		// A known step may have completed after the caller's snapshot; refresh and
		// retry review rather than losing that evidence in an administrator notice.
		return queue.ErrConflict
	}
	if err := completeOverdueOperation(ctx, tx, claim, outcome, reviewCode); err != nil {
		return err
	}
	for _, draft := range drafts {
		if _, err := enqueueOutbox(ctx, tx, draft); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func completeOverdueOperation(ctx context.Context, tx pgx.Tx, claim queue.Operation, outcome string, reviewCode int) error {
	tag, err := tx.Exec(ctx, `UPDATE invoice_overdue_actions SET outcome = $2, review_code = $3, finished_at = clock_timestamp()
        WHERE operation_id = $1 AND outcome = 'pending'`, claim.ID, outcome, reviewCode)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrLeaseLost
	}
	tag, err = tx.Exec(ctx, `UPDATE operations SET status = 'completed', locked_until = NULL,
        completed_at = clock_timestamp(), last_error_code = $3
        WHERE id = $1 AND attempts = $2 AND kind = $4 AND status = 'running' AND locked_until > clock_timestamp()`,
		claim.ID, claim.Attempt, reviewCode, lifecycle.OverdueOperationKind)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrLeaseLost
	}
	return nil
}

func lockOverdueClaim(ctx context.Context, tx pgx.Tx, claim queue.Operation, botID int64) (int64, string, error) {
	if claim.ID <= 0 || claim.Attempt <= 0 || claim.ClientID <= 0 || claim.Kind != lifecycle.OverdueOperationKind || botID <= 0 {
		return 0, "", queue.ErrLeaseLost
	}
	var clientID int64
	var invoiceID, firstInvoiceID string
	err := tx.QueryRow(ctx, `SELECT a.client_id, a.invoice_id, COALESCE(s.first_invoice_id, '')
        FROM invoice_overdue_actions a JOIN subscriptions s ON s.client_id = a.client_id
        WHERE a.operation_id = $1 AND a.bot_id = $2`, claim.ID, botID).Scan(&clientID, &invoiceID, &firstInvoiceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", queue.ErrLeaseLost
	}
	if err != nil {
		return 0, "", err
	}
	if clientID != claim.ClientID {
		return 0, "", queue.ErrConflict
	}
	if err := lockOperationClient(ctx, tx, claim.ID); err != nil {
		return 0, "", err
	}
	var lockedFirst string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(first_invoice_id, '') FROM subscriptions WHERE client_id = $1 FOR NO KEY UPDATE`, clientID).Scan(&lockedFirst); errors.Is(err, pgx.ErrNoRows) {
		return 0, "", queue.ErrConflict
	} else if err != nil {
		return 0, "", err
	}
	if err := tx.QueryRow(ctx, `SELECT client_id FROM payment_checks WHERE client_id = $1 AND bot_id = $2 FOR UPDATE`, clientID, botID).Scan(&clientID); errors.Is(err, pgx.ErrNoRows) {
		return 0, "", queue.ErrConflict
	} else if err != nil {
		return 0, "", err
	}
	confirmationIDs := []string{invoiceID}
	if lockedFirst != "" && lockedFirst != invoiceID {
		confirmationIDs = append(confirmationIDs, lockedFirst)
	}
	rows, err := tx.Query(ctx, `SELECT invoice_id FROM payment_confirmations WHERE invoice_id = ANY($1::text[])
        ORDER BY invoice_id FOR UPDATE`, confirmationIDs)
	if err != nil {
		return 0, "", err
	}
	if _, err := pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return 0, "", err
	}
	var leaseClient int64
	err = tx.QueryRow(ctx, `SELECT client_id FROM operations WHERE id = $1 AND client_id = $2 AND kind = $3
        AND status = 'running' AND attempts = $4 AND locked_until > clock_timestamp() FOR UPDATE`,
		claim.ID, claim.ClientID, lifecycle.OverdueOperationKind, claim.Attempt).Scan(&leaseClient)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", queue.ErrLeaseLost
	}
	if err != nil {
		return 0, "", err
	}
	var actionClient int64
	if err := tx.QueryRow(ctx, `SELECT client_id FROM invoice_overdue_actions
        WHERE operation_id = $1 AND bot_id = $2 AND outcome = 'pending' AND review_code = 0 FOR UPDATE`, claim.ID, botID).Scan(&actionClient); errors.Is(err, pgx.ErrNoRows) {
		return 0, "", queue.ErrLeaseLost
	} else if err != nil {
		return 0, "", err
	}
	if actionClient != clientID {
		return 0, "", queue.ErrConflict
	}
	rows, err = tx.Query(ctx, `SELECT step FROM invoice_overdue_steps WHERE operation_id = $1 ORDER BY step FOR UPDATE`, claim.ID)
	if err != nil {
		return 0, "", err
	}
	if _, err := pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return 0, "", err
	}
	return clientID, invoiceID, nil
}

func overdueDatePassed(sentDate, today string) bool {
	sent, err := time.Parse(time.DateOnly, sentDate)
	if err != nil {
		return false
	}
	current, err := time.Parse(time.DateOnly, today)
	return err == nil && sent.Before(current)
}

func sameOverdueCompletionFacts(actual, expected lifecycle.EnforcementSnapshot) bool {
	actual.WriteEpoch, actual.WriterBusy, actual.WriterOperationID, actual.WriterStep = 0, false, 0, ""
	expected.WriteEpoch, expected.WriterBusy, expected.WriterOperationID, expected.WriterStep = 0, false, 0, ""
	return reflect.DeepEqual(actual, expected)
}

func containsID(ids []int64, value int64) bool {
	for _, id := range ids {
		if id == value {
			return true
		}
	}
	return false
}

func validateOverdueDrafts(drafts []queue.Draft, botID int64) error {
	if len(drafts) < 2 || len(drafts) > 100 {
		return errors.New("invoice overdue completion requires client and administrator notices")
	}
	for _, draft := range drafts {
		if draft.BotID != botID || draft.Validate() != nil {
			return errors.New("invalid invoice overdue notice set")
		}
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
