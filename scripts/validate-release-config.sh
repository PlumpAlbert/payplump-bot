#!/bin/sh
set -eu
if [ "$#" -ne 1 ]; then echo "usage: validate-release-config.sh REGISTRY/IMAGE@sha256:DIGEST" >&2; exit 2; fi
image=$1
printf '%s\n' "$image" | grep -Eq '^[A-Za-z0-9.-]+(:[0-9]+)?/[A-Za-z0-9._/-]+@sha256:[a-f0-9]{64}$' || { echo "image must be registry-qualified and pinned by sha256 digest" >&2; exit 1; }
f=compose.release.yaml
for required in 'user: "65532:65532"' 'read_only: true' 'cap_drop: [ALL]' 'no-new-privileges:true' 'max-size: "10m"' 'max-file: "3"' 'TELEGRAM_ENABLED: "false"' 'REGISTRATION_ENABLED: "false"' 'PAYMENTS_ENABLED: "false"' 'ACTIVATION_ENABLED: "false"' 'CONFIGURATION_DELIVERY_ENABLED: "false"' 'BROADCAST_ENABLED: "false"' 'LIFECYCLE_ENABLED: "false"'; do
  grep -Fq "$required" "$f" || { echo "release template missing required safe setting" >&2; exit 1; }
done
command -v docker >/dev/null 2>&1 || { echo "docker compose is required for configuration validation" >&2; exit 1; }
PAYPLUMP_IMAGE=$image docker compose -f "$f" config --quiet || { echo "release compose configuration is invalid" >&2; exit 1; }
grep -Eq '^[[:space:]]*privileged:[[:space:]]*true' "$f" && { echo "privileged containers are forbidden" >&2; exit 1; }
printf 'release template safe settings validated; image digest pinned\n'
