package invoiceninja

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
)

const (
	maxImportReaderPages = 100
	maxImportCustomers   = 10000
	maxImportRecurring   = 2000
)

// ImportReader is a GET-only, bounded inventory capability used
// to reconcile legacy records. It intentionally performs no adoption policy.
type ImportReader struct {
	transport *integrationhttp.Client
	pageSize  int
	maxPages  int
}

// NewImportReader creates only the shared GET transport. It never constructs a
// BillingClient or an integrationhttp.Writer.
func NewImportReader(base, token string, timeout time.Duration, pageSize, maxPages int) (*ImportReader, error) {
	if pageSize < 1 || pageSize > 100 || maxPages < 1 || maxPages > maxImportReaderPages {
		return nil, errors.New("invalid Invoice Ninja import reader pagination bounds")
	}
	client, err := New(base, token, timeout)
	if err != nil {
		return nil, errors.New("invalid Invoice Ninja import reader configuration")
	}
	return &ImportReader{transport: client.transport, pageSize: pageSize, maxPages: maxPages}, nil
}

// FindCustomers reads every page selected by an exact canonical id_number
// filter. It returns every matching record, including archived/deleted records,
// and rejects ignored filters, malformed records and duplicate IDs.
func (r *ImportReader) FindCustomers(ctx context.Context, telegramID int64) ([]Customer, error) {
	if telegramID <= 0 {
		return nil, errors.New("invalid Invoice Ninja import customer identity")
	}
	id := strconv.FormatInt(telegramID, 10)
	query := url.Values{
		"id_number": {id}, "status": {billingStatuses}, "per_page": {strconv.Itoa(r.pageSize)}, "sort": {"id|asc"},
	}
	return importInventoryPages(ctx, r, "clients", query, maxImportCustomers, func(raw []byte) (Customer, string, error) {
		var customer Customer
		if !uniqueNinjaJSON(raw) || !billingCustomerFields(raw) || json.Unmarshal(raw, &customer) != nil {
			return Customer{}, "", ErrLookupIncomplete
		}
		parsed, err := customer.TelegramID("id_number")
		if !billingID(customer.ID) || err != nil || parsed != telegramID || customer.IDNumber != id || len(customer.IDNumber) > 19 {
			return Customer{}, "", ErrLookupIncomplete
		}
		return customer, customer.ID, nil
	})
}

// ListCustomers returns the complete bounded customer inventory before any
// actor filtering, so duplicate/archived bindings cannot be hidden by discovery.
type CustomerInventoryReader struct{ *ImportReader }

func NewCustomerInventoryReader(base, token string, timeout time.Duration, pageSize, maxPages int) (*CustomerInventoryReader, error) {
	r, err := NewImportReader(base, token, timeout, pageSize, maxPages)
	if err != nil {
		return nil, err
	}
	return &CustomerInventoryReader{ImportReader: r}, nil
}

