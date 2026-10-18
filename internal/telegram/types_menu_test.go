package telegram

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode"

	"github.com/PlumpAlbert/payplump-bot/internal/clientmenu"
)

const menuActorID int64 = 4503599627370497

func menuUpdate(text string) Update {
	command := text
	if end := strings.IndexFunc(command, unicode.IsSpace); end >= 0 {
		command = command[:end]
	}
	return Update{ID: 71, Message: &Message{
		From: &User{ID: menuActorID, FirstName: "Иван", LastName: "Иванов"},
		Chat: Chat{ID: menuActorID, Type: "private"}, Text: text,
		Entities: []Entity{{Type: "bot_command", Offset: 0, Length: len(command)}},
	}}
}

func menuRoundTrip(t *testing.T, update Update) Update {
	t.Helper()
	wire, err := json.Marshal(update)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Update
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestPrivateMenuCommandsRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		command string
		action  clientmenu.Action
	}{
		{"/start", clientmenu.Start}, {"/menu", clientmenu.Home},
		{"/invoice", clientmenu.Invoice}, {"/status", clientmenu.Status},
		{"/config", clientmenu.Configuration}, {"/help", clientmenu.Help},
	} {
		for _, mention := range []string{"", "@PayplumpBot", "@pAyPlUmPbOt"} {
			arguments := []string{"", " \t\n\u2003"}
			if tc.action == clientmenu.Start {
				arguments = append(arguments, " payload", "\ncustomer_id=999999 invoice_id=888888", "\u20034503599627370498")
			}
			for _, args := range arguments {
				text := tc.command + mention + args
				t.Run(text, func(t *testing.T) {
					u := menuRoundTrip(t, menuUpdate(text))
					identity, action, ok := u.PrivateMenu("PayplumpBot")
					want := Identity{TelegramID: menuActorID, ChatID: menuActorID, DisplayName: "Иван Иванов"}
					if !ok || identity != want || action != tc.action {
						t.Fatalf("PrivateMenu = %+v, %q, %v; want %+v, %q", identity, action, ok, want, tc.action)
					}
					_, start := u.PrivateStart("PayplumpBot")
					if start != (tc.action == clientmenu.Start) {
						t.Fatal("PrivateStart recognized a non-start command or rejected a deep link")
					}
				})
			}
		}
	}
}

func TestPrivateMenuButtonsRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		label  string
		action clientmenu.Action
	}{
		{clientmenu.InvoiceLabel, clientmenu.Invoice},
		{clientmenu.StatusLabel, clientmenu.Status},
		{clientmenu.ConfigurationLabel, clientmenu.Configuration},
		{clientmenu.HelpLabel, clientmenu.Help},
	} {
		t.Run(tc.label, func(t *testing.T) {
			u := menuUpdate(tc.label)
			u.Message.Entities = nil
			u = menuRoundTrip(t, u)
			identity, action, ok := u.PrivateMenu("PayplumpBot")
			want := Identity{TelegramID: menuActorID, ChatID: menuActorID, DisplayName: "Иван Иванов"}
			if !ok || action != tc.action || identity != want {
				t.Fatalf("button = %+v, %q, %v", identity, action, ok)
			}
			if _, ok := u.PrivateStart("PayplumpBot"); ok {
				t.Fatal("PrivateStart recognized a menu label")
			}
			for _, text := range []string{" " + tc.label, tc.label + "\n", tc.label + " 123", strings.ToLower(tc.label)} {
				if _, _, ok := menuUpdate(text).PrivateMenu("PayplumpBot"); ok {
					t.Fatalf("inexact label accepted: %q", text)
				}
			}
		})
	}
}

func TestPrivateMenuRejectsTextArgumentsAndForeignCommands(t *testing.T) {
	texts := []string{
		"", "hello", "/unknown", "/starter", "/START", "/MENU", "/configuration", " /menu", "\u2003/menu", "prefix /menu",
		"/menu\u200b", "/menu\x00", "/help@OtherBot", "/start@OtherBot payload", "/invoice@", "/status@PayplumpBot@PayplumpBot",
		"/config@PayplumpBоt", // Cyrillic 'о' is not the configured ASCII username.
	}
	for _, command := range []string{"/menu", "/invoice", "/status", "/config", "/help"} {
		for _, mention := range []string{"", "@PayplumpBot"} {
			for _, args := range []string{" 4503599627370498", "\ncustomer_id=42", "\tinvoice_id=99", "\u2003subscription=secret"} {
				texts = append(texts, command+mention+args)
			}
		}
	}
	for _, text := range texts {
		t.Run(text, func(t *testing.T) {
			identity, action, ok := menuUpdate(text).PrivateMenu("PayplumpBot")
			if ok || identity != (Identity{}) || action != "" {
				t.Fatal("invalid menu text accepted or returned identity/action")
			}
		})
	}
	// EqualFold alone accepts the Kelvin sign as ASCII K. Neither a byte-length
	// entity nor a correct UTF-16-length entity makes a non-ASCII token valid.
	for _, length := range []int{len("/menu@PayplumpBotK"), len([]rune("/menu@PayplumpBotK"))} {
		u := menuUpdate("/menu@PayplumpBotK")
		u.Message.Entities[0].Length = length
		if _, _, ok := u.PrivateMenu("PayplumpBotK"); ok {
			t.Fatal("non-ASCII command token accepted")
		}
	}
}

