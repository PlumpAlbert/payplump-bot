package broadcast

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
)

type Command struct{ Name, Argument string }

func (Command) String() string                 { return "[private broadcast command]" }
func (c Command) GoString() string             { return c.String() }
func (c Command) LogValue() slog.Value         { return slog.StringValue(c.String()) }
func (c Command) MarshalJSON() ([]byte, error) { return json.Marshal(c.String()) }

// ParseCommand accepts only an exact private command entity directed to this bot.
// Payloads are never interpreted as client or job selectors beyond an opaque token.
func ParseCommand(u telegram.Update, botUsername string, admins map[int64]struct{}) (telegram.Identity, Command, bool) {
	m := u.Message
	if u.ID < 0 || m == nil || m.From == nil || m.From.IsBot || m.From.ID <= 0 || m.Chat.Type != "private" || m.Chat.ID != m.From.ID {
		return telegram.Identity{}, Command{}, false
	}
	if !utf8.ValidString(m.Text) || !validBotUsername(botUsername) {
		return telegram.Identity{}, Command{}, false
	}
	_, ok := admins[m.From.ID]
	if !ok {
		return telegram.Identity{}, Command{}, false
	}
	space := strings.IndexFunc(m.Text, unicode.IsSpace)
	head, arg := m.Text, ""
	if space >= 0 {
		head = m.Text[:space]
		arg = strings.TrimLeftFunc(m.Text[space:], unicode.IsSpace)
		if name, _, _ := strings.Cut(head, "@"); name == "/broadcast_text" && len(m.Text) > space+1 {
			_, width := utf8.DecodeRuneInString(m.Text[space:])
			arg = m.Text[space+width:]
		}
	}
	for _, r := range head {
		if r >= utf8.RuneSelf {
			return telegram.Identity{}, Command{}, false
		}
	}
	name, mention, addressed := strings.Cut(head, "@")
	if addressed && (mention == "" || !strings.EqualFold(mention, botUsername)) {
		return telegram.Identity{}, Command{}, false
	}
	valid := false
	for _, e := range m.Entities {
		if e.Type == "bot_command" && e.Offset == 0 && e.Length == len(head) {
			valid = true
			break
		}
	}
	if !valid {
		return telegram.Identity{}, Command{}, false
	}
	switch name {
	case "/broadcast", "/broadcast_text", "/broadcast_all", "/broadcast_confirm", "/broadcast_cancel", "/broadcast_status":
	default:
		return telegram.Identity{}, Command{}, false
	}
	return telegram.Identity{TelegramID: m.From.ID, ChatID: m.Chat.ID, DisplayName: strings.TrimSpace(m.From.FirstName + " " + m.From.LastName)}, Command{Name: name, Argument: arg}, true
}

func validBotUsername(s string) bool {
	if len(s) < 5 || len(s) > 32 {
		return false
	}
	for _, c := range []byte(s) {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

func ValidateBody(body string) error {
	if !utf8.ValidString(body) || strings.TrimSpace(body) == "" || body == queue.ConfigurationIntentBody || len([]byte(body)) > 3000 || strings.IndexByte(body, 0) >= 0 {
		return errors.New("invalid broadcast message")
	}
	for _, r := range body {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return errors.New("invalid broadcast message")
		}
	}
	return nil
}

// NewToken uses 192 bits from the OS CSPRNG and returns URL-safe opaque text.
func NewToken() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.New("generate broadcast token")
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
