package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"
)

// ProtectedMessage contains sensitive plain text. NotAfter bounds the send
// attempt, not Telegram retention or recall. Formatting is always redacted.
type ProtectedMessage struct {
	Text     string
	NotAfter time.Time
}

func (ProtectedMessage) String() string         { return "[sensitive Telegram message]" }
func (ProtectedMessage) GoString() string       { return "[sensitive Telegram message]" }
func (m ProtectedMessage) LogValue() slog.Value { return slog.StringValue(m.String()) }

// The transport explicitly serializes message.Text only after authorization.
// Generic JSON serialization is never a delivery or persistence capability.
func (m ProtectedMessage) MarshalJSON() ([]byte, error) { return json.Marshal(m.String()) }

// GetPrivateChat checks only the exact positive ID and private-chat scope. It
// returns no user information and does not authorize access to any configuration.
func (c *Client) GetPrivateChat(ctx context.Context, chatID int64) error {
	if chatID <= 0 {
		return errors.New("invalid Telegram private chat")
	}
	var chat Chat
	if err := c.callRequest(ctx, "getChat", struct {
		ChatID int64 `json:"chat_id"`
	}{chatID}, &chat, true); err != nil {
		return err
	}
	if chat.ID != chatID || chat.Type != "private" {
		return errors.New("invalid Telegram private chat identity")
	}
	return nil
}

// SendProtectedMessage calls prepare once before constructing any payload or
// attempting a POST. The callback owns final authorization/reservation; this
// method never retries. Failures without APIError.Rejected may follow delivery.
//
// Contract: https://core.telegram.org/bots/api#sendmessage and #linkpreviewoptions.
// protect_content prevents forwarding
// and saving, disable_notification silences sound, and is_disabled suppresses
// URL previews. These are best-effort restrictions, not recall or screenshot
// prevention. No parse_mode, entities, keyboard or URL fetching is used.
func (c *Client) SendProtectedMessage(ctx context.Context, chatID int64, prepare func(context.Context) (ProtectedMessage, error)) (int64, error) {
	if chatID <= 0 || prepare == nil {
		return 0, errors.New("invalid Telegram protected message request")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	message, err := prepare(ctx)
	if err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if message.Text == "" || !utf8.ValidString(message.Text) || strings.ContainsRune(message.Text, '\x00') || utf8.RuneCountInString(message.Text) > 4096 || message.NotAfter.IsZero() || message.NotAfter.Year() < 1 || message.NotAfter.Year() > 9999 {
		return 0, errors.New("invalid Telegram protected message")
	}
	if !message.NotAfter.After(time.Now()) {
		return 0, context.DeadlineExceeded
	}
	ctx, cancel := context.WithDeadline(ctx, message.NotAfter)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	payload := struct {
		ChatID              int64  `json:"chat_id"`
		Text                string `json:"text"`
		ProtectContent      bool   `json:"protect_content"`
		DisableNotification bool   `json:"disable_notification"`
		LinkPreviewOptions  struct {
			IsDisabled bool `json:"is_disabled"`
		} `json:"link_preview_options"`
	}{ChatID: chatID, Text: message.Text, ProtectContent: true, DisableNotification: true}
	payload.LinkPreviewOptions.IsDisabled = true
	var result struct {
		ID   int64 `json:"message_id"`
		Chat Chat  `json:"chat"`
	}
	if err := c.callRequest(ctx, "sendMessage", payload, &result, true); err != nil {
		return 0, err
	}
	if result.ID <= 0 || result.Chat.ID != chatID || result.Chat.Type != "private" {
		return 0, errors.New("invalid Telegram protected message identity")
	}
	return result.ID, nil
}
