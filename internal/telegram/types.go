package telegram

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/PlumpAlbert/payplump-bot/internal/clientmenu"
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
	identity, ok := u.privateIdentity()
	if !ok {
		return Identity{}, false
	}
	m := u.Message
	command := m.Text
	if end := strings.IndexFunc(command, unicode.IsSpace); end >= 0 {
		command = command[:end]
	}
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
	return identity, true
}

// PrivateMenu accepts only verified private commands or exact keyboard labels.
// Arguments are ignored for /start deep links and rejected for other commands;
// neither message text nor keyboard payloads supply an actor or resource ID.
func (u Update) PrivateMenu(botUsername string) (Identity, clientmenu.Action, bool) {
	identity, ok := u.privateIdentity()
	if !ok {
		return Identity{}, "", false
	}
	m := u.Message
	switch m.Text {
	case clientmenu.InvoiceLabel:
		return identity, clientmenu.Invoice, true
	case clientmenu.StatusLabel:
		return identity, clientmenu.Status, true
	case clientmenu.ConfigurationLabel:
		return identity, clientmenu.Configuration, true
	case clientmenu.HelpLabel:
		return identity, clientmenu.Help, true
	}

	command, arguments := m.Text, ""
	if end := strings.IndexFunc(command, unicode.IsSpace); end >= 0 {
		command, arguments = command[:end], command[end:]
	}
	// Telegram entity offsets use UTF-16 units. ASCII command tokens have
	// identical byte and UTF-16 lengths, including any addressed bot username.
	for i := 0; i < len(command); i++ {
		if command[i] >= utf8.RuneSelf {
			return Identity{}, "", false
		}
	}
	name, mention, addressed := strings.Cut(command, "@")
	if addressed && (mention == "" || !strings.EqualFold(mention, botUsername)) {
		return Identity{}, "", false
	}
	var action clientmenu.Action
	switch name {
	case "/start":
		action = clientmenu.Start
	case "/menu":
		action = clientmenu.Home
	case "/invoice":
		action = clientmenu.Invoice
	case "/status":
		action = clientmenu.Status
	case "/config":
		action = clientmenu.Configuration
	case "/help":
		action = clientmenu.Help
	default:
		return Identity{}, "", false
	}
	if action != clientmenu.Start && strings.TrimSpace(arguments) != "" {
		return Identity{}, "", false
	}
	for _, e := range m.Entities {
		if e.Type == "bot_command" && e.Offset == 0 && e.Length == len(command) {
			return identity, action, true
		}
	}
	return Identity{}, "", false
}

func (u Update) privateIdentity() (Identity, bool) {
	m := u.Message
	if m == nil || m.From == nil || m.From.IsBot || m.Chat.Type != "private" || m.From.ID <= 0 || m.Chat.ID != m.From.ID {
		return Identity{}, false
	}
	displayName := strings.TrimSpace(m.From.FirstName + " " + m.From.LastName)
	if !utf8.ValidString(displayName) || strings.ContainsRune(displayName, '\x00') || utf8.RuneCountInString(displayName) > 256 {
		return Identity{}, false
	}
	return Identity{TelegramID: m.From.ID, ChatID: m.Chat.ID, DisplayName: displayName}, true
}
