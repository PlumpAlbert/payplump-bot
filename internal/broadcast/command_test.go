package broadcast

import (
	"strings"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
)

func TestParseCommandRequiresExactPrivateAdminCommand(t *testing.T) {
	admins := map[int64]struct{}{7: {}}
	base := telegram.Update{ID: 1, Message: &telegram.Message{From: &telegram.User{ID: 7}, Chat: telegram.Chat{ID: 7, Type: "private"}, Text: "/broadcast_text hi", Entities: []telegram.Entity{{Type: "bot_command", Offset: 0, Length: 15}}}}
	_, cmd, ok := ParseCommand(base, "mybot", admins)
	if !ok || cmd.Name != "/broadcast_text" || cmd.Argument != "hi" {
		t.Fatalf("got %#v, %v", cmd, ok)
	}
	cases := make([]telegram.Update, 4)
	for i := range cases {
		copied := *base.Message
		from := *base.Message.From
		copied.From = &from
		copied.Entities = append([]telegram.Entity(nil), base.Message.Entities...)
		cases[i] = telegram.Update{ID: base.ID, Message: &copied}
	}
	cases[0].Message.Chat.Type = "group"
	cases[1].Message.Chat.ID = 8
	cases[2].Message.From.IsBot = true
	cases[3].Message.Entities = nil
	for i, u := range cases {
		if _, _, ok := ParseCommand(u, "mybot", admins); ok {
			t.Fatalf("case %d accepted", i)
		}
	}
	base.Message.From.ID = 8
	base.Message.Chat.ID = 8
	if _, _, ok := ParseCommand(base, "mybot", admins); ok {
		t.Fatal("non-admin accepted")
	}
}
func TestParseCommandBotMentionAndBody(t *testing.T) {
	u := telegram.Update{Message: &telegram.Message{From: &telegram.User{ID: 1}, Chat: telegram.Chat{ID: 1, Type: "private"}, Text: "/broadcast@other", Entities: []telegram.Entity{{Type: "bot_command", Offset: 0, Length: 16}}}}
	if _, _, ok := ParseCommand(u, "mybot", map[int64]struct{}{1: {}}); ok {
		t.Fatal("foreign bot mention accepted")
	}
	for _, body := range []string{"ok\ntext", "tab\ttext", "текст"} {
		if err := ValidateBody(body); err != nil {
			t.Fatalf("%q: %v", body, err)
		}
	}
	for _, body := range []string{"", strings.Repeat("x", 3001), "a\x00b", "a\rb"} {
		if ValidateBody(body) == nil {
			t.Fatalf("accepted invalid body %q", body)
		}
	}
}
func TestNewTokenIsOpaque(t *testing.T) {
	a, e := NewToken()
	if e != nil {
		t.Fatal(e)
	}
	b, e := NewToken()
	if e != nil || a == b || len(a) < 32 {
		t.Fatalf("bad tokens")
	}
}
