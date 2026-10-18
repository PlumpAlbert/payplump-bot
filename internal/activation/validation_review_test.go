package activation

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/sui"
)

func jsonbSpacesForTest(raw []byte) []byte {
	var b bytes.Buffer
	quoted, escaped := false, false
	for _, c := range raw {
		b.WriteByte(c)
		if quoted {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				quoted = false
			}
			continue
		}
		if c == '"' {
			quoted = true
		} else if c == ',' || c == ':' {
			b.WriteByte(' ')
		}
	}
	return b.Bytes()
}
func TestActivationIntentAcceptsJSONBSpacingRoundTrip(t *testing.T) {
	s := validationFullSteps(t)
	var original struct {
		Before  sui.ClientRecord `json:"before"`
		Binding sui.Binding      `json:"binding"`
	}
	if DecodeIntent(s.Steps[SUIEnable].Payload, &original) != nil {
		t.Fatal("base enable image invalid")
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(original.Before.Raw(), &fields)
	var compact json.RawMessage
	for n := 600; n <= 1200; n++ {
		links := make([]map[string]string, n)
		for i := range links {
			links[i] = map[string]string{"type": "local", "uri": "synthetic"}
		}
		fields["links"] = validationPayload(t, links)
		record, e := sui.ParseClientRecord(validationPayload(t, fields))
		if e != nil {
			t.Fatal("synthetic local link extension failed")
		}
		draft, e := sui.PrepareEnable(record, original.Binding)
		if e != nil {
			t.Fatal("synthetic enable preparation failed")
		}
		compact = validationPayload(t, draft)
		if len(compact) >= 31600 {
			break
		}
	}
	expanded := jsonbSpacesForTest(compact)
	if len(compact) > 32768 || len(expanded) <= 32768 || len(expanded) > 65536 {
		t.Fatal("synthetic case did not cross only the JSONB formatting bound")
	}
	if ValidateIntent(s, SUIEnable, compact) != nil {
		t.Fatal("compact valid intent rejected")
	}
	if ValidateIntent(s, SUIEnable, expanded) != nil {
		t.Fatal("accepted intent became unreadable after JSONB serialization")
	}
	var decoded sui.EnableDraft
	if DecodeIntent(expanded, &decoded) != nil || decoded.Binding() != original.Binding {
		t.Fatal("JSONB formatting changed frozen identity")
	}
	// Compaction is not permission to erase duplicate evidence or accept huge input.
	duplicate := append([]byte(`{"binding":{},`), expanded[1:]...)
	if DecodeIntent(duplicate, &decoded) == nil {
		t.Fatal("compaction erased duplicate-key evidence")
	}
	if DecodeIntent(append([]byte(strings.Repeat(" ", 65536)), compact...), &decoded) == nil {
		t.Fatal("unbounded whitespace accepted")
	}
}

func TestActivationRecurringReceiptFactsCanonicalAndBounded(t *testing.T) {
	for _, date := range []string{"2040-02-29T12:00:00Z", "2040-02-29T15:00:00+03:00", "2040-02-29 12:00:00"} {
		amount, instant, e := NormalizeRecurringFacts("100.000", date)
		if e != nil || amount != "100" || instant != "2040-02-29T12:00:00Z" {
			t.Fatal("equivalent recurring receipt facts changed")
		}
	}
	for _, amount := range []json.Number{"0", "-1", "1e99999", "NaN"} {
		if _, _, e := NormalizeRecurringFacts(amount, "2040-02-29T12:00:00Z"); e == nil {
			t.Fatal("invalid recurring amount accepted")
		}
	}
	for _, date := range []string{"2040-02-29", "2040-02-29T12:00:00", "2040-02-29 2:00:00", "0000-01-01T00:00:00Z"} {
		if _, _, e := NormalizeRecurringFacts("100", date); e == nil {
			t.Fatal("ambiguous recurring instant accepted")
		}
	}
	s := validationFullSteps(t)
	for _, step := range []Step{RecurringCreate, RecurringStart} {
		original := s.Steps[step]
		changed := original
		changed.RecurringAmount = ""
		s.Steps[step] = changed
		if ValidateSnapshot(s) == nil {
			t.Fatal("known recurring result has no original total")
		}
		changed = original
		changed.RecurringDatetime = ""
		s.Steps[step] = changed
		if ValidateSnapshot(s) == nil {
			t.Fatal("known recurring result has no original UTC schedule")
		}
		s.Steps[step] = original
	}
}
