package telegram

import (
	"strings"
	"testing"
)

func startUpdate(id int64, text string) Update {
	command := strings.Fields(text)
	length := 0
	if len(command) > 0 {
		length = len(command[0])
	}
	return Update{ID: id, Message: &Message{
		From: &User{ID: 4503599627370497, FirstName: "Иван", LastName: "Иванов"},
		Chat: Chat{ID: 4503599627370497, Type: "private"}, Text: text,
		Entities: []Entity{{Type: "bot_command", Offset: 0, Length: length}},
	}}
}

func TestPrivateStart(t *testing.T) {
	for _, text := range []string{"/start", "/start payload", "/start\npayload", "/start@PayplumpBot", "/start@payplumpbot payload"} {
		identity, ok := startUpdate(1, text).PrivateStart("PayplumpBot")
		if !ok || identity.TelegramID != 4503599627370497 || identity.ChatID != identity.TelegramID || identity.DisplayName != "Иван Иванов" {
			t.Fatalf("private command not recognized: %q", text)
		}
	}
	for _, tc := range []struct {
		name   string
		change func(*Update)
	}{
		{"unsupported update", func(u *Update) { u.Message = nil }},
		{"no sender", func(u *Update) { u.Message.From = nil }},
		{"bot sender", func(u *Update) { u.Message.From.IsBot = true }},
		{"group", func(u *Update) { u.Message.Chat.Type = "group" }},
		{"other private chat", func(u *Update) { u.Message.Chat.ID = 7 }},
		{"invalid sender", func(u *Update) { u.Message.From.ID = 0; u.Message.Chat.ID = 0 }},
		{"missing entity", func(u *Update) { u.Message.Entities = nil }},
		{"text not command", func(u *Update) { u.Message.Entities[0].Type = "bold" }},
		{"shifted entity", func(u *Update) { u.Message.Entities[0].Offset = 1 }},
		{"invalid length", func(u *Update) { u.Message.Entities[0].Length = 99 }},
		{"long name", func(u *Update) { u.Message.From.FirstName = strings.Repeat("я", 257) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := startUpdate(1, "/start")
			tc.change(&u)
			if _, ok := u.PrivateStart("PayplumpBot"); ok {
				t.Fatal("invalid command accepted")
			}
		})
	}
	for _, text := range []string{"", "hello", "/starter", " /start", "/START", "/start@OtherBot", "/start@", "prefix /start"} {
		if _, ok := startUpdate(1, text).PrivateStart("PayplumpBot"); ok {
			t.Fatalf("invalid command accepted: %q", text)
		}
	}
}
