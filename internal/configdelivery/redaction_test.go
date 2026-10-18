package configdelivery

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/sui"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
)

func TestSensitiveDeliveryWrappersRemainRedactedInStructuredAndNestedJSON(t *testing.T) {
	const marker = "synthetic-private-redaction-sentinel"
	cfg := configurationTestConfig()
	cfg.Token = marker
	cfg.SubscriptionBaseURL = "https://subscription.example.test/" + marker
	intent := activation.Intent{Payload: json.RawMessage(`{"password":"` + marker + `"}`)}
	e := Entitlement{Activation: activation.Snapshot{Steps: map[activation.Step]activation.Intent{activation.SUICreate: intent}}, Binding: sui.Binding{Name: marker}}
	v := View{Request: Request{InvoiceID: marker}, Entitlement: e}
	message := telegram.ProtectedMessage{Text: "https://subscription.example.test/" + marker, NotAfter: time.Now().Add(time.Minute)}
	values := []any{cfg, v.Request, e, v, message, intent, e.Activation, e.Binding}
	for _, value := range values {
		for _, jsonLog := range []bool{false, true} {
			var output bytes.Buffer
			var handler slog.Handler = slog.NewTextHandler(&output, nil)
			if jsonLog {
				handler = slog.NewJSONHandler(&output, nil)
			}
			slog.New(handler).Info("redaction", slog.Any("value", value))
			if strings.Contains(output.String(), marker) || strings.Contains(output.String(), "password") {
				t.Fatal("sensitive typed value escaped structured logging redaction")
			}
		}
	}
	for _, value := range []any{cfg, v.Request, e, v, message} {
		raw, err := json.Marshal(struct{ Private any }{value})
		if err != nil || bytes.Contains(raw, []byte(marker)) || bytes.Contains(raw, []byte("password")) {
			t.Fatal("sensitive application wrapper escaped nested JSON redaction")
		}
	}
}
func TestSensitiveSUIWireSerializationIsExplicitButDirectLoggingRedacted(t *testing.T) {
	draft, err := sui.NewDisabledDraft([]int64{7})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(draft)
	if err != nil || !bytes.Contains(raw, []byte(draft.Name())) {
		t.Fatal("protected durable SUI wire capability was accidentally removed")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		t.Fatal("invalid synthetic draft")
	}
	fields["id"] = json.RawMessage(`17`)
	raw, _ = json.Marshal(fields)
	record, err := sui.ParseClientRecord(raw)
	if err != nil {
		t.Fatal("invalid synthetic record")
	}
	binding := sui.Binding{ID: record.ID(), Name: record.Name(), CredentialsHash: record.CredentialsHash()}
	enable, err := sui.PrepareEnable(record, binding)
	if err != nil {
		t.Fatal("invalid synthetic enable")
	}
	for _, value := range []any{draft, record, binding, enable} {
		var output bytes.Buffer
		slog.New(slog.NewJSONHandler(&output, nil)).Info("redaction", slog.Any("value", value))
		if strings.Contains(output.String(), record.Name()) || strings.Contains(output.String(), "password") {
			t.Fatal("SUI wire type leaked credentials in direct structured logging")
		}
	}
}
