package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSendMessagePlainText(t *testing.T) {
	const chatID int64 = 4503599627370497
	const text = "Счёт <123> & оплата: https://example.com/invoice"
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/bottest-only-token/sendMessage" {
			t.Error("unexpected sendMessage request")
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		var gotChat int64
		var gotText string
		if json.Unmarshal(body["chat_id"], &gotChat) != nil || json.Unmarshal(body["text"], &gotText) != nil || gotChat != chatID || gotText != text || len(body) != 2 {
			t.Error("message changed or formatting enabled")
		}
		fmt.Fprintf(w, `{"ok":true,"result":{"message_id":42,"chat":{"id":%d,"type":"private"}}}`, chatID)
	})
	id, err := c.SendMessage(context.Background(), chatID, text)
	if err != nil || id != 42 {
		t.Fatalf("sendMessage: id=%d, err=%v", id, err)
	}
}

func TestSendMessageValidationAndResponse(t *testing.T) {
	for _, tc := range []struct {
		chatID int64
		text   string
	}{
		{0, "message"}, {-42, "message"}, {42, ""}, {42, string([]byte{0xff})}, {42, "null\x00text"}, {42, strings.Repeat("я", 4097)},
	} {
		c := testClient(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid message was sent") })
		if _, err := c.SendMessage(context.Background(), tc.chatID, tc.text); err == nil {
			t.Fatal("invalid message accepted")
		}
	}
	for _, result := range []string{
		`{}`, `{"message_id":0,"chat":{"id":42}}`, `{"message_id":7,"chat":{"id":99}}`,
	} {
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, `{"ok":true,"result":%s}`, result) })
		if _, err := c.SendMessage(context.Background(), 42, "message"); err == nil {
			t.Fatal("invalid sent identity accepted")
		}
	}
}

func TestSendMessagePreservesRetryAfter(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"ok":false,"error_code":429,"parameters":{"retry_after":17}}`)
	})
	_, err := c.SendMessage(context.Background(), 42, "message")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.RetryAfter != 17*time.Second || apiErr.Permanent() {
		t.Fatalf("retry_after was lost: %v", err)
	}
}
