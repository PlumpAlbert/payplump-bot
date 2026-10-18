package invoiceninja

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/billing"
	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
)

// Source contract: Invoice Ninja 493c3884e4214028bc5970cb41bafbd9c4d96401
// (5.13.47), RecurringInvoice controller/factory/model/request/transformer and
// BaseRepository. These are source-verified adapters, not live-write verification
// or an activation runtime. Plain creation makes a DRAFT, not an ordinary invoice.
// Company update_products can enqueue product updates; installed event handlers
// and external automation are unknown. No claim of side-effect-free writes is made.
const SupportedRecurringVersion = "5.13.47"

var (
	errRecurringDraft   = errors.New("invalid Invoice Ninja recurring draft")
	ErrRecurringExpired = errors.New("Invoice Ninja recurring first-cycle schedule is no longer safely in the future")
	// ErrRecurringUnknown requires HOLD, not replay, even after a later GET shows
	// status 2. The caller must journal BEFORE writing; there is no server-side
	// exactly-once, compare-and-swap or atomic cross-system activation guarantee.
	ErrRecurringUnknown = errors.New("Invoice Ninja recurring write outcome is unknown; hold journal intent")
	recurringMarker     = regexp.MustCompile(`^payplump:[A-Za-z0-9_-]{20,128}:recurring_create$`)
)

// RecurringClient is a separate, explicitly constructed GET + writer capability.
// It does not add mutations to Client or BillingClient. No endpoint is mounted by
// constructing it. Paid ordinary invoice links belong in a future caller journal,
// never in this API payload. Stop/resume are deliberately outside this capability.
type RecurringClient struct {
	reader   *integrationhttp.Client
	writer   *integrationhttp.Writer
	pageSize int
	maxPages int
	now      func() time.Time
}

func NewRecurringClient(base, token string, timeout time.Duration, pageSize, maxPages int) (*RecurringClient, error) {
	if pageSize < 1 || pageSize > 100 || maxPages < 1 || maxPages > 100 {
		return nil, errors.New("invalid Invoice Ninja recurring pagination bounds")
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
		return nil, errors.New("invalid Invoice Ninja recurring client configuration")
	}
	return &RecurringClient{reader: client.transport, writer: writer, pageSize: pageSize, maxPages: maxPages, now: time.Now}, nil
}

// RecurringDraft intentionally cannot encode status, currency, first_invoice_id,
// amount, payment, clone, send_now or email flags. FrequencyID is a JSON INTEGER;
// the API's corresponding response field is a string.
type RecurringDraft struct {
	ClientID           string        `json:"client_id"`
	FrequencyID        int           `json:"frequency_id"`
	NextSendDate       string        `json:"next_send_date"`
	DueDateDays        string        `json:"due_date_days"`
	AutoBill           string        `json:"auto_bill"`
	RemainingCycles    int           `json:"remaining_cycles"`
	PrivateNotes       string        `json:"private_notes"`
	UsesInclusiveTaxes bool          `json:"uses_inclusive_taxes"`
	LineItems          []InvoiceLine `json:"line_items"`
}

// BuildRecurringDraft freezes the product's sale price and tax fields. Payment.date,
// not arrival time, anchors the next calendar month (clamping days 29 through 31).
// today is an explicit canonical local date for preparation; future payment dates
// are invalid. CreateDraft checks the actual local clock again and never silently
// reschedules an expired intent.
func BuildRecurringDraft(customerID, paymentDate, today string, product Product, inclusive bool, marker string) (RecurringDraft, error) {
	if !billingDate(paymentDate) || !billingDate(today) || paymentDate > today || !billingID(product.ID) || !product.Active() {
		return RecurringDraft{}, errRecurringDraft
	}
	next, err := billing.NextBillingDate(paymentDate)
	if err != nil {
		return RecurringDraft{}, errRecurringDraft
	}
	if next <= today {
		return RecurringDraft{}, ErrRecurringExpired
	}
	draft := RecurringDraft{
		ClientID: customerID, FrequencyID: 5, NextSendDate: next,
		DueDateDays: "on_receipt", AutoBill: "off", RemainingCycles: -1,
		PrivateNotes: marker, UsesInclusiveTaxes: inclusive,
		LineItems: []InvoiceLine{{
			ProductKey: product.ProductKey, Notes: product.Notes, Cost: product.Price,
			Quantity: "1", Discount: "0", IsAmountDiscount: false, TypeID: "1",
			TaxName1: product.TaxName1, TaxName2: product.TaxName2, TaxName3: product.TaxName3,
			TaxRate1: product.TaxRate1, TaxRate2: product.TaxRate2, TaxRate3: product.TaxRate3, TaxID: product.TaxID,
		}},
	}
	if err := ValidateRecurringDraft(draft); err != nil {
		return RecurringDraft{}, err
	}
	return draft, nil
}

