//go:build integration

package postgres

import (
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/billing"
	"github.com/PlumpAlbert/payplump-bot/internal/importadmission"
)

// The status and schedule are set on the real loopback source BEFORE Prepare;
// admission must freeze them unchanged, not acquire pause authority from SQL.
func TestImportedBillingPausedNewPaidCycleRenewalWorkflow(t *testing.T) {
	for _, kind := range []string{"missing", "stale", "same target missing UTC", "same target stale UTC", "same target future UTC"} {
		t.Run(kind, func(t *testing.T) {
			var originalDate, originalUTC string
			runImportedPaidRenewalWithSource(t, false, false, func(src *importedOverdueSource) {
				src.recurring["status_id"] = "3"
				switch kind {
				case "stale":
					originalDate = time.Now().In(time.Local).AddDate(0, 0, -40).Format(time.DateOnly)
					originalUTC = originalDate + "T00:00:00.000000Z"
				case "same target missing UTC", "same target stale UTC", "same target future UTC":
					var err error
					originalDate, err = billing.NextBillingDate(time.Now().In(time.Local).Format(time.DateOnly))
					if err != nil {
						t.Fatal(err)
					}
					if kind == "same target stale UTC" {
						originalUTC = time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339Nano)
					} else if kind == "same target future UTC" {
						originalUTC = originalDate + "T00:00:00.000000Z"
					}
				}
				src.recurring["next_send_date"], src.recurring["next_send_datetime"] = originalDate, originalUTC
			}, func(rig *importedOverdueRig, prepared importadmission.PreparedPlan, botID int64) {
				paymentSQL(t, rig.s, rig.ctx, `SELECT io.lifecycle='active' AND io.current_invoice_id=io.paid_anchor_invoice_id
                    AND io.template->>'status_id'='3' AND io.template->>'next_send_date'=$2 AND io.template->>'next_send_datetime'=$3
                    AND a.origin_mode='ledger' AND a.invoice_id<>io.current_invoice_id AND a.invoice_id<>io.paid_anchor_invoice_id
                    AND a.baseline->>'status_id'='3' AND a.baseline->>'next_send_date'=$2 AND a.baseline->>'next_send_datetime'=$3
                    AND NOT a.requires_pause AND a.requires_schedule=$4 AND NOT a.requires_enable AND a.requires_resume
                    AND a.recurring_next_send_datetime>clock_timestamp()
                    FROM verified_import_origins io JOIN invoice_renewal_actions a ON a.imported_origin_client_id=io.client_id
                    WHERE io.bot_id=$1`, botID, originalDate, originalUTC, kind != "same target future UTC")
				paymentSQL(t, rig.s, rig.ctx, `SELECT pc.invoice_id=a.invoice_id AND pc.invoice_id<>io.current_invoice_id
                    AND encode(pc.proof_hash,'hex')<>io.evidence->>'proof_hash'
                    FROM invoice_renewal_actions a JOIN verified_import_origins io ON io.client_id=a.imported_origin_client_id
                    JOIN payment_confirmations pc ON pc.invoice_id=a.invoice_id WHERE a.bot_id=$1`, botID)
				// Exercises readImportedRenewalAudit, including the real schedule and
				// resume receipts, after the ordinary new-payment management policy.
				assertImportedRenewalProtectedConfiguration(t, importedConfigurationScenario{
					store: rig.s, ctx: rig.ctx, prepared: prepared, botID: botID, ninjaBase: rig.ninjaBase,
					suiBase: strings.TrimSuffix(rig.ninjaBase, "/ninja") + "/panel", invoiceID: "invoice-imported-new", originInvoiceID: admissionFixtureInv,
					ninjaToken: importedOverdueNinjaToken, suiToken: importedOverdueSUIToken,
					sourceWrites: func() int { return len(rig.source.postSnapshot()) }, writesBefore: len(rig.source.postSnapshot()),
				})
			})
		})
	}
}
