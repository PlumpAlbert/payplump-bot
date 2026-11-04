package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/PlumpAlbert/payplump-bot/internal/ninjaauthority"
	"github.com/PlumpAlbert/payplump-bot/templates"
	"github.com/jackc/pgx/v5"
)

var authorityInvoiceAmount = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

func validateAuthorityInvoices(o ninjaauthority.Observation, invoices []ninjaauthority.Invoice) error {
	if len(invoices) > 10000 || !authorityKey(o.CustomerID) || len(o.ScopeHash) > 256 {
		return ninjaauthority.ErrConflict
	}
	seen := make(map[string]bool, len(invoices))
	selected := o.InvoiceID == ""
	for _, i := range invoices {
		// Empty numbers/dates are valid draft fields. No status, amount, payment,
		// recurring, or entitlement policy belongs at this announcement boundary.
		if !authorityKey(i.ID) || strings.TrimSpace(i.ID) == "" || i.CustomerID != o.CustomerID || seen[i.ID] ||
			!utf8.ValidString(i.Number) || len(i.Number) > 256 || authorityControl.MatchString(i.Number) ||
			!authorityKey(i.Status) || len(i.Status) > 64 || strings.TrimSpace(i.Status) == "" ||
			len(i.Amount) > 64 || !authorityInvoiceAmount.MatchString(i.Amount) || i.ArchivedAt < 0 {
			return ninjaauthority.ErrConflict
		}
		if i.Date != "" {
			d, err := time.Parse("2006-01-02", i.Date)
			if err != nil || d.Format("2006-01-02") != i.Date {
				return ninjaauthority.ErrConflict
			}
		}
		seen[i.ID] = true
		if i.ID == o.InvoiceID {
			if i.Status != o.InvoiceStatus || i.Number != o.InvoiceNumber || i.Amount != o.InvoiceAmount || i.Date != o.InvoiceDate {
				return ninjaauthority.ErrConflict
			}
			selected = true
		}
	}
	if !selected {
		return ninjaauthority.ErrConflict
	}
	return nil
}

// Called under the existing bot-before-client locks, in the observation's
// transaction. A binding-only save never enters this path.
func saveAuthorityInvoices(ctx context.Context, tx pgx.Tx, bot, client int64, previousRaw []byte, o ninjaauthority.Observation, invoices []ninjaauthority.Invoice, admins []int64) error {
	var customer, scope string
	err := tx.QueryRow(ctx, `SELECT customer_id,scope_hash FROM authority_invoice_baselines WHERE bot_id=$1 AND client_id=$2`, bot, client).Scan(&customer, &scope)
	baseline := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !baseline {
		return err
	}
	if !baseline && (customer != o.CustomerID || scope != o.ScopeHash) {
		return ninjaauthority.ErrConflict
	}
	var previous ninjaauthority.Observation
	if baseline {
		if len(previousRaw) > 0 && json.Unmarshal(previousRaw, &previous) != nil {
			return ninjaauthority.ErrConflict
		}
		if _, err = tx.Exec(ctx, `INSERT INTO authority_invoice_baselines(bot_id,client_id,customer_id,scope_hash) VALUES($1,$2,$3,$4)`, bot, client, o.CustomerID, o.ScopeHash); err != nil {
			return err
		}
	}
	for _, i := range invoices {
		raw, err := json.Marshal(i)
		if err != nil {
			return err
		}
		fingerprint := sha256.Sum256(raw)
		var oldHash []byte
		var owner int64
		var oldCustomer, oldScope string
		var generation int64
		err = tx.QueryRow(ctx, `SELECT client_id,customer_id,scope_hash,fingerprint,generation FROM authority_invoice_ledger WHERE bot_id=$1 AND invoice_id=$2`, bot, i.ID).Scan(&owner, &oldCustomer, &oldScope, &oldHash, &generation)
		fresh := errors.Is(err, pgx.ErrNoRows)
		if err != nil && !fresh {
			return err
		}
		if !fresh && (owner != client || oldCustomer != o.CustomerID || oldScope != o.ScopeHash) {
			return ninjaauthority.ErrConflict
		}
		if !fresh && bytes.Equal(oldHash, fingerprint[:]) {
			continue
		}
		generation++
		_, err = tx.Exec(ctx, `INSERT INTO authority_invoice_ledger(bot_id,invoice_id,client_id,customer_id,scope_hash,snapshot,fingerprint,generation) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(bot_id,invoice_id) DO UPDATE SET snapshot=excluded.snapshot,fingerprint=excluded.fingerprint,generation=excluded.generation`, bot, i.ID, client, o.CustomerID, o.ScopeHash, raw, fingerprint[:], generation)
		if err != nil {
			return err
		}
		if baseline {
			// Compatibility only for an already-observed client's selected invoice.
			// Every other record in this first complete inventory is historical.
			if len(previousRaw) == 0 || i.ID != o.InvoiceID || (previous.InvoiceID == o.InvoiceID && previous.InvoiceStatus == o.InvoiceStatus) {
				continue
			}
		}
		status := i.Status
		if i.ArchivedAt != 0 {
			status += " (архивный)"
		}
		if i.IsDeleted {
			status += " (удалённый)"
		}
		var clientText, adminText string
		if !baseline && fresh {
			clientText = templates.Render("authority/invoice_created", templates.Data{InvoiceNumber: i.Number, Amount: i.Amount, Status: status})
			adminText = templates.Render("authority/invoice_created_admin", templates.Data{ActorID: strconv.FormatInt(o.Actor, 10), InvoiceNumber: i.Number, Amount: i.Amount, Status: status})
		} else {
			clientText = templates.Render("authority/invoice_client", templates.Data{InvoiceNumber: i.Number, Amount: i.Amount, Status: status})
			adminText = templates.Render("authority/invoice_updated_admin", templates.Data{ActorID: strconv.FormatInt(o.Actor, 10), InvoiceNumber: i.Number, Amount: i.Amount, Status: status})
		}
		digest := sha256.Sum256([]byte(i.ID))
		err = enqueueAuthorityClientAndAdmins(ctx, tx, bot, o.Actor, fmt.Sprintf("authority-invoice:%d:%x:%d", client, digest, generation),
			clientText, adminText, admins)
		if err != nil {
			return err
		}
	}
	return nil
}

// Read the legacy wake counters without registering a bot or a wake. Webhook
// bodies are deliberately not consulted; inventories come from canonical GETs.
func (s *Store) LoadAuthorityWake(ctx context.Context, bot int64) (ninjaauthority.WakeGeneration, error) {
	var w ninjaauthority.WakeGeneration
	if bot <= 0 {
		return w, ninjaauthority.ErrConflict
	}
	err := s.pool.QueryRow(ctx, `SELECT coalesce((SELECT generation FROM invoice_lifecycle_wake_state WHERE bot_id=$1),0),coalesce((SELECT generation FROM payment_wake_state WHERE bot_id=$1),0)`, bot).Scan(&w.Invoices, &w.Payments)
	return w, err
}
