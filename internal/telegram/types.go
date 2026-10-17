package telegram

import (
	"strings"
	"unicode/utf8"
)

type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type Entity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
}

type Message struct {
	From     *User    `json:"from"`
	Chat     Chat     `json:"chat"`
	Text     string   `json:"text"`
	Entities []Entity `json:"entities"`
}

type Update struct {
	ID      int64    `json:"update_id"`
	Message *Message `json:"message"`
}

type Identity struct {
	TelegramID  int64
	ChatID      int64
	DisplayName string
}

// PrivateStart recognizes a Telegram command, not arbitrary matching text.
// Command names and bot usernames are ASCII, so their entity length also equals
// their UTF-16 length. A deep-link payload after the command is allowed.
func (u Update) PrivateStart(botUsername string) (Identity, bool) {
	m := u.Message
	if m == nil || m.From == nil || m.From.IsBot || m.Chat.Type != "private" || m.From.ID <= 0 || m.Chat.ID != m.From.ID {
		return Identity{}, false
	}
	fields := strings.Fields(m.Text)
	if len(fields) == 0 || !strings.HasPrefix(m.Text, fields[0]) {
		return Identity{}, false
	}
	command := fields[0]
	name, mention, addressed := strings.Cut(command, "@")
	if name != "/start" || (addressed && (mention == "" || !strings.EqualFold(mention, botUsername))) {
		return Identity{}, false
	}
	validEntity := false
	for _, e := range m.Entities {
		if e.Type == "bot_command" && e.Offset == 0 && e.Length == len(command) {
			validEntity = true
			break
		}
	}
	if !validEntity {
		return Identity{}, false
	}
	displayName := strings.TrimSpace(m.From.FirstName + " " + m.From.LastName)
	if !utf8.ValidString(displayName) || strings.ContainsRune(displayName, '\x00') || utf8.RuneCountInString(displayName) > 256 {
		return Identity{}, false
	}
	return Identity{TelegramID: m.From.ID, ChatID: m.Chat.ID, DisplayName: displayName}, true
}
