package postgres

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/importadmission"
	"github.com/jackc/pgx/v5"
)

type ImportAdmissionResult struct {
	Admitted        int
	AlreadyAdmitted int
}

func importDigest(value string) ([]byte, error) {
	b, err := hex.DecodeString(value)
	if err != nil || len(b) != 32 || hex.EncodeToString(b) != value || value == "0000000000000000000000000000000000000000000000000000000000000000" {
		return nil, errors.New("invalid import admission digest")
	}
	return b, nil
}

// SyncImportAdmissionPolicy uses monotonic revision CAS. Disabling is an
// immediate revocation; deleting a policy (which could reset its revision) is
// prohibited by migration 018.
func (s *Store) SyncImportAdmissionPolicy(ctx context.Context, botID int64, revision int64, policyHash, sourceScopeHash string, enabled bool) error {
	ph, err := importDigest(policyHash)
	if err != nil {
		return errors.New("invalid import admission policy")
	}
	sh, err := importDigest(sourceScopeHash)
	if err != nil {
		return errors.New("invalid import admission policy")
	}
	if botID <= 0 || revision <= 0 {
		return errors.New("invalid import admission policy")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return errors.New("could not begin import policy update")
	}
	defer rollback(tx)
	if _, err = tx.Exec(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		return errors.New("could not require durable import policy update")
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || ':import-admission-policy:' || $1::bigint::text,0))`, botID); err != nil {
		return errors.New("could not serialize import policy update")
	}
	var botExists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM telegram_bots WHERE bot_id=$1)`, botID).Scan(&botExists); err != nil || !botExists {
		return errors.New("import admission bot is unavailable")
	}
	var currentRevision int64
	var currentPolicy, currentScope []byte
	var currentEnabled bool
	err = tx.QueryRow(ctx, `SELECT revision,policy_hash,source_scope_hash,enabled FROM import_admission_policy WHERE bot_id=$1 FOR UPDATE`, botID).Scan(&currentRevision, &currentPolicy, &currentScope, &currentEnabled)
	if errors.Is(err, pgx.ErrNoRows) {
		_, err = tx.Exec(ctx, `INSERT INTO import_admission_policy(bot_id,revision,policy_hash,source_scope_hash,enabled) VALUES($1,$2,$3,$4,$5)`, botID, revision, ph, sh, enabled)
	} else if err != nil {
		return errors.New("could not read import admission policy")
	} else {
		if revision < currentRevision || (revision == currentRevision && (!equalImportBytes(ph, currentPolicy) || !equalImportBytes(sh, currentScope) || (!currentEnabled && enabled))) {
			return errors.New("import admission policy revision conflict")
		}
		if _, err = tx.Exec(ctx, `UPDATE import_admission_policy SET revision=$2,policy_hash=$3,source_scope_hash=$4,enabled=$5,updated_at=clock_timestamp() WHERE bot_id=$1`, botID, revision, ph, sh, enabled); err != nil {
			return errors.New("could not update import admission policy")
		}
	}
	if err != nil {
		return errors.New("could not save import admission policy")
	}
	if err = tx.Commit(ctx); err != nil {
		return errors.New("could not commit import admission policy")
	}
	return nil
}

func equalImportBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// ApplyVerifiedImport atomically stages a fully verified plan and writes
// imported identities/subscriptions plus immutable provenance. It makes no
// external calls and creates no operations or capability journals.
func (s *Store) ApplyVerifiedImport(ctx context.Context, prepared importadmission.PreparedPlan) (ImportAdmissionResult, error) {
	if ctx == nil || prepared.Validate() != nil {
		return ImportAdmissionResult{}, errors.New("invalid prepared import")
	}
	plan := prepared.Plan()
	planHash, err := importDigest(prepared.PlanHash())
	if err != nil || plan.ValidateHash(prepared.PlanHash()) != nil || len(plan.Candidates) != prepared.RowCount() {
		return ImportAdmissionResult{}, errors.New("invalid prepared import")
	}
	policyHash, err := importDigest(prepared.PolicyHash())
	if err != nil {
		return ImportAdmissionResult{}, errors.New("invalid prepared import")
	}
	scopeHash, err := importDigest(prepared.SourceScopeHash())
	if err != nil {
		return ImportAdmissionResult{}, errors.New("invalid prepared import")
	}
	rows := prepared.Rows()
	sort.Slice(rows, func(i, j int) bool {
		a, _ := strconv.ParseInt(rows[i].TelegramID, 10, 64)
		b, _ := strconv.ParseInt(rows[j].TelegramID, 10, 64)
		return a < b
	})
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return ImportAdmissionResult{}, errors.New("could not begin import admission")
	}
	defer rollback(tx)
	if _, err = tx.Exec(ctx, `SET LOCAL synchronous_commit=on`); err != nil {
		return ImportAdmissionResult{}, errors.New("could not require durable import admission")
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64FromDigest(planHash)); err != nil {
		return ImportAdmissionResult{}, errors.New("could not serialize import admission")
	}
	var enabled bool
	var currentRevision int64
	var currentPolicy, currentScope []byte
	if err = tx.QueryRow(ctx, `SELECT revision,policy_hash,source_scope_hash,enabled FROM import_admission_policy WHERE bot_id=$1 FOR UPDATE`, prepared.BotID()).Scan(&currentRevision, &currentPolicy, &currentScope, &enabled); err != nil || !enabled || currentRevision != prepared.Revision() || !equalImportBytes(currentPolicy, policyHash) || !equalImportBytes(currentScope, scopeHash) {
		return ImportAdmissionResult{}, errors.New("import admission policy is not currently enabled")
	}
	var dbNow time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&dbNow); err != nil {
		return ImportAdmissionResult{}, errors.New("could not validate import evidence freshness")
	}
	if prepared.VerifiedAt().After(dbNow) || !prepared.NotAfter().After(dbNow) || prepared.NotAfter().Sub(prepared.VerifiedAt()) > 15*time.Minute {
		return ImportAdmissionResult{}, errors.New("verified import evidence is stale")
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || ':import-identities:' || $1::bigint::text,0))`, prepared.BotID()); err != nil {
		return ImportAdmissionResult{}, errors.New("could not serialize import identities")
	}
	if _, err = stageImportTx(ctx, tx, plan, prepared.PlanHash()); err != nil {
		return ImportAdmissionResult{}, err
	}

	// Lock all extant clients and subscriptions in canonical Telegram-ID order
	// before touching the shared external-write guard.
	var result ImportAdmissionResult
	for _, r := range rows {
		tg, convErr := strconv.ParseInt(r.TelegramID, 10, 64)
		if convErr != nil || tg <= 0 || strconv.FormatInt(tg, 10) != r.TelegramID {
			return ImportAdmissionResult{}, errors.New("invalid prepared import identity")
		}
		var clientID int64
		var chatID int64
		var configRevision int64
		var ninjaID, suiID *string
		qerr := tx.QueryRow(ctx, `SELECT id,chat_id,invoice_ninja_id,sui_id,configuration_revision FROM clients WHERE telegram_id=$1 FOR UPDATE`, tg).Scan(&clientID, &chatID, &ninjaID, &suiID, &configRevision)
		newClient := errors.Is(qerr, pgx.ErrNoRows)
		if qerr != nil && !newClient {
			return ImportAdmissionResult{}, errors.New("could not lock import identity")
		}
		if newClient {
			if err = tx.QueryRow(ctx, `INSERT INTO clients(telegram_id,chat_id,display_name,invoice_ninja_id,sui_id) VALUES($1,$1,'',$2,$3) ON CONFLICT(telegram_id) DO NOTHING RETURNING id`, tg, r.CustomerID, strconv.FormatInt(r.SUIID, 10)).Scan(&clientID); errors.Is(err, pgx.ErrNoRows) {
				if err = tx.QueryRow(ctx, `SELECT id,chat_id,invoice_ninja_id,sui_id,configuration_revision FROM clients WHERE telegram_id=$1 FOR UPDATE`, tg).Scan(&clientID, &chatID, &ninjaID, &suiID, &configRevision); err != nil {
					return ImportAdmissionResult{}, errors.New("could not lock import identity")
				}
				newClient = false
			} else if err != nil {
				return ImportAdmissionResult{}, errors.New("import identity conflict")
			}
		}
		var subExists bool
		var state string
		var desired bool
		var invoice, recurring *string
		var paymentDate, nextDate *time.Time
		var firstInvoice *string
		subErr := tx.QueryRow(ctx, `SELECT state,desired_access,current_invoice_id,recurring_id,payment_date,next_invoice_date,first_invoice_id FROM subscriptions WHERE client_id=$1 FOR UPDATE`, clientID).Scan(&state, &desired, &invoice, &recurring, &paymentDate, &nextDate, &firstInvoice)
		if subErr == nil {
			subExists = true
		} else if !errors.Is(subErr, pgx.ErrNoRows) {
			return ImportAdmissionResult{}, errors.New("could not lock import subscription")
		}
		if !newClient {
			var originBot, originRevision, originConfigRevision int64
			var originPlan, originPolicy, originScope, originStable []byte
			originErr := tx.QueryRow(ctx, `SELECT bot_id,configuration_revision_at_admission,admission_revision,plan_hash,policy_hash,source_scope_hash,stable_hash FROM verified_import_origins WHERE client_id=$1 FOR UPDATE`, clientID).Scan(&originBot, &originConfigRevision, &originRevision, &originPlan, &originPolicy, &originScope, &originStable)
			if originErr != nil {
				return ImportAdmissionResult{}, errors.New("existing client is not an identical verified import")
			}
			if originBot != prepared.BotID() || originRevision != prepared.Revision() || !equalImportBytes(originPlan, planHash) || !equalImportBytes(originPolicy, policyHash) || !equalImportBytes(originScope, scopeHash) {
				return ImportAdmissionResult{}, errors.New("existing import origin conflicts with prepared plan")
			}
			stable, decErr := importDigest(r.StableHash)
			if decErr != nil || !equalImportBytes(originStable, stable) {
				return ImportAdmissionResult{}, errors.New("existing import facts conflict with prepared plan")
			}
			if !subExists || chatID != tg || configRevision != originConfigRevision || ninjaID == nil || *ninjaID != r.CustomerID || suiID == nil || *suiID != strconv.FormatInt(r.SUIID, 10) || firstInvoice != nil || invoice == nil || *invoice != r.CurrentInvoiceID || recurring == nil || *recurring != r.RecurringID {
				return ImportAdmissionResult{}, errors.New("existing imported client state conflicts")
			}
			if state != importedState(r.Lifecycle) || desired != (r.Lifecycle == "active") || paymentDate == nil || nextDate == nil || paymentDate.Format("2006-01-02") != r.PaymentDate || nextDate.Format("2006-01-02") != r.NextInvoiceDate {
				return ImportAdmissionResult{}, errors.New("existing imported subscription conflicts")
			}
			var effects bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE client_id=$1) OR EXISTS(SELECT 1 FROM payment_checks WHERE client_id=$1) OR EXISTS(SELECT 1 FROM activation_sui_bindings WHERE client_id=$1) OR EXISTS(SELECT 1 FROM payment_confirmations WHERE client_id=$1 OR invoice_id IN (SELECT current_invoice_id FROM subscriptions WHERE client_id=$1)) OR EXISTS(SELECT 1 FROM invoice_lifecycle_checks WHERE client_id=$1) OR EXISTS(SELECT 1 FROM invoice_lifecycle_invoices WHERE client_id=$1) OR EXISTS(SELECT 1 FROM outbox WHERE bot_id=$2 AND chat_id=$3) OR EXISTS(SELECT 1 FROM configuration_requests WHERE client_id=$1) OR EXISTS(SELECT 1 FROM configuration_chat_guards WHERE bot_id=$2 AND chat_id=$3)`, clientID, prepared.BotID(), tg).Scan(&effects); err != nil || effects {
				return ImportAdmissionResult{}, errors.New("existing client has managed workflow state")
			}
			result.AlreadyAdmitted++
			continue
		}
		if subExists {
			return ImportAdmissionResult{}, errors.New("unmanaged client subscription conflicts")
		}
		var invoiceConflict bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM payment_confirmations WHERE invoice_id=$1 OR invoice_id=$2) OR EXISTS(SELECT 1 FROM invoice_lifecycle_invoices WHERE invoice_id=$1 OR invoice_id=$2) OR EXISTS(SELECT 1 FROM configuration_chat_guards WHERE bot_id=$3 AND chat_id=$4)`, r.CurrentInvoiceID, r.PaidAnchorInvoiceID, prepared.BotID(), tg).Scan(&invoiceConflict); err != nil || invoiceConflict {
			return ImportAdmissionResult{}, errors.New("verified invoice identity conflicts")
		}
		// Preserve client/subscription lock order before acquiring the shared guard.
		if _, err = tx.Exec(ctx, `INSERT INTO subscriptions(client_id,state,desired_access,current_invoice_id,recurring_id,payment_date,next_invoice_date) VALUES($1,$2,$3,$4,$5,$6::date,$7::date)`, clientID, importedState(r.Lifecycle), r.Lifecycle == "active", r.CurrentInvoiceID, r.RecurringID, r.PaymentDate, r.NextInvoiceDate); err != nil {
			return ImportAdmissionResult{}, errors.New("import subscription conflict")
		}
		result.Admitted++
	}
	// This is the shared global barrier. Lock it only after identity and
	// subscription rows; never clear its owner or advance its epoch here.
	var operationFree, stepFree bool
	if err = tx.QueryRow(ctx, `SELECT operation_id IS NULL,step IS NULL FROM activation_write_guard WHERE id=1 FOR UPDATE`).Scan(&operationFree, &stepFree); err != nil || !operationFree || !stepFree {
		return ImportAdmissionResult{}, errors.New("external write guard is occupied")
	}
	for _, r := range rows {
		tg, _ := strconv.ParseInt(r.TelegramID, 10, 64)
		var clientID int64
		if err = tx.QueryRow(ctx, `SELECT id FROM clients WHERE telegram_id=$1`, tg).Scan(&clientID); err != nil {
			return ImportAdmissionResult{}, errors.New("could not resolve imported client")
		}
		// Existing idempotent origins need no mutation. A fresh source GET is
		// still required and validated above before this path is reached.
		var existing bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM verified_import_origins WHERE client_id=$1)`, clientID).Scan(&existing); err != nil {
			return ImportAdmissionResult{}, errors.New("could not check import origin")
		}
		if existing {
			continue
		}
		ph, _ := importDigest(prepared.PolicyHash())
		sh, _ := importDigest(prepared.SourceScopeHash())
		stable, _ := importDigest(r.StableHash)
		credential, _ := importDigest(r.CredentialHash)
		sourceFacts, _ := importDigest(r.SourceFactsHash)
		history := jsonBytes(r.HistoricalInvoiceIDs)
		var admissionConfigRevision int64
		if err = tx.QueryRow(ctx, `SELECT configuration_revision FROM clients WHERE id=$1`, clientID).Scan(&admissionConfigRevision); err != nil {
			return ImportAdmissionResult{}, errors.New("could not capture imported client revision")
		}
		if _, err = tx.Exec(ctx, `INSERT INTO verified_import_origins(client_id,bot_id,configuration_revision_at_admission,admission_revision,plan_hash,source_index,policy_hash,source_scope_hash,stable_hash,source_facts_hash,evidence,customer_id,recurring_id,current_invoice_id,paid_anchor_invoice_id,sui_id,sui_name,credentials_hash,lifecycle,historical_invoice_ids,invoice_watermark,template,proof,amount,currency,payment_date,next_invoice_date) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb,$12,$13,$14,$15,$16,$17,$18,$19,$20::jsonb,$21,$22::jsonb,$23::jsonb,$24::numeric,$25,$26::date,$27::date)`, clientID, prepared.BotID(), admissionConfigRevision, prepared.Revision(), planHash, r.SourceIndex, ph, sh, stable, sourceFacts, r.Evidence, r.CustomerID, r.RecurringID, r.CurrentInvoiceID, r.PaidAnchorInvoiceID, r.SUIID, r.SUIName, credential, r.Lifecycle, history, r.InvoiceWatermark, r.Template, r.Proof, r.Amount, r.Currency, r.PaymentDate, r.NextInvoiceDate); err != nil {
			return ImportAdmissionResult{}, errors.New("could not persist verified import origin")
		}
	}
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&dbNow); err != nil || prepared.VerifiedAt().After(dbNow) || !prepared.NotAfter().After(dbNow) {
		return ImportAdmissionResult{}, errors.New("verified import evidence expired before commit")
	}
	if err = tx.Commit(ctx); err != nil {
		return ImportAdmissionResult{}, errors.New("could not commit verified import admission")
	}
	return result, nil
}

func importedState(lifecycle string) string {
	if lifecycle == "active" {
		return "active"
	}
	return "overdue"
}
func int64FromDigest(b []byte) int64 {
	if len(b) < 8 {
		return 0
	}
	var n int64
	for i := 0; i < 8; i++ {
		n = (n << 8) | int64(b[i])
	}
	return n
}
func jsonBytes(v any) []byte { b, _ := json.Marshal(v); return b }
