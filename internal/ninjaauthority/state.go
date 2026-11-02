package ninjaauthority

import (
	"crypto/sha256"
	"encoding/json"
	"slices"
	"strconv"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
)

// ObserveCustomer does not derive a period from a payment or impose the bot's
// new-subscription tariff. Paused recurring means disabled; inactive customers
// remain represented even when their recurring was archived too.
func ObserveCustomer(c invoiceninja.Customer, records []invoiceninja.RecurringRecord) (Observation, string) {
	o := Observation{CustomerID: c.ID, Binding: Binding{CustomerID: c.ID}}
	actor, err := strconv.ParseInt(c.IDNumber, 10, 64)
	if err != nil || actor <= 0 || strconv.FormatInt(actor, 10) != c.IDNumber || c.ID == "" {
		return o, "invalid_identity"
	}
	o.Actor = actor
	records = slices.Clone(records)
	slices.SortFunc(records, func(a, b invoiceninja.RecurringRecord) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	raw, err := json.Marshal(struct {
		Customer  invoiceninja.Customer
		Recurring []invoiceninja.RecurringRecord
	}{c, records})
	if err != nil {
		return o, "invalid_source"
	}
	o.SourceHash = sha256.Sum256(raw)
	live := []invoiceninja.RecurringRecord{}
	seen := map[string]bool{}
	for _, r := range records {
		if r.ID == "" || r.ClientID != c.ID || seen[r.ID] {
			return o, "ambiguous_recurring"
		}
		seen[r.ID] = true
		if !r.IsDeleted && r.ArchivedAt == 0 {
			live = append(live, r)
		}
	}
	if !c.Active() {
		o.StateKnown = true
		if len(live) == 0 && len(records) == 1 {
			live = records
		}
	} else if len(live) != 1 {
		return o, "ambiguous_recurring"
	}
	if len(live) == 1 {
		r := live[0]
		o.RecurringID = r.ID
		// A missing date is observed as missing, never replaced with payment+month.
		if r.NextSendDate != "" {
			if _, err := time.Parse(time.DateOnly, r.NextSendDate); err == nil {
				o.NextInvoiceDate = r.NextSendDate
			}
		}
		if c.Active() {
			switch r.StatusID {
			case "2":
				o.StateKnown = true
				o.Enabled = true
			case "3":
				o.StateKnown = true
			default:
				return o, "unsupported_recurring_state"
			}
		}
	}
	return o, ""
}
