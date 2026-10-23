package invoiceninja

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"time"
)

// ImportedRecurringProfile freezes the source record, including legacy notes and
// price. Its fields are private so imported authority cannot be manufactured from
// a mutable catalog or a caller-provided hash.
type ImportedRecurringProfile struct {
	baseline   RecurringRecord
	productKey string
	hash       string
}

func NewImportedRecurringProfile(record RecurringRecord, expectedProductKey string) (ImportedRecurringProfile, error) {
	if !importedProfileValid(record, expectedProductKey) {
		return ImportedRecurringProfile{}, ErrRecurringReceipt
	}
	p := ImportedRecurringProfile{baseline: cloneLifecycleRecord(record), productKey: expectedProductKey}
	p.hash = p.digest()
	return p, nil
}
func (p ImportedRecurringProfile) digest() string {
	b, _ := json.Marshal(struct {
		Baseline   RecurringRecord `json:"baseline"`
		ProductKey string          `json:"product_key"`
	}{p.baseline, p.productKey})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (p ImportedRecurringProfile) Hash() string         { return p.hash }
func (p ImportedRecurringProfile) String() string       { return "ImportedRecurringProfile[redacted]" }
func (p ImportedRecurringProfile) GoString() string     { return p.String() }
func (p ImportedRecurringProfile) LogValue() slog.Value { return slog.StringValue(p.String()) }
func (p ImportedRecurringProfile) MarshalJSON() ([]byte, error) {
	if !p.valid() {
		return nil, ErrRecurringReceipt
	}
	return json.Marshal(struct {
		Version    int             `json:"version"`
		Baseline   RecurringRecord `json:"baseline"`
		ProductKey string          `json:"product_key"`
		Hash       string          `json:"hash"`
	}{1, p.baseline, p.productKey, p.hash})
}
func (p *ImportedRecurringProfile) UnmarshalJSON(raw []byte) error {
	if !lifecycleExactObject(raw, "version", "baseline", "product_key", "hash") {
		return errInvalidResponse
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	if !importedRecordJournalJSON(fields["baseline"]) {
		return errInvalidResponse
	}
	var v struct {
		Version    int             `json:"version"`
		Baseline   RecurringRecord `json:"baseline"`
		ProductKey string          `json:"product_key"`
		Hash       string          `json:"hash"`
	}
	if json.Unmarshal(raw, &v) != nil || v.Version != 1 {
		return errInvalidResponse
	}
	q, e := NewImportedRecurringProfile(v.Baseline, v.ProductKey)
	if e != nil || q.hash != v.Hash {
		return errInvalidResponse
	}
	*p = q
	return nil
}

// Native v1 journals are stricter than provider GET DTOs: require the complete
// canonical record and line-item shapes before decoding through permissive DTOs.
func importedRecordJournalJSON(raw []byte) bool {
	if !lifecycleExactObject(raw,
		"id", "client_id", "status_id", "private_notes", "frequency_id",
		"next_send_date", "next_send_datetime", "last_sent_date", "due_date", "due_date_days",
		"auto_bill", "auto_bill_enabled", "remaining_cycles", "amount", "balance",
		"uses_inclusive_taxes", "line_items", "is_deleted", "archived_at", "discount",
		"is_amount_discount", "tax_rate1", "tax_rate2", "tax_rate3", "custom_surcharge1",
		"custom_surcharge2", "custom_surcharge3", "custom_surcharge4", "partial", "paid_to_date", "exchange_rate") {
		return false
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	var lines []json.RawMessage
	if json.Unmarshal(fields["line_items"], &lines) != nil || len(lines) != 1 {
		return false
	}
	for _, line := range lines {
		if !lifecycleExactObject(line, "discount", "is_amount_discount", "type_id", "product_key", "notes", "cost", "quantity", "tax_name1", "tax_name2", "tax_name3", "tax_rate1", "tax_rate2", "tax_rate3", "tax_id") {
			return false
		}
	}
	return true
}

func (p ImportedRecurringProfile) valid() bool {
	return p.hash != "" && p.hash == p.digest() && importedProfileValid(p.baseline, p.productKey)
}

func importedProfileValid(r RecurringRecord, productKey string) bool {
	if !validRecurringRecord(r) || (r.StatusID != "2" && r.StatusID != "3") || r.IsDeleted || r.ArchivedAt != 0 || r.ID == "" || productKey == "" || r.FrequencyID != "5" || r.DueDateDays != "on_receipt" || r.AutoBill != "off" || r.AutoBillEnabled || r.RemainingCycles != -1 || r.UsesInclusiveTaxes || !recurringEqual(r.ExchangeRate, "1") || len(r.LineItems) != 1 {
		return false
	}
	a, ok := billingNumber(r.Amount)
	if !ok || a.Sign() <= 0 || !recurringEqual(r.Balance, r.Amount) {
		return false
	}
	for _, n := range recurringModifiers(r) {
		if !recurringEqual(n, "0") {
			return false
		}
	}
	l := r.LineItems[0]
	return l.ProductKey == productKey && l.TypeID == "1" && recurringEqual(l.Quantity, "1") && recurringEqual(l.Cost, r.Amount) && recurringEqual(l.Discount, "0") && recurringEqual(l.TaxRate1, "0") && recurringEqual(l.TaxRate2, "0") && recurringEqual(l.TaxRate3, "0")
}

type ImportedPauseDraft struct {
	profile ImportedRecurringProfile
	before  RecurringRecord
}
type ImportedScheduleDraft struct {
	profile ImportedRecurringProfile
	before  RecurringRecord
	date    string
}
type ImportedResumeDraft struct {
	profile ImportedRecurringProfile
	before  RecurringRecord
	receipt RecurringReceipt
}

func (d ImportedPauseDraft) ProfileHash() string    { return d.profile.Hash() }
func (d ImportedScheduleDraft) ProfileHash() string { return d.profile.Hash() }
func (d ImportedResumeDraft) ProfileHash() string   { return d.profile.Hash() }

func (d ImportedPauseDraft) MarshalJSON() ([]byte, error) {
	if !d.profile.valid() || !importedSame(d.profile, d.before, "2") {
		return nil, ErrRecurringReceipt
	}
	return json.Marshal(struct {
		Version int                      `json:"version"`
		Profile ImportedRecurringProfile `json:"profile"`
		Before  RecurringRecord          `json:"before"`
	}{1, d.profile, d.before})
}
func (d *ImportedPauseDraft) UnmarshalJSON(raw []byte) error {
	var v struct {
		Version int                      `json:"version"`
		Profile ImportedRecurringProfile `json:"profile"`
		Before  RecurringRecord          `json:"before"`
	}
	if !lifecycleExactObject(raw, "version", "profile", "before") {
		return errInvalidResponse
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	if !importedRecordJournalJSON(fields["before"]) || json.Unmarshal(raw, &v) != nil || v.Version != 1 {
		return errInvalidResponse
	}
	x, e := PrepareImportedPause(v.Profile, v.Before)
	if e != nil {
		return errInvalidResponse
	}
	*d = x
	return nil
}
func (d ImportedScheduleDraft) MarshalJSON() ([]byte, error) {
	if !d.profile.valid() || !importedSame(d.profile, d.before, "3") || !billingDate(d.date) {
		return nil, ErrRecurringReceipt
	}
	return json.Marshal(struct {
		Version int                      `json:"version"`
		Profile ImportedRecurringProfile `json:"profile"`
		Before  RecurringRecord          `json:"before"`
		Date    string                   `json:"date"`
	}{1, d.profile, d.before, d.date})
}
func (d *ImportedScheduleDraft) UnmarshalJSON(raw []byte) error {
	if !lifecycleExactObject(raw, "version", "profile", "before", "date") {
		return errInvalidResponse
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	if !importedRecordJournalJSON(fields["before"]) {
		return errInvalidResponse
	}
	var v struct {
		Version int                      `json:"version"`
		Profile ImportedRecurringProfile `json:"profile"`
		Before  RecurringRecord          `json:"before"`
		Date    string                   `json:"date"`
	}
	if json.Unmarshal(raw, &v) != nil || v.Version != 1 || !v.Profile.valid() || !importedSame(v.Profile, v.Before, "3") || !billingDate(v.Date) {
		return errInvalidResponse
	}
	*d = ImportedScheduleDraft{profile: v.Profile, before: cloneLifecycleRecord(v.Before), date: v.Date}
	return nil
}
func (d ImportedResumeDraft) MarshalJSON() ([]byte, error) {
	if !d.profile.valid() || !importedSame(d.profile, d.before, "3") || !recurringReceiptMatches(d.before, d.receipt) {
		return nil, ErrRecurringReceipt
	}
	return json.Marshal(struct {
		Version int                      `json:"version"`
		Profile ImportedRecurringProfile `json:"profile"`
		Before  RecurringRecord          `json:"before"`
		Receipt RecurringReceipt         `json:"receipt"`
	}{1, d.profile, d.before, d.receipt})
}
func (d *ImportedResumeDraft) UnmarshalJSON(raw []byte) error {
	if !lifecycleExactObject(raw, "version", "profile", "before", "receipt") {
		return errInvalidResponse
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	if !importedRecordJournalJSON(fields["before"]) || !lifecycleExactObject(fields["receipt"], "Amount", "NextSendDatetime") {
		return errInvalidResponse
	}
	var v struct {
		Version int                      `json:"version"`
		Profile ImportedRecurringProfile `json:"profile"`
		Before  RecurringRecord          `json:"before"`
		Receipt RecurringReceipt         `json:"receipt"`
	}
	if json.Unmarshal(raw, &v) != nil || v.Version != 1 || !v.Profile.valid() || !importedSame(v.Profile, v.Before, "3") || !recurringReceiptMatches(v.Before, v.Receipt) {
		return errInvalidResponse
	}
	*d = ImportedResumeDraft{profile: v.Profile, before: cloneLifecycleRecord(v.Before), receipt: v.Receipt}
	return nil
}

func PrepareImportedPause(p ImportedRecurringProfile, r RecurringRecord) (ImportedPauseDraft, error) {
	if !p.valid() || !importedSame(p, r, "2") {
		return ImportedPauseDraft{}, ErrRecurringReceipt
	}
	return ImportedPauseDraft{p, cloneLifecycleRecord(r)}, nil
}
func PrepareImportedSchedule(p ImportedRecurringProfile, r RecurringRecord, date, today string) (ImportedScheduleDraft, error) {
	if !p.valid() || !importedSame(p, r, "3") || !billingDate(date) || !billingDate(today) || date <= today {
		return ImportedScheduleDraft{}, ErrRecurringReceipt
	}
	return ImportedScheduleDraft{p, cloneLifecycleRecord(r), date}, nil
}
func PrepareImportedResume(p ImportedRecurringProfile, r RecurringRecord, receipt RecurringReceipt) (ImportedResumeDraft, error) {
	if !p.valid() || !importedSame(p, r, "3") || !recurringReceiptMatches(r, receipt) {
		return ImportedResumeDraft{}, ErrRecurringReceipt
	}
	if !importedResumeFuture(r, time.Now()) {
		return ImportedResumeDraft{}, ErrRecurringExpired
	}
	return ImportedResumeDraft{p, cloneLifecycleRecord(r), receipt}, nil
}
func importedResumeFuture(r RecurringRecord, now time.Time) bool {
	if !billingDate(r.NextSendDate) || r.NextSendDate <= now.In(time.Local).Format(time.DateOnly) {
		return false
	}
	when, e := recurringUTC(r.NextSendDatetime)
	return e == nil && when.After(now)
}
func importedSame(p ImportedRecurringProfile, a RecurringRecord, status string) bool {
	b := p.baseline
	if !p.valid() || !validRecurringRecord(a) || !importedProfileValid(a, p.productKey) || a.ID != b.ID || a.ClientID != b.ClientID || a.StatusID != status || a.PrivateNotes != b.PrivateNotes || a.FrequencyID != b.FrequencyID || a.DueDateDays != b.DueDateDays || a.AutoBill != b.AutoBill || a.AutoBillEnabled != b.AutoBillEnabled || a.RemainingCycles != b.RemainingCycles || a.UsesInclusiveTaxes != b.UsesInclusiveTaxes || a.IsAmountDiscount != b.IsAmountDiscount || a.LineItems[0].IsAmountDiscount != b.LineItems[0].IsAmountDiscount || a.IsDeleted || a.ArchivedAt != 0 || !recurringEqual(a.Amount, b.Amount) || !recurringEqual(a.Balance, b.Balance) || !recurringEqual(a.ExchangeRate, b.ExchangeRate) || !recurringEqual(a.PaidToDate, b.PaidToDate) || !recurringEqual(a.Partial, b.Partial) || !recurringEqual(a.Discount, b.Discount) || !recurringEqual(a.TaxRate1, b.TaxRate1) || !recurringEqual(a.TaxRate2, b.TaxRate2) || !recurringEqual(a.TaxRate3, b.TaxRate3) || !recurringEqual(a.CustomSurcharge1, b.CustomSurcharge1) || !recurringEqual(a.CustomSurcharge2, b.CustomSurcharge2) || !recurringEqual(a.CustomSurcharge3, b.CustomSurcharge3) || !recurringEqual(a.CustomSurcharge4, b.CustomSurcharge4) || len(a.LineItems) != 1 || len(b.LineItems) != 1 || !sameBillingLine(a.LineItems[0], b.LineItems[0]) || a.LineItems[0].Notes != b.LineItems[0].Notes || a.LineItems[0].ProductKey != p.productKey {
		return false
	}
	return true
}

// ImportedRecurringClient intentionally exposes no create/start capability.
type ImportedRecurringClient struct{ base *RecurringClient }

func (*ImportedRecurringClient) String() string   { return "ImportedRecurringClient[redacted]" }
func (*ImportedRecurringClient) GoString() string { return "ImportedRecurringClient[redacted]" }
func (*ImportedRecurringClient) LogValue() slog.Value {
	return slog.StringValue("ImportedRecurringClient[redacted]")
}

func NewImportedRecurringClient(base, token string, timeout time.Duration, pageSize, maxPages int) (*ImportedRecurringClient, error) {
	c, e := NewRecurringClient(base, token, timeout, pageSize, maxPages)
	if e != nil {
		return nil, e
	}
	return &ImportedRecurringClient{c}, nil
}
func (c *ImportedRecurringClient) Get(ctx context.Context, id string) (RecurringRecord, error) {
	if c == nil || c.base == nil {
		return RecurringRecord{}, errRecurringDraft
	}
	return c.base.Get(ctx, id)
}
func (c *ImportedRecurringClient) PauseAuthorized(ctx context.Context, d ImportedPauseDraft, authorize func(context.Context) error) (RecurringRecord, error) {
	b := d.before
	if authorize == nil || !d.profile.valid() || !importedSame(d.profile, b, "2") {
		return RecurringRecord{}, errRecurringDraft
	}
	if e := c.preflight(ctx, b, d.profile); e != nil {
		return RecurringRecord{}, e
	}
	if e := authorize(ctx); e != nil {
		return RecurringRecord{}, e
	}
	if e := ctx.Err(); e != nil {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, e)
	}
	x, e := c.base.lifecyclePost(ctx, b.ID, "stop")
	if e != nil {
		return RecurringRecord{}, e
	}
	if x[0].StatusID != "3" || !importedSame(d.profile, x[0], "3") || !importedTransitionSame(b, x[0], "3", "", false) {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, errInvalidResponse)
	}
	return c.confirm(ctx, d.profile, b, "3", b.NextSendDate)
}
func (c *ImportedRecurringClient) preflight(ctx context.Context, b RecurringRecord, p ImportedRecurringProfile) error {
	if e := c.base.probe(ctx); e != nil {
		return e
	}
	f, e := c.Get(ctx, b.ID)
	if e != nil {
		return e
	}
	if f.StatusID != b.StatusID || !importedSame(p, f, b.StatusID) || !importedFullSame(b, f) {
		return ErrRecurringReceipt
	}
	return nil
}
func importedFullSame(a, b RecurringRecord) bool {
	a.StatusID = b.StatusID
	return lifecycleSameImportedFields(a, b)
}
func lifecycleSameImportedFields(a, b RecurringRecord) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
func importedTransitionSame(before, after RecurringRecord, status, date string, rescheduled bool) bool {
	before.StatusID = status
	if rescheduled {
		before.NextSendDate = date
		before.NextSendDatetime = after.NextSendDatetime
	}
	return lifecycleSameImportedFields(before, after)
}
func (c *ImportedRecurringClient) confirm(ctx context.Context, p ImportedRecurringProfile, before RecurringRecord, status, date string) (RecurringRecord, error) {
	a, e := c.Get(ctx, before.ID)
	if e != nil {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, e)
	}
	if a.StatusID != status || a.NextSendDate != date || !importedSame(p, a, status) || !importedTransitionSame(before, a, status, "", false) {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, errInvalidResponse)
	}
	return a, nil
}
func (c *ImportedRecurringClient) RescheduleAuthorized(ctx context.Context, d ImportedScheduleDraft, authorize func(context.Context) error) (RecurringRecord, error) {
	b := d.before
	if authorize == nil || !d.profile.valid() || !importedSame(d.profile, b, "3") || !billingDate(d.date) {
		return RecurringRecord{}, errRecurringDraft
	}
	now := c.base.now()
	if d.date <= now.In(time.Local).Format(time.DateOnly) {
		return RecurringRecord{}, ErrRecurringExpired
	}
	if e := c.preflight(ctx, b, d.profile); e != nil {
		return RecurringRecord{}, e
	}
	if e := authorize(ctx); e != nil {
		return RecurringRecord{}, e
	}
	if e := ctx.Err(); e != nil {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, e)
	}
	if d.date <= c.base.now().In(time.Local).Format(time.DateOnly) {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, ErrRecurringExpired)
	}
	payload := struct {
		Next     string      `json:"next_send_date"`
		Exchange json.Number `json:"exchange_rate"`
	}{d.date, b.ExchangeRate}
	var raw json.RawMessage
	h, e := c.base.writer.Put(ctx, []string{"api", "v1", "recurring_invoices", b.ID}, nil, payload, &raw)
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
	if post.StatusID != "3" || after.StatusID != "3" || post.NextSendDate != d.date || after.NextSendDate != d.date || !importedSame(d.profile, post, "3") || !importedSame(d.profile, after, "3") || !importedTransitionSame(b, post, "3", d.date, true) || !importedTransitionSame(b, after, "3", d.date, true) || !recurringReceiptMatches(after, RecurringReceipt{Amount: post.Amount, NextSendDatetime: post.NextSendDatetime}) {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, errInvalidResponse)
	}
	when, e := recurringUTC(after.NextSendDatetime)
	if e != nil || !when.After(c.base.now()) || d.date <= c.base.now().In(time.Local).Format(time.DateOnly) {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, ErrRecurringExpired)
	}
	return after, nil
}
func (c *ImportedRecurringClient) ResumeAuthorized(ctx context.Context, d ImportedResumeDraft, authorize func(context.Context) error) (RecurringRecord, error) {
	b := d.before
	if authorize == nil || !d.profile.valid() || !importedSame(d.profile, b, "3") || !recurringReceiptMatches(b, d.receipt) {
		return RecurringRecord{}, ErrRecurringReceipt
	}
	if !importedResumeFuture(b, c.base.now()) {
		return RecurringRecord{}, ErrRecurringExpired
	}
	e := c.preflight(ctx, b, d.profile)
	if e != nil {
		return RecurringRecord{}, e
	}
	if e = authorize(ctx); e != nil {
		return RecurringRecord{}, e
	}
	if e = ctx.Err(); e != nil {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, e)
	}
	if !importedResumeFuture(b, c.base.now()) {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, ErrRecurringExpired)
	}
	records, e := c.base.lifecyclePost(ctx, b.ID, "start")
	if e != nil {
		return RecurringRecord{}, e
	}
	post := records[0]
	if post.StatusID != "2" || !importedSame(d.profile, post, "2") || !importedTransitionSame(b, post, "2", "", false) || !recurringReceiptMatches(post, d.receipt) || !importedResumeFuture(post, c.base.now()) {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, errInvalidResponse)
	}
	after, e := c.Get(ctx, b.ID)
	if e != nil {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, e)
	}
	if after.StatusID != "2" || !importedSame(d.profile, after, "2") || !importedTransitionSame(b, after, "2", "", false) || !recurringReceiptMatches(after, d.receipt) {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, errInvalidResponse)
	}
	if !importedResumeFuture(after, c.base.now()) {
		return RecurringRecord{}, errors.Join(ErrRecurringUnknown, ErrRecurringExpired)
	}
	return after, nil
}
func (ImportedPauseDraft) String() string   { return "ImportedPauseDraft[redacted]" }
func (ImportedPauseDraft) GoString() string { return "ImportedPauseDraft[redacted]" }
func (ImportedPauseDraft) LogValue() slog.Value {
	return slog.StringValue("ImportedPauseDraft[redacted]")
}
func (ImportedScheduleDraft) String() string   { return "ImportedScheduleDraft[redacted]" }
func (ImportedScheduleDraft) GoString() string { return "ImportedScheduleDraft[redacted]" }
func (ImportedScheduleDraft) LogValue() slog.Value {
	return slog.StringValue("ImportedScheduleDraft[redacted]")
}
func (ImportedResumeDraft) String() string   { return "ImportedResumeDraft[redacted]" }
func (ImportedResumeDraft) GoString() string { return "ImportedResumeDraft[redacted]" }
func (ImportedResumeDraft) LogValue() slog.Value {
	return slog.StringValue("ImportedResumeDraft[redacted]")
}
