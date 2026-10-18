#!/usr/bin/env bash
# Run tests without inheriting production provider endpoints, tokens or actors.
# Database settings are retained; integration helpers always create own schemas.
# This script intentionally never sources .env or starts the application.
set -euo pipefail

for prefix in TELEGRAM_ REGISTRATION_ PAYMENTS_ ACTIVATION_ CONFIGURATION_ INVOICE_NINJA_ SUI_ LIFECYCLE_ RENEWALS_ BROADCAST_ IMPORT_ PAYMENT_PHONE PAYMENT_BANK PAYMENT_RECIPIENT; do
  while IFS= read -r name; do
    unset "$name"
  done < <(compgen -A variable "$prefix" || true)
done
# Integration helpers accept TEST_DATABASE_URL or PG*. Never inherit an app URL.
export DATABASE_URL=""
export TELEGRAM_ENABLED=false REGISTRATION_ENABLED=false PAYMENTS_ENABLED=false
export ACTIVATION_ENABLED=false CONFIGURATION_DELIVERY_ENABLED=false
export LIFECYCLE_ENABLED=false RENEWALS_ENABLED=false BROADCAST_ENABLED=false IMPORT_ENABLED=false

if [ "$#" -eq 0 ]; then
  set -- -race -tags=integration -count=1 -timeout=60m ./...
fi
exec go test "$@"
