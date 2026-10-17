// Package invoiceninja provides read-only Invoice Ninja v5 API access.
package invoiceninja

import (
	"encoding/json"
	"errors"
	"strconv"
)

// Pagination contains only local paging information. Remote links are ignored.
type Pagination struct {
	Total       int `json:"total"`
	Count       int `json:"count"`
	PerPage     int `json:"per_page"`
	CurrentPage int `json:"current_page"`
	TotalPages  int `json:"total_pages"`
}

type Page[T any] struct {
	Data       []T
	Pagination Pagination
	AppVersion string
}

type Customer struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Number       string `json:"number"`
	IDNumber     string `json:"id_number"`
	CustomValue1 string `json:"custom_value1"`
	CustomValue2 string `json:"custom_value2"`
	CustomValue3 string `json:"custom_value3"`
	CustomValue4 string `json:"custom_value4"`
	IsDeleted    bool   `json:"is_deleted"`
	ArchivedAt   int64  `json:"archived_at"`
}

func (c Customer) Active() bool { return !c.IsDeleted && c.ArchivedAt == 0 }

// TelegramID reads only an explicitly configured external-identity field. Ninja
// record IDs and customer numbers are never interpreted as Telegram IDs.
func (c Customer) TelegramID(field string) (int64, error) {
	var value string
	switch field {
	case "id_number":
		value = c.IDNumber
	case "custom_value1":
		value = c.CustomValue1
	case "custom_value2":
		value = c.CustomValue2
	case "custom_value3":
		value = c.CustomValue3
	case "custom_value4":
		value = c.CustomValue4
	default:
		return 0, errors.New("invalid Telegram identifier field")
	}
	if len(value) == 0 || value[0] < '1' || value[0] > '9' {
		return 0, errors.New("invalid Telegram identifier")
	}
	for i := 1; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return 0, errors.New("invalid Telegram identifier")
		}
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, errors.New("invalid Telegram identifier")
	}
	return id, nil
}

type Product struct {
	ID         string      `json:"id"`
	ProductKey string      `json:"product_key"`
	Notes      string      `json:"notes"`
	Price      json.Number `json:"price"`
	IsDeleted  bool        `json:"is_deleted"`
	ArchivedAt int64       `json:"archived_at"`
}

func (p Product) Active() bool { return !p.IsDeleted && p.ArchivedAt == 0 }

type Invoice struct {
	ID          string      `json:"id"`
	ClientID    string      `json:"client_id"`
	RecurringID string      `json:"recurring_id"`
	StatusID    string      `json:"status_id"`
	Number      string      `json:"number"`
	Date        string      `json:"date"`
	DueDate     string      `json:"due_date"`
	Amount      json.Number `json:"amount"`
	Balance     json.Number `json:"balance"`
	IsDeleted   bool        `json:"is_deleted"`
	ArchivedAt  int64       `json:"archived_at"`
}

type Payment struct {
	ID           string        `json:"id"`
	ClientID     string        `json:"client_id"`
	Date         string        `json:"date"`
	Amount       json.Number   `json:"amount"`
	Paymentables []Paymentable `json:"paymentables"`
	Invoices     []Invoice     `json:"invoices"`
	IsDeleted    bool          `json:"is_deleted"`
	ArchivedAt   int64         `json:"archived_at"`
}

type Paymentable struct {
	ID         string      `json:"id"`
	InvoiceID  string      `json:"invoice_id"`
	PaymentID  string      `json:"payment_id"`
	Amount     json.Number `json:"amount"`
	Refunded   json.Number `json:"refunded"`
	ArchivedAt int64       `json:"archived_at"`
}

type RecurringInvoice struct {
	ID           string `json:"id"`
	ClientID     string `json:"client_id"`
	StatusID     string `json:"status_id"`
	FrequencyID  string `json:"frequency_id"`
	NextSendDate string `json:"next_send_date"`
	LastSentDate string `json:"last_sent_date"`
	IsDeleted    bool   `json:"is_deleted"`
	ArchivedAt   int64  `json:"archived_at"`
}
