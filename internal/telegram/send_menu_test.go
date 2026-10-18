package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/clientmenu"
)

func TestSendMenuMessageStaticKeyboardAndPlainText(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"literal formatting and URL", "  <b>Иван & счёт</b> *123*\nhttps://private.example/invoice?token=menu-secret  "},
		{"maximum Unicode length", strings.Repeat("я", 4096)},
		{"whitespace unchanged", " \t\n "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/bottest-only-token/sendMessage" || r.Header.Get("Content-Type") != "application/json" {
					t.Error("unexpected sendMessage request")
				}
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				var chatID int64
				var text string
				if len(body) != 3 || json.Unmarshal(body["chat_id"], &chatID) != nil || chatID != menuActorID || json.Unmarshal(body["text"], &text) != nil || text != tc.text {
					t.Error("message changed or unexpected formatting/payload enabled")
				}
				var markup map[string]json.RawMessage
				if json.Unmarshal(body["reply_markup"], &markup) != nil || len(markup) != 3 || string(markup["resize_keyboard"]) != "true" || string(markup["is_persistent"]) != "true" {
					t.Error("keyboard must be persistent and resizable, with no other options")
				}
				var keyboard [][]string
				want := [][]string{
					{clientmenu.InvoiceLabel}, {clientmenu.StatusLabel}, {clientmenu.ConfigurationLabel}, {clientmenu.HelpLabel},
				}
				if json.Unmarshal(markup["keyboard"], &keyboard) != nil || !reflect.DeepEqual(keyboard, want) {
					t.Error("keyboard is not exactly four static, text-only, one-button rows")
				}
				// Round-trip the actual wire labels into inbound private messages.
				actions := []clientmenu.Action{clientmenu.Invoice, clientmenu.Status, clientmenu.Configuration, clientmenu.Help}
				for row, buttons := range keyboard {
					if row >= len(actions) || len(buttons) != 1 {
						t.Error("unexpected keyboard shape")
						continue
					}
					u := menuUpdate(buttons[0])
					u.Message.Entities = nil
					identity, action, ok := u.PrivateMenu("PayplumpBot")
					if !ok || identity.TelegramID != menuActorID || identity.ChatID != menuActorID || action != actions[row] {
						t.Error("wire label does not resolve to its actor-scoped menu action")
					}
				}
				fmt.Fprintf(w, `{"ok":true,"result":{"message_id":42,"chat":{"id":%d,"type":"private"}}}`, menuActorID)
			})
			id, err := c.SendMenuMessage(context.Background(), menuActorID, tc.text)
			if err != nil || id != 42 {
				t.Fatalf("SendMenuMessage: id=%d, err=%v", id, err)
			}
		})
	}
}

func TestSendMessageWireRemainsUnchanged(t *testing.T) {
	const text = "<b>Иван & счёт</b>\nhttps://private.example/invoice?token=menu-secret"
	encodedText, err := json.Marshal(text)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`{"chat_id":%d,"text":%s}`, menuActorID, encodedText)
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != want {
			t.Error("ordinary SendMessage JSON changed or acquired reply markup")
		}
		fmt.Fprintf(w, `{"ok":true,"result":{"message_id":42,"chat":{"id":%d}}}`, menuActorID)
	})
	if id, err := c.SendMessage(context.Background(), menuActorID, text); err != nil || id != 42 {
		t.Fatalf("SendMessage: id=%d, err=%v", id, err)
	}
}

func TestSendMenuMessageValidationBeforeHTTP(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chatID int64
		text   string
	}{
		{"zero chat", 0, "message"}, {"negative chat", -42, "message"},
		{"empty text", 42, ""}, {"invalid UTF-8", 42, string([]byte{0xff})},
		{"NUL", 42, "https://private.example/invoice?token=menu-secret\x00"},
		{"too long", 42, strings.Repeat("я", 4097)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid menu message reached HTTP") })
			id, err := c.SendMenuMessage(context.Background(), tc.chatID, tc.text)
			if err == nil || id != 0 {
				t.Fatal("invalid menu message accepted")
			}
			if strings.Contains(err.Error(), "menu-secret") || strings.Contains(err.Error(), "https://") {
				t.Fatal("invalid private text leaked in validation error")
			}
		})
	}
}

func TestSendMenuMessageValidatesResponseIdentity(t *testing.T) {
	for _, result := range []string{
		`{}`, `{"message_id":0,"chat":{"id":42}}`, `{"message_id":-7,"chat":{"id":42}}`,
		`{"message_id":7}`, `{"message_id":7,"chat":{"id":99}}`, `null`,
	} {
		t.Run(result, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, `{"ok":true,"result":%s}`, result) })
			if id, err := c.SendMenuMessage(context.Background(), 42, "message"); err == nil || id != 0 {
				t.Fatal("invalid sent menu identity accepted")
			}
		})
	}
}

func TestSendMenuMessageErrorsRedactPrivateData(t *testing.T) {
	const privateURL = "https://private.example/invoice?token=menu-secret"
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"API description", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprintf(w, `{"ok":false,"error_code":429,"description":%q,"parameters":{"retry_after":17}}`, "test-only-token "+privateURL)
		}},
		{"malformed response", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "test-only-token "+privateURL) }},
		{"transport failure", func(w http.ResponseWriter, r *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, tc.handler)
			id, err := c.SendMenuMessage(context.Background(), 42, privateURL)
			if err == nil || id != 0 {
				t.Fatal("failed request reported success")
			}
			for _, secret := range []string{"test-only-token", privateURL, "menu-secret", c.baseURL} {
				if strings.Contains(err.Error(), secret) {
					t.Fatal("private text, token or raw request URL leaked in error")
				}
			}
			if tc.name == "API description" {
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.RetryAfter != 17*time.Second || apiErr.Permanent() {
					t.Fatalf("retry metadata lost: %v", err)
				}
			}
		})
	}
}

func TestSendMenuMessagePreservesCancellation(t *testing.T) {
	c := testClient(t, func(http.ResponseWriter, *http.Request) { t.Error("canceled request reached HTTP") })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if id, err := c.SendMenuMessage(ctx, 42, "message"); id != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: id=%d, err=%v", id, err)
	}
}
