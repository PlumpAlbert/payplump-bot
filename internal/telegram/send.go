package telegram

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"
)

// SendMessage sends plain text to a private chat. No parse_mode is used, so
// names and invoice data cannot inject formatting. The caller owns retries:
// a lost response does not prove Telegram failed to deliver the message.
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string) (int64, error) {
	if chatID <= 0 || text == "" || !utf8.ValidString(text) || strings.ContainsRune(text, '\x00') || utf8.RuneCountInString(text) > 4096 {
		return 0, errors.New("invalid Telegram private message")
	}
	var result struct {
		ID   int64 `json:"message_id"`
		Chat Chat  `json:"chat"`
	}
	err := c.call(ctx, "sendMessage", struct {
		ChatID int64  `json:"chat_id"`
		Text   string `json:"text"`
	}{chatID, text}, &result)
	if err != nil {
		return 0, err
	}
	if result.ID <= 0 || result.Chat.ID != chatID {
		return 0, errors.New("invalid Telegram sent message identity")
	}
	return result.ID, nil
}