// ValidateRecurringDraft is a clock-independent structural policy check, suitable
// before reserving a durable intent. False inclusive-tax policy is explicit JSON.
func ValidateRecurringDraft(draft RecurringDraft) error {
	if !billingID(draft.ClientID) || draft.FrequencyID != 5 || !billingDate(draft.NextSendDate) || draft.DueDateDays != "on_receipt" || draft.AutoBill != "off" || draft.RemainingCycles != -1 || !recurringMarker.MatchString(draft.PrivateNotes) || len(draft.LineItems) != 1 || !billingPayload(draft) {
		return errRecurringDraft
	}
	line := draft.LineItems[0]
	if !validBillingLine(line) || line.Discount == "" || line.IsAmountDiscount {
		return errRecurringDraft
	}
	return nil
}

// RecurringRecord is strict and independent of the permissive read-only DTO.
// Get/Find preserve other histories (including multiple lines and nonzero
// modifiers). Only ValidateRecurringForDraft applies the first-cycle policy.
type RecurringRecord struct {
	ID                 string        `json:"id"`
	ClientID           string        `json:"client_id"`
	StatusID           string        `json:"status_id"`
	PrivateNotes       string        `json:"private_notes"`
	FrequencyID        string        `json:"frequency_id"`
	NextSendDate       string        `json:"next_send_date"`
	NextSendDatetime   string        `json:"next_send_datetime"`
	LastSentDate       string        `json:"last_sent_date"`
	DueDate            string        `json:"due_date"`
	DueDateDays        string        `json:"due_date_days"`
	AutoBill           string        `json:"auto_bill"`
	AutoBillEnabled    bool          `json:"auto_bill_enabled"`
	RemainingCycles    int           `json:"remaining_cycles"`
	Amount             json.Number   `json:"amount"`
	Balance            json.Number   `json:"balance"`
	UsesInclusiveTaxes bool          `json:"uses_inclusive_taxes"`
	LineItems          []InvoiceLine `json:"line_items"`
	IsDeleted          bool          `json:"is_deleted"`
	ArchivedAt         int64         `json:"archived_at"`
	Discount           json.Number   `json:"discount"`
	IsAmountDiscount   bool          `json:"is_amount_discount"`
	TaxRate1           json.Number   `json:"tax_rate1"`
	TaxRate2           json.Number   `json:"tax_rate2"`
	TaxRate3           json.Number   `json:"tax_rate3"`
	CustomSurcharge1   json.Number   `json:"custom_surcharge1"`
	CustomSurcharge2   json.Number   `json:"custom_surcharge2"`
	CustomSurcharge3   json.Number   `json:"custom_surcharge3"`
	CustomSurcharge4   json.Number   `json:"custom_surcharge4"`
	Partial            json.Number   `json:"partial"`
	PaidToDate         json.Number   `json:"paid_to_date"`
	ExchangeRate       json.Number   `json:"exchange_rate"`
}

