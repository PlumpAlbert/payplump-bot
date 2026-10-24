#!/usr/bin/env python3
"""Conservative checks over Docker Compose's no-env-resolution JSON output."""
import json
import os
from pathlib import Path
import re
import sys

SAFE = "release compose configuration rejected"
FORBIDDEN_ENV = re.compile(r"(TOKEN|SECRET|PASSWORD|API_KEY|DATABASE_URL)", re.I)
CLEARED_DATABASE_ENV = {
    "DATABASE_URL", "PGPASSWORD", "PGOPTIONS", "PGSERVICE", "PGSERVICEFILE",
    "PGSSLKEY", "PGSSLCERT", "PGSSLSNI", "PGSSLROOTCERT", "PGSSLPASSWORD",
    "PGSSLNEGOTIATION", "PGTARGETSESSIONATTRS", "PGTZ", "PGMINPROTOCOLVERSION",
    "PGMAXPROTOCOLVERSION", "PGCHANNELBINDING", "PGREQUIREAUTH", "PGAPPNAME",
    "PGCONNECT_TIMEOUT",
}
ROOT = Path(__file__).resolve().parent.parent


def reject():
    raise ValueError(SAFE)


def private_external_file(path, *, check_mode=False):
    if not isinstance(path, str) or not os.path.isabs(path):
        reject()
    candidate = Path(path)
    if candidate.is_symlink():
        reject()
    resolved = candidate.resolve(strict=True)
    if resolved == ROOT or ROOT in resolved.parents or not resolved.is_file():
        reject()
    if check_mode and (resolved.stat().st_mode & 0o777) != 0o600:
        reject()
    return str(resolved)


def validate(path, expected):
    try:
        if not re.fullmatch(r"[A-Za-z0-9.-]+(?::[0-9]+)?/[A-Za-z0-9._/-]+@sha256:[a-f0-9]{64}", expected):
            reject()
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
        if s.get("cap_drop") != ["ALL"] or s.get("cap_add"):
            reject()
        if s.get("security_opt") != ["no-new-privileges:true"]:
            reject()
        if any(s.get(k) for k in ("ports", "devices", "device_cgroup_rules", "volumes_from", "sysctls")) or any(s.get(k) is not None for k in ("entrypoint", "command", "build", "post_start", "pre_stop")) or s.get("userns_mode") == "host":
            reject()
        files = s.get("env_file")
        if not isinstance(files, list) or len(files) != 1 or not isinstance(files[0], dict) or files[0].get("required", True) is not True or set(files[0]) - {"path", "required"}:
            reject()
        private_external_file(files[0].get("path"), check_mode=True)
        env = s.get("environment")
        if not isinstance(env, dict):
            reject()
        allowed_env = {"HTTP_ADDR", "DB_MAX_CONNS", "PGHOST", "PGPORT", "PGUSER", "PGDATABASE", "PGSSLMODE", "PGPASSFILE"} | CLEARED_DATABASE_ENV
        if set(env) - allowed_env or any(FORBIDDEN_ENV.search(str(k)) and k not in CLEARED_DATABASE_ENV for k in env):
            reject()
        if any(env.get(k) != "" for k in CLEARED_DATABASE_ENV):
            reject()
        if not all(isinstance(env.get(k), str) and env[k] for k in ("PGHOST", "PGUSER", "PGDATABASE")):
            reject()
        if env.get("PGSSLMODE") != "verify-full" or env.get("PGPASSFILE") != "/run/secrets/pgpass":
            reject()
        if s.get("logging", {}).get("driver") != "json-file" or s["logging"].get("options", {}).get("max-size") != "10m" or s["logging"].get("options", {}).get("max-file") != "3":
            reject()
        healthcheck = s.get("healthcheck")
        if not isinstance(healthcheck, dict) or healthcheck.get("test") != ["CMD", "/payplump", "healthcheck"] or healthcheck.get("disable", False) is not False:
            reject()
        if s.get("volumes") or cfg.get("volumes"):
            reject()
        secrets = s.get("secrets")
        if not isinstance(secrets, list) or len(secrets) != 1 or not (secrets[0] == "pgpass" or isinstance(secrets[0], dict) and secrets[0].get("source") == "pgpass" and secrets[0].get("target", "pgpass") in ("pgpass", "/run/secrets/pgpass")):
            reject()
        if set(cfg.get("secrets", {})) != {"pgpass"} or not isinstance(cfg["secrets"]["pgpass"], dict):
            reject()
        private_external_file(cfg["secrets"]["pgpass"].get("file"), check_mode=True)
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
