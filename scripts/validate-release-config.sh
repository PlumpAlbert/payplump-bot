#!/bin/sh
set -eu
umask 077
if [ "$#" -ne 1 ]; then echo "usage: validate-release-config.sh REGISTRY/IMAGE@sha256:DIGEST" >&2; exit 2; fi
image=$1
printf '%s\n' "$image" | grep -Eq '^[A-Za-z0-9.-]+(:[0-9]+)?/[A-Za-z0-9._/-]+@sha256:[a-f0-9]{64}$' || { echo "invalid immutable image reference" >&2; exit 1; }
command -v docker >/dev/null 2>&1 || { echo "docker compose is required for configuration validation" >&2; exit 1; }
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
if ! PAYPLUMP_IMAGE=$image docker compose --env-file /dev/null -f compose.release.yaml config --no-env-resolution --format json >"$tmp/config.json" 2>"$tmp/docker-error"; then echo "release compose configuration failed (Compose must support config --no-env-resolution)" >&2; exit 1; fi
python3 scripts/validate-release-config.py "$tmp/config.json" "$image"