func (record *RecurringRecord) UnmarshalJSON(raw []byte) error {
	type plain RecurringRecord
	var decoded plain
	fields, err := recurringFields(raw,
		"id", "client_id", "status_id", "private_notes", "frequency_id",
		"next_send_date", "next_send_datetime", "last_sent_date", "due_date", "due_date_days",
		"auto_bill", "auto_bill_enabled", "remaining_cycles", "amount", "balance",
		"uses_inclusive_taxes", "line_items", "is_deleted", "archived_at", "discount",
		"is_amount_discount", "tax_rate1", "tax_rate2", "tax_rate3", "custom_surcharge1",
		"custom_surcharge2", "custom_surcharge3", "custom_surcharge4", "partial", "paid_to_date", "exchange_rate")
	if err != nil {
		return err
	}
	var lines []json.RawMessage
	if json.Unmarshal(fields["line_items"], &lines) != nil || lines == nil {
		return errInvalidResponse
	}
	for _, line := range lines {
		if _, err := recurringFields(line, "product_key", "notes", "cost", "quantity", "discount", "is_amount_discount", "type_id", "tax_name1", "tax_name2", "tax_name3", "tax_rate1", "tax_rate2", "tax_rate3", "tax_id"); err != nil {
			return err
		}
	}
	if json.Unmarshal(raw, &decoded) != nil || !validRecurringRecord(RecurringRecord(decoded)) {
		return errInvalidResponse
	}
	*record = RecurringRecord(decoded)
	return nil
}

func recurringFields(raw []byte, names ...string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, errInvalidResponse
	}
	for _, name := range names {
		value := bytes.TrimSpace(fields[name])
		if len(value) == 0 || bytes.Equal(value, []byte("null")) {
			return nil, errInvalidResponse
		}
	}
	return fields, nil
}

// Financial parsing is bounded and exact, never float64. Negative values in
// foreign histories are preserved; the matching helper rejects policy conflicts.
func recurringNumbers(values ...json.Number) bool {
	for _, value := range values {
		if _, ok := billingNumber(json.Number(strings.TrimPrefix(value.String(), "-"))); !ok {
			return false
		}
	}
	return true
}

func recurringModifiers(r RecurringRecord) []json.Number {
	return []json.Number{r.Discount, r.TaxRate1, r.TaxRate2, r.TaxRate3, r.CustomSurcharge1, r.CustomSurcharge2, r.CustomSurcharge3, r.CustomSurcharge4, r.Partial, r.PaidToDate}
}

func validRecurringRecord(r RecurringRecord) bool {
	if !billingID(r.ID) || !billingID(r.ClientID) || !billingID(r.StatusID) || !billingID(r.FrequencyID) || r.ArchivedAt < 0 || r.LineItems == nil || !recurringNumbers(r.Amount, r.Balance, r.ExchangeRate) || !recurringNumbers(recurringModifiers(r)...) {
		return false
	}
	for _, date := range []string{r.NextSendDate, r.DueDate} {
		if date != "" && !billingDate(date) {
			return false
		}
	}
	// SendRecurring stores now() in a datetime column; the transformer returns it
	// unchanged. Preserve SQL UTC/Carbon timestamps and legacy date-only history.
	if r.LastSentDate != "" && !billingDate(r.LastSentDate) {
		if _, err := recurringUTC(r.LastSentDate); err != nil {
			return false
		}
	}
	if r.NextSendDatetime != "" {
		if _, err := recurringUTC(r.NextSendDatetime); err != nil {
			return false
		}
	}
	for _, line := range r.LineItems {
		if !recurringNumbers(line.Cost, line.Quantity, line.Discount, line.TaxRate1, line.TaxRate2, line.TaxRate3) {
			return false
		}
	}
	return true
}

func recurringEqual(a, b json.Number) bool {
	x, xok := billingNumber(a)
	y, yok := billingNumber(b)
	return xok && yok && x.Cmp(y) == 0
}

