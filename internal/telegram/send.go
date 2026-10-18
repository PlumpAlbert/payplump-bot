package telegram

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/PlumpAlbert/payplump-bot/internal/clientmenu"
)

// SendMessage sends plain text to a private chat. No parse_mode is used, so
// names and invoice data cannot inject formatting. The caller owns retries:
// a lost response does not prove Telegram failed to deliver the message.
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string) (int64, error) {
	return c.sendPrivateMessage(ctx, chatID, text, false)
}

// SendMenuMessage sends plain text with the fixed private client menu keyboard.
// Buttons contain only labels, never URLs, callbacks or external identifiers.
func (c *Client) SendMenuMessage(ctx context.Context, chatID int64, text string) (int64, error) {
	return c.sendPrivateMessage(ctx, chatID, text, true)
}

type menuKeyboard struct {
	Keyboard       [][]string `json:"keyboard"`
	ResizeKeyboard bool       `json:"resize_keyboard"`
	IsPersistent   bool       `json:"is_persistent"`
}

func (c *Client) sendPrivateMessage(ctx context.Context, chatID int64, text string, menu bool) (int64, error) {
	if chatID <= 0 || text == "" || !utf8.ValidString(text) || strings.ContainsRune(text, '\x00') || utf8.RuneCountInString(text) > 4096 {
		return 0, errors.New("invalid Telegram private message")
	}
	var result struct {
		ID   int64 `json:"message_id"`
		Chat Chat  `json:"chat"`
	}
	payload := struct {
		ChatID      int64         `json:"chat_id"`
		Text        string        `json:"text"`
		ReplyMarkup *menuKeyboard `json:"reply_markup,omitempty"`
	}{ChatID: chatID, Text: text}
	if menu {
		payload.ReplyMarkup = &menuKeyboard{
			Keyboard: [][]string{
				{clientmenu.InvoiceLabel},
				{clientmenu.StatusLabel},
				{clientmenu.ConfigurationLabel},
				{clientmenu.HelpLabel},
			},
			ResizeKeyboard: true,
			IsPersistent:   true,
		}
	}
	err := c.call(ctx, "sendMessage", payload, &result)
	if err != nil {
		return 0, err
	}
	if result.ID <= 0 || result.Chat.ID != chatID {
		return 0, errors.New("invalid Telegram sent message identity")
	}
	return result.ID, nil
}
