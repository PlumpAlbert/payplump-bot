package invoiceninja

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
)

// Write contracts were inspected at Invoice Ninja source commit
// 493c3884e4214028bc5970cb41bafbd9c4d96401 (v5.13.47): Client/Invoice
// controllers, filters, requests, transformers and Invoice markSent service.
// These are source-verified contracts, NOT live-write contract verification.
// SupportedWriteVersion is the only app version accepted by billing operations.
const SupportedWriteVersion = "5.13.47"

const (
	billingStatuses = "active,archived,deleted"
	maxBillingPages = 1000
	maxBillingBody  = 64 << 10
)

var (
	ErrWriteVersion     = errors.New("Invoice Ninja billing requires app version 5.13.47")
	ErrLookupIncomplete = errors.New("Invoice Ninja billing lookup is incomplete")
	errBillingDraft     = errors.New("invalid Invoice Ninja billing draft")
	customerMarker      = regexp.MustCompile(`^payplump:[A-Za-z0-9_-]{20,128}:customer_create$`)
	invoiceMarker       = regexp.MustCompile(`^payplump:[A-Za-z0-9_-]{20,128}:invoice_create$`)
)

// BillingClient is an explicit write capability, separate from the read-only
// Client used by preflight. No POST is retried and no idempotency header exists.
// The caller must durably journal each intent BEFORE calling a mutation, and
// hold uncertain outcomes instead of replaying a request.
type BillingClient struct {
	*Client
	writer   *integrationhttp.Writer
	pageSize int
	maxPages int
}

// NewBillingClient bounds each inventory to at most 1000 pages of 100 records.
// Like New, base is the service URL including any installation prefix, not /api/v1.
func NewBillingClient(base, token string, timeout time.Duration, pageSize, maxPages int) (*BillingClient, error) {
	if pageSize < 1 || pageSize > 100 || maxPages < 1 || maxPages > maxBillingPages {
		return nil, errors.New("invalid Invoice Ninja billing pagination bounds")
	}
	client, err := New(base, token, timeout)
	if err != nil {
		return nil, err
	}
	headers := make(http.Header)
	headers.Set("X-API-TOKEN", token)
	headers.Set("X-Requested-With", "XMLHttpRequest")
	writer, err := integrationhttp.NewWriter(base, headers, timeout)
	if err != nil {
		return nil, errors.New("invalid Invoice Ninja billing client configuration")
	}
	return &BillingClient{Client: client, writer: writer, pageSize: pageSize, maxPages: maxPages}, nil
}

type CustomerDraft struct {
	Name         string `json:"name"`
	IDNumber     string `json:"id_number"`
	PrivateNotes string `json:"private_notes"`
}

type InvoiceLine struct {
	Discount         json.Number `json:"discount"`
	IsAmountDiscount bool        `json:"is_amount_discount"`
	TypeID           string      `json:"type_id"`
	ProductKey       string      `json:"product_key"`
	Notes            string      `json:"notes"`
	Cost             json.Number `json:"cost"`
	Quantity         json.Number `json:"quantity"`
	TaxName1         string      `json:"tax_name1"`
	TaxName2         string      `json:"tax_name2"`
	TaxName3         string      `json:"tax_name3"`
	TaxRate1         json.Number `json:"tax_rate1"`
	TaxRate2         json.Number `json:"tax_rate2"`
	TaxRate3         json.Number `json:"tax_rate3"`
	TaxID            string      `json:"tax_id"`
}

// Legacy saved line items can expose 0/1 instead of JSON booleans. This is
// presentation of the discount mode, not permission for a nonzero discount.
func (line *InvoiceLine) UnmarshalJSON(raw []byte) error {
	type plain InvoiceLine
	var decoded plain
	wire := struct {
		*plain
		Mode json.RawMessage `json:"is_amount_discount"`
	}{plain: &decoded}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	switch string(bytes.TrimSpace(wire.Mode)) {
	case "", "null", "false", "0":
		decoded.IsAmountDiscount = false
	case "true", "1":
		decoded.IsAmountDiscount = true
	default:
		return errInvalidResponse
	}
	*line = InvoiceLine(decoded)
	return nil
}