// ValidateRecurringForDraft compares immutable first-cycle identity/policy, not
// lifecycle status or deletion/archive state. It cannot authorize adopting an
// uncertain write. Presentation notes may be sanitized or expand reserved
// keywords, so they are deliberately not compared; all financial line fields,
// product key, tax ID, discount mode and the private marker must match exactly.
func ValidateRecurringForDraft(record RecurringRecord, draft RecurringDraft) error {
	if err := ValidateRecurringDraft(draft); err != nil {
		return err
	}
	if !validRecurringRecord(record) || record.ClientID != draft.ClientID || record.PrivateNotes != draft.PrivateNotes || record.FrequencyID != "5" || record.NextSendDate != draft.NextSendDate || record.NextSendDatetime == "" || record.LastSentDate != "" || record.DueDate != "" || record.DueDateDays != "on_receipt" || record.AutoBill != "off" || record.AutoBillEnabled || record.RemainingCycles != -1 || record.UsesInclusiveTaxes != draft.UsesInclusiveTaxes || len(record.LineItems) != 1 {
		return errInvalidResponse
	}
	amount, ok := billingNumber(record.Amount)
	// BaseRepository calls getRecurringInvoice(): both InvoiceSum variants set
	// balance to the authoritative amount, even for DRAFT (unlike invoices).
	if !ok || amount.Sign() <= 0 || !recurringEqual(record.Balance, record.Amount) || !recurringEqual(record.ExchangeRate, "1") {
		return errInvalidResponse
	}
	for _, value := range recurringModifiers(record) {
		if !recurringEqual(value, "0") {
			return errInvalidResponse
		}
	}
	got, want := record.LineItems[0], draft.LineItems[0]
	if got.IsAmountDiscount != want.IsAmountDiscount || !sameBillingLine(got, want) {
		return errInvalidResponse
	}
	return nil
}

// Carbon serializes timestamps as ISO8601 with a zone; older stored timestamps
// can be UTC SQL datetimes. A date alone or a zone-less ISO timestamp is unsafe.
func recurringUTC(value string) (time.Time, error) {
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil && parsed.Year() > 0 {
		return parsed.UTC(), nil
	}
	if parsed, err := time.ParseInLocation("2006-01-02 15:04:05", value, time.UTC); err == nil && parsed.Year() > 0 && parsed.Format("2006-01-02 15:04:05") == value {
		return parsed, nil
	}
	return time.Time{}, errInvalidResponse
}

func (c *RecurringClient) futureDate(draft RecurringDraft, now time.Time) error {
	if draft.NextSendDate <= now.In(time.Local).Format(time.DateOnly) {
		return ErrRecurringExpired
	}
	return nil
}

func (c *RecurringClient) futureFirst(record RecurringRecord, draft RecurringDraft, now time.Time) error {
	if err := c.futureDate(draft, now); err != nil {
		return err
	}
	scheduled, err := recurringUTC(record.NextSendDatetime)
	if err != nil || !scheduled.After(now) {
		return ErrRecurringExpired
	}
	return nil
}

func recurringVersion(headers http.Header) error {
	values := headers.Values("X-APP-VERSION")
	if len(values) != 1 || values[0] != SupportedRecurringVersion {
		return ErrWriteVersion
	}
	return nil
}

func (c *RecurringClient) get(ctx context.Context, path []string, query url.Values, result any) error {
	var raw json.RawMessage
	headers, err := c.reader.Get(ctx, path, query, &raw)
	if err != nil {
		return err
	}
	if err := recurringVersion(headers); err != nil {
		return err
	}
	if json.Unmarshal(raw, result) != nil {
		return errInvalidResponse
	}
	return nil
}

func (c *RecurringClient) probe(ctx context.Context) error {
	var raw json.RawMessage
	return c.get(ctx, []string{"api", "v1", "ping"}, nil, &raw)
}

// Get includes archived/deleted entities at the source pin and checks the exact
// requested ID. It is for explicit receipt/reconciliation reads, never adoption.
func (c *RecurringClient) Get(ctx context.Context, id string) (RecurringRecord, error) {
	if !billingID(id) {
		return RecurringRecord{}, errRecurringDraft
	}
	var envelope struct {
		Data *RecurringRecord `json:"data"`
	}
	if err := c.get(ctx, []string{"api", "v1", "recurring_invoices", id}, nil, &envelope); err != nil {
		return RecurringRecord{}, err
	}
	if envelope.Data == nil || envelope.Data.ID != id {
		return RecurringRecord{}, errInvalidResponse
	}
	return *envelope.Data, nil
}

