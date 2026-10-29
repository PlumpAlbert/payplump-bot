package invoiceninja

import (
	"context"
	"net/url"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
)

// RecurringReader exposes only exact-ID reads of the complete strict recurring
// DTO. Construction does not construct a writer or an inventory capability.
type RecurringReader struct {
	reader *integrationhttp.Client
}

// NewRecurringReader preserves the installed service prefix and normal TLS
// verification. Responses are validated by their data, not release claims.
func NewRecurringReader(base, token string, timeout time.Duration) (*RecurringReader, error) {
	client, err := New(base, token, timeout)
	if err != nil {
		return nil, err
	}
	return &RecurringReader{reader: client.transport}, nil
}

func (c *RecurringReader) get(ctx context.Context, path []string, query url.Values, result any) error {
	return getRecurring(ctx, c.reader, path, query, result)
}

// Get includes archived/deleted records without applying first-cycle draft
// policy. An exact provider ID is not proof of local authorization or ownership.
func (c *RecurringReader) Get(ctx context.Context, id string) (RecurringRecord, error) {
	return getRecurringRecord(ctx, c.get, id)
}