type InvoiceDraft struct {
	ClientID           string        `json:"client_id"`
	Date               string        `json:"date"`
	DueDate            string        `json:"due_date"`
	PrivateNotes       string        `json:"private_notes"`
	UsesInclusiveTaxes bool          `json:"uses_inclusive_taxes"`
	LineItems          []InvoiceLine `json:"line_items"`
}

type IssueDraft struct {
	Action string   `json:"action"`
	IDs    []string `json:"ids"`
}

type Invitation struct {
	ID   string `json:"id"`
	Link string `json:"link"`
}

// FindCustomers uses only id_number equality, including archived and deleted
// records. private_notes is returned but is NOT a supported server-side filter.
// An ignored filter is an inconsistent inventory, not evidence of absence.
func billingLookupTemporaryFailure(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var remote *integrationhttp.HTTPError
	if errors.As(err, &remote) {
		return remote.StatusCode == http.StatusRequestTimeout || remote.StatusCode == http.StatusTooManyRequests || remote.StatusCode >= 500
	}
	// The shared transport deliberately exposes only bounded, redacted errors.
	// These two exact transport outcomes are retryable; malformed/oversized JSON
	// and all other response-contract errors remain fail-closed.
	return err != nil && (err.Error() == "integration API connection failed" || err.Error() == "could not read integration API response")
}

func billingLookupFailure(err error) error {
	if billingLookupTemporaryFailure(err) {
		return err
	}
	return errors.Join(ErrLookupIncomplete, err)
}

// validateBoundedInventoryPage provides the shared stable-pagination contract
// for source inventories. The caller retains its own GET and error policy.
func validateBoundedInventoryPage(w wirePagination, page, count, pageSize, maxPages, maxTotal int, first *Pagination) (Pagination, bool) {
	p, ok := w.validate(page, count)
	if !ok || p.PerPage != pageSize || p.TotalPages > maxPages || p.Total > maxTotal {
		return Pagination{}, false
	}
	if page == 1 {
		*first = p
		return p, true
	}
	if p.Total != first.Total || p.TotalPages != first.TotalPages || p.PerPage != first.PerPage {
		return Pagination{}, false
	}
	return p, true
}

func decodeUniqueNinjaJSON(raw []byte, result any) error {
	if !uniqueNinjaJSON(raw) || json.Unmarshal(raw, result) != nil {
		return errInvalidResponse
	}
	return nil
}

func (c *BillingClient) FindCustomers(ctx context.Context, telegramID int64) ([]Customer, error) {
	if telegramID <= 0 {
		return nil, errBillingDraft
	}
	id := strconv.FormatInt(telegramID, 10)
	return billingLookup(ctx, c, "clients", url.Values{"id_number": {id}}, func(customer Customer) (string, bool) {
		parsed, err := customer.TelegramID("id_number")
		return customer.ID, billingID(customer.ID) && len(customer.IDNumber) <= 19 && err == nil && parsed == telegramID && customer.IDNumber == id
	})
}

func (c *BillingClient) FindInvoices(ctx context.Context, customerID string) ([]Invoice, error) {
	if !billingID(customerID) {
		return nil, errBillingDraft
	}
	return billingLookup(ctx, c, "invoices", url.Values{"client_id": {customerID}}, func(invoice Invoice) (string, bool) {
		return invoice.ID, validBillingInvoice(invoice) && invoice.ClientID == customerID
	})
}

// All pages are constructed locally. Remote pagination links are never followed.
// Stable totals/page sizes and unique IDs are required; pagination is not a
// transactional snapshot, so the caller must still serialize registration work.
func billingLookup[T any](ctx context.Context, c *BillingClient, resource string, query url.Values, identify func(T) (string, bool)) ([]T, error) {
	query.Set("status", billingStatuses)
	query.Set("per_page", strconv.Itoa(c.pageSize))
	items := make([]T, 0)
	seen := make(map[string]struct{})
	var first Pagination
	for page := 1; page <= c.maxPages; page++ {
		query.Set("page", strconv.Itoa(page))
		var envelope struct {
			Data []T `json:"data"`
			Meta *struct {
				Pagination *wirePagination `json:"pagination"`
			} `json:"meta"`
		}
		err := getUniqueNinjaJSON(ctx, c.transport, []string{"api", "v1", resource}, query, SupportedWriteVersion, ErrWriteVersion, &envelope)
		if err != nil {
			return nil, billingLookupFailure(err)
		}
		if envelope.Data == nil || envelope.Meta == nil || envelope.Meta.Pagination == nil {
			return nil, ErrLookupIncomplete
		}
		p, ok := validateBoundedInventoryPage(*envelope.Meta.Pagination, page, len(envelope.Data), c.pageSize, c.maxPages, c.maxPages*c.pageSize, &first)
		if !ok {
			return nil, ErrLookupIncomplete
		}
		for _, item := range envelope.Data {
			id, valid := identify(item)
			if _, duplicate := seen[id]; !valid || duplicate {
				return nil, ErrLookupIncomplete
			}
			seen[id] = struct{}{}
			items = append(items, item)
		}
		if page >= p.TotalPages {
			if len(items) != p.Total {
				return nil, ErrLookupIncomplete
			}
			return items, nil
		}
	}
	return nil, ErrLookupIncomplete
}

