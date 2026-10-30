package importverify

import (
	"context"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
)

func TestDiscoveryExplainsLifecycleMismatchWithoutChangingProviders(t *testing.T) {
	for _, tc := range []struct {
		status, identity, check string
	}{
		{"2", `[{"id":1,"name":"42","enable":false}]`, "active_recurring_sui_disabled"},
		{"1", `[{"id":1,"name":"42","enable":true}]`, "recurring_draft"},
		{"4", `[{"id":1,"name":"42","enable":false}]`, "recurring_completed"},
		{"unknown", `[{"id":1,"name":"42","enable":false}]`, "recurring_status_unsupported"},
	} {
		t.Run(tc.check, func(t *testing.T) {
			customer := invoiceninja.Customer{ID: "customer-A", IDNumber: "42"}
			record := invoiceninja.RecurringRecord{ID: "recurring-A", ClientID: customer.ID, StatusID: tc.status}
			svc := discoveryService(t, discoveryCustomers{recurrings: []invoiceninja.RecurringRecord{record}}, discoveryIdentityReader{}, discoveryOtherProviders{})
			identity := discoveryIdentities(t, tc.identity)[0]
			_, reason, d := svc.discoverRecord(context.Background(), customer, identity, time.Now())
			if reason != ReasonUnsupported || d.Stage != "recurring" || d.Check != tc.check {
				t.Fatalf("mismatch not explained: reason=%s diagnostic=%+v", reason, d)
			}
		})
	}
}
