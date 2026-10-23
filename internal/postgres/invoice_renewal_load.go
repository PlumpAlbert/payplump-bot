package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/importorigin"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/lifecycle"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
	"github.com/jackc/pgx/v5"
)

var _ lifecycle.RenewalStore = (*Store)(nil)

func (s *Store) SeedRenewalOperations(ctx context.Context, botID int64, allowed []int64, limit int) (count int, err error) {
	defer func() { err = invoiceLifecycleStageError("seed paid renewal actions", err) }()
	if botID <= 0 || !lifecycleIDs(allowed) || len(allowed) == 0 || limit < 1 || limit > 1000 {
		return 0, errors.New("invalid paid renewal seed bounds")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, err
	}
	defer rollback(tx)
	if err = prepareLifecycleBot(ctx, tx, botID); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `SELECT c.id, v.invoice_id, COALESCE(s.first_invoice_id,''), COALESCE(io.client_id,0)
        FROM clients c JOIN subscriptions s ON s.client_id = c.id
        JOIN invoice_lifecycle_invoices v ON v.client_id = c.id AND v.invoice_id = s.current_invoice_id
        JOIN payment_confirmations pc ON pc.invoice_id = v.invoice_id AND pc.client_id = c.id AND pc.bot_id = $1
        LEFT JOIN verified_import_origins io ON io.client_id=c.id AND io.bot_id=$1
        LEFT JOIN import_admission_policy ap ON ap.bot_id=io.bot_id
        LEFT JOIN imported_runtime_permissions rp ON rp.bot_id=io.bot_id
        LEFT JOIN imported_effect_permissions ep ON ep.bot_id=io.bot_id
        LEFT JOIN payment_checks p ON p.client_id=c.id AND p.bot_id=$1
        LEFT JOIN invoice_lifecycle_checks lc ON lc.client_id=c.id AND lc.bot_id=$1
        WHERE (((io.client_id IS NULL) AND NOT EXISTS (SELECT 1 FROM verified_import_origins owned_origin WHERE owned_origin.client_id=c.id) AND (`+invoiceLifecycleManaged+`)) OR
          (io.client_id=c.id AND c.chat_id=c.telegram_id AND c.telegram_id=ANY($2::bigint[])
           AND s.state IN ('active','overdue') AND ((s.state='active' AND s.desired_access) OR (s.state='overdue' AND NOT s.desired_access))
           AND c.invoice_ninja_id=io.customer_id AND s.first_invoice_id IS NULL AND s.current_invoice_id IS NOT NULL AND s.current_invoice_id<>''
           AND s.current_invoice_id<>io.current_invoice_id AND s.current_invoice_id<>io.paid_anchor_invoice_id
           AND (io.lifecycle='active' OR (io.lifecycle='paused' AND EXISTS (SELECT 1 FROM invoice_renewal_actions ia WHERE ia.client_id=c.id AND ia.origin_mode='imported_initial_paused' AND ia.outcome='completed')))
           AND s.recurring_id=io.recurring_id AND c.sui_id=io.sui_id::text AND io.sui_name=c.telegram_id::text
           AND ap.bot_id=io.bot_id AND ap.enabled AND ap.policy_hash=io.policy_hash AND ap.source_scope_hash=io.source_scope_hash
           AND rp.bot_id=io.bot_id AND rp.enabled AND rp.policy_hash=io.policy_hash AND rp.source_scope_hash=io.source_scope_hash
           AND ep.bot_id=io.bot_id AND ep.enabled AND ep.policy_hash=io.policy_hash AND ep.source_scope_hash=io.source_scope_hash))
          AND c.telegram_id=ANY($2::bigint[]) AND c.chat_id=c.telegram_id
          AND s.state IN ('active','overdue') AND ((s.state='active' AND s.desired_access) OR (s.state='overdue' AND NOT s.desired_access))
          AND v.bot_id=$1 AND v.customer_id=c.invoice_ninja_id AND v.recurring_id=s.recurring_id
          AND v.invoice_id IS DISTINCT FROM s.first_invoice_id AND v.status IN ('sent','partial','paid')
          AND pc.customer_id=c.invoice_ninja_id AND NOT pc.review_required AND pc.review_code=0
          AND p.client_id IS NOT NULL AND p.review_code=0 AND (p.locked_until IS NULL OR p.locked_until<=clock_timestamp())
          AND lc.client_id IS NOT NULL AND lc.review_code=0
          AND NOT EXISTS (SELECT 1 FROM invoice_renewal_actions a WHERE a.client_id=c.id AND a.invoice_id=v.invoice_id)
        ORDER BY pc.payment_date,c.id LIMIT $3 FOR NO KEY UPDATE OF c SKIP LOCKED`, botID, allowed, limit)
	if err != nil {
		return 0, err
	}
	type candidate struct {
		clientID         int64
		invoiceID        string
		firstInvoiceID   string
		importedOriginID int64
		initialPaused    bool
	}
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
		var c candidate
		e := row.Scan(&c.clientID, &c.invoiceID, &c.firstInvoiceID, &c.importedOriginID)
		return c, e
	})
	if err != nil {
		return 0, err
	}
	remaining := limit - len(candidates)
	if remaining > 0 {
		initialRows, queryErr := tx.Query(ctx, `SELECT c.id, io.current_invoice_id, COALESCE(s.first_invoice_id,''), io.client_id
        FROM clients c JOIN subscriptions s ON s.client_id=c.id
        JOIN verified_import_origins io ON io.client_id=c.id AND io.bot_id=$1
        JOIN payment_confirmations pc ON pc.invoice_id=io.current_invoice_id AND pc.client_id=c.id AND pc.bot_id=$1
        JOIN payment_checks p ON p.client_id=c.id AND p.bot_id=$1
        LEFT JOIN invoice_lifecycle_checks lc ON lc.client_id=c.id AND lc.bot_id=$1
        JOIN import_admission_policy ap ON ap.bot_id=io.bot_id
        JOIN imported_runtime_permissions rp ON rp.bot_id=io.bot_id
        JOIN imported_effect_permissions ep ON ep.bot_id=io.bot_id
        WHERE c.telegram_id=ANY($2::bigint[]) AND c.chat_id=c.telegram_id
          AND c.invoice_ninja_id=io.customer_id AND s.first_invoice_id IS NULL
          AND s.state='overdue' AND NOT s.desired_access AND s.current_invoice_id=io.current_invoice_id
          AND s.recurring_id=io.recurring_id AND c.sui_id=io.sui_id::text AND io.sui_name=c.telegram_id::text
          AND io.lifecycle='paused' AND io.current_invoice_id<>io.paid_anchor_invoice_id
          AND NOT EXISTS (SELECT 1 FROM invoice_lifecycle_invoices v WHERE v.client_id=c.id AND v.invoice_id=io.current_invoice_id)
          AND pc.customer_id=io.customer_id AND NOT pc.review_required AND pc.review_code=0
          AND p.review_code=0 AND (p.locked_until IS NULL OR p.locked_until<=clock_timestamp())
          AND (lc.client_id IS NULL OR lc.review_code=0)
          AND ap.enabled AND ap.policy_hash=io.policy_hash AND ap.source_scope_hash=io.source_scope_hash
          AND rp.enabled AND rp.policy_hash=io.policy_hash AND rp.source_scope_hash=io.source_scope_hash
          AND ep.enabled AND ep.policy_hash=io.policy_hash AND ep.source_scope_hash=io.source_scope_hash
          AND NOT EXISTS (SELECT 1 FROM invoice_renewal_actions a WHERE a.client_id=c.id AND a.invoice_id=io.current_invoice_id)
        ORDER BY pc.payment_date,c.id LIMIT $3 FOR NO KEY UPDATE OF c SKIP LOCKED`, botID, allowed, remaining)
		if queryErr != nil {
			return 0, queryErr
		}
		initialCandidates, collectErr := pgx.CollectRows(initialRows, func(row pgx.CollectableRow) (candidate, error) {
			var c candidate
			c.initialPaused = true
			e := row.Scan(&c.clientID, &c.invoiceID, &c.firstInvoiceID, &c.importedOriginID)
			return c, e
		})
		if collectErr != nil {
			return 0, collectErr
		}
		candidates = append(candidates, initialCandidates...)
	}
	if len(candidates) == 0 {
		return 0, tx.Commit(ctx)
	}
	clientIDs := make([]int64, 0, len(candidates))
	invoiceIDs := make([]string, 0, len(candidates)*2)
	for _, c := range candidates {
		clientIDs = append(clientIDs, c.clientID)
		invoiceIDs = append(invoiceIDs, c.invoiceID)
		if !c.initialPaused && c.firstInvoiceID != "" && c.firstInvoiceID != c.invoiceID {
			invoiceIDs = append(invoiceIDs, c.firstInvoiceID)
		}
	}
	if err = lockPaymentSubscriptions(ctx, tx, clientIDs); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `SELECT client_id FROM payment_checks WHERE client_id = ANY($1::bigint[]) AND bot_id = $2 ORDER BY client_id FOR UPDATE`, clientIDs, botID); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `SELECT invoice_id FROM payment_confirmations WHERE invoice_id = ANY($1::text[]) ORDER BY invoice_id FOR UPDATE`, invoiceIDs); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `SELECT invoice_id FROM invoice_lifecycle_invoices WHERE invoice_id = ANY($1::text[]) ORDER BY invoice_id FOR UPDATE`, invoiceIDs); err != nil {
		return 0, err
	}
	for _, c := range candidates {
		var admissionRevision, runtimeRevision, effectRevision int64
		if c.importedOriginID != 0 {
			if err = lockImportedEffectPermissions(ctx, tx, c.importedOriginID, botID); err != nil {
				return 0, err
			}
		}
		var eligible bool
		err = tx.QueryRow(ctx, `SELECT (CASE WHEN $5 THEN
              io.client_id=c.id AND io.bot_id=$1 AND io.lifecycle='paused' AND io.current_invoice_id=$3 AND io.current_invoice_id<>io.paid_anchor_invoice_id
              AND c.telegram_id=ANY($2::bigint[]) AND c.chat_id=c.telegram_id
              AND c.invoice_ninja_id=io.customer_id AND s.first_invoice_id IS NULL AND s.current_invoice_id=$3 AND s.state='overdue' AND NOT s.desired_access
              AND s.recurring_id=io.recurring_id AND c.sui_id=io.sui_id::text AND io.sui_name=c.telegram_id::text
              AND ap.enabled AND ap.policy_hash=io.policy_hash AND ap.source_scope_hash=io.source_scope_hash
              AND rp.enabled AND rp.policy_hash=io.policy_hash AND rp.source_scope_hash=io.source_scope_hash
              AND ep.enabled AND ep.policy_hash=io.policy_hash AND ep.source_scope_hash=io.source_scope_hash
              AND pc.customer_id=io.customer_id AND NOT pc.review_required AND pc.review_code=0
              AND p.client_id IS NOT NULL AND p.review_code=0 AND (p.locked_until IS NULL OR p.locked_until<=clock_timestamp())
              AND (lc.client_id IS NULL OR lc.review_code=0)
              AND NOT EXISTS (SELECT 1 FROM invoice_lifecycle_invoices iv WHERE iv.client_id=c.id AND iv.invoice_id=$3)
              AND NOT EXISTS (SELECT 1 FROM invoice_renewal_actions a WHERE a.client_id=c.id AND a.invoice_id=$3)
          ELSE (((io.client_id IS NULL) AND NOT EXISTS (SELECT 1 FROM verified_import_origins owned_origin WHERE owned_origin.client_id=c.id) AND (`+invoiceLifecycleManaged+`)) OR
            (io.client_id=c.id AND io.bot_id=$1 AND c.chat_id=c.telegram_id AND c.telegram_id=ANY($2::bigint[])
             AND c.invoice_ninja_id=io.customer_id AND s.first_invoice_id IS NULL AND s.current_invoice_id IS NOT NULL AND s.current_invoice_id<>''
             AND s.current_invoice_id<>io.current_invoice_id AND s.current_invoice_id<>io.paid_anchor_invoice_id
             AND (io.lifecycle='active' OR (io.lifecycle='paused' AND EXISTS (SELECT 1 FROM invoice_renewal_actions ia WHERE ia.client_id=c.id AND ia.origin_mode='imported_initial_paused' AND ia.outcome='completed')))
             AND s.recurring_id=io.recurring_id AND c.sui_id=io.sui_id::text AND io.sui_name=c.telegram_id::text
             AND ap.enabled AND ap.policy_hash=io.policy_hash AND ap.source_scope_hash=io.source_scope_hash
             AND rp.enabled AND rp.policy_hash=io.policy_hash AND rp.source_scope_hash=io.source_scope_hash
             AND ep.enabled AND ep.policy_hash=io.policy_hash AND ep.source_scope_hash=io.source_scope_hash))
            AND c.telegram_id=ANY($2::bigint[]) AND c.chat_id=c.telegram_id
            AND s.state IN ('active','overdue') AND ((s.state='active' AND s.desired_access) OR (s.state='overdue' AND NOT s.desired_access))
            AND s.current_invoice_id=$3 AND v.bot_id=$1 AND v.customer_id=c.invoice_ninja_id AND v.recurring_id=s.recurring_id
            AND v.invoice_id IS DISTINCT FROM s.first_invoice_id AND v.status IN ('sent','partial','paid')
            AND pc.customer_id=c.invoice_ninja_id AND NOT pc.review_required AND pc.review_code=0
            AND p.client_id IS NOT NULL AND p.review_code=0 AND (p.locked_until IS NULL OR p.locked_until<=clock_timestamp())
            AND lc.client_id IS NOT NULL AND lc.review_code=0
            AND NOT EXISTS (SELECT 1 FROM invoice_renewal_actions a WHERE a.client_id=c.id AND a.invoice_id=v.invoice_id) END),
            COALESCE(ap.revision,0),COALESCE(rp.revision,0),COALESCE(ep.revision,0)
            FROM clients c JOIN subscriptions s ON s.client_id=c.id
            LEFT JOIN invoice_lifecycle_invoices v ON v.client_id=c.id AND v.invoice_id=$3
            JOIN payment_confirmations pc ON pc.invoice_id=$3 AND pc.client_id=c.id AND pc.bot_id=$1
            LEFT JOIN verified_import_origins io ON io.client_id=c.id AND io.bot_id=$1
            LEFT JOIN import_admission_policy ap ON ap.bot_id=io.bot_id
            LEFT JOIN imported_runtime_permissions rp ON rp.bot_id=io.bot_id
            LEFT JOIN imported_effect_permissions ep ON ep.bot_id=io.bot_id
            LEFT JOIN payment_checks p ON p.client_id=c.id AND p.bot_id=$1
            LEFT JOIN invoice_lifecycle_checks lc ON lc.client_id=c.id AND lc.bot_id=$1
            WHERE c.id=$4`, botID, allowed, c.invoiceID, c.clientID, c.initialPaused).Scan(&eligible, &admissionRevision, &runtimeRevision, &effectRevision)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if !eligible {
			continue
		}
		if c.importedOriginID != 0 && (admissionRevision <= 0 || runtimeRevision <= 0 || effectRevision <= 0) {
			continue
		}
		digest := sha256.Sum256([]byte(c.invoiceID))
		key := "invoice-renewal:" + hex.EncodeToString(digest[:])
		var operationID int64
		err = tx.QueryRow(ctx, `INSERT INTO operations (client_id, operation_key, kind, status)
            VALUES ($1, $2, $3, 'pending') ON CONFLICT (operation_key) DO NOTHING RETURNING id`, c.clientID, key, lifecycle.RenewalOperationKind).Scan(&operationID)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO invoice_renewal_actions (operation_id,client_id,bot_id,invoice_id,origin_mode,imported_origin_client_id,imported_admission_permission_revision,imported_runtime_revision,imported_effect_revision)
            VALUES ($1,$2,$3,$4,CASE WHEN $9 THEN 'imported_initial_paused' ELSE 'ledger' END,NULLIF($5,0),NULLIF($6,0),NULLIF($7,0),NULLIF($8,0))`, operationID, c.clientID, botID, c.invoiceID, c.importedOriginID, admissionRevision, runtimeRevision, effectRevision, c.initialPaused)
		if err != nil {
			return 0, err
		}
		count++
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Store) LoadRenewalSnapshot(ctx context.Context, claim queue.Operation, botID int64) (snapshot lifecycle.RenewalSnapshot, err error) {
	defer func() { err = invoiceLifecycleStageError("load paid renewal snapshot", err) }()
	if claim.ID <= 0 || claim.Attempt <= 0 || claim.ClientID <= 0 || claim.Kind != lifecycle.RenewalOperationKind || botID <= 0 {
		return snapshot, queue.ErrLeaseLost
	}
	snapshot, err = readRenewalSnapshot(ctx, s.pool, claim, botID)
	if err != nil {
		return lifecycle.RenewalSnapshot{}, err
	}
	return snapshot, nil
}

type renewalSnapshotReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func readRenewalSnapshot(ctx context.Context, reader renewalSnapshotReader, claim queue.Operation, botID int64) (lifecycle.RenewalSnapshot, error) {
	var s lifecycle.RenewalSnapshot
	var recurringRequest, evidence, proofHash, credentialHash, baseline []byte
	var amount, currencyID, paymentDate, nextDate, planDate string
	var actionOutcome string
	var reviewCode int
	var planFrozen, planPause, planSchedule, planEnable, planResume bool
	var suiID int64
	var paymentCheckReview, paymentConfirmationReview bool
	var hasOwnedRecurringJournal bool
	var importedOriginID, importedAdmissionPermissionRevision, importedRuntimeRevision, importedEffectRevision int64
	var importedBotID, importedAdmissionRevision, importedSUIID int64
	var currentAdmissionPermissionRevision, currentRuntimeRevision, currentEffectRevision, currentConfigurationRevision int64
	var importedStable, importedPolicy, importedScope, importedCredential, importedTemplate, importedEvidence []byte
	var importedSUIName, importedPrivateNotes, importedBaselineCurrentInvoiceID, importedPaidAnchorInvoiceID string
	var importedAllowed bool
	var originMode string
	err := reader.QueryRow(ctx, `SELECT c.id, c.telegram_id, c.chat_id, a.bot_id, COALESCE(a.invoice_id, ''), a.origin_mode,
        COALESCE(c.invoice_ninja_id, ''), COALESCE(s.first_invoice_id, ''), COALESCE(s.current_invoice_id, ''),
        COALESCE(s.recurring_id, ''), COALESCE(c.sui_id, ''), COALESCE(reg.token, ''), COALESCE(act.token, ''),
        s.state, s.desired_access, COALESCE(v.customer_id,''), COALESCE(v.recurring_id,''), COALESCE(v.invoice_number,''), COALESCE(v.status,''), COALESCE(v.amount::text,''), COALESCE(v.balance::text,''), COALESCE(v.paid_to_date::text,''), COALESCE(v.sent_date::text,''),
        pc.customer_id, pc.invoice_id, pc.invoice_number, pc.amount::text, pc.currency_id, pc.payment_date::text, pc.next_invoice_date::text, pc.proof_hash, pc.evidence::text,
        COALESCE(rec.request, '{}'::jsonb)::text, (rec.request IS NOT NULL), COALESCE(binding.sui_id, 0), COALESCE(binding.name, ''), COALESCE(binding.credentials_hash, ''::bytea),
        ((a.imported_origin_client_id IS NULL AND NOT EXISTS (SELECT 1 FROM verified_import_origins owned_origin WHERE owned_origin.client_id=c.id) AND (`+invoiceLifecycleManaged+`)) OR
          (a.imported_origin_client_id=c.id AND io.client_id=c.id AND io.bot_id=a.bot_id AND c.chat_id=c.telegram_id
           AND c.invoice_ninja_id=io.customer_id AND s.first_invoice_id IS NULL AND s.current_invoice_id IS NOT NULL AND s.current_invoice_id<>''
           AND ((a.origin_mode='ledger' AND s.current_invoice_id<>io.current_invoice_id AND s.current_invoice_id<>io.paid_anchor_invoice_id
                 AND (io.lifecycle='active' OR (io.lifecycle='paused' AND EXISTS (SELECT 1 FROM invoice_renewal_actions ia WHERE ia.client_id=c.id AND ia.origin_mode='imported_initial_paused' AND ia.outcome='completed'))))
             OR (a.origin_mode='imported_initial_paused' AND io.lifecycle='paused' AND io.current_invoice_id=s.current_invoice_id AND io.current_invoice_id<>io.paid_anchor_invoice_id))
           AND s.recurring_id=io.recurring_id AND c.sui_id=io.sui_id::text AND io.sui_name=c.telegram_id::text
           AND s.state IN ('active','overdue') AND ((s.state='active' AND s.desired_access) OR (s.state='overdue' AND NOT s.desired_access)))), p.review_code <> 0, (p.locked_until IS NOT NULL AND p.locked_until > clock_timestamp()),
        (pc.review_required OR pc.review_code <> 0), (CASE WHEN a.origin_mode='imported_initial_paused' THEN (lc.client_id IS NOT NULL AND lc.review_code <> 0) ELSE (lc.client_id IS NULL OR lc.review_code <> 0) END),
        g.epoch, (g.operation_id IS NOT NULL), COALESCE(g.operation_id, 0), COALESCE(g.step, ''),
        a.outcome, a.review_code, (a.plan_frozen_at IS NOT NULL), COALESCE(a.target_date::text, ''),
        COALESCE(a.requires_pause, false), COALESCE(a.requires_schedule, false), COALESCE(a.requires_enable, false), COALESCE(a.requires_resume, false), COALESCE(a.baseline, '{}'::jsonb)::text,
        COALESCE(a.imported_origin_client_id,0), COALESCE(a.imported_admission_permission_revision,0), COALESCE(a.imported_runtime_revision,0), COALESCE(a.imported_effect_revision,0), c.configuration_revision,
        COALESCE(io.bot_id,0), COALESCE(io.admission_revision,0), COALESCE(io.stable_hash,''::bytea), COALESCE(io.policy_hash,''::bytea), COALESCE(io.source_scope_hash,''::bytea),
        COALESCE(io.template,'{}'::jsonb)::text, COALESCE(io.evidence,'{}'::jsonb)::text,
        COALESCE(io.sui_id,0), COALESCE(io.sui_name,''), COALESCE(io.credentials_hash,''::bytea), COALESCE(io.template->>'private_notes',''), COALESCE(io.current_invoice_id,''), COALESCE(io.paid_anchor_invoice_id,''),
        COALESCE(ap.revision,0), COALESCE(rp.revision,0), COALESCE(ep.revision,0),
        (io.client_id IS NOT NULL AND COALESCE(ap.enabled,false) AND COALESCE(rp.enabled,false) AND COALESCE(ep.enabled,false)
          AND ap.policy_hash=io.policy_hash AND ap.source_scope_hash=io.source_scope_hash
          AND rp.policy_hash=io.policy_hash AND rp.source_scope_hash=io.source_scope_hash
          AND ep.policy_hash=io.policy_hash AND ep.source_scope_hash=io.source_scope_hash
          AND ap.revision=a.imported_admission_permission_revision AND rp.revision=a.imported_runtime_revision AND ep.revision=a.imported_effect_revision)
        FROM invoice_renewal_actions a JOIN operations o ON o.id = a.operation_id
        JOIN clients c ON c.id = a.client_id JOIN subscriptions s ON s.client_id = c.id
        LEFT JOIN invoice_lifecycle_invoices v ON v.client_id = a.client_id AND v.invoice_id = a.ledger_invoice_id
        JOIN payment_checks p ON p.client_id = c.id AND p.bot_id = a.bot_id
        JOIN payment_confirmations pc ON pc.invoice_id = a.invoice_id AND pc.client_id = a.client_id AND pc.bot_id = a.bot_id
        LEFT JOIN invoice_lifecycle_checks lc ON lc.client_id = c.id AND lc.bot_id = a.bot_id
        LEFT JOIN LATERAL (SELECT rf.token FROM operations ro JOIN registration_flows rf ON rf.operation_id = ro.id WHERE ro.client_id = c.id AND ro.kind = 'register_client' AND ro.status = 'completed' AND rf.bot_id = a.bot_id ORDER BY ro.id DESC LIMIT 1) reg ON true
        LEFT JOIN LATERAL (SELECT af.token, af.operation_id FROM operations ao JOIN activation_flows af ON af.operation_id = ao.id WHERE ao.client_id = c.id AND ao.kind = 'activate_first_subscription' AND ao.status = 'completed' AND af.bot_id = a.bot_id ORDER BY ao.id DESC LIMIT 1) act ON true
        LEFT JOIN activation_steps rec ON rec.operation_id = act.operation_id AND rec.step = 'recurring_create' AND rec.confirmed_at IS NOT NULL
        LEFT JOIN activation_sui_bindings binding ON binding.client_id = c.id AND binding.bot_id = a.bot_id AND binding.sui_id::text = c.sui_id
        LEFT JOIN verified_import_origins io ON io.client_id=a.imported_origin_client_id
        LEFT JOIN import_admission_policy ap ON ap.bot_id=io.bot_id
        LEFT JOIN imported_runtime_permissions rp ON rp.bot_id=io.bot_id
        LEFT JOIN imported_effect_permissions ep ON ep.bot_id=io.bot_id
        CROSS JOIN activation_write_guard g
        WHERE g.id = 1 AND a.operation_id = $2 AND a.bot_id = $1 AND a.client_id = $3 AND a.outcome = 'pending' AND a.review_code = 0
          AND o.kind = $4 AND o.status = 'running' AND o.attempts = $5 AND o.locked_until > clock_timestamp()`,
		botID, claim.ID, claim.ClientID, lifecycle.RenewalOperationKind, claim.Attempt).Scan(
		&s.ClientID, &s.TelegramID, &s.ChatID, &s.BotID, &s.InvoiceID, &originMode, &s.CustomerID, &s.FirstInvoiceID, &s.CurrentInvoiceID,
		&s.RecurringID, &s.SUIID, &s.RegistrationToken, &s.ActivationToken, &s.State, &s.DesiredAccess,
		&s.LedgerCustomerID, &s.LedgerRecurringID, &s.LedgerInvoiceNumber, &s.LedgerStatus, &s.LedgerAmount, &s.LedgerBalance, &s.LedgerPaidToDate, &s.LedgerSentDate,
		&s.Payment.CustomerID, &s.Payment.InvoiceID, &s.Proof.InvoiceNumber, &amount, &currencyID, &paymentDate, &nextDate, &proofHash, &evidence,
		&recurringRequest, &hasOwnedRecurringJournal, &suiID, &s.SUIBinding.Name, &credentialHash, &s.ManagedProvenance, &paymentCheckReview, &s.PaymentLocked,
		&paymentConfirmationReview, &s.LifecycleReview, &s.WriteEpoch, &s.WriterBusy, &s.WriterOperationID, &s.WriterStep,
		&actionOutcome, &reviewCode, &planFrozen, &planDate, &planPause, &planSchedule, &planEnable, &planResume, &baseline,
		&importedOriginID, &importedAdmissionPermissionRevision, &importedRuntimeRevision, &importedEffectRevision, &currentConfigurationRevision,
		&importedBotID, &importedAdmissionRevision, &importedStable, &importedPolicy, &importedScope, &importedTemplate, &importedEvidence,
		&importedSUIID, &importedSUIName, &importedCredential, &importedPrivateNotes, &importedBaselineCurrentInvoiceID, &importedPaidAnchorInvoiceID,
		&currentAdmissionPermissionRevision, &currentRuntimeRevision, &currentEffectRevision, &importedAllowed)
	if errors.Is(err, pgx.ErrNoRows) {
		return lifecycle.RenewalSnapshot{}, queue.ErrLeaseLost
	}
	if err != nil {
		return lifecycle.RenewalSnapshot{}, err
	}
	if actionOutcome != "pending" || reviewCode != 0 || len(proofHash) != 32 || originMode != "ledger" && originMode != "imported_initial_paused" || originMode == "imported_initial_paused" && importedOriginID == 0 {
		return lifecycle.RenewalSnapshot{}, queue.ErrConflict
	}
	if json.Unmarshal(evidence, &s.Proof.Evidence) != nil {
		return lifecycle.RenewalSnapshot{}, queue.ErrConflict
	}
	s.Proof.Amount = amount
	s.Proof.CurrencyID = currencyID
	s.Proof.PaymentDate = paymentDate
	s.Proof.NextInvoiceDate = nextDate
	copy(s.Proof.Hash[:], proofHash)
	s.Proof.JSON, err = json.Marshal(s.Proof.Evidence)
	if err != nil || s.Proof.Validate() != nil {
		return lifecycle.RenewalSnapshot{}, queue.ErrConflict
	}
	s.Payment.ClientID = s.ClientID
	s.Payment.TelegramID = s.TelegramID
	s.Payment.ChatID = s.ChatID
	s.Payment.BotID = botID
	s.Payment.CustomerID = s.CustomerID
	s.Payment.InvoiceID = s.CurrentInvoiceID
	s.Payment.FirstInvoiceID = s.FirstInvoiceID
	s.Payment.RecurringID = s.RecurringID
	s.Payment.State = s.State
	s.Payment.DesiredAccess = s.DesiredAccess
	s.Payment.PaymentDate = paymentDate
	s.Payment.NextInvoiceDate = nextDate
	s.PaymentReview = paymentCheckReview || paymentConfirmationReview
	s.SUIBinding.ID = suiID
	copy(s.SUIBinding.CredentialsHash[:], credentialHash)
	if importedOriginID != 0 {
		s.ImportedInitialPaused = originMode == "imported_initial_paused"
		stable, okStable := importorigin.ParseHash(importedStable)
		policy, okPolicy := importorigin.ParseHash(importedPolicy)
		scope, okScope := importorigin.ParseHash(importedScope)
		credential, okCredential := importorigin.ParseHash(importedCredential)
		if !okStable || !okPolicy || !okScope || !okCredential || importedOriginID != s.ClientID || importedBotID != botID || importedAdmissionRevision <= 0 || importedAdmissionPermissionRevision <= 0 || importedRuntimeRevision <= 0 || importedEffectRevision <= 0 || importedSUIID <= 0 || importedSUIName == "" || importedBaselineCurrentInvoiceID == "" || importedPaidAnchorInvoiceID == "" || len(importedTemplate) == 0 || len(importedEvidence) == 0 || s.ImportedInitialPaused && (s.InvoiceID != importedBaselineCurrentInvoiceID || s.InvoiceID == importedPaidAnchorInvoiceID) || !s.ImportedInitialPaused && (s.InvoiceID == importedBaselineCurrentInvoiceID || s.InvoiceID == importedPaidAnchorInvoiceID) {
			return lifecycle.RenewalSnapshot{}, queue.ErrConflict
		}
		var template invoiceninja.RecurringRecord
		if json.Unmarshal(importedTemplate, &template) != nil || len(template.LineItems) != 1 || template.ID != s.RecurringID || template.ClientID != s.CustomerID || importedPrivateNotes != template.PrivateNotes {
			return lifecycle.RenewalSnapshot{}, queue.ErrConflict
		}
		profile, profileErr := invoiceninja.NewImportedRecurringProfile(template, template.LineItems[0].ProductKey)
		if profileErr != nil {
			return lifecycle.RenewalSnapshot{}, queue.ErrConflict
		}
		var importedFacts struct {
			InboundIDs []int64 `json:"inbound_ids"`
		}
		if json.Unmarshal(importedEvidence, &importedFacts) != nil || len(importedFacts.InboundIDs) == 0 {
			return lifecycle.RenewalSnapshot{}, queue.ErrConflict
		}
		seen := make(map[int64]bool, len(importedFacts.InboundIDs))
		for _, id := range importedFacts.InboundIDs {
			if id <= 0 || seen[id] {
				return lifecycle.RenewalSnapshot{}, queue.ErrConflict
			}
			seen[id] = true
		}
		s.ImportedOrigin = importorigin.Ref{ClientID: importedOriginID, BotID: importedBotID, AdmissionRevision: importedAdmissionRevision, RuntimeRevision: importedRuntimeRevision, ConfigurationRevision: currentConfigurationRevision, StableHash: stable, PolicyHash: policy, SourceScopeHash: scope}
		s.Payment.ImportedOrigin = s.ImportedOrigin
		if !s.ImportedOrigin.Valid() {
			return lifecycle.RenewalSnapshot{}, queue.ErrConflict
		}
		s.ImportedAdmissionPermissionRevision = importedAdmissionPermissionRevision
		s.ImportedEffectsRevision = importedEffectRevision
		s.ImportedEffectsAllowed = importedAllowed
		s.ImportedRecurringProfile = profile
		s.ImportedPrivateNotes = importedPrivateNotes
		s.ImportedInboundIDs = append([]int64(nil), importedFacts.InboundIDs...)
		s.ImportedPaidAnchorInvoiceID = importedPaidAnchorInvoiceID
		if s.ImportedInitialPaused {
			var frozen struct {
				CurrentInvoice json.RawMessage `json:"current_invoice"`
			}
			if json.Unmarshal(importedEvidence, &frozen) != nil || len(frozen.CurrentInvoice) == 0 || json.Unmarshal(frozen.CurrentInvoice, &s.ImportedInitialDebt) != nil {
				return lifecycle.RenewalSnapshot{}, queue.ErrConflict
			}
		}
		s.SUIBinding = sui.Binding{ID: importedSUIID, Name: importedSUIName, CredentialsHash: credential}
		if suiID != 0 || hasOwnedRecurringJournal || s.FirstInvoiceID != "" || s.RegistrationToken != "" || s.ActivationToken != "" {
			return lifecycle.RenewalSnapshot{}, queue.ErrConflict
		}
	} else {
		if len(credentialHash) != 32 || json.Unmarshal(recurringRequest, &s.RecurringDraft) != nil || invoiceninja.ValidateRecurringDraft(s.RecurringDraft) != nil {
			return lifecycle.RenewalSnapshot{}, queue.ErrConflict
		}
		s.SUIBinding.ID = suiID
		copy(s.SUIBinding.CredentialsHash[:], credentialHash)
	}
	if planFrozen {
		var facts struct {
			Amount           json.Number `json:"amount"`
			NextSendDatetime string      `json:"next_send_datetime"`
		}
		if planDate == "" || json.Unmarshal(baseline, &facts) != nil || facts.Amount == "" || (facts.NextSendDatetime == "" && !(s.ImportedInitialPaused && planSchedule)) {
			return lifecycle.RenewalSnapshot{}, queue.ErrConflict
		}
		when, parseErr := invoiceninja.ParseRecurringUTC(facts.NextSendDatetime)
		if facts.NextSendDatetime == "" && s.ImportedInitialPaused && planSchedule {
			parseErr = nil
		} else if parseErr != nil || !planSchedule && !when.After(time.Now()) {
			return lifecycle.RenewalSnapshot{}, queue.ErrConflict
		}
		s.Plan = &lifecycle.RenewalPlan{TargetDate: planDate, Pause: planPause, Schedule: planSchedule, Enable: planEnable, Resume: planResume, BaselineReceipt: invoiceninja.RecurringReceipt{Amount: facts.Amount, NextSendDatetime: facts.NextSendDatetime}}
		s.PlanBaseline = append([]byte(nil), baseline...)
	}
	s.Steps = make(map[lifecycle.RenewalStep]lifecycle.RenewalStepRecord, 4)
	rows, err := reader.Query(ctx, `SELECT step, request::text, COALESCE(external_id, ''), COALESCE(receipt::text, ''), dispatch_attempt, confirmed_at IS NOT NULL FROM invoice_renewal_steps WHERE operation_id = $1 ORDER BY step`, claim.ID)
	if err != nil {
		return lifecycle.RenewalSnapshot{}, err
	}
	for rows.Next() {
		var name string
		var request, receipt []byte
		var step lifecycle.RenewalStepRecord
		if err = rows.Scan(&name, &request, &step.ExternalID, &receipt, &step.DispatchAttempt, &step.Confirmed); err != nil {
			rows.Close()
			return lifecycle.RenewalSnapshot{}, err
		}
		step.Request = append([]byte(nil), request...)
		step.Receipt = append([]byte(nil), receipt...)
		key := lifecycle.RenewalStep(name)
		if !key.Valid() {
			rows.Close()
			return lifecycle.RenewalSnapshot{}, queue.ErrConflict
		}
		s.Steps[key] = step
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return lifecycle.RenewalSnapshot{}, err
	}
	rows.Close()
	s.OverdueSteps = make(map[string]bool)
	rows, err = reader.Query(ctx, `SELECT st.step, st.external_id FROM invoice_overdue_actions a JOIN invoice_overdue_steps st ON st.operation_id = a.operation_id WHERE a.client_id=$1 AND a.invoice_id=$2 AND a.bot_id=$4 AND ((($3=0) AND a.imported_origin_client_id IS NULL) OR (($3<>0) AND a.imported_origin_client_id=$3)) AND st.confirmed_at IS NOT NULL`, s.ClientID, s.InvoiceID, s.ImportedOrigin.ClientID, botID)
	if err != nil {
		return lifecycle.RenewalSnapshot{}, err
	}
	for rows.Next() {
		var name, external string
		if err = rows.Scan(&name, &external); err != nil {
			rows.Close()
			return lifecycle.RenewalSnapshot{}, err
		}
		if name == "pause_recurring" && external == s.RecurringID || name == "disable_sui" && external == s.SUIID {
			s.OverdueSteps[name] = true
		} else {
			s.OverdueOtherSteps = true
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return lifecycle.RenewalSnapshot{}, err
	}
	rows.Close()
	if err = reader.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM invoice_overdue_actions a JOIN invoice_overdue_steps st ON st.operation_id=a.operation_id WHERE a.client_id=$1 AND a.invoice_id=$2 AND a.bot_id=$4 AND ((($3=0) AND a.imported_origin_client_id IS NULL) OR (($3<>0) AND a.imported_origin_client_id=$3)) AND st.confirmed_at IS NULL)`, s.ClientID, s.InvoiceID, s.ImportedOrigin.ClientID, botID).Scan(&s.OverduePending); err != nil {
		return lifecycle.RenewalSnapshot{}, err
	}
	if err = reader.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM invoice_overdue_actions a WHERE a.client_id=$1 AND a.invoice_id=$2 AND a.bot_id=$4 AND ((($3=0) AND a.imported_origin_client_id IS NULL) OR (($3<>0) AND a.imported_origin_client_id=$3)) AND a.outcome='review' AND a.review_code=$5)`, s.ClientID, s.InvoiceID, s.ImportedOrigin.ClientID, botID, lifecycle.ReviewOverduePayment).Scan(&s.OverdueReviewPayment); err != nil {
		return lifecycle.RenewalSnapshot{}, err
	}
	return s, nil
}
