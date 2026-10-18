package activation

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/PlumpAlbert/payplump-bot/internal/billing"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{20,128}$`)

// NormalizeRecurringFacts records exact provider totals and the UTC instant.
// It does not recalculate taxes or authorize an expired schedule.
func NormalizeRecurringFacts(amount json.Number, datetime string) (string, string, error) {
	value, err := billing.FormatAmount(amount)
	if err != nil || value == "0" {
		return "", "", ErrReviewRequired
	}
	instant, err := time.Parse(time.RFC3339Nano, datetime)
	if err != nil {
		instant, err = time.ParseInLocation("2006-01-02 15:04:05", datetime, time.UTC)
		if err != nil || instant.Format("2006-01-02 15:04:05") != datetime {
			return "", "", ErrReviewRequired
		}
	}
	if instant.Year() < 1 || instant.Year() > 9999 {
		return "", "", ErrReviewRequired
	}
	return value, instant.UTC().Format(time.RFC3339Nano), nil
}

func Marker(token string) string { return "payplump:" + token + ":recurring_create" }
func ValidDate(value string) bool {
	t, e := time.Parse(time.DateOnly, value)
	return e == nil && t.Year() > 0 && t.Format(time.DateOnly) == value
}
func ValidID(value string) bool {
	return value != "" && len(value) <= 256 && value != "." && value != ".." && utf8.ValidString(value) && strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0 && !strings.ContainsAny(value, "/\\")
}
func ValidSUIID(value string) bool {
	n, e := strconv.ParseInt(value, 10, 64)
	return e == nil && n > 0 && strconv.FormatInt(n, 10) == value
}

// ValidateSnapshot is structural. Review flags remain visible to callers and
// must separately deny authorization; a reviewed proof is still an audit fact.
func ValidateSnapshot(s Snapshot) error {
	a, p := s.Account, s.Proof
	if a.ClientID <= 0 || a.TelegramID <= 0 || a.ChatID != a.TelegramID || a.BotID <= 0 || !ValidID(a.CustomerID) || !ValidID(a.InvoiceID) || a.InvoiceID != a.FirstInvoiceID || a.SUIID != "" || a.RecurringID != "" || a.State != "awaiting_payment" || a.DesiredAccess || !tokenPattern.MatchString(s.Token) || s.PolicyHash == ([32]byte{}) || p.Validate() != nil || p.InvoiceID != a.InvoiceID || p.CustomerID != a.CustomerID || p.PaymentDate != a.PaymentDate || p.NextInvoiceDate != a.NextInvoiceDate {
		return ErrReviewRequired
	}
	for step, intent := range s.Steps {
		if !step.Valid() || len(intent.Payload) == 0 || intent.DispatchAttempt <= 0 || (intent.ExternalID != "" && !ValidID(intent.ExternalID)) {
			return ErrReviewRequired
		}
		if (step == RecurringCreate || step == RecurringStart) && intent.ExternalID != "" {
			if _, _, err := NormalizeRecurringFacts(json.Number(intent.RecurringAmount), intent.RecurringDatetime); err != nil {
				return ErrReviewRequired
			}
		} else if intent.RecurringAmount != "" || intent.RecurringDatetime != "" {
			return ErrReviewRequired
		}
	}
	return nil
}

// NextStep rejects gaps, forward-created steps, and more than one unresolved
// dispatch. An existing unresolved step is returned, never permission to replay.
func NextStep(s Snapshot) (Step, error) {
	var next Step
	blocked := false
	for _, step := range OrderedSteps() {
		intent, exists := s.Steps[step]
		if blocked && exists {
			return "", ErrReviewRequired
		}
		if !exists || intent.ExternalID == "" {
			if next == "" {
				next = step
			}
			blocked = true
		}
	}
	return next, nil
}