// ProductKey reads only the exact configured active product, using the same
// unique JSON boundary as import inventories. Errors never include remote values.
func (r *CustomerInventoryReader) ProductKey(ctx context.Context, id string) (string, error) {
	if !billingID(id) {
		return "", fmt.Errorf("%w: invalid configured product ID", ErrLookupIncomplete)
	}
	var raw json.RawMessage
	if err := getUniqueNinjaJSON(ctx, r.transport, []string{"api", "v1", "products", id}, nil, &raw); err != nil {
		if errors.Is(err, errInvalidResponse) || errors.Is(err, integrationhttp.ErrInvalidResponse) {
			return "", fmt.Errorf("%w: invalid product JSON response", ErrLookupIncomplete)
		}
		return "", err
	}
	var shape map[string]json.RawMessage
	if json.Unmarshal(raw, &shape) != nil || shape == nil {
		return "", fmt.Errorf("%w: invalid product envelope", ErrLookupIncomplete)
	}
	if len(shape["data"]) == 0 || string(shape["data"]) == "null" {
		return "", fmt.Errorf("%w: missing product data envelope", ErrLookupIncomplete)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(shape["data"], &fields) != nil || fields == nil {
		return "", fmt.Errorf("%w: invalid product data envelope", ErrLookupIncomplete)
	}
	for _, key := range []string{"id", "product_key", "is_deleted", "archived_at"} {
		if len(fields[key]) == 0 || string(fields[key]) == "null" {
			return "", fmt.Errorf("%w: missing required product field %s", ErrLookupIncomplete, key)
		}
	}
	var productID, key string
	var deleted bool
	var archived int64
	if json.Unmarshal(fields["id"], &productID) != nil {
		return "", fmt.Errorf("%w: invalid product id", ErrLookupIncomplete)
	}
	if productID != id {
		return "", fmt.Errorf("%w: product ID does not match configured ID", ErrLookupIncomplete)
	}
	if json.Unmarshal(fields["is_deleted"], &deleted) != nil {
		return "", fmt.Errorf("%w: invalid product is_deleted", ErrLookupIncomplete)
	}
	if deleted {
		return "", fmt.Errorf("%w: product is deleted", ErrLookupIncomplete)
	}
	if json.Unmarshal(fields["archived_at"], &archived) != nil {
		return "", fmt.Errorf("%w: invalid product archived_at", ErrLookupIncomplete)
	}
	if archived != 0 {
		return "", fmt.Errorf("%w: product is archived", ErrLookupIncomplete)
	}
	if json.Unmarshal(fields["product_key"], &key) != nil {
		return "", fmt.Errorf("%w: invalid product_key", ErrLookupIncomplete)
	}
	if key == "" {
		return "", fmt.Errorf("%w: empty product_key", ErrLookupIncomplete)
	}
	if !billingText(key, 256) || strings.TrimSpace(key) != key || strings.ContainsAny(key, "\r\n\t") {
		return "", fmt.Errorf("%w: invalid product_key", ErrLookupIncomplete)
	}
	return key, nil
}

func (r *CustomerInventoryReader) ListCustomers(ctx context.Context) ([]Customer, error) {
	query := url.Values{"status": {billingStatuses}, "per_page": {strconv.Itoa(r.pageSize)}, "sort": {"id|asc"}}
	return importInventoryPages(ctx, r.ImportReader, "clients", query, 1000, func(raw []byte) (Customer, string, error) {
		var c Customer
		if !uniqueNinjaJSON(raw) || !billingCustomerFields(raw) || json.Unmarshal(raw, &c) != nil || !billingID(c.ID) {
			return Customer{}, "", ErrLookupIncomplete
		}
		return c, c.ID, nil
	})
}

// ListRecurring returns the complete bounded recurring inventory for a
// customer. Legacy records are preserved as observed; no marker, status or
// first-cycle policy is imposed here.
func (r *ImportReader) ListRecurring(ctx context.Context, customerID string) ([]RecurringRecord, error) {
	if !billingID(customerID) {
		return nil, errors.New("invalid Invoice Ninja import customer identifier")
	}
	query := url.Values{
		"client_id": {customerID}, "status": {billingStatuses}, "client_status": {"all"},
		"sort": {"id|asc"}, "per_page": {strconv.Itoa(r.pageSize)},
	}
	return importInventoryPages(ctx, r, "recurring_invoices", query, maxImportRecurring, func(raw []byte) (RecurringRecord, string, error) {
		var record RecurringRecord
		if !uniqueNinjaJSON(raw) || json.Unmarshal(raw, &record) != nil || record.ClientID != customerID {
			return RecurringRecord{}, "", ErrLookupIncomplete
		}
		return record, record.ID, nil
	})
}

// importInventoryPages owns strict pagination and exact inventory accounting.
// Pages are always constructed locally; response-provided links are ignored.
func importInventoryPages[T any](ctx context.Context, reader *ImportReader, resource string, query url.Values, maxTotal int, decode func([]byte) (T, string, error)) ([]T, error) {
	out := make([]T, 0)
	seen := make(map[string]struct{})
	var first Pagination
	for page := 1; page <= reader.maxPages; page++ {
		q := query.Clone()
		q.Set("page", strconv.Itoa(page))
		var raw json.RawMessage
		err := getUniqueNinjaJSON(ctx, reader.transport, []string{"api", "v1", resource}, q, &raw)
		if err != nil {
			if billingLookupTemporaryFailure(err) {
				return nil, err
			}
			return nil, billingLookupFailure(errors.Join(ErrLookupIncomplete, err))
		}
		var envelope struct {
			Data  []json.RawMessage `json:"data"`
			Links json.RawMessage   `json:"links"`
			Meta  *struct {
				Pagination *wirePagination `json:"pagination"`
			} `json:"meta"`
		}
		if json.Unmarshal(raw, &envelope) != nil || envelope.Data == nil || envelope.Meta == nil || envelope.Meta.Pagination == nil {
			return nil, ErrLookupIncomplete
		}
		p, ok := validateBoundedInventoryPage(*envelope.Meta.Pagination, page, len(envelope.Data), reader.pageSize, reader.maxPages, maxTotal, &first)
		if !ok {
			return nil, ErrLookupIncomplete
		}
		for _, item := range envelope.Data {
			record, id, err := decode(item)
			if err != nil || id == "" {
				return nil, ErrLookupIncomplete
			}
			if _, duplicate := seen[id]; duplicate {
				return nil, ErrLookupIncomplete
			}
			seen[id] = struct{}{}
			out = append(out, record)
		}
		if page == p.TotalPages || (p.Total == 0 && page == 1) {
			if len(out) != p.Total {
				return nil, ErrLookupIncomplete
			}
			return out, nil
		}
	}
	return nil, ErrLookupIncomplete
}
