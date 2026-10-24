#!/bin/sh
set -eu
umask 077
if [ "$#" -ne 1 ]; then echo "usage: validate-release-config.sh REGISTRY/IMAGE@sha256:DIGEST" >&2; exit 2; fi
image=$1
printf '%s\n' "$image" | grep -Eq '^[A-Za-z0-9.-]+(:[0-9]+)?/[A-Za-z0-9._/-]+@sha256:[a-f0-9]{64}$' || { echo "invalid immutable image reference" >&2; exit 1; }
command -v docker >/dev/null 2>&1 || { echo "docker compose is required for configuration validation" >&2; exit 1; }
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
# Some older Compose versions still parse env_file contents despite this flag.
# Reject them using an owned malformed synthetic file BEFORE pointing Compose
# at the operator's runtime file. No version string alone proves the capability.
python3 - "$tmp" <<'PY'
import json
from pathlib import Path
import sys
p = Path(sys.argv[1])
f = p / "opaque.env"
f.write_text('UNTERMINATED="SYNTHETIC_OPAQUE_PROBE\n')
(p / "probe.yaml").write_text('name: payplump-config-probe\nservices:\n  probe:\n    image: scratch\n    env_file:\n      - path: ' + json.dumps(str(f)) + '\n        required: true\n')
PY
if ! docker compose --env-file /dev/null -f "$tmp/probe.yaml" config --no-env-resolution --format json >"$tmp/probe.json" 2>"$tmp/probe-error" || grep -q 'SYNTHETIC_OPAQUE_PROBE' "$tmp/probe.json"; then
  echo "Compose must support opaque env_file metadata without parsing its contents (tested: v5.6.1)" >&2; exit 1
fi
if ! PAYPLUMP_IMAGE=$image docker compose --env-file /dev/null -f compose.release.yaml config --no-env-resolution --format json >"$tmp/config.json" 2>"$tmp/docker-error"; then echo "release compose configuration failed (Compose must support config --no-env-resolution)" >&2; exit 1; fi
python3 scripts/validate-release-config.py "$tmp/config.json" "$image"
