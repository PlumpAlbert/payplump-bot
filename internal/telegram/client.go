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
	"unicode"
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
	// Rejected is evidence of a validated, explicit 4xx rejection, not merely
	// a permanent error. False means delivery may have happened; never replay
	// automatically. A 429 is a rejection but still requires bot-wide cooldown.
	Rejected bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Telegram API error: HTTP %d, code %d", e.HTTPStatus, e.Code)
}
func (e *APIError) Permanent() bool {
	return e.Code >= 400 && e.Code < 500 && e.Code != 429 && e.Code != 408
}

func (c *Client) call(ctx context.Context, method string, payload any, result any) error {
	return c.callRequest(ctx, method, payload, result, false)
}

func (c *Client) callRequest(ctx context.Context, method string, payload any, result any, strict bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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
	// A nonempty write must not be replayed by net/http after an unknown
	// outcome. Redirects are separately disabled by NewClient.
	req.GetBody = nil
	if err := ctx.Err(); err != nil {
		return err
	}
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
	if (strict && !uniqueTelegramJSON(body)) || json.Unmarshal(body, &envelope) != nil || envelope.OK == nil {
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
		// HTTP 408 and all 5xx responses are ambiguous even if their body
		// claims a terminal error. Never infer rejection from the fallback code.
		saneHTTP := response.StatusCode == http.StatusOK || (response.StatusCode >= 400 && response.StatusCode < 500 && response.StatusCode != http.StatusRequestTimeout)
		// Even a null result contradicts definitive rejection. The raw field
		// also detects encoding/json aliases of result, not just its exact spelling.
		rejected := !*envelope.OK && len(envelope.Result) == 0 && envelope.Code >= 400 && envelope.Code < 500 && envelope.Code != http.StatusRequestTimeout && saneHTTP && uniqueTelegramJSON(body)
		return &APIError{HTTPStatus: response.StatusCode, Code: code, RetryAfter: time.Duration(delay) * time.Second, Rejected: rejected}
	}
	if strict {
		// Presence matters even for zero/null retry_after. Keep ordinary calls'
		// historical decoding/error behavior, but never accept contradictory
		// error facts as a protected-send or private-chat success.
		var facts struct {
			Parameters struct {
				RetryAfter json.RawMessage `json:"retry_after"`
			} `json:"parameters"`
		}
		if envelope.Code != 0 || json.Unmarshal(body, &facts) != nil || len(facts.Parameters.RetryAfter) != 0 {
			return errors.New("invalid Telegram response")
		}
	}
	if len(envelope.Result) == 0 || bytes.Equal(envelope.Result, []byte("null")) || json.Unmarshal(envelope.Result, result) != nil {
		return errors.New("invalid Telegram result")
	}
	return nil
}

// uniqueTelegramJSON rejects duplicate (including case-folded) object keys at
// every depth before encoding/json can erase conflicting identity/error facts.
func uniqueTelegramJSON(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if !telegramJSONValue(d, 0) {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}

func telegramJSONValue(d *json.Decoder, depth int) bool {
	if depth > 128 {
		return false
	}
	token, err := d.Token()
	if err != nil {
		return false
	}
	switch token {
	case json.Delim('{'):
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			name, ok := key.(string)
			name = telegramJSONFold(name)
			if err != nil || !ok || seen[name] {
				return false
			}
			seen[name] = true
			if !telegramJSONValue(d, depth+1) {
				return false
			}
		}
		token, err = d.Token()
		return err == nil && token == json.Delim('}')
	case json.Delim('['):
		for d.More() {
			if !telegramJSONValue(d, depth+1) {
				return false
			}
		}
		token, err = d.Token()
		return err == nil && token == json.Delim(']')
	default:
		_, delimiter := token.(json.Delim)
		return !delimiter
	}
}

// Match encoding/json's Unicode simple-fold equivalence, including ſ/S and
// K/K. Lowercasing alone misses aliases that could overwrite decoded fields.
func telegramJSONFold(name string) string {
	folded := []rune(name)
	for i, r := range folded {
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < folded[i] {
				folded[i] = next
			}
		}
	}
	return string(folded)
}

const maxUpdateBatch = 100
const pollRequestTimeout = 25 * time.Second

// DeleteWebhook switches a bot to polling without dropping queued updates.
func (c *Client) DeleteWebhook(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var accepted bool
	if err := c.callRequest(ctx, "deleteWebhook", struct {
		Drop bool `json:"drop_pending_updates"`
	}{false}, &accepted, true); err != nil {
		return err
	}
	if !accepted {
		return errors.New("Telegram did not accept webhook removal")
	}
	return nil
}

// GetUpdates uses a request-scoped longer HTTP deadline; outbound calls retain
// the client's ordinary ten-second timeout.
func (c *Client) GetUpdates(ctx context.Context, offset int64) ([]Update, error) {
	if offset < 0 || offset == math.MaxInt64 {
		return nil, errors.New("invalid Telegram polling offset")
	}
	ctx, cancel := context.WithTimeout(ctx, pollRequestTimeout+5*time.Second)
	defer cancel()
	var rawUpdates []json.RawMessage
	poller := *c
	client := *c.http
	client.Timeout = pollRequestTimeout + 5*time.Second
	poller.http = &client
	if err := poller.callRequest(ctx, "getUpdates", struct {
		Offset  int64    `json:"offset"`
		Limit   int      `json:"limit"`
		Timeout int      `json:"timeout"`
		Allowed []string `json:"allowed_updates"`
	}{offset, maxUpdateBatch, int(pollRequestTimeout.Seconds()), []string{"message"}}, &rawUpdates, true); err != nil {
		return nil, err
	}
	if len(rawUpdates) > maxUpdateBatch {
		return nil, errors.New("Telegram update batch exceeds size limit")
	}
	updates := make([]Update, 0, len(rawUpdates))
	previous := int64(-1)
	for _, raw := range rawUpdates {
		var wire struct {
			ID      *int64   `json:"update_id"`
			Message *Message `json:"message"`
		}
		if !uniqueTelegramJSON(raw) || json.Unmarshal(raw, &wire) != nil || wire.ID == nil || *wire.ID < 0 || *wire.ID == math.MaxInt64 || *wire.ID <= previous {
			return nil, errors.New("invalid Telegram update batch")
		}
		updates = append(updates, Update{ID: *wire.ID, Message: wire.Message})
		previous = *wire.ID
	}
	return updates, nil
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
