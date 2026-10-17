package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"time"
)

const maxResponseBytes = 4 << 20

type Client struct {
	token   string
	baseURL string
	http    *http.Client
}

func NewClient(token string) *Client {
	return &Client{token: token, baseURL: "https://api.telegram.org", http: &http.Client{
		Timeout: 40 * time.Second,
		// Never forward a token-bearing URL to a redirect destination.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// APIError deliberately excludes Telegram descriptions and request URLs, which
// can contain credentials or message contents. RetryAfter is a minimum delay.
type APIError struct {
	HTTPStatus int
	Code       int
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Telegram API error: HTTP %d, code %d", e.HTTPStatus, e.Code)
}
func (e *APIError) Permanent() bool {
	return e.Code >= 400 && e.Code < 500 && e.Code != 429 && e.Code != 408
}

func (c *Client) call(ctx context.Context, method string, payload any, result any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return errors.New("invalid Telegram request")
	}
	endpoint := c.baseURL + "/bot" + url.PathEscape(c.token) + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return errors.New("invalid Telegram request")
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("Telegram request failed")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("could not read Telegram response")
	}
	if len(body) > maxResponseBytes {
		return errors.New("Telegram response exceeds size limit")
	}
	var envelope struct {
		OK         *bool           `json:"ok"`
		Code       int             `json:"error_code"`
		Result     json.RawMessage `json:"result"`
		Parameters struct {
			RetryAfter int64 `json:"retry_after"`
		} `json:"parameters"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.OK == nil {
		// HTTP authentication/conflict errors remain terminal even with a broken body.
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return &APIError{HTTPStatus: response.StatusCode, Code: response.StatusCode}
		}
		return errors.New("invalid Telegram response")
	}
	if !*envelope.OK || response.StatusCode < 200 || response.StatusCode >= 300 {
		code := envelope.Code
		if code == 0 {
			code = response.StatusCode
		}
		delay := envelope.Parameters.RetryAfter
		if delay < 0 || delay > math.MaxInt64/int64(time.Second) {
			return errors.New("invalid Telegram retry_after")
		}
		return &APIError{HTTPStatus: response.StatusCode, Code: code, RetryAfter: time.Duration(delay) * time.Second}
	}
	if len(envelope.Result) == 0 || bytes.Equal(envelope.Result, []byte("null")) || json.Unmarshal(envelope.Result, result) != nil {
		return errors.New("invalid Telegram result")
	}
	return nil
}

func (c *Client) GetMe(ctx context.Context) (User, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var user User
	if err := c.call(ctx, "getMe", struct{}{}, &user); err != nil {
		return User{}, err
	}
	if user.ID <= 0 || !user.IsBot || user.Username == "" {
		return User{}, errors.New("invalid Telegram bot identity")
	}
	return user, nil
}

func (c *Client) GetUpdates(ctx context.Context, offset int64) ([]Update, error) {
	if offset < 0 {
		return nil, errors.New("invalid Telegram offset")
	}
	var wire []struct {
		ID      *int64   `json:"update_id"`
		Message *Message `json:"message"`
	}
	err := c.call(ctx, "getUpdates", struct {
		Offset         int64    `json:"offset"`
		Limit          int      `json:"limit"`
		Timeout        int      `json:"timeout"`
		AllowedUpdates []string `json:"allowed_updates"`
	}{offset, 100, 30, []string{"message"}}, &wire)
	if err != nil {
		return nil, err
	}
	if len(wire) > 100 {
		return nil, errors.New("too many Telegram updates")
	}
	updates := make([]Update, 0, len(wire))
	for i, u := range wire {
		if u.ID == nil || *u.ID < 0 || *u.ID == math.MaxInt64 || (i > 0 && *u.ID <= updates[i-1].ID) {
			return nil, errors.New("invalid Telegram update sequence")
		}
		updates = append(updates, Update{ID: *u.ID, Message: u.Message})
	}
	return updates, nil
}