func TestPrivateMenuRejectsForgedEntities(t *testing.T) {
	for _, command := range []string{"/start", "/menu", "/invoice", "/status", "/config", "/help"} {
		for _, tc := range []struct {
			name   string
			change func(*Message)
		}{
			{"missing", func(m *Message) { m.Entities = nil }},
			{"wrong type", func(m *Message) { m.Entities[0].Type = "bold" }},
			{"shifted", func(m *Message) { m.Entities[0].Offset = 1 }},
			{"negative offset", func(m *Message) { m.Entities[0].Offset = -1 }},
			{"zero length", func(m *Message) { m.Entities[0].Length = 0 }},
			{"negative length", func(m *Message) { m.Entities[0].Length = -1 }},
			{"short length", func(m *Message) { m.Entities[0].Length-- }},
			{"long length", func(m *Message) { m.Entities[0].Length++ }},
			{"mention excluded", func(m *Message) { m.Entities[0].Length = strings.IndexByte(m.Text, '@') }},
		} {
			t.Run(command+"/"+tc.name, func(t *testing.T) {
				u := menuUpdate(command + "@PayplumpBot")
				tc.change(u.Message)
				identity, action, ok := u.PrivateMenu("PayplumpBot")
				if ok || identity != (Identity{}) || action != "" {
					t.Fatal("forged command entity accepted")
				}
			})
		}
	}
}

func TestPrivateMenuRejectsIdentityAttacks(t *testing.T) {
	for _, text := range []string{
		"/start customer_id=999999", "/menu", "/invoice@PayplumpBot", "/status", "/config", "/help",
		clientmenu.InvoiceLabel, clientmenu.StatusLabel, clientmenu.ConfigurationLabel, clientmenu.HelpLabel,
	} {
		for _, tc := range []struct {
			name   string
			change func(*Update)
		}{
			{"unsupported update", func(u *Update) { u.Message = nil }},
			{"missing sender", func(u *Update) { u.Message.From = nil }},
			{"bot", func(u *Update) { u.Message.From.IsBot = true }},
			{"group", func(u *Update) { u.Message.Chat.Type = "group" }},
			{"supergroup", func(u *Update) { u.Message.Chat.Type = "supergroup" }},
			{"channel", func(u *Update) { u.Message.Chat.Type = "channel" }},
			{"foreign private chat", func(u *Update) { u.Message.Chat.ID++ }},
			{"zero sender", func(u *Update) { u.Message.From.ID = 0; u.Message.Chat.ID = 0 }},
			{"negative sender", func(u *Update) { u.Message.From.ID = -42; u.Message.Chat.ID = -42 }},
			{"invalid UTF-8 name", func(u *Update) { u.Message.From.FirstName = string([]byte{0xff}) }},
			{"NUL first name", func(u *Update) { u.Message.From.FirstName = "bad\x00name" }},
			{"NUL last name", func(u *Update) { u.Message.From.LastName = "bad\x00name" }},
			{"long name", func(u *Update) { u.Message.From.FirstName = strings.Repeat("я", 257) }},
			{"long combined name", func(u *Update) {
				u.Message.From.FirstName = strings.Repeat("я", 128)
				u.Message.From.LastName = strings.Repeat("я", 128)
			}},
		} {
			t.Run(text+"/"+tc.name, func(t *testing.T) {
				u := menuUpdate(text)
				tc.change(&u)
				identity, action, ok := u.PrivateMenu("PayplumpBot")
				if ok || identity != (Identity{}) || action != "" {
					t.Fatal("invalid actor accepted or returned identity/action")
				}
				if _, ok := u.PrivateStart("PayplumpBot"); ok {
					t.Fatal("PrivateStart accepted an invalid actor")
				}
			})
		}
	}
}

func TestPrivateMenuDisplayNameAndAuthoritativeSender(t *testing.T) {
	for _, name := range []string{"", " \t ", "  Иван Иванов  ", strings.Repeat("я", 256), " " + strings.Repeat("я", 256) + " "} {
		u := menuUpdate("/start customer_id=999999 invoice_id=888888")
		u.Message.From.FirstName, u.Message.From.LastName = name, ""
		u.Message.From.Username = "victim_999999"
		identity, action, ok := u.PrivateMenu("PayplumpBot")
		want := Identity{TelegramID: menuActorID, ChatID: menuActorID, DisplayName: strings.TrimSpace(name)}
		if !ok || action != clientmenu.Start || identity != want {
			t.Fatalf("identity not derived from sender: %+v, %q, %v", identity, action, ok)
		}
		startIdentity, ok := u.PrivateStart("PayplumpBot")
		if !ok || startIdentity != identity {
			t.Fatal("PrivateStart and PrivateMenu display-name rules differ")
		}
	}
}

func TestPrivateMenuDoesNotModelCallbacks(t *testing.T) {
	for _, wire := range []string{
		`{"update_id":71,"callback_query":{"id":"untrusted","from":{"id":42},"message":{"chat":{"id":42,"type":"private"},"text":"/invoice"},"data":"/invoice customer_id=999999"}}`,
		`{"update_id":71,"edited_message":{"from":{"id":42},"chat":{"id":42,"type":"private"},"text":"/menu","entities":[{"type":"bot_command","offset":0,"length":5}]}}`,
	} {
		var u Update
		if err := json.Unmarshal([]byte(wire), &u); err != nil {
			t.Fatal(err)
		}
		if _, _, ok := u.PrivateMenu("PayplumpBot"); ok {
			t.Fatal("unsupported update accepted")
		}
	}
}
