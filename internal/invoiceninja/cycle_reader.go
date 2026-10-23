package invoiceninja

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
)

var ErrCycleLookupIncomplete = errors.New("Invoice Ninja cycle invoice inventory is incomplete")

type CycleInvitation struct {
	ID       string `json:"id"`
	SentDate string `json:"sent_date"`
}

func (invitation *CycleInvitation) UnmarshalJSON(raw []byte) error {
	type plain CycleInvitation
	var decoded plain
	if _, err := settlementFields(raw, &decoded, "id", "sent_date"); err != nil {
		return err
	}
	if !settlementID(decoded.ID) || len(decoded.SentDate) > 128 || strings.TrimSpace(decoded.SentDate) != decoded.SentDate {
		return errInvalidResponse
	}
	*invitation = CycleInvitation(decoded)
	return nil
}

type CycleInvoice struct {
	ID           string            `json:"id"`
	ClientID     string            `json:"client_id"`
	RecurringID  string            `json:"recurring_id"`
	Number       string            `json:"number"`
	PrivateNotes string            `json:"private_notes"`
	StatusID     string            `json:"status_id"`
	Date         string            `json:"date"`
	DueDate      string            `json:"due_date"`
	Amount       json.Number       `json:"amount"`
	Balance      json.Number       `json:"balance"`
	PaidToDate   json.Number       `json:"paid_to_date"`
	ExchangeRate json.Number       `json:"exchange_rate"`
	IsDeleted    bool              `json:"is_deleted"`
	ArchivedAt   int64             `json:"archived_at"`
	UpdatedAt    int64             `json:"updated_at"`
	Invitations  []CycleInvitation `json:"invitations"`
}

func (CycleInvoice) String() string   { return "[Invoice Ninja cycle invoice]" }
func (CycleInvoice) GoString() string { return "[Invoice Ninja cycle invoice]" }

func (invoice *CycleInvoice) UnmarshalJSON(raw []byte) error {
	type plain CycleInvoice
	var decoded plain
	_, err := settlementFields(raw, &decoded, "id", "client_id", "recurring_id", "number", "private_notes", "status_id", "date", "due_date", "amount", "balance", "paid_to_date", "exchange_rate", "is_deleted", "archived_at", "updated_at", "invitations")
	if err != nil {
		return err
	}
	if !settlementID(decoded.ID) || !settlementID(decoded.ClientID) || !settlementOptionalID(decoded.RecurringID) || !settlementID(decoded.StatusID) || !validCycleDate(decoded.Date) || !validCycleDate(decoded.DueDate) || decoded.ArchivedAt < 0 || decoded.UpdatedAt < 0 || decoded.Invitations == nil || !settlementNumbers(decoded.Amount, decoded.Balance, decoded.PaidToDate, decoded.ExchangeRate) {
		return errInvalidResponse
	}
	seenInvitations := make(map[string]struct{}, len(decoded.Invitations))
	for _, invitation := range decoded.Invitations {
		if _, duplicate := seenInvitations[invitation.ID]; duplicate {
			return errInvalidResponse
		}
		seenInvitations[invitation.ID] = struct{}{}
	}
	*invoice = CycleInvoice(decoded)
	return nil
}

func validCycleDate(value string) bool {
	// Draft/foreign histories may have no due date. Preserve that observation;
	// only the lifecycle authorizer may accept an issued, date-bound invoice.
	if value == "" {
		return true
	}
	parsed, err := time.Parse("2006-01-02", value)
	return err == nil && parsed.Format("2006-01-02") == value
}

// CycleReader is a read-only Invoice Ninja capability for invoice lifecycle discovery.
type CycleReader struct {
	transport *integrationhttp.Client
	pageSize  int
	maxPages  int
}

func NewCycleReader(base, token string, timeout time.Duration, pageSize, maxPages int) (*CycleReader, error) {
	if pageSize < 1 || pageSize > 100 || maxPages < 1 || maxPages > 100 || timeout <= 0 {
		return nil, errors.New("invalid Invoice Ninja cycle reader configuration")
	}
	client, err := New(base, token, timeout)
	if err != nil {
		return nil, err
	}
	return &CycleReader{transport: client.transport, pageSize: pageSize, maxPages: maxPages}, nil
}

func (c *CycleReader) get(ctx context.Context, path []string, query url.Values, result any) error {
	return getUniqueNinjaJSON(ctx, c.transport, path, query, SupportedSettlementVersion, ErrSettlementVersion, result)
}

func (c *CycleReader) GetInvoice(ctx context.Context, id string) (CycleInvoice, error) {
	if !settlementID(id) {
		return CycleInvoice{}, errors.New("invalid Invoice Ninja cycle invoice identifier")
	}
	var envelope struct {
		Data *CycleInvoice `json:"data"`
	}
	if err := c.get(ctx, []string{"api", "v1", "invoices", id}, nil, &envelope); err != nil {
		return CycleInvoice{}, err
	}
	if envelope.Data == nil || envelope.Data.ID != id {
		return CycleInvoice{}, errInvalidResponse
	}
	return *envelope.Data, nil
}

// ListInvoices reads the complete company inventory. Customer selection happens
// locally only after every record and every page has been strictly validated.
func (c *CycleReader) ListInvoices(ctx context.Context, customerID string) ([]CycleInvoice, error) {
	if !settlementID(customerID) {
		return nil, errors.New("invalid Invoice Ninja cycle customer identifier")
	}
	query := url.Values{"status": {"active,archived,deleted"}, "client_status": {"all"}, "sort": {"id|asc"}, "per_page": {strconv.Itoa(c.pageSize)}}
	var result []CycleInvoice
	seen := make(map[string]struct{})
	var first Pagination
	for page := 1; page <= c.maxPages; page++ {
		query.Set("page", strconv.Itoa(page))
		var envelope struct {
			Data []CycleInvoice `json:"data"`
			Meta *struct {
				Pagination *wirePagination `json:"pagination"`
			} `json:"meta"`
		}
		if err := c.get(ctx, []string{"api", "v1", "invoices"}, query, &envelope); err != nil {
			return nil, errors.Join(ErrCycleLookupIncomplete, err)
		}
		if envelope.Data == nil || envelope.Meta == nil || envelope.Meta.Pagination == nil {
			return nil, ErrCycleLookupIncomplete
		}
		p, ok := envelope.Meta.Pagination.validate(page, len(envelope.Data))
		if !ok || p.PerPage != c.pageSize || p.Total > 10000 || p.TotalPages > c.maxPages {
			return nil, ErrCycleLookupIncomplete
		}
		if page == 1 {
			first = p
		} else if p.Total != first.Total || p.PerPage != first.PerPage || p.TotalPages != first.TotalPages {
			return nil, ErrCycleLookupIncomplete
		}
		for _, invoice := range envelope.Data {
			if _, duplicate := seen[invoice.ID]; duplicate {
				return nil, ErrCycleLookupIncomplete
			}
			seen[invoice.ID] = struct{}{}
			if invoice.ClientID == customerID {
				result = append(result, invoice)
			}
		}
		if page == p.TotalPages || (p.Total == 0 && page == 1) {
			if len(seen) != p.Total {
				return nil, ErrCycleLookupIncomplete
			}
			return result, nil
		}
	}
	return nil, ErrCycleLookupIncomplete
}
