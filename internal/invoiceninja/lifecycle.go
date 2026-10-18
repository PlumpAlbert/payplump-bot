package invoiceninja

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"
)

// Lifecycle drafts are durable, sensitive before-images. Their JSON is intentionally
// complete for journals; human-oriented formatting never reveals it.
type PauseDraft struct {
	before   RecurringRecord
	template RecurringDraft
}
type ScheduleDraft struct {
	before      RecurringRecord
	template    RecurringDraft
	desiredDate string
}
type ResumeDraft struct {
	before   RecurringRecord
	template RecurringDraft
	receipt  RecurringReceipt
}

func lifecycleExactObject(raw []byte, names ...string) bool {
	if !uniqueNinjaJSON(raw) {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil || len(fields) != len(names) {
		return false
	}
	for _, name := range names {
		if _, ok := fields[name]; !ok {
			return false
		}
	}
	return true
}

func lifecycleTemplateJSON(raw []byte) bool {
	if !lifecycleExactObject(raw, "client_id", "frequency_id", "next_send_date", "due_date_days", "auto_bill", "remaining_cycles", "private_notes", "uses_inclusive_taxes", "line_items") {
		return false
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	var lines []json.RawMessage
	if json.Unmarshal(fields["line_items"], &lines) != nil || len(lines) != 1 {
		return false
	}
	return lifecycleExactObject(lines[0], "product_key", "notes", "cost", "quantity", "discount", "is_amount_discount", "type_id", "tax_name1", "tax_name2", "tax_name3", "tax_rate1", "tax_rate2", "tax_rate3", "tax_id")
}
func lifecycleLastSentEqual(a, b string) bool {
	if a == b {
		return true
	}
	if billingDate(a) || billingDate(b) {
		return false
	}
	x, e := recurringUTC(a)
	if e != nil {
		return false
	}
	y, e := recurringUTC(b)
	return e == nil && x.Equal(y)
}

func (d PauseDraft) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Before   RecurringRecord `json:"before"`
		Template RecurringDraft  `json:"template"`
	}{d.before, d.template})
}
func (d *ScheduleDraft) UnmarshalJSON(b []byte) error {
	if !lifecycleExactObject(b, "before", "template", "desired_date") {
		return errInvalidResponse
	}
	var nested map[string]json.RawMessage
	_ = json.Unmarshal(b, &nested)
	if !lifecycleTemplateJSON(nested["template"]) {
		return errInvalidResponse
	}
	var v struct {
		Before   RecurringRecord `json:"before"`
		Template RecurringDraft  `json:"template"`
		Desired  string          `json:"desired_date"`
	}
	if _, e := recurringFields(b, "before", "template", "desired_date"); e != nil {
		return e
	}
	if e := json.Unmarshal(b, &v); e != nil {
		return e
	}
	d.before, d.template, d.desiredDate = v.Before, v.Template, v.Desired
	return nil
}
func (d ScheduleDraft) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Before   RecurringRecord `json:"before"`
		Template RecurringDraft  `json:"template"`
		Desired  string          `json:"desired_date"`
	}{d.before, d.template, d.desiredDate})
}
func (d *PauseDraft) UnmarshalJSON(b []byte) error {
	if !lifecycleExactObject(b, "before", "template") {
		return errInvalidResponse
	}
	var nested map[string]json.RawMessage
	_ = json.Unmarshal(b, &nested)
	if !lifecycleTemplateJSON(nested["template"]) {
		return errInvalidResponse
	}
	var v struct {
		Before   RecurringRecord `json:"before"`
		Template RecurringDraft  `json:"template"`
	}
	if _, e := recurringFields(b, "before", "template"); e != nil {
		return e
	}
	if e := json.Unmarshal(b, &v); e != nil {
		return e
	}
	d.before, d.template = v.Before, v.Template
	return nil
}
func (d ResumeDraft) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Before   RecurringRecord  `json:"before"`
		Template RecurringDraft   `json:"template"`
		Receipt  RecurringReceipt `json:"receipt"`
	}{d.before, d.template, d.receipt})
}
func (d *ResumeDraft) UnmarshalJSON(b []byte) error {
	if !lifecycleExactObject(b, "before", "template", "receipt") {
		return errInvalidResponse
	}
	var nested map[string]json.RawMessage
	_ = json.Unmarshal(b, &nested)
	if !lifecycleTemplateJSON(nested["template"]) || !lifecycleExactObject(nested["receipt"], "Amount", "NextSendDatetime") {
		return errInvalidResponse
	}
	var v struct {
		Before   RecurringRecord  `json:"before"`
		Template RecurringDraft   `json:"template"`
		Receipt  RecurringReceipt `json:"receipt"`
	}
	if _, e := recurringFields(b, "before", "template", "receipt"); e != nil {
		return e
	}
	if e := json.Unmarshal(b, &v); e != nil {
		return e
	}
	d.before, d.template, d.receipt = v.Before, v.Template, v.Receipt
	return nil
}
func (PauseDraft) String() string          { return "PauseDraft[redacted]" }
func (PauseDraft) GoString() string        { return "PauseDraft[redacted]" }
func (PauseDraft) LogValue() slog.Value    { return slog.StringValue("PauseDraft[redacted]") }
func (ScheduleDraft) String() string       { return "ScheduleDraft[redacted]" }
func (ScheduleDraft) GoString() string     { return "ScheduleDraft[redacted]" }
func (ScheduleDraft) LogValue() slog.Value { return slog.StringValue("ScheduleDraft[redacted]") }
func (ResumeDraft) String() string         { return "ResumeDraft[redacted]" }
func (ResumeDraft) GoString() string       { return "ResumeDraft[redacted]" }
func (ResumeDraft) LogValue() slog.Value   { return slog.StringValue("ResumeDraft[redacted]") }