// Individual endpoints include trashed records at the pinned source revision.
func (c *BillingClient) GetCustomer(ctx context.Context, id string) (Customer, error) {
	return billingGet(ctx, c, "clients", id, func(customer Customer) bool {
		return billingID(customer.ID) && customer.completeBillingFields && customer.ID == id
	})
}

func (c *BillingClient) GetInvoice(ctx context.Context, id string) (Invoice, error) {
	return billingGet(ctx, c, "invoices", id, func(invoice Invoice) bool {
		return validBillingInvoice(invoice) && invoice.completeBillingFields && invoice.ID == id
	})
}

func billingGet[T any](ctx context.Context, c *BillingClient, resource, id string, validate func(T) bool) (T, error) {
	var zero T
	if !billingID(id) {
		return zero, errBillingDraft
	}
	var envelope struct {
		Data *T `json:"data"`
	}
	err := getUniqueNinjaJSON(ctx, c.transport, []string{"api", "v1", resource, id}, nil, SupportedWriteVersion, ErrWriteVersion, &envelope)
	if err != nil {
		if errors.Is(err, errInvalidResponse) || errors.Is(err, integrationhttp.ErrInvalidResponse) {
			return zero, errors.Join(ErrLookupIncomplete, err)
		}
		return zero, err
	}
	if envelope.Data == nil || !validate(*envelope.Data) {
		return zero, errors.Join(ErrLookupIncomplete, errInvalidResponse)
	}
	return *envelope.Data, nil
}

func verifyAppVersion(headers http.Header) error {
	values := headers.Values("X-APP-VERSION")
	if len(values) != 1 || values[0] != SupportedWriteVersion {
		return ErrWriteVersion
	}
	return nil
}

// Revalidate immediately before EVERY POST, even when a prior read or mutation
// observed the right version. This authenticated GET has bounded transport/body
// size, timeout and one requested record; it does not enumerate an inventory.
func (c *BillingClient) verifyVersion(ctx context.Context) error {
	var body json.RawMessage
	err := getUniqueNinjaJSON(ctx, c.transport, []string{"api", "v1", "clients"}, url.Values{
		"page": {"1"}, "per_page": {"1"}, "status": {billingStatuses},
	}, SupportedWriteVersion, ErrWriteVersion, &body)
	return err
}

