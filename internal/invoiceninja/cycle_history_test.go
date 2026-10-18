package invoiceninja_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
)

func TestCycleReaderPreservesUndatedDraftHistoryWithoutAuthorizingIt(t *testing.T) {
	raw := strings.ReplaceAll(cycleInvoiceJSON, `"status_id":"2"`, `"status_id":"1"`)
	raw = strings.ReplaceAll(raw, `"date":"2025-01-01"`, `"date":""`)
	raw = strings.ReplaceAll(raw, `"due_date":"2025-02-01"`, `"due_date":""`)
	var invoice invoiceninja.CycleInvoice
	if err := json.Unmarshal([]byte(raw), &invoice); err != nil || invoice.Date != "" || invoice.DueDate != "" || invoice.StatusID != "1" {
		t.Fatal("reader erased or rejected an incomplete draft observation")
	}
}