func lifecycleTemplate(r RecurringRecord, d RecurringDraft) error {
	if d.ClientID != r.ClientID || d.PrivateNotes != r.PrivateNotes || d.NextSendDate == "" {
		return ErrRecurringReceipt
	}
	d.NextSendDate = r.NextSendDate
	// Lifecycle records may have been sent in prior cycles; the activation-only
	// validator requires an empty first-cycle LastSentDate.
	validationRecord := r
	validationRecord.LastSentDate = ""
	if err := ValidateRecurringForDraft(validationRecord, d); err != nil {
		return ErrRecurringReceipt
	}
	if !recurringEqual(r.CustomSurcharge1, "0") || !recurringEqual(r.CustomSurcharge2, "0") || !recurringEqual(r.CustomSurcharge3, "0") || !recurringEqual(r.CustomSurcharge4, "0") {
		return ErrRecurringReceipt
	}
	return nil
}
func lifecycleBase(r RecurringRecord, d RecurringDraft, status string) error {
	if !validRecurringRecord(r) || r.StatusID != status || r.IsDeleted || r.ArchivedAt != 0 || r.ID == "" || lifecycleTemplate(r, d) != nil {
		return ErrRecurringReceipt
	}
	return nil
}
func PreparePause(r RecurringRecord, d RecurringDraft) (PauseDraft, error) {
	if e := lifecycleBase(r, d, "2"); e != nil {
		return PauseDraft{}, e
	}
	return PauseDraft{before: cloneLifecycleRecord(r), template: cloneRecurringDraft(d)}, nil
}
func PrepareSchedule(r RecurringRecord, d RecurringDraft, date, today string) (ScheduleDraft, error) {
	if e := lifecycleBase(r, d, "3"); e != nil {
		return ScheduleDraft{}, e
	}
	if !billingDate(date) || !billingDate(today) || date <= today {
		return ScheduleDraft{}, ErrRecurringExpired
	}
	return ScheduleDraft{before: cloneLifecycleRecord(r), template: cloneRecurringDraft(d), desiredDate: date}, nil
}
func PrepareResume(r RecurringRecord, d RecurringDraft, receipt RecurringReceipt, today string) (ResumeDraft, error) {
	if e := lifecycleBase(r, d, "3"); e != nil {
		return ResumeDraft{}, e
	}
	if !billingDate(today) || r.NextSendDate <= today || !recurringReceiptMatches(r, receipt) {
		return ResumeDraft{}, ErrRecurringReceipt
	}
	when, e := recurringUTC(r.NextSendDatetime)
	if e != nil || !when.After(time.Now()) {
		return ResumeDraft{}, ErrRecurringExpired
	}
	return ResumeDraft{before: cloneLifecycleRecord(r), template: cloneRecurringDraft(d), receipt: receipt}, nil
}
func cloneRecurringDraft(d RecurringDraft) RecurringDraft {
	d.LineItems = append([]InvoiceLine(nil), d.LineItems...)
	return d
}
func cloneLifecycleRecord(r RecurringRecord) RecurringRecord {
	r.LineItems = append([]InvoiceLine(nil), r.LineItems...)
	return r
}
func lifecycleSame(a, b RecurringRecord, d RecurringDraft) bool {
	if a.ID != b.ID || a.ClientID != b.ClientID || a.PrivateNotes != b.PrivateNotes || a.NextSendDate != b.NextSendDate || !lifecycleLastSentEqual(a.LastSentDate, b.LastSentDate) || a.FrequencyID != b.FrequencyID || a.DueDate != b.DueDate || a.DueDateDays != b.DueDateDays || a.IsDeleted != b.IsDeleted || a.ArchivedAt != b.ArchivedAt || a.IsAmountDiscount != b.IsAmountDiscount || a.AutoBill != b.AutoBill || a.AutoBillEnabled != b.AutoBillEnabled || a.RemainingCycles != b.RemainingCycles || a.UsesInclusiveTaxes != b.UsesInclusiveTaxes || !recurringEqual(a.Amount, b.Amount) || !recurringEqual(a.Balance, b.Balance) || !recurringEqual(a.ExchangeRate, b.ExchangeRate) || !recurringReceiptMatches(a, RecurringReceipt{Amount: b.Amount, NextSendDatetime: b.NextSendDatetime}) || !recurringEqual(a.Discount, b.Discount) || !recurringEqual(a.TaxRate1, b.TaxRate1) || !recurringEqual(a.TaxRate2, b.TaxRate2) || !recurringEqual(a.TaxRate3, b.TaxRate3) || !recurringEqual(a.CustomSurcharge1, b.CustomSurcharge1) || !recurringEqual(a.CustomSurcharge2, b.CustomSurcharge2) || !recurringEqual(a.CustomSurcharge3, b.CustomSurcharge3) || !recurringEqual(a.CustomSurcharge4, b.CustomSurcharge4) || !recurringEqual(a.Partial, b.Partial) || !recurringEqual(a.PaidToDate, b.PaidToDate) || len(a.LineItems) != len(b.LineItems) {
		return false
	}
	for i := range a.LineItems {
		if a.LineItems[i].IsAmountDiscount != b.LineItems[i].IsAmountDiscount || !sameBillingLine(a.LineItems[i], b.LineItems[i]) {
			return false
		}
	}
	return lifecycleTemplate(a, d) == nil
}
func (c *RecurringClient) lifecyclePreflight(ctx context.Context, b RecurringRecord, d RecurringDraft) error {
	if e := c.probe(ctx); e != nil {
		return e
	}
	fresh, e := c.Get(ctx, b.ID)
	if e != nil {
		return e
	}
	if fresh.StatusID != b.StatusID || !lifecycleSame(b, fresh, d) {
		return ErrRecurringReceipt
	}
	return nil
}
func (c *RecurringClient) lifecyclePost(ctx context.Context, id, action string) ([]RecurringRecord, error) {
	var out struct {
		Data []RecurringRecord `json:"data"`
	}
	p := struct {
		IDs    []string `json:"ids"`
		Action string   `json:"action"`
	}{[]string{id}, action}
	if e := c.post(ctx, []string{"api", "v1", "recurring_invoices", "bulk"}, p, &out); e != nil {
		return nil, e
	}
	if len(out.Data) != 1 || out.Data[0].ID != id {
		return nil, errors.Join(ErrRecurringUnknown, errInvalidResponse)
	}
	return out.Data, nil
}
func (c *RecurringClient) PauseAuthorized(ctx context.Context, d PauseDraft, authorize func(context.Context) error) (RecurringRecord, error) {
	b := d.before
	if authorize == nil || lifecycleBase(b, d.template, "2") != nil {
		return RecurringRecord{}, errRecurringDraft
	}
	if e := c.lifecyclePreflight(ctx, b, d.template); e != nil {
		return RecurringRecord{}, e
	}
	if e := authorize(ctx); e != nil {
		return RecurringRecord{}, e
	}
	if e := ctx.Err(); e != nil {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, e)
	}
	receipts, e := c.lifecyclePost(ctx, b.ID, "stop")
	if e != nil {
		return RecurringRecord{}, e
	}
	if receipts[0].StatusID != "3" || !lifecycleSame(b, receipts[0], d.template) {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, errInvalidResponse)
	}
	after, e := c.Get(ctx, b.ID)
	if e != nil {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, e)
	}
	if after.StatusID != "3" || !lifecycleSame(b, after, d.template) {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, errInvalidResponse)
	}
	return after, nil
}
func (c *RecurringClient) RescheduleAuthorized(ctx context.Context, d ScheduleDraft, authorize func(context.Context) error) (RecurringRecord, error) {
	b := d.before
	if authorize == nil || lifecycleBase(b, d.template, "3") != nil || !billingDate(d.desiredDate) {
		return RecurringRecord{}, errRecurringDraft
	}
	if d.desiredDate <= c.now().In(time.Local).Format(time.DateOnly) {
		return RecurringRecord{}, ErrRecurringExpired
	}
	if e := c.lifecyclePreflight(ctx, b, d.template); e != nil {
		return RecurringRecord{}, e
	}
	if e := authorize(ctx); e != nil {
		return RecurringRecord{}, e
	}
	if e := ctx.Err(); e != nil {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, e)
	}
	if d.desiredDate <= c.now().In(time.Local).Format(time.DateOnly) {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, ErrRecurringExpired)
	}
	payload := struct {
		Next     string      `json:"next_send_date"`
		Exchange json.Number `json:"exchange_rate"`
	}{d.desiredDate, b.ExchangeRate}
	var raw json.RawMessage
	h, e := c.writer.Put(ctx, []string{"api", "v1", "recurring_invoices", b.ID}, nil, payload, &raw)
	if e != nil {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, e)
	}
	if recurringVersion(h) != nil || !uniqueNinjaJSON(raw) {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, errInvalidResponse)
	}
	var env struct {
		Data *RecurringRecord `json:"data"`
	}
	if json.Unmarshal(raw, &env) != nil || env.Data == nil {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, errInvalidResponse)
	}
	post := *env.Data
	after, e := c.Get(ctx, b.ID)
	if e != nil {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, e)
	}
	if post.NextSendDate != d.desiredDate || post.StatusID != "3" || !lifecycleSameSchedule(b, post, d.template, d.desiredDate) || after.NextSendDate != d.desiredDate || after.StatusID != "3" || !lifecycleSameSchedule(b, after, d.template, d.desiredDate) || !recurringReceiptMatches(after, RecurringReceipt{Amount: post.Amount, NextSendDatetime: post.NextSendDatetime}) {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, errInvalidResponse)
	}
	when, e := recurringUTC(after.NextSendDatetime)
	if e != nil || !when.After(c.now()) {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, ErrRecurringExpired)
	}
	return after, nil
}
func lifecycleSameSchedule(b, a RecurringRecord, d RecurringDraft, date string) bool {
	if a.NextSendDate != date {
		return false
	} // retain the original receipt's other facts while allowing a new UTC schedule
	oldDate, old := b.NextSendDate, b.NextSendDatetime
	b.NextSendDate = date
	b.NextSendDatetime = a.NextSendDatetime
	ok := lifecycleSame(b, a, d)
	b.NextSendDate, b.NextSendDatetime = oldDate, old
	return ok
}
func (c *RecurringClient) ResumeAuthorized(ctx context.Context, d ResumeDraft, authorize func(context.Context) error) (RecurringRecord, error) {
	b := d.before
	if authorize == nil || lifecycleBase(b, d.template, "3") != nil || !recurringReceiptMatches(b, d.receipt) || b.NextSendDate <= c.now().In(time.Local).Format(time.DateOnly) {
		return RecurringRecord{}, ErrRecurringReceipt
	}
	when, e := recurringUTC(b.NextSendDatetime)
	if e != nil || !when.After(c.now()) {
		return RecurringRecord{}, ErrRecurringExpired
	}
	if e = c.lifecyclePreflight(ctx, b, d.template); e != nil {
		return RecurringRecord{}, e
	}
	if e = authorize(ctx); e != nil {
		return RecurringRecord{}, e
	}
	if e = ctx.Err(); e != nil {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, e)
	}
	if b.NextSendDate <= c.now().In(time.Local).Format(time.DateOnly) {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, ErrRecurringExpired)
	}
	when, e = recurringUTC(b.NextSendDatetime)
	if e != nil || !when.After(c.now()) {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, ErrRecurringExpired)
	}
	if e = ctx.Err(); e != nil {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, e)
	}
	records, e := c.lifecyclePost(ctx, b.ID, "start")
	if e != nil {
		return RecurringRecord{}, e
	}
	post := records[0]
	after, e := c.Get(ctx, b.ID)
	if e != nil {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, e)
	}
	if post.StatusID != "2" || after.StatusID != "2" || !lifecycleSame(b, post, d.template) || !lifecycleSame(b, after, d.template) || !recurringReceiptMatches(post, d.receipt) || !recurringReceiptMatches(after, d.receipt) {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, errInvalidResponse)
	}
	return after, nil
}
