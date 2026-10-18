package invoiceninja

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
)

// SupportedSettlementVersion pins the read contract to Invoice Ninja source
// commit 493c3884e4214028bc5970cb41bafbd9c4d96401 (v5.13.47). This is a
// source-verified contract, not evidence of live settlement or write testing.
const SupportedSettlementVersion = "5.13.47"

var (
	ErrSettlementContract   = errInvalidResponse
	ErrSettlementVersion    = errors.New("Invoice Ninja settlement requires app version 5.13.47")
	ErrSettlementIncomplete = errors.New("Invoice Ninja settlement inventory is incomplete")
)

// SettlementClient is a GET-only capability. It neither embeds a billing client
// nor constructs a writer. Accounting and currency-policy decisions belong to
// the caller; these reads preserve pending, failed, refunded and manual payments.
type SettlementClient struct {
	transport *integrationhttp.Client
	pageSize  int
	maxPages  int
}

// NewSettlementClient accepts the service base URL (including an installation
// prefix, without /api/v1). Inventories are bounded to 100 pages of 100 records.
// Transport timeouts and response sizes have the same bounds as New.
func NewSettlementClient(base, token string, timeout time.Duration, pageSize, maxPages int) (*SettlementClient, error) {
	if pageSize < 1 || pageSize > 100 || maxPages < 1 || maxPages > 100 || timeout <= 0 {
		return nil, errors.New("invalid Invoice Ninja settlement client configuration")
	}
	client, err := New(base, token, timeout)
	if err != nil {
		return nil, err
	}
	return &SettlementClient{transport: client.transport, pageSize: pageSize, maxPages: maxPages}, nil
}

type SettlementCustomer struct {
	ID         string `json:"id"`
	IDNumber   string `json:"id_number"`
	CurrencyID string `json:"-"` // Only an explicit settings.currency_id override.
	GroupID    string `json:"group_settings_id"`
	IsDeleted  bool   `json:"is_deleted"`
	ArchivedAt int64  `json:"archived_at"`
}

type SettlementInvoice struct {
	ID           string      `json:"id"`
	ClientID     string      `json:"client_id"`
	RecurringID  string      `json:"recurring_id"`
	Number       string      `json:"number"`
	StatusID     string      `json:"status_id"`
	Date         string      `json:"date"`
	DueDate      string      `json:"due_date"`
	Amount       json.Number `json:"amount"`
	Balance      json.Number `json:"balance"`
	PaidToDate   json.Number `json:"paid_to_date"`
	ExchangeRate json.Number `json:"exchange_rate"`
	IsDeleted    bool        `json:"is_deleted"`
	ArchivedAt   int64       `json:"archived_at"`
}

type SettlementAllocation struct {
	ID         string      `json:"id"`
	InvoiceID  string      `json:"invoice_id"`
	CreditID   string      `json:"credit_id"`
	PaymentID  string      `json:"payment_id"`
	Amount     json.Number `json:"amount"`
	Refunded   json.Number `json:"refunded"`
	ArchivedAt int64       `json:"archived_at"`
}

type SettlementPayment struct {
	ID                 string                 `json:"id"`
	ClientID           string                 `json:"client_id"`
	Date               string                 `json:"date"`
	StatusID           string                 `json:"status_id"`
	CurrencyID         string                 `json:"currency_id"`
	ExchangeCurrencyID string                 `json:"exchange_currency_id"`
	Amount             json.Number            `json:"amount"`
	Refunded           json.Number            `json:"refunded"`
	Applied            json.Number            `json:"applied"`
	ExchangeRate       json.Number            `json:"exchange_rate"`
	IsManual           bool                   `json:"is_manual"`
	IsDeleted          bool                   `json:"is_deleted"`
	ArchivedAt         int64                  `json:"archived_at"`
	Allocations        []SettlementAllocation `json:"paymentables"`
}

// Individual GETs include archived/deleted records at the pinned revision.
func (c *SettlementClient) GetSettlementCustomer(ctx context.Context, id string) (SettlementCustomer, error) {
	return settlementGet[SettlementCustomer](ctx, c, "clients", id, func(customer SettlementCustomer) string { return customer.ID })
}

func (c *SettlementClient) GetSettlementInvoice(ctx context.Context, id string) (SettlementInvoice, error) {
	return settlementGet[SettlementInvoice](ctx, c, "invoices", id, func(invoice SettlementInvoice) string { return invoice.ID })
}

func settlementGet[T any](ctx context.Context, c *SettlementClient, resource, id string, identify func(T) string) (T, error) {
	var zero T
	if !settlementID(id) {
		return zero, errors.New("invalid Invoice Ninja settlement identifier")
	}
	var envelope struct {
		Data *T `json:"data"`
	}
	if err := c.get(ctx, []string{"api", "v1", resource, id}, nil, &envelope); err != nil {
		return zero, err
	}
	if envelope.Data == nil || identify(*envelope.Data) != id {
		return zero, errInvalidResponse
	}
	return *envelope.Data, nil
}

