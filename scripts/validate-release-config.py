#!/usr/bin/env python3
"""Conservative checks over Docker Compose's resolved JSON configuration."""
import json
import re
import sys

SAFE = "release compose configuration rejected"
FLAGS = ("TELEGRAM_ENABLED", "REGISTRATION_ENABLED", "PAYMENTS_ENABLED", "ACTIVATION_ENABLED",
         "CONFIGURATION_DELIVERY_ENABLED", "BROADCAST_ENABLED", "LIFECYCLE_ENABLED")
FORBIDDEN_ENV = re.compile(r"(TOKEN|SECRET|PASSWORD|API_KEY|DATABASE_URL|PGPASSWORD)", re.I)


def reject():
    raise ValueError(SAFE)


def validate(path, expected):
    try:
        with open(path, "rb") as f:
            data = f.read(1_048_577)
        if len(data) > 1_048_576:
            reject()
        cfg = json.loads(data)
        services = cfg.get("services")
        if not isinstance(services, dict) or set(services) != {"payplump"}:
            reject()
        s = services["payplump"]
        if s.get("image") != expected or s.get("user") != "65532:65532" or s.get("read_only") is not True:
            reject()
        if s.get("privileged", False) is not False or any(k in cfg for k in ("host_pid", "host_ipc", "host_network")):
            reject()
        if s.get("pid") == "host" or s.get("ipc") == "host" or s.get("network_mode") == "host":
            reject()
        if s.get("cap_drop") != ["ALL"]:
            reject()
        sec = s.get("security_opt", [])
        if not isinstance(sec, list) or not any(x == "no-new-privileges:true" for x in sec):
            reject()
        if s.get("ports") or s.get("devices") or s.get("env_file") or "entrypoint" in s or "command" in s:
            reject()
        env = s.get("environment")
        if not isinstance(env, dict) or any(env.get(k) != "false" for k in FLAGS):
            reject()
        if any(FORBIDDEN_ENV.search(str(k)) for k in env):
            reject()
        if not all(isinstance(env.get(k), str) and env[k] for k in ("PGHOST", "PGUSER", "PGDATABASE")):
            reject()
        if env.get("PGSSLMODE") != "verify-full" or env.get("PGPASSFILE") != "/run/secrets/pgpass":
            reject()
        if s.get("logging", {}).get("driver") != "json-file" or s["logging"].get("options", {}).get("max-size") != "10m" or s["logging"].get("options", {}).get("max-file") != "3":
            reject()
        volumes = s.get("volumes", [])
        if not isinstance(volumes, list):
            reject()
        for v in volumes:
            if not isinstance(v, dict) or v.get("type") != "volume" or v.get("source") == "/var/run/docker.sock":
                reject()
        secrets = s.get("secrets")
        if not isinstance(secrets, list) or not any((x == "pgpass" or isinstance(x, dict) and x.get("source") == "pgpass" and x.get("target", "pgpass") == "pgpass") for x in secrets):
            reject()
        if not isinstance(cfg.get("secrets", {}).get("pgpass"), dict):
            reject()
    except Exception:
        reject()


if __name__ == "__main__":
    if len(sys.argv) != 3:
        print("usage: validate-release-config.py RESOLVED_JSON EXPECTED_IMAGE", file=sys.stderr)
        raise SystemExit(2)
    try:
        validate(sys.argv[1], sys.argv[2])
    except Exception:
        print(SAFE, file=sys.stderr)
        raise SystemExit(1)
    print("resolved release configuration validated")
