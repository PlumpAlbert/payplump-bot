package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
)

// resolveServiceEnvironment fills only omitted business defaults. Explicit
// settings remain authoritative; the process environment is never mutated.
// Reads use the configured Ninja origin and never write to external services.
func resolveServiceEnvironment(ctx context.Context, get func(string) string) (func(string) string, error) {
	needCurrency := get("PAYMENTS_CURRENCY_ID") == "" || get("PAYMENTS_CURRENCY_CODE") == ""
	needTaxes := get("INVOICE_NINJA_INCLUSIVE_TAXES") == ""
	if !needCurrency && !needTaxes {
		return get, nil
	}
	timeout := 10 * time.Second
	if raw := get("INTEGRATION_HTTP_TIMEOUT"); raw != "" {
		var err error
		timeout, err = time.ParseDuration(raw)
		if err != nil || timeout <= 0 || timeout > 30*time.Second {
			return nil, errors.New("INTEGRATION_HTTP_TIMEOUT must be positive and at most 30s")
		}
	}
	if err := invoiceninja.ValidateClientConfiguration(get("INVOICE_NINJA_BASE_URL"), get("INVOICE_NINJA_API_TOKEN"), timeout); err != nil {
		return nil, fmt.Errorf("Invoice Ninja startup defaults: %w", err)
	}
	client, err := integrationhttp.New(get("INVOICE_NINJA_BASE_URL"), http.Header{
		"X-API-TOKEN": {get("INVOICE_NINJA_API_TOKEN")}, "X-Requested-With": {"XMLHttpRequest"},
	}, timeout)
	if err != nil {
		return nil, fmt.Errorf("Invoice Ninja startup defaults: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*timeout)
	defer cancel()
	resolved := make(map[string]string)
	currencyID := get("PAYMENTS_CURRENCY_ID")
	if currencyID == "" || needTaxes {
		var product struct {
			Data *struct {
				ID      string `json:"id"`
				Company *struct {
					Settings *struct {
						CurrencyID     json.RawMessage `json:"currency_id"`
						InclusiveTaxes *bool           `json:"inclusive_taxes"`
					} `json:"settings"`
				} `json:"company"`
			} `json:"data"`
		}
		_, err = client.Get(ctx, []string{"api", "v1", "products", get("INVOICE_NINJA_PRODUCT_ID")}, url.Values{"include": {"company"}}, &product)
		if err != nil {
			return nil, fmt.Errorf("Invoice Ninja product/company settings (INVOICE_NINJA_PRODUCT_ID): %w", err)
		}
		if product.Data == nil || product.Data.ID != get("INVOICE_NINJA_PRODUCT_ID") || product.Data.Company == nil || product.Data.Company.Settings == nil {
			return nil, errors.New("Invoice Ninja product response is missing the selected product or company.settings; set PAYMENTS_CURRENCY_ID, PAYMENTS_CURRENCY_CODE and INVOICE_NINJA_INCLUSIVE_TAXES explicitly to bypass defaults discovery")
		}
		settings := product.Data.Company.Settings
		if currencyID == "" {
			currencyID = ninjaCurrencyID(settings.CurrencyID)
			if currencyID == "" {
				return nil, errors.New("Invoice Ninja company.settings.currency_id is missing or invalid; set PAYMENTS_CURRENCY_ID explicitly")
			}
			resolved["PAYMENTS_CURRENCY_ID"] = currencyID
		}
		if needTaxes {
			if settings.InclusiveTaxes == nil {
				return nil, errors.New("Invoice Ninja company.settings.inclusive_taxes is missing; set INVOICE_NINJA_INCLUSIVE_TAXES explicitly")
			}
			resolved["INVOICE_NINJA_INCLUSIVE_TAXES"] = strconv.FormatBool(*settings.InclusiveTaxes)
		}
	}
	if needCurrency {
		var statics struct {
			Currencies []struct {
				ID   json.RawMessage `json:"id"`
				Code string          `json:"code"`
			} `json:"currencies"`
		}
		if _, err := client.Get(ctx, []string{"api", "v1", "statics"}, nil, &statics); err != nil {
			return nil, fmt.Errorf("Invoice Ninja currency lookup (GET /api/v1/statics): %w", err)
		}
		code := ""
		for _, currency := range statics.Currencies {
			if ninjaCurrencyID(currency.ID) != currencyID {
				continue
			}
			if code != "" || len(currency.Code) != 3 || strings.Trim(currency.Code, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
				return nil, errors.New("Invoice Ninja currency lookup returned an ambiguous or invalid ISO code")
			}
			code = currency.Code
		}
		if code == "" {
			return nil, errors.New("Invoice Ninja currency lookup did not return the configured/company currency; set PAYMENTS_CURRENCY_ID and PAYMENTS_CURRENCY_CODE explicitly")
		}
		if supplied := get("PAYMENTS_CURRENCY_CODE"); supplied != "" && supplied != code {
			return nil, errors.New("PAYMENTS_CURRENCY_CODE does not match the currency ID resolved from Invoice Ninja")
		}
		resolved["PAYMENTS_CURRENCY_CODE"] = code
	}
	return func(key string) string {
		if value, ok := resolved[key]; ok {
			return value
		}
		return get(key)
	}, nil
}

func ninjaCurrencyID(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) != nil {
		var number json.Number
		if json.Unmarshal(raw, &number) != nil {
			return ""
		}
		text = number.String()
	}
	id, err := strconv.ParseInt(text, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != text {
		return ""
	}
	return text
}
