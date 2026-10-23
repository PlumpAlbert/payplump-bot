package invoiceninja

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
)

type Client struct {
	transport *integrationhttp.Client
}

// ValidateClientConfiguration checks the same read-client settings as New
// without creating a transport or other provider capability.
func ValidateClientConfiguration(baseURL, token string, timeout time.Duration) error {
	if token == "" || strings.TrimSpace(token) != token || strings.IndexFunc(token, unicode.IsControl) >= 0 || !utf8.ValidString(token) {
		return errors.New("invalid Invoice Ninja token")
	}
	if !validServiceBase(baseURL) {
		return errors.New("invalid Invoice Ninja base URL")
	}
	if integrationhttp.ValidateBaseURL(baseURL) != nil || timeout <= 0 || timeout > 30*time.Second {
		return errors.New("invalid Invoice Ninja client configuration")
	}
	return nil
}

// New expects the service base URL, including any installation subpath but
// without /api/v1. All requests use the shared bounded GET-only transport.
func New(baseURL, token string, timeout time.Duration) (*Client, error) {
	if err := ValidateClientConfiguration(baseURL, token, timeout); err != nil {
		return nil, err
	}
	headers := make(http.Header)
	headers.Set("X-API-TOKEN", token)
	headers.Set("X-Requested-With", "XMLHttpRequest")
	transport, err := integrationhttp.New(baseURL, headers, timeout)
	if err != nil {
		return nil, errors.New("invalid Invoice Ninja client configuration")
	}
	return &Client{transport: transport}, nil
}

func (c *Client) ListCustomers(ctx context.Context, page, perPage int) (Page[Customer], error) {
	return list[Customer](ctx, c, "clients", page, perPage, nil, validCustomer)
}

func (c *Client) ListProducts(ctx context.Context, page, perPage int) (Page[Product], error) {
	return list[Product](ctx, c, "products", page, perPage, nil, validProduct)
}

func (c *Client) GetProduct(ctx context.Context, id string) (Product, error) {
	product, _, err := c.GetProductWithVersion(ctx, id)
	return product, err
}

// GetProductWithVersion is the same permissive read as GetProduct, with the
// response's validated version metadata available to scoped preflight callers.
func (c *Client) GetProductWithVersion(ctx context.Context, id string) (Product, string, error) {
	if !validID(id) {
		return Product{}, "", errors.New("invalid Invoice Ninja product identifier")
	}
	var envelope struct {
		Data *Product `json:"data"`
	}
	headers, err := c.transport.Get(ctx, []string{"api", "v1", "products", id}, nil, &envelope)
	if err != nil {
		return Product{}, "", err
	}
	version, err := appVersion(headers)
	if err != nil {
		return Product{}, "", err
	}
	if envelope.Data == nil || !validProduct(*envelope.Data) || envelope.Data.ID != id {
		return Product{}, "", errInvalidResponse
	}
	return *envelope.Data, version, nil
}

// GetCustomer is read-only. The caller must bind id_number to its own
// authenticated Telegram actor; an opaque API ID is not authorization.
func (c *Client) GetCustomer(ctx context.Context, id string) (Customer, error) {
	if !validID(id) {
		return Customer{}, errors.New("invalid Invoice Ninja customer identifier")
	}
	var envelope struct {
		Data *Customer `json:"data"`
	}
	headers, err := c.transport.Get(ctx, []string{"api", "v1", "clients", id}, nil, &envelope)
	if err != nil {
		return Customer{}, err
	}
	if _, err := appVersion(headers); err != nil {
		return Customer{}, err
	}
	if envelope.Data == nil || !validCustomer(*envelope.Data) || envelope.Data.ID != id {
		return Customer{}, errInvalidResponse
	}
	return *envelope.Data, nil
}

func (c *Client) GetInvoice(ctx context.Context, id string) (Invoice, error) {
	if !validID(id) {
		return Invoice{}, errors.New("invalid Invoice Ninja invoice identifier")
	}
	var envelope struct {
		Data *Invoice `json:"data"`
	}
	headers, err := c.transport.Get(ctx, []string{"api", "v1", "invoices", id}, nil, &envelope)
	if err != nil {
		return Invoice{}, err
	}
	if _, err := appVersion(headers); err != nil {
		return Invoice{}, err
	}
	if envelope.Data == nil || !validInvoice(*envelope.Data) || !validID(envelope.Data.ClientID) || envelope.Data.ID != id {
		return Invoice{}, errInvalidResponse
	}
	return *envelope.Data, nil
}

func (c *Client) ListInvoices(ctx context.Context, page, perPage int) (Page[Invoice], error) {
	return list[Invoice](ctx, c, "invoices", page, perPage, nil, validInvoice)
}

func (c *Client) ListPayments(ctx context.Context, page, perPage int) (Page[Payment], error) {
	return list[Payment](ctx, c, "payments", page, perPage, url.Values{"include": {"invoices"}}, validPayment)
}

func (c *Client) ListRecurringInvoices(ctx context.Context, page, perPage int) (Page[RecurringInvoice], error) {
	return list[RecurringInvoice](ctx, c, "recurring_invoices", page, perPage, nil, validRecurringInvoice)
}

// list fetches exactly one explicitly requested page. Pagination links in the
// response cannot change the configured origin or trigger extra requests.
func list[T any](ctx context.Context, c *Client, resource string, page, perPage int, query url.Values, validate func(T) bool) (Page[T], error) {
	if page < 1 || perPage < 1 || perPage > 100 {
		return Page[T]{}, errors.New("invalid Invoice Ninja pagination arguments")
	}
	if query == nil {
		query = make(url.Values)
	}
	query.Set("page", strconv.Itoa(page))
	query.Set("per_page", strconv.Itoa(perPage))
	var envelope struct {
		Data []T `json:"data"`
		Meta *struct {
			Pagination *wirePagination `json:"pagination"`
		} `json:"meta"`
	}
	headers, err := c.transport.Get(ctx, []string{"api", "v1", resource}, query, &envelope)
	if err != nil {
		return Page[T]{}, err
	}
	if envelope.Data == nil || envelope.Meta == nil || envelope.Meta.Pagination == nil {
		return Page[T]{}, errInvalidResponse
	}
	pagination, ok := envelope.Meta.Pagination.validate(page, len(envelope.Data))
	if !ok {
		return Page[T]{}, errInvalidResponse
	}
	for _, item := range envelope.Data {
		if !validate(item) {
			return Page[T]{}, errInvalidResponse
		}
	}
	version, err := appVersion(headers)
	if err != nil {
		return Page[T]{}, err
	}
	return Page[T]{Data: envelope.Data, Pagination: pagination, AppVersion: version}, nil
}
