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
		Timeout: 10 * time.Second,
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

// SetWebhook preserves pending updates and limits concurrent deliveries for the
// initial single-process service. Secret validation matches Telegram's contract.
func (c *Client) SetWebhook(ctx context.Context, webhookURL, secret string) error {
	if !validWebhookURL(webhookURL) || !validWebhookSecret(secret) {
		return errors.New("invalid Telegram webhook configuration")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var accepted bool
	err := c.call(ctx, "setWebhook", struct {
		URL         string   `json:"url"`
		Secret      string   `json:"secret_token"`
		Allowed     []string `json:"allowed_updates"`
		Connections int      `json:"max_connections"`
		Drop        bool     `json:"drop_pending_updates"`
	}{webhookURL, secret, []string{"message"}, 1, false}, &accepted)
	if err != nil {
		return err
	}
	if !accepted {
		return errors.New("Telegram did not accept webhook registration")
	}
	return nil
}
