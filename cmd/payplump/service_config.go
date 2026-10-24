package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

var serviceFeatures = []string{
	"REGISTRATION_ENABLED", "PAYMENTS_ENABLED", "ACTIVATION_ENABLED",
	"CONFIGURATION_DELIVERY_ENABLED", "BROADCAST_ENABLED", "INVOICE_LIFECYCLE_ENABLED",
	"INVOICE_OVERDUE_ENFORCEMENT_ENABLED", "INVOICE_RENEWAL_ENABLED",
	"IMPORTED_RUNTIME_ENABLED", "IMPORTED_EFFECTS_ENABLED",
}

// healthEnvironment explicitly disables every service feature without mutating the
// process environment. Business settings remain readable, but disabled features
// must not require their credentials.
func healthEnvironment(get func(string) string) func(string) string {
	return func(key string) string {
		if key == "TELEGRAM_ENABLED" {
			return "false"
		}
		for _, feature := range serviceFeatures {
			if key == feature {
				return "false"
			}
		}
		return get(key)
	}
}

// serviceEnvironment applies product defaults only to ordinary startup.
// Explicit maintenance and read-only commands are dispatched before this call.
func serviceEnvironment(get func(string) string) (func(string) string, bool, error) {
	// Diagnose old default-off templates before reporting their missing business
	// credentials, so migration is explicit rather than mistaken for a mode.
	for _, key := range append(append([]string{"TELEGRAM_ENABLED"}, serviceFeatures...), "ACTIVATION_EXCLUSIVE_WRITER", "INVOICE_OVERDUE_EXCLUSIVE_WRITER", "INVOICE_RENEWAL_EXCLUSIVE_WRITER") {
		if raw := get(key); raw != "" {
			v, err := strconv.ParseBool(raw)
			if err != nil || !v {
				return nil, false, fmt.Errorf("legacy setting %s prevents normal service startup; migrate the old configuration by removing feature switches (see docs/configuration.md)", key)
			}
		}
	}
	for _, key := range []string{"TELEGRAM_BOT_TOKEN", "TELEGRAM_ADMIN_IDS", "TELEGRAM_WEBHOOK_URL", "TELEGRAM_WEBHOOK_SECRET", "INVOICE_NINJA_BASE_URL", "INVOICE_NINJA_API_TOKEN", "INVOICE_NINJA_PRODUCT_ID", "INVOICE_NINJA_INCLUSIVE_TAXES", "INVOICE_NINJA_WEBHOOK_SECRET", "INVOICE_LIFECYCLE_WEBHOOK_SECRET", "PAYMENTS_CURRENCY_ID", "PAYMENTS_CURRENCY_CODE", "SUI_BASE_URL", "SUI_API_TOKEN", "SUI_INBOUND_IDS", "SUI_SUBSCRIPTION_BASE_URL", "PAYMENT_PHONE", "TZ"} {
		if get(key) == "" {
			return nil, false, fmt.Errorf("%s is required for normal service startup", key)
		}
	}
	if _, err := time.LoadLocation(get("TZ")); err != nil {
		return nil, false, errors.New("TZ must name a supported calendar timezone")
	}
	for key, supported := range map[string]string{"SUI_VERSION": "1.6.4", "INVOICE_NINJA_VERSION": "5.13.47"} {
		if value := get(key); value != "" && strings.TrimPrefix(value, "v") != supported {
			return nil, false, fmt.Errorf("%s does not match the supported API version %s", key, supported)
		}
	}
	if value := get("INVOICE_NINJA_TELEGRAM_ID_FIELD"); value != "" && value != "id_number" {
		return nil, false, errors.New("INVOICE_NINJA_TELEGRAM_ID_FIELD must be id_number for the supported write profile")
	}
	code := get("PAYMENTS_CURRENCY_CODE")
	if len(code) != 3 || code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' || code[2] < 'A' || code[2] > 'Z' {
		return nil, false, errors.New("PAYMENTS_CURRENCY_CODE must identify the native currency as three uppercase ASCII letters")
	}
	defaults := map[string]string{
		"TELEGRAM_ENABLED": "true", "ACTIVATION_EXCLUSIVE_WRITER": "true",
		"INVOICE_OVERDUE_EXCLUSIVE_WRITER": "true", "INVOICE_RENEWAL_EXCLUSIVE_WRITER": "true",
		"CONFIGURATION_POLICY_REVISION": "1", "BROADCAST_POLICY_REVISION": "1",
		"INVOICE_NINJA_TELEGRAM_ID_FIELD": "id_number", "INVOICE_NINJA_VERSION": "5.13.47", "SUI_VERSION": "1.6.4",
	}
	for _, key := range serviceFeatures {
		defaults[key] = "true"
	}
	for _, key := range []string{"ACTIVATION_ALLOWED_TELEGRAM_IDS", "INVOICE_LIFECYCLE_ALLOWED_TELEGRAM_IDS", "CONFIGURATION_POLICY_REVISION", "BROADCAST_POLICY_REVISION", "IMPORTED_RUNTIME_REVISION", "IMPORTED_EFFECTS_REVISION", "IMPORT_VERIFY_BOT_ID", "IMPORT_VERIFY_ALLOWED_TELEGRAM_IDS", "IMPORT_VERIFY_EXCLUDED_TELEGRAM_IDS", "IMPORT_VERIFY_CURRENCY_CODE", "IMPORT_VERIFY_PRODUCT_KEY", "IMPORT_VERIFY_MAX_RECORDS", "IMPORT_VERIFY_MAX_INVENTORY_RECORDS", "IMPORT_VERIFY_MAX_HISTORICAL_ANCHORS", "IMPORT_VERIFY_EVIDENCE_TTL"} {
		if get(key) != "" {
			return nil, false, fmt.Errorf("legacy setting %s requires configuration migration; managed scope and revisions are internal (see docs/configuration.md)", key)
		}
	}
	return func(key string) string {
		if v := get(key); v != "" {
			return v
		}
		return defaults[key]
	}, true, nil
}
