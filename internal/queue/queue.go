// Package queue defines durable work independently of its PostgreSQL storage.
package queue

import (
	"errors"
	"strings"
	"unicode/utf8"
)

var (
	ErrLeaseLost = errors.New("work lease expired or was superseded")
	ErrConflict  = errors.New("idempotency key was reused with different message data")
)

// Attempt is a monotonically increasing fencing token, not a retry limit.
// Every completion or retry must present the exact attempt that was claimed.
type Lease struct {
	ID      int64
	Attempt int
}

type Operation struct {
	Lease
	ClientID int64
	Kind     string
}

// Draft keys are scoped to a bot and must identify the business notification,
// not a delivery attempt. Text and destination become immutable on insertion.
type Draft struct {
	BotID      int64
	ChatID     int64
	Key        string
	Text       string
	ClientMenu bool
}

func (d Draft) Validate() error {
	if d.BotID <= 0 || d.ChatID <= 0 || d.Key == "" || len(d.Key) > 256 || !utf8.ValidString(d.Key) || strings.ContainsRune(d.Key, '\x00') || d.Text == "" || !utf8.ValidString(d.Text) || strings.ContainsRune(d.Text, '\x00') || utf8.RuneCountInString(d.Text) > 4096 {
		return errors.New("invalid outbox message")
	}
	return nil
}

type Message struct {
	Lease
	BotID      int64
	ChatID     int64
	Text       string
	ClientMenu bool
}