func (c *BillingClient) CreateCustomer(ctx context.Context, draft CustomerDraft) (Customer, error) {
	if err := ValidateCustomerDraft(draft); err != nil {
		return Customer{}, err
	}
	if err := c.verifyVersion(ctx); err != nil {
		return Customer{}, err
	}
	var raw json.RawMessage
	headers, err := c.writer.Post(ctx, []string{"api", "v1", "clients"}, nil, draft, &raw)
	if err != nil {
		return Customer{}, err
	}
	if err := verifyAppVersion(headers); err != nil {
		return Customer{}, err
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := decodeUniqueNinjaJSON(raw, &envelope); err != nil {
		return Customer{}, err
	}
	var customer Customer
	if json.Unmarshal(envelope.Data, &customer) != nil || !customer.Active() || ValidateCustomerForDraft(customer, draft) != nil {
		return Customer{}, errInvalidResponse
	}
	return customer, nil
}

// CreateInvoice only creates a draft. The caller must explicitly obtain the
// inclusive-tax policy from operator configuration and copy product.Price
// (never purchase cost), product key, notes and all product tax fields into the
// single line. No product_id, amount, payment, auto-bill or email flags are sent.
func (c *BillingClient) CreateInvoice(ctx context.Context, draft InvoiceDraft) (Invoice, error) {
	if err := ValidateInvoiceDraft(draft); err != nil {
		return Invoice{}, err
	}
	// Snapshot caller-owned slice data before the version check and POST.
	draft.LineItems = append([]InvoiceLine(nil), draft.LineItems...)
	if err := c.verifyVersion(ctx); err != nil {
		return Invoice{}, err
	}
	var raw json.RawMessage
	headers, err := c.writer.Post(ctx, []string{"api", "v1", "invoices"}, nil, draft, &raw)
	if err != nil {
		return Invoice{}, err
	}
	if err := verifyAppVersion(headers); err != nil {
		return Invoice{}, err
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := decodeUniqueNinjaJSON(raw, &envelope); err != nil {
		return Invoice{}, err
	}
	var invoice Invoice
	if !billingInvoiceFields(envelope.Data) || json.Unmarshal(envelope.Data, &invoice) != nil || ValidateInvoiceForDraft(invoice, draft) != nil || invoice.IsDeleted || invoice.ArchivedAt != 0 || invoice.StatusID != "1" || invoice.RecurringID != "" || !draftBillingInvoice(invoice) {
		return Invoice{}, errInvalidResponse
	}
	// The API's amount is authoritative; taxes/rounding are not computed here.
	return invoice, nil
}

// IssueInvoice marks exactly one invoice sent. This action does not directly
// email the customer, but installed settings can schedule reminders, events and
// admin email. Its sent-status claim precedes ledger updates: after ANY ambiguous
// POST, observing status 2 later is NOT proof that accounting completed. Hold the
// journal entry; never retry/adopt based on status alone.
func (c *BillingClient) IssueInvoice(ctx context.Context, draft IssueDraft) (Invoice, error) {
	if draft.Action != "mark_sent" || len(draft.IDs) != 1 || !billingID(draft.IDs[0]) || !billingPayload(draft) {
		return Invoice{}, errBillingDraft
	}
	draft.IDs = append([]string(nil), draft.IDs...)
	if err := c.verifyVersion(ctx); err != nil {
		return Invoice{}, err
	}
	var raw json.RawMessage
	headers, err := c.writer.Post(ctx, []string{"api", "v1", "invoices", "bulk"}, nil, draft, &raw)
	if err != nil {
		return Invoice{}, err
	}
	if err := verifyAppVersion(headers); err != nil {
		return Invoice{}, err
	}
	var envelope struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := decodeUniqueNinjaJSON(raw, &envelope); err != nil {
		return Invoice{}, err
	}
	if len(envelope.Data) != 1 {
		return Invoice{}, errInvalidResponse
	}
	var invoice Invoice
	if json.Unmarshal(envelope.Data[0], &invoice) != nil {
		return Invoice{}, errInvalidResponse
	}
	if !invoice.completeBillingFields || !unadjustedBillingInvoice(invoice) || invoice.RecurringID != "" || len(invoice.LineItems) != 1 || !validBillingLine(invoice.LineItems[0]) || !validBillingInvoice(invoice) || invoice.ID != draft.IDs[0] || invoice.StatusID != "2" || invoice.IsDeleted || invoice.ArchivedAt != 0 || !unpaidBillingInvoice(invoice) {
		return Invoice{}, errInvalidResponse
	}
	return invoice, nil
}

// ValidateCustomerDraft performs the same local checks as CreateCustomer, with
// no network access. Callers can validate before reserving a durable intent.
func ValidateCustomerDraft(draft CustomerDraft) error {
	if !billingText(draft.Name, 256) || strings.TrimSpace(draft.Name) == "" || !canonicalBillingIDNumber(draft.IDNumber) || !customerMarker.MatchString(draft.PrivateNotes) || !billingPayload(draft) {
		return errBillingDraft
	}
	return nil
}

// ValidateInvoiceDraft performs the same local checks as CreateInvoice. The
// inclusive-tax bool must come from explicit operator configuration; its false
// value is valid and is always included in the JSON payload.
func ValidateInvoiceDraft(draft InvoiceDraft) error {
	if !billingID(draft.ClientID) || !billingDate(draft.Date) || draft.DueDate != draft.Date || !invoiceMarker.MatchString(draft.PrivateNotes) || len(draft.LineItems) != 1 || !validBillingLine(draft.LineItems[0]) || !billingPayload(draft) {
		return errBillingDraft
	}
	return nil
}

// ValidateCustomerForDraft compares identity and creation payload, independently
// of active/archived/deleted state. It is shared by writes and reconciliation.
func ValidateCustomerForDraft(customer Customer, draft CustomerDraft) error {
	if err := ValidateCustomerDraft(draft); err != nil {
		return err
	}
	if (customer.fromWire && !customer.completeBillingFields) || !billingID(customer.ID) || customer.IDNumber != draft.IDNumber || customer.PrivateNotes != draft.PrivateNotes {
		return errInvalidResponse
	}
	return nil
}

// ValidateInvoiceForDraft checks identity and payload, NOT lifecycle status,
// activity or unpaid state. Callers must validate those separately. In
// particular a matching sent invoice never proves an uncertain issue POST
// completed accounting. Monetary values are compared exactly, without float64.
func ValidateInvoiceForDraft(invoice Invoice, draft InvoiceDraft) error {
	if err := ValidateInvoiceDraft(draft); err != nil {
		return err
	}
	if (invoice.fromWire && !invoice.completeBillingFields) || !validBillingInvoice(invoice) || !unadjustedBillingInvoice(invoice) || invoice.RecurringID != "" || invoice.ClientID != draft.ClientID || invoice.Date != draft.Date || invoice.DueDate != draft.DueDate || invoice.PrivateNotes != draft.PrivateNotes || invoice.UsesInclusiveTaxes != draft.UsesInclusiveTaxes || len(invoice.LineItems) != 1 || !sameBillingLine(invoice.LineItems[0], draft.LineItems[0]) {
		return errInvalidResponse
	}
	return nil
}

func billingID(id string) bool { return len(id) <= 256 && validID(id) }

func canonicalBillingIDNumber(value string) bool {
	if len(value) > 19 {
		return false
	}
	_, err := (Customer{IDNumber: value}).TelegramID("id_number")
	return err == nil
}

func billingText(value string, maxRunes int) bool {
	return len(value) <= maxRunes*utf8.UTFMax && utf8.ValidString(value) && !strings.ContainsRune(value, 0) && utf8.RuneCountInString(value) <= maxRunes
}

func billingDate(value string) bool {
	if len(value) != len("2006-01-02") {
		return false
	}
	date, err := time.Parse("2006-01-02", value)
	return err == nil && date.Year() > 0 && date.Format("2006-01-02") == value
}

func billingPayload(value any) bool {
	body, err := json.Marshal(value)
	return err == nil && len(body) <= maxBillingBody
}

// Bounds are applied BEFORE big.Rat parsing so attacker-controlled exponents
// cannot demand unbounded work. No monetary/tax value ever passes via float64.
func billingNumber(value json.Number) (*big.Rat, bool) {
	s := value.String()
	if len(s) == 0 || len(s) > 128 || !numberPattern.MatchString(s) {
		return nil, false
	}
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		exponent := s[i+1:]
		if len(exponent) > 4 {
			return nil, false
		}
		n, err := strconv.Atoi(exponent)
		if err != nil || n < -128 || n > 128 {
			return nil, false
		}
	}
	r, ok := new(big.Rat).SetString(s)
	return r, ok && r.Sign() >= 0
}

func validBillingInvoice(invoice Invoice) bool {
	_, amountOK := billingNumber(invoice.Amount)
	_, balanceOK := billingNumber(invoice.Balance)
	return validInvoice(invoice) && billingID(invoice.ID) && amountOK && balanceOK
}

// Drafts have no outstanding balance yet. MarkSent adds the invoice amount
// to balance and the client ledger; issuing a nonzero draft could double it.
func draftBillingInvoice(invoice Invoice) bool {
	amount, aok := billingNumber(invoice.Amount)
	balance, bok := billingNumber(invoice.Balance)
	return aok && bok && amount.Sign() > 0 && balance.Sign() == 0
}

func unpaidBillingInvoice(invoice Invoice) bool {
	amount, aok := billingNumber(invoice.Amount)
	balance, bok := billingNumber(invoice.Balance)
	return aok && bok && amount.Sign() > 0 && amount.Cmp(balance) == 0
}

// Empty Number is the Go zero value; strict wire-presence checks below keep
// an omitted/null API field from being mistaken for an actual zero.
func zeroBillingNumber(value json.Number) bool {
	if value == "" {
		return true
	}
	n, ok := billingNumber(value)
	return ok && n.Sign() == 0
}

func unadjustedBillingInvoice(invoice Invoice) bool {
	for _, value := range []json.Number{invoice.Discount, invoice.TaxRate1, invoice.TaxRate2, invoice.TaxRate3, invoice.CustomSurcharge1, invoice.CustomSurcharge2, invoice.CustomSurcharge3, invoice.CustomSurcharge4, invoice.Partial, invoice.PaidToDate} {
		if !zeroBillingNumber(value) {
			return false
		}
	}
	return true
}

func validBillingLine(line InvoiceLine) bool {
	if !zeroBillingNumber(line.Discount) || line.TypeID != "1" {
		return false
	}
	if !billingText(line.ProductKey, 256) || strings.TrimSpace(line.ProductKey) == "" || !billingText(line.Notes, 4096) || !billingText(line.TaxID, 256) || !billingText(line.TaxName1, 256) || !billingText(line.TaxName2, 256) || !billingText(line.TaxName3, 256) {
		return false
	}
	cost, cok := billingNumber(line.Cost)
	quantity, qok := billingNumber(line.Quantity)
	if !cok || cost.Sign() <= 0 || !qok || quantity.Cmp(big.NewRat(1, 1)) != 0 {
		return false
	}
	for _, rate := range []json.Number{line.TaxRate1, line.TaxRate2, line.TaxRate3} {
		if _, ok := billingNumber(rate); !ok {
			return false
		}
	}
	return true
}

func sameBillingLine(got, want InvoiceLine) bool {
	if !validBillingLine(got) || got.ProductKey != want.ProductKey || got.TaxID != want.TaxID || got.TaxName1 != want.TaxName1 || got.TaxName2 != want.TaxName2 || got.TaxName3 != want.TaxName3 {
		return false
	}
	for _, pair := range [][2]json.Number{{got.Cost, want.Cost}, {got.Quantity, want.Quantity}, {got.TaxRate1, want.TaxRate1}, {got.TaxRate2, want.TaxRate2}, {got.TaxRate3, want.TaxRate3}} {
		a, aok := billingNumber(pair[0])
		b, bok := billingNumber(pair[1])
		if !aok || !bok || a.Cmp(b) != 0 {
			return false
		}
	}
	return true
}

func billingCustomerFields(raw json.RawMessage) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	for _, name := range []string{"id", "id_number", "private_notes", "is_deleted", "archived_at"} {
		value := bytes.TrimSpace(fields[name])
		if len(value) == 0 || bytes.Equal(value, []byte("null")) {
			return false
		}
	}
	return true
}

