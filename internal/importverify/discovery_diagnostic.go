package importverify

import (
	"context"
	"errors"
	"fmt"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

// Diagnostic contains only static application codes and non-sensitive counts.
// Count describes Check (matches, live records, latest-date ties, or rows).
// It never retains the original error or any source identifiers or payloads.
// It is also the safe typed error returned for incomplete global inventories.
type Diagnostic struct {
	Stage      string
	Check      string
	Category   string
	HTTPStatus int
	Count      int
}

func (d Diagnostic) Error() string {
	return fmt.Sprintf("automatic import: stage=%s check=%s category=%s status=%d count=%d", d.Stage, d.Check, d.Category, d.HTTPStatus, d.Count)
}

// FailureDiagnostic classifies an error without retaining or formatting it.
// stage and check must be static application codes, never source data.
func FailureDiagnostic(stage, check string, err error) Diagnostic {
	d := Diagnostic{Stage: stage, Check: check, Category: "unavailable"}
	var status *integrationhttp.HTTPError
	switch {
	case errors.Is(err, context.Canceled):
		d.Category = "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		d.Category = "deadline"
	case errors.As(err, &status):
		d.Category = "http"
		if status.StatusCode >= 100 && status.StatusCode <= 599 {
			d.HTTPStatus = status.StatusCode
		}
	case errors.Is(err, integrationhttp.ErrInvalidResponse), errors.Is(err, invoiceninja.ErrSettlementContract), errors.Is(err, sui.ErrManagedResponse):
		d.Category = "invalid_response"
	case errors.Is(err, invoiceninja.ErrLookupIncomplete), errors.Is(err, invoiceninja.ErrCycleLookupIncomplete), errors.Is(err, invoiceninja.ErrSettlementIncomplete):
		d.Category = "incomplete_inventory"
	case errors.Is(err, integrationhttp.ErrConnection):
		d.Category = "connection"
	}
	return d
}