// Find is a complete bounded inventory, not a server-side private_notes search.
// Foreign client IDs, duplicate IDs and changing pagination are errors, never
// evidence of absence. Stable pages still cannot supply a transactional snapshot.
func (c *RecurringClient) Find(ctx context.Context, customerID string) ([]RecurringRecord, error) {
	if !billingID(customerID) {
		return nil, errRecurringDraft
	}
	query := url.Values{
		"client_id": {customerID}, "status": {"active,archived,deleted"},
		"client_status": {"all"}, "sort": {"id|asc"}, "per_page": {strconv.Itoa(c.pageSize)},
	}
	records := make([]RecurringRecord, 0)
	seen := make(map[string]struct{})
	var first Pagination
	for page := 1; page <= c.maxPages; page++ {
		query.Set("page", strconv.Itoa(page))
		var envelope struct {
			Data []RecurringRecord `json:"data"`
			Meta *struct {
				Pagination *wirePagination `json:"pagination"`
			} `json:"meta"`
		}
		if err := c.get(ctx, []string{"api", "v1", "recurring_invoices"}, query, &envelope); err != nil {
			return nil, errors.Join(ErrLookupIncomplete, err)
		}
		if envelope.Data == nil || envelope.Meta == nil || envelope.Meta.Pagination == nil {
			return nil, ErrLookupIncomplete
		}
		p, ok := envelope.Meta.Pagination.validate(page, len(envelope.Data))
		if !ok || p.PerPage != c.pageSize || p.TotalPages > c.maxPages || p.Total > 2000 {
			return nil, ErrLookupIncomplete
		}
		if page == 1 {
			first = p
		} else if p.Total != first.Total || p.TotalPages != first.TotalPages || p.PerPage != first.PerPage {
			return nil, ErrLookupIncomplete
		}
		for _, record := range envelope.Data {
			if _, duplicate := seen[record.ID]; duplicate || record.ClientID != customerID {
				return nil, ErrLookupIncomplete
			}
			seen[record.ID] = struct{}{}
			records = append(records, record)
		}
		if page >= p.TotalPages {
			if len(records) != p.Total {
				return nil, ErrLookupIncomplete
			}
			return records, nil
		}
	}
	return nil, ErrLookupIncomplete
}

// post never retries, redirects, looks up an uncertain write, or adopts a match.
// Every error from the moment Writer.Post is invoked is conservatively unknown.
func (c *RecurringClient) post(ctx context.Context, path []string, payload, result any) error {
	var raw json.RawMessage
	headers, err := c.writer.Post(ctx, path, nil, payload, &raw)
	if err != nil {
		return errors.Join(ErrRecurringUnknown, err)
	}
	if err := recurringVersion(headers); err != nil {
		return errors.Join(ErrRecurringUnknown, err)
	}
	if json.Unmarshal(raw, result) != nil {
		return errors.Join(ErrRecurringUnknown, errInvalidResponse)
	}
	return nil
}

func (c *RecurringClient) CreateDraft(ctx context.Context, draft RecurringDraft) (RecurringRecord, error) {
	return c.createDraft(ctx, draft, nil)
}

// CreateDraftAuthorized requires final authorization after the version probe and
// before the final clock check. Rejection is unchanged and never attempts a POST.
func (c *RecurringClient) CreateDraftAuthorized(ctx context.Context, draft RecurringDraft, authorize func(context.Context) error) (RecurringRecord, error) {
	if authorize == nil {
		return RecurringRecord{}, errRecurringDraft
	}
	return c.createDraft(ctx, draft, authorize)
}

func (c *RecurringClient) createDraft(ctx context.Context, draft RecurringDraft, authorize func(context.Context) error) (RecurringRecord, error) {
	draft.LineItems = append([]InvoiceLine(nil), draft.LineItems...)
	if err := ValidateRecurringDraft(draft); err != nil {
		return RecurringRecord{}, err
	}
	if err := c.futureDate(draft, c.now()); err != nil {
		return RecurringRecord{}, err
	}
	if err := c.probe(ctx); err != nil {
		return RecurringRecord{}, err
	}
	if authorize != nil {
		if err := authorize(ctx); err != nil {
			return RecurringRecord{}, err
		}
	}
	if err := c.futureDate(draft, c.now()); err != nil {
		return RecurringRecord{}, err
	}
	if authorize != nil {
		if err := ctx.Err(); err != nil {
			return RecurringRecord{}, err
		}
	}
	var envelope struct {
		Data *RecurringRecord `json:"data"`
	}
	if err := c.post(ctx, []string{"api", "v1", "recurring_invoices"}, draft, &envelope); err != nil {
		return RecurringRecord{}, err
	}
	if envelope.Data == nil || envelope.Data.StatusID != "1" || envelope.Data.IsDeleted || envelope.Data.ArchivedAt != 0 || ValidateRecurringForDraft(*envelope.Data, draft) != nil {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, errInvalidResponse)
	}
	return *envelope.Data, nil
}