// Distinguish an absent/null inclusive flag (especially false) and empty tax
// fields from explicit response values, without changing legacy read decoders.
func billingInvoiceFields(raw json.RawMessage) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	has := func(fields map[string]json.RawMessage, names ...string) bool {
		for _, name := range names {
			value := bytes.TrimSpace(fields[name])
			if len(value) == 0 || bytes.Equal(value, []byte("null")) {
				return false
			}
		}
		return true
	}
	if !has(fields, "id", "client_id", "date", "due_date", "private_notes", "uses_inclusive_taxes", "status_id", "amount", "balance", "line_items", "discount", "is_amount_discount", "tax_rate1", "tax_rate2", "tax_rate3", "custom_surcharge1", "custom_surcharge2", "custom_surcharge3", "custom_surcharge4", "partial", "paid_to_date", "is_deleted", "archived_at", "recurring_id") {
		return false
	}
	var lines []map[string]json.RawMessage
	if json.Unmarshal(fields["line_items"], &lines) != nil || lines == nil {
		return false
	}
	// Presence evidence is structural, not a one-line business rule. Preserve
	// conflicting histories for the worker to hold instead of endlessly retrying.
	for _, line := range lines {
		if !has(line, "product_key", "notes", "cost", "quantity", "tax_name1", "tax_name2", "tax_name3", "tax_rate1", "tax_rate2", "tax_rate3", "tax_id", "discount", "is_amount_discount", "type_id") {
			return false
		}
	}
	return true
}