// ListSettlementPayments uses client_id, NOT invoice_id: the pinned payment
// filter ignores invoice_id. Paymentables are included by default; invoices is
// requested explicitly. Only paymentables, not invoice totals, are allocations.
// Pages are constructed locally; response links/URLs are never followed. Stable
// pagination is required but cannot provide a transactional database snapshot.
func (c *SettlementClient) ListSettlementPayments(ctx context.Context, customerID string) ([]SettlementPayment, error) {
	if !settlementID(customerID) {
		return nil, errors.New("invalid Invoice Ninja settlement customer identifier")
	}
	query := url.Values{
		"client_id": {customerID}, "include": {"invoices"},
		"status": {"active,archived,deleted"}, "sort": {"id|asc"},
		"per_page": {strconv.Itoa(c.pageSize)},
	}
	payments := make([]SettlementPayment, 0)
	seen := make(map[string]struct{})
	seenAllocations := make(map[string]struct{})
	var first Pagination
	for page := 1; page <= c.maxPages; page++ {
		query.Set("page", strconv.Itoa(page))
		var envelope struct {
			Data []SettlementPayment `json:"data"`
			Meta *struct {
				Pagination *wirePagination `json:"pagination"`
			} `json:"meta"`
		}
		if err := c.get(ctx, []string{"api", "v1", "payments"}, query, &envelope); err != nil {
			return nil, errors.Join(ErrSettlementIncomplete, err)
		}
		if envelope.Data == nil || envelope.Meta == nil || envelope.Meta.Pagination == nil {
			return nil, ErrSettlementIncomplete
		}
		p, ok := envelope.Meta.Pagination.validate(page, len(envelope.Data))
		if !ok || p.PerPage != c.pageSize || p.TotalPages > c.maxPages || p.Total > 2000 {
			return nil, ErrSettlementIncomplete
		}
		if page == 1 {
			first = p
		} else if p.Total != first.Total || p.PerPage != first.PerPage || p.TotalPages != first.TotalPages {
			return nil, ErrSettlementIncomplete
		}
		for _, payment := range envelope.Data {
			if _, duplicate := seen[payment.ID]; duplicate || payment.ClientID != customerID {
				return nil, ErrSettlementIncomplete
			}
			seen[payment.ID] = struct{}{}
			for _, allocation := range payment.Allocations {
				if len(seenAllocations) >= 4000 {
					return nil, ErrSettlementIncomplete
				}
				if _, duplicate := seenAllocations[allocation.ID]; duplicate {
					return nil, ErrSettlementIncomplete
				}
				seenAllocations[allocation.ID] = struct{}{}
			}
			payments = append(payments, payment)
		}
		if page >= p.TotalPages {
			if len(payments) != p.Total {
				return nil, ErrSettlementIncomplete
			}
			return payments, nil
		}
	}
	return nil, ErrSettlementIncomplete
}

// Decode the envelope only after checking the authenticated response version.
// The shared transport is bounded, GET-only and does not follow redirects.
func (c *SettlementClient) get(ctx context.Context, path []string, query url.Values, result any) error {
	var raw json.RawMessage
	headers, err := c.transport.Get(ctx, path, query, &raw)
	if err != nil {
		return err
	}
	versions := headers.Values("X-APP-VERSION")
	if len(versions) != 1 || versions[0] != SupportedSettlementVersion {
		return ErrSettlementVersion
	}
	if !uniqueNinjaJSON(raw) || json.Unmarshal(raw, result) != nil {
		return errInvalidResponse
	}
	return nil
}

// Strict settlement decoding is independent of the permissive legacy DTOs.
// Raw fields distinguish absent/null from explicit false, zero and empty strings.
func settlementFields(raw []byte, result any, required ...string) (map[string]json.RawMessage, error) {
	if !uniqueNinjaJSON(raw) {
		return nil, errInvalidResponse
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, errInvalidResponse
	}
	for _, name := range required {
		if value := bytes.TrimSpace(fields[name]); len(value) == 0 || bytes.Equal(value, []byte("null")) {
			return nil, errInvalidResponse
		}
	}
	if json.Unmarshal(raw, result) != nil {
		return nil, errInvalidResponse
	}
	return fields, nil
}

func (customer *SettlementCustomer) UnmarshalJSON(raw []byte) error {
	type plain SettlementCustomer
	var decoded plain
	fields, err := settlementFields(raw, &decoded, "id", "id_number", "group_settings_id", "is_deleted", "archived_at", "settings")
	if err != nil {
		return err
	}
	if !settlementID(decoded.ID) || !settlementOptionalID(decoded.GroupID) || decoded.ArchivedAt < 0 {
		return errInvalidResponse
	}
	currency, err := settlementCustomerCurrency(fields["settings"])
	if err != nil {
		return err
	}
	decoded.CurrencyID = currency
	*customer = SettlementCustomer(decoded)
	return nil
}