// DecodeIntent bounds the compact document at 32 KiB, while accepting bounded
// PostgreSQL JSONB spacing on reads. Duplicate keys/depth/trailing data remain
// invalid. Never return decoder text or normalize away duplicate evidence.
func DecodeIntent(raw []byte, destination any) error {
	if len(raw) == 0 || len(raw) > 65536 || destination == nil {
		return ErrReviewRequired
	}
	var compact bytes.Buffer
	if json.Compact(&compact, raw) != nil || compact.Len() > 32768 || !uniqueObject(compact.Bytes()) {
		return ErrReviewRequired
	}
	decoder := json.NewDecoder(bytes.NewReader(compact.Bytes()))
	decoder.DisallowUnknownFields()
	if decoder.Decode(destination) != nil || decoder.Decode(new(any)) != io.EOF {
		return ErrReviewRequired
	}
	return nil
}
func uniqueObject(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return false
	}
	if !uniqueContainer(d, '}', 0) {
		return false
	}
	_, err = d.Token()
	return err == io.EOF
}
func uniqueContainer(d *json.Decoder, end byte, depth int) bool {
	if depth > 64 {
		return false
	}
	seen := map[string]bool{}
	for d.More() {
		if end == '}' {
			key, err := d.Token()
			if err != nil {
				return false
			}
			k, ok := key.(string)
			if !ok || seen[k] {
				return false
			}
			seen[k] = true
		}
		value, err := d.Token()
		if err != nil {
			return false
		}
		if delim, ok := value.(json.Delim); ok {
			switch delim {
			case '{':
				if !uniqueContainer(d, '}', depth+1) {
					return false
				}
			case '[':
				if !uniqueContainer(d, ']', depth+1) {
					return false
				}
			default:
				return false
			}
		}
	}
	t, err := d.Token()
	return err == nil && t == json.Delim(end)
}

func SUIBinding(s Snapshot) (sui.Binding, error) {
	intent, ok := s.Steps[SUICreate]
	if !ok || !ValidSUIID(intent.ExternalID) {
		return sui.Binding{}, ErrReviewRequired
	}
	var draft sui.CreateDraft
	if DecodeIntent(intent.Payload, &draft) != nil {
		return sui.Binding{}, ErrReviewRequired
	}
	id, _ := strconv.ParseInt(intent.ExternalID, 10, 64)
	return sui.Binding{ID: id, Name: draft.Name(), CredentialsHash: draft.CredentialsHash()}, nil
}

// ValidateIntent binds each frozen intent to the flow's original payment and
// prior confirmed objects. It does not establish provider state or ownership by
// itself; the worker's final callback performs GET verification first.
func ValidateIntent(s Snapshot, step Step, raw []byte) error {
	if ValidateSnapshot(s) != nil || !step.Valid() {
		return ErrReviewRequired
	}
	switch step {
	case SUICreate:
		var draft sui.CreateDraft
		if DecodeIntent(raw, &draft) != nil {
			return ErrReviewRequired
		}
	case RecurringCreate:
		var draft invoiceninja.RecurringDraft
		if DecodeIntent(raw, &draft) != nil || invoiceninja.ValidateRecurringDraft(draft) != nil || draft.ClientID != s.Account.CustomerID || draft.NextSendDate != s.Proof.NextInvoiceDate || draft.PrivateNotes != Marker(s.Token) {
			return ErrReviewRequired
		}
	case SUIEnable:
		var draft sui.EnableDraft
		if DecodeIntent(raw, &draft) != nil {
			return ErrReviewRequired
		}
		binding, err := SUIBinding(s)
		if err != nil || draft.Binding() != binding {
			return ErrReviewRequired
		}
		var saved sui.CreateDraft
		if DecodeIntent(s.Steps[SUICreate].Payload, &saved) != nil {
			return ErrReviewRequired
		}
		var image struct {
			Before  sui.ClientRecord `json:"before"`
			Binding sui.Binding      `json:"binding"`
		}
		if DecodeIntent(raw, &image) != nil || image.Before.ID() != binding.ID || sui.ValidateRecordForDraft(image.Before, saved, false) != nil {
			return ErrReviewRequired
		}
	case RecurringStart:
		var request StartRequest
		if DecodeIntent(raw, &request) != nil || request.Action != "start" || len(request.IDs) != 1 || !ValidID(request.IDs[0]) || request.IDs[0] != s.Steps[RecurringCreate].ExternalID {
			return ErrReviewRequired
		}
	}
	return nil
}