// StartFirst is only a DRAFT -> ACTIVE first start. It freshly GETs the exact
// entity and refuses already-active adoption. At the source pin start only
// changes status: it does not generate, advance or dispatch an ordinary invoice.
// The hourly cron selects ACTIVE invoices when UTC-due; even stop cannot cancel
// in-flight work. Local dates AND the actual UTC schedule must remain future.
// The checks are not CAS: callers must serialize intents and HOLD unknown POSTs.
func (c *RecurringClient) StartFirst(ctx context.Context, id string, draft RecurringDraft) (RecurringRecord, error) {
	return c.startFirst(ctx, id, draft, nil)
}

// StartFirstAuthorized requires final authorization after the exact GET and
// version probe, before the final date/UTC schedule check. It never retries.
func (c *RecurringClient) StartFirstAuthorized(ctx context.Context, id string, draft RecurringDraft, authorize func(context.Context) error) (RecurringRecord, error) {
	if authorize == nil {
		return RecurringRecord{}, errRecurringDraft
	}
	return c.startFirst(ctx, id, draft, authorize)
}

func (c *RecurringClient) startFirst(ctx context.Context, id string, draft RecurringDraft, authorize func(context.Context) error) (RecurringRecord, error) {
	draft.LineItems = append([]InvoiceLine(nil), draft.LineItems...)
	if !billingID(id) {
		return RecurringRecord{}, errRecurringDraft
	}
	if err := ValidateRecurringDraft(draft); err != nil {
		return RecurringRecord{}, err
	}
	if err := c.futureDate(draft, c.now()); err != nil {
		return RecurringRecord{}, err
	}
	before, err := c.Get(ctx, id)
	if err != nil {
		return RecurringRecord{}, err
	}
	if before.StatusID != "1" || before.IsDeleted || before.ArchivedAt != 0 || ValidateRecurringForDraft(before, draft) != nil {
		return RecurringRecord{}, errInvalidResponse
	}
	if err := c.futureFirst(before, draft, c.now()); err != nil {
		return RecurringRecord{}, err
	}
	if err := c.probe(ctx); err != nil {
		return RecurringRecord{}, err
	}
	if authorize != nil {
		if err := authorize(ctx); err != nil {
			return RecurringRecord{}, err
		}
	}
	if err := c.futureFirst(before, draft, c.now()); err != nil {
		return RecurringRecord{}, err
	}
	if authorize != nil {
		if err := ctx.Err(); err != nil {
			return RecurringRecord{}, err
		}
	}
	var envelope struct {
		Data []RecurringRecord `json:"data"`
	}
	payload := struct {
		IDs    []string `json:"ids"`
		Action string   `json:"action"`
	}{IDs: []string{id}, Action: "start"}
	if err := c.post(ctx, []string{"api", "v1", "recurring_invoices", "bulk"}, payload, &envelope); err != nil {
		return RecurringRecord{}, err
	}
	if len(envelope.Data) != 1 {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, errInvalidResponse)
	}
	after := envelope.Data[0]
	if after.ID != id || after.StatusID != "2" || after.IsDeleted || after.ArchivedAt != 0 || ValidateRecurringForDraft(after, draft) != nil || after.NextSendDatetime != before.NextSendDatetime || !recurringEqual(after.Amount, before.Amount) {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, errInvalidResponse)
	}
	if err := c.futureFirst(after, draft, c.now()); err != nil {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, err)
	}
	return after, nil
}