// No company/group defaults are inferred here. Empty settings (including an
// empty JSON array), a missing currency key and null all mean no override.
func settlementCustomerCurrency(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", nil
	}
	if raw[0] == '[' {
		var empty []json.RawMessage
		if json.Unmarshal(raw, &empty) == nil && len(empty) == 0 {
			return "", nil
		}
		return "", errInvalidResponse
	}
	var settings map[string]json.RawMessage
	if json.Unmarshal(raw, &settings) != nil || settings == nil {
		return "", errInvalidResponse
	}
	value := bytes.TrimSpace(settings["currency_id"])
	if len(value) == 0 || bytes.Equal(value, []byte("null")) {
		return "", nil
	}
	var currency string
	if json.Unmarshal(value, &currency) != nil || !settlementOptionalID(currency) {
		return "", errInvalidResponse
	}
	return currency, nil
}

func (invoice *SettlementInvoice) UnmarshalJSON(raw []byte) error {
	type plain SettlementInvoice
	var decoded plain
	_, err := settlementFields(raw, &decoded, "id", "client_id", "recurring_id", "number", "status_id", "date", "due_date", "amount", "balance", "paid_to_date", "exchange_rate", "is_deleted", "archived_at")
	if err != nil {
		return err
	}
	if !settlementID(decoded.ID) || !settlementID(decoded.ClientID) || !settlementOptionalID(decoded.RecurringID) || !settlementID(decoded.StatusID) || decoded.ArchivedAt < 0 || !settlementNumbers(decoded.Amount, decoded.Balance, decoded.PaidToDate, decoded.ExchangeRate) {
		return errInvalidResponse
	}
	// Calendar/number/business validation is deliberately left to the verifier.
	*invoice = SettlementInvoice(decoded)
	return nil
}

func (allocation *SettlementAllocation) UnmarshalJSON(raw []byte) error {
	type plain SettlementAllocation
	var decoded plain
	fields, err := settlementFields(raw, &decoded, "id", "payment_id", "amount", "refunded", "archived_at")
	if err != nil {
		return err
	}
	// PaymentableTransformer emits only the applicable invoice_id OR credit_id.
	// An explicitly null key is not an empty opaque identifier.
	for _, name := range []string{"invoice_id", "credit_id"} {
		if value, present := fields[name]; present && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return errInvalidResponse
		}
	}
	if !settlementID(decoded.ID) || !settlementID(decoded.PaymentID) || !settlementOptionalID(decoded.InvoiceID) || !settlementOptionalID(decoded.CreditID) || (decoded.InvoiceID == "") == (decoded.CreditID == "") || decoded.ArchivedAt < 0 || !settlementNumbers(decoded.Amount, decoded.Refunded) {
		return errInvalidResponse
	}
	*allocation = SettlementAllocation(decoded)
	return nil
}

func (payment *SettlementPayment) UnmarshalJSON(raw []byte) error {
	type plain SettlementPayment
	var decoded plain
	_, err := settlementFields(raw, &decoded, "id", "client_id", "date", "status_id", "currency_id", "exchange_currency_id", "amount", "refunded", "applied", "exchange_rate", "is_manual", "is_deleted", "archived_at", "paymentables")
	if err != nil {
		return err
	}
	if !settlementID(decoded.ID) || !settlementID(decoded.ClientID) || !settlementID(decoded.StatusID) || !settlementOptionalID(decoded.CurrencyID) || !settlementOptionalID(decoded.ExchangeCurrencyID) || decoded.ArchivedAt < 0 || decoded.Allocations == nil || !settlementNumbers(decoded.Amount, decoded.Refunded, decoded.Applied, decoded.ExchangeRate) {
		return errInvalidResponse
	}
	seen := make(map[string]struct{})
	for _, allocation := range decoded.Allocations {
		if _, duplicate := seen[allocation.ID]; duplicate || allocation.PaymentID != decoded.ID {
			return errInvalidResponse
		}
		seen[allocation.ID] = struct{}{}
	}
	*payment = SettlementPayment(decoded)
	return nil
}

func settlementID(value string) bool { return len(value) <= 256 && validID(value) }

func settlementOptionalID(value string) bool { return value == "" || settlementID(value) }

// Bound work before any caller uses arbitrary-precision arithmetic. Values are
// never converted through float64; signs and accounting relationships are left
// to the verifier. Quoted JSON numbers have the same lexical requirements.
func settlementNumbers(values ...json.Number) bool {
	for _, value := range values {
		s := value.String()
		if len(s) == 0 || len(s) > 128 || !numberPattern.MatchString(s) {
			return false
		}
		if index := strings.IndexAny(s, "eE"); index >= 0 {
			exponent, err := strconv.Atoi(s[index+1:])
			if err != nil || exponent < -128 || exponent > 128 {
				return false
			}
		}
	}
	return true
}
