package importplan

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func validRecord() Record {
	return Record{TelegramID: "123", InvoiceCustomerID: "customer-A", InvoiceNumber: "123", CurrentInvoiceID: "invoice-A", InvoiceStatus: "paid", RecurringID: "recurring-A", SUIID: "sui-A", SUIName: "private-name", ConfigHash: strings.Repeat("a", 64), PeriodStart: "2025-01-01", PeriodEnd: "2025-02-01", Currency: "USD", Lifecycle: "active", SourceVersion: "synthetic-1", PinnedProvenance: true, CompleteProof: true, Amount: "12.3400"}
}
func TestAnalyzeCreatesCandidateAndStableHash(t *testing.T) {
	m := Manifest{Records: []Record{validRecord()}}
	a, b := Analyze(m), Analyze(m)
	if a.Imported != 1 || a.Hash != b.Hash || len(a.Hash) != 64 {
		t.Fatalf("unexpected plan: %+v", a)
	}
	if a.ValidateHash(a.Hash) != nil || a.ValidateHash(strings.Repeat("0", 64)) == nil {
		t.Fatal("hash confirmation failed")
	}
}
func TestExclusionAndAmbiguityDoNotImport(t *testing.T) {
	a := validRecord()
	b := validRecord()
	b.Excluded = true
	p := Analyze(Manifest{Records: []Record{a, b}})
	if p.Imported != 0 || p.Reasons["excluded"] != 1 || p.Reasons["ambiguous_pair"] != 1 {
		t.Fatalf("unexpected reasons: %+v", p.Reasons)
	}
}
func TestExclusionWinsBeforeOtherReviewReasons(t *testing.T) {
	r := validRecord()
	r.Excluded = true
	r.TelegramID = "bad"
	p := Analyze(Manifest{Records: []Record{r}})
	if p.Reasons["excluded"] != 1 || len(p.Reasons) != 1 {
		t.Fatalf("exclusion did not win: %+v", p.Reasons)
	}
}
func TestRejectInvalidIdentityProofMoneyAndDate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Record)
		reason string
	}{{"id mismatch", func(r *Record) { r.InvoiceNumber = "0123" }, "invalid_identity"}, {"proof", func(r *Record) { r.CompleteProof = false }, "incomplete_payment_proof"}, {"money exponent", func(r *Record) { r.Amount = "1e6" }, "invalid_amount"}, {"money zero", func(r *Record) { r.Amount = "0.00" }, "invalid_amount"}, {"money leading zero", func(r *Record) { r.Amount = "01.00" }, "invalid_amount"}, {"date", func(r *Record) { r.PeriodEnd = "2024-02-30" }, "invalid_period"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := validRecord()
			tt.mutate(&r)
			p := Analyze(Manifest{Records: []Record{r}})
			if p.Imported != 0 || p.Reasons[tt.reason] != 1 {
				t.Fatalf("unexpected plan: %+v", p)
			}
		})
	}
}
func TestDecodeRejectsDuplicateUnknownTrailingDepth(t *testing.T) {
	cases := []string{`{"records":[],"records":[]}`, `{"records":[],"Records":[]}`, `{"records":[],"RECORDS":[]}`, `{"records":[],"x":{"K":1,"K":2}}`, `{"records":[],"secret":"x"}{}`, `{"records":[]} {}`, `{"records":[` + strings.Repeat("[", 40) + `0` + strings.Repeat("]", 40) + `]}`, `[]`, `{"records":null}`}
	for _, in := range cases {
		if _, err := Decode(strings.NewReader(in)); err == nil {
			t.Fatalf("accepted invalid input %s", in)
		}
	}
}
func TestDecodeRequiresExactCompleteRecord(t *testing.T) {
	recordJSON, _ := validRecord().PrivateJSON()
	b := append([]byte(`{"records":[`), recordJSON...)
	b = append(b, []byte(`]}`)...)
	var obj map[string]any
	_ = json.Unmarshal(b, &obj)
	records := obj["records"].([]any)
	record := records[0].(map[string]any)
	raw, _ := json.Marshal(obj)
	if _, err := Decode(strings.NewReader(string(raw))); err != nil {
		t.Fatalf("valid record rejected: %v", err)
	}
	for _, key := range []string{"TELEGRAM_ID", "Telegram_id"} {
		copy := map[string]any{}
		for k, v := range record {
			copy[k] = v
		}
		copy[key] = copy["telegram_id"]
		obj["records"] = []any{copy}
		raw, _ = json.Marshal(obj)
		if _, err := Decode(strings.NewReader(string(raw))); err == nil {
			t.Fatalf("accepted case alias %s", key)
		}
	}
	for _, mutate := range []func(map[string]any){func(r map[string]any) { delete(r, "complete_proof") }, func(r map[string]any) { r["complete_proof"] = nil }, func(r map[string]any) { r["private_extra"] = true }} {
		copy := map[string]any{}
		for k, v := range record {
			copy[k] = v
		}
		mutate(copy)
		obj["records"] = []any{copy}
		raw, _ = json.Marshal(obj)
		if _, err := Decode(strings.NewReader(string(raw))); err == nil {
			t.Fatal("accepted incomplete/null/unknown record")
		}
	}
}
func TestValidateHashDetectsMutatedPlan(t *testing.T) {
	p := Analyze(Manifest{Records: []Record{validRecord()}})
	p.Candidates[0].Facts.SUIName = "forged"
	if p.ValidateHash(p.Hash) == nil {
		t.Fatal("accepted mutated candidate facts")
	}
	p = Analyze(Manifest{Records: []Record{validRecord()}})
	p.Candidates = nil
	if p.ValidateHash(p.Hash) == nil {
		t.Fatal("accepted mutated candidate list")
	}
	p = Analyze(Manifest{Records: []Record{validRecord()}})
	p.Hash = "x"
	if p.ValidateHash(p.Hash) == nil {
		t.Fatal("accepted malformed hash")
	}
}
func TestGenericRecordSerializationIsRedacted(t *testing.T) {
	r := validRecord()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{r.TelegramID, r.InvoiceCustomerID, r.SUIName, r.ConfigHash} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("generic JSON leaked a private fact: %s", b)
		}
	}
	private, err := r.PrivateJSON()
	if err != nil || !strings.Contains(string(private), r.InvoiceCustomerID) {
		t.Fatal("private facts projection unavailable")
	}
	p := Analyze(Manifest{Records: []Record{r}})
	for _, generic := range []string{fmt.Sprintf("%+v", p), fmt.Sprintf("%#v", p)} {
		if strings.Contains(generic, r.InvoiceCustomerID) || strings.Contains(generic, r.SUIName) {
			t.Fatal("generic plan formatting leaked private facts")
		}
	}
}
func TestSafeSummaryOmitsBindings(t *testing.T) {
	p := Analyze(Manifest{Records: []Record{validRecord()}})
	s := string(SafeSummary(p))
	if !strings.Contains(s, `"candidate_indexes":[0]`) {
		t.Fatalf("local candidate index missing: %s", s)
	}
	for _, v := range []string{"customer-A", "private-name", "sui-A", strings.Repeat("a", 64)} {
		if strings.Contains(s, v) {
			t.Fatalf("summary leaked %q: %s", v, s)
		}
	}
}
