// Package sui provides validated access to the S-UI API.
// Its partial DTOs must never be used to save clients or protocol settings.
package sui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
)

var (
	errInvalidConfiguration = errors.New("invalid SUI configuration")
	errInvalidResponse      = errors.New("invalid SUI API response")
	errUnsuccessful         = errors.New("SUI API request was not successful")
)

// Account contains only the fields needed for read-only inspection. Name is
// the existing subscription identifier, not an authoritative Telegram ID.
// Description is the panel's display label; Remark is a separate field.
// Expiry and Volume retain the API's values without inferring access policy.
type Account struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	Enabled     bool    `json:"enable"`
	Description string  `json:"desc"`
	Remark      string  `json:"remark"`
	Inbounds    []int64 `json:"inbounds"`
	Expiry      int64   `json:"expiry"`
	Volume      int64   `json:"volume"`
}

type Inbound struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
	Tag  string `json:"tag"`
}

// Settings deliberately excludes TLS file paths, templates and other settings.
// SubPort is a string in S-UI v1.6.4, even when it contains a numeric port.
type Settings struct {
	SubURI    string `json:"subURI"`
	SubPath   string `json:"subPath"`
	SubPort   string `json:"subPort"`
	SubDomain string `json:"subDomain"`
}

type Client struct {
	http *integrationhttp.Client
}

// ValidateClientConfiguration checks the same read-client settings as New
// without constructing an HTTP transport.
func ValidateClientConfiguration(baseURL, token string, timeout time.Duration) error {
	base, err := parseBaseURL(baseURL)
	if err != nil {
		return err
	}
	for _, segment := range strings.Split(base.Path, "/") {
		if strings.EqualFold(segment, "apiv2") {
			return errInvalidConfiguration
		}
	}
	if token == "" || strings.TrimSpace(token) != token || strings.IndexFunc(token, unicode.IsControl) >= 0 || timeout <= 0 || timeout > 30*time.Second {
		return errInvalidConfiguration
	}
	return nil
}

// New takes the installed panel URL, including its custom path but excluding
// /apiv2 or an endpoint suffix. HTTP is supported for private NetBird networks;
// HTTPS retains normal certificate verification. Requests never redirect.
func New(baseURL, token string, timeout time.Duration) (*Client, error) {
	if err := ValidateClientConfiguration(baseURL, token, timeout); err != nil {
		return nil, err
	}
	httpClient, err := integrationhttp.New(baseURL, http.Header{"Token": {token}}, timeout)
	if err != nil {
		return nil, errInvalidConfiguration
	}
	return &Client{http: httpClient}, nil
}

func parseBaseURL(raw string) (*url.URL, error) {
	if integrationhttp.ValidateBaseURL(raw) != nil || strings.Contains(raw, "#") || strings.IndexFunc(raw, unicode.IsControl) >= 0 {
		return nil, errInvalidConfiguration
	}
	u, err := url.Parse(raw)
	if err != nil || strings.IndexFunc(u.Path, unicode.IsControl) >= 0 {
		return nil, errInvalidConfiguration
	}
	return u, nil
}

// get unwraps the API envelope, deliberately ignoring the remote msg field.
// A successful HTTP status alone does not mean that the API accepted a request.
func (c *Client) get(ctx context.Context, endpoint string, result any) error {
	var envelope struct {
		Success *bool           `json:"success"`
		Object  json.RawMessage `json:"obj"`
	}
	if _, err := c.http.Get(ctx, []string{"apiv2", endpoint}, nil, &envelope); err != nil {
		return err
	}
	if envelope.Success == nil {
		return errInvalidResponse
	}
	if !*envelope.Success {
		return errUnsuccessful
	}
	if len(envelope.Object) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Object), []byte("null")) || json.Unmarshal(envelope.Object, result) != nil {
		return errInvalidResponse
	}
	return nil
}

// Pointer fields distinguish missing/null scalars from valid zero values.
// Arrays must be present and non-null; empty arrays are supported.
type accountResponse struct {
	ID          *int64  `json:"id"`
	Name        *string `json:"name"`
	Enabled     *bool   `json:"enable"`
	Description *string `json:"desc"`
	Remark      *string `json:"remark"`
	Inbounds    []int64 `json:"inbounds"`
	Expiry      *int64  `json:"expiry"`
	Volume      *int64  `json:"volume"`
}

func (c *Client) ListAccounts(ctx context.Context) ([]Account, error) {
	var object struct {
		Clients []accountResponse `json:"clients"`
	}
	if err := c.get(ctx, "clients", &object); err != nil {
		return nil, err
	}
	if object.Clients == nil {
		return nil, errInvalidResponse
	}
	accounts := make([]Account, 0, len(object.Clients))
	for _, account := range object.Clients {
		if account.ID == nil || *account.ID <= 0 || account.Name == nil || *account.Name == "" || account.Enabled == nil || account.Description == nil || account.Remark == nil || account.Inbounds == nil || account.Expiry == nil || account.Volume == nil {
			return nil, errInvalidResponse
		}
		for _, id := range account.Inbounds {
			if id <= 0 {
				return nil, errInvalidResponse
			}
		}
		accounts = append(accounts, Account{
			ID: *account.ID, Name: *account.Name, Enabled: *account.Enabled,
			Description: *account.Description, Remark: *account.Remark,
			Inbounds: account.Inbounds, Expiry: *account.Expiry, Volume: *account.Volume,
		})
	}
	return accounts, nil
}

func (c *Client) ListInbounds(ctx context.Context) ([]Inbound, error) {
	var object struct {
		Inbounds []struct {
			ID   *int64  `json:"id"`
			Type *string `json:"type"`
			Tag  *string `json:"tag"`
		} `json:"inbounds"`
	}
	if err := c.get(ctx, "inbounds", &object); err != nil {
		return nil, err
	}
	if object.Inbounds == nil {
		return nil, errInvalidResponse
	}
	inbounds := make([]Inbound, 0, len(object.Inbounds))
	for _, inbound := range object.Inbounds {
		if inbound.ID == nil || *inbound.ID <= 0 || inbound.Type == nil || *inbound.Type == "" || inbound.Tag == nil || *inbound.Tag == "" {
			return nil, errInvalidResponse
		}
		inbounds = append(inbounds, Inbound{ID: *inbound.ID, Type: *inbound.Type, Tag: *inbound.Tag})
	}
	return inbounds, nil
}

func (c *Client) GetSettings(ctx context.Context) (Settings, error) {
	// The verified endpoint returns a flat map in obj, not obj.settings.
	var object struct {
		SubURI    *string `json:"subURI"`
		SubPath   *string `json:"subPath"`
		SubPort   *string `json:"subPort"`
		SubDomain *string `json:"subDomain"`
	}
	if err := c.get(ctx, "settings", &object); err != nil {
		return Settings{}, err
	}
	if object.SubURI == nil || object.SubPath == nil || object.SubPort == nil || object.SubDomain == nil {
		return Settings{}, errInvalidResponse
	}
	return Settings{SubURI: *object.SubURI, SubPath: *object.SubPath, SubPort: *object.SubPort, SubDomain: *object.SubDomain}, nil
}
