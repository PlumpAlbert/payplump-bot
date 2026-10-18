#!/bin/sh
# Safe PostgreSQL custom-archive restore harness. Dry run is the default.
set -eu
apply=0
if [ "${1:-}" = "--apply" ]; then apply=1; shift; fi
if [ "$#" -ne 2 ]; then echo "usage: restore-check.sh [--apply] CUSTOM_ARCHIVE TARGET_DB" >&2; exit 2; fi
archive=$1
target=$2
printf '%s\n' "$target" | grep -Eq '^payplump_restore_check_[A-Za-z0-9_]+$' || { echo "target must match payplump_restore_check_[A-Za-z0-9_]+" >&2; exit 2; }
[ -n "${PGDATABASE:-}" ] || { echo "PGDATABASE must identify the source database for comparison" >&2; exit 2; }
[ "$target" != "$PGDATABASE" ] || { echo "target must differ from source database" >&2; exit 2; }
case "${PGHOST:-}" in localhost|127.0.0.1|::1) ;; *) echo "restore-check only permits loopback PGHOST" >&2; exit 2 ;; esac
# Ignore libpq routing overrides and force all client invocations to IPv4 loopback.
unset PGSERVICE PGSERVICEFILE PGHOSTADDR PGOPTIONS PGHOST
export PGHOST=127.0.0.1
port=${PGPORT:-5432}
printf '%s\n' "$port" | grep -Eq '^[0-9]{1,5}$' && [ "$port" -ge 1 ] && [ "$port" -le 65535 ] || { echo "PGPORT must be a valid TCP port" >&2; exit 2; }
export PGPORT=$port
[ -f "$archive" ] && [ ! -L "$archive" ] && [ -r "$archive" ] || { echo "archive must be a readable regular non-symlink file" >&2; exit 2; }
[ "$(stat -f '%Lp' "$archive" 2>/dev/null || stat -c '%a' "$archive" 2>/dev/null)" = 600 ] || { echo "archive file must have mode 0600" >&2; exit 2; }
if [ -n "${PGPASSFILE:-}" ]; then
  [ -f "$PGPASSFILE" ] && [ ! -L "$PGPASSFILE" ] && [ "$(stat -f '%Lp' "$PGPASSFILE" 2>/dev/null || stat -c '%a' "$PGPASSFILE" 2>/dev/null)" = 600 ] || { echo "PGPASSFILE must be a regular mode-0600 file" >&2; exit 2; }
fi
command -v pg_restore >/dev/null 2>&1 || { echo "pg_restore is required" >&2; exit 2; }
command -v psql >/dev/null 2>&1 || { echo "psql is required" >&2; exit 2; }
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
if ! pg_restore --list "$archive" >/dev/null 2>&1; then echo "archive preflight failed" >&2; exit 1; fi
# Cap the captured listing even for a malformed archive with a huge TOC.
pg_restore --list "$archive" 2>/dev/null | head -c 1048577 >"$tmp/list"
[ -s "$tmp/list" ] || { echo "archive preflight failed" >&2; exit 1; }
[ "$(wc -c <"$tmp/list" | tr -d ' ')" -le 1048576 ] || { echo "archive listing exceeds 1 MiB" >&2; exit 1; }
# Permit only PostgreSQL's built-in plpgsql extension/language; reject database
# creation, event triggers, and every unknown extension before executing archive SQL.
if ! awk '/ EXTENSION - / && $NF != "plpgsql" { bad=1 }
          / EVENT TRIGGER / { bad=1 }
          / DATABASE - / { bad=1 }
          / PROCEDURAL LANGUAGE - / && $NF != "plpgsql" { bad=1 }
          END { exit bad }' "$tmp/list"; then
  echo "archive contains unsupported executable objects" >&2; exit 1
fi
if [ "$apply" -eq 0 ]; then echo "dry-run: custom archive readable; restore not performed"; exit 0; fi
# Owner-verified archive only: PostgreSQL restore can execute archive DDL and functions.
empty=$(psql -X -A -t -v ON_ERROR_STOP=1 --host=127.0.0.1 --port="$PGPORT" --dbname="$target" -c "SELECT CASE WHEN NOT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname NOT IN ('pg_catalog','information_schema') UNION ALL SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname NOT IN ('pg_catalog','information_schema') UNION ALL SELECT 1 FROM pg_type t JOIN pg_namespace n ON n.oid=t.typnamespace WHERE n.nspname NOT IN ('pg_catalog','information_schema') UNION ALL SELECT 1 FROM pg_namespace WHERE nspname NOT IN ('pg_catalog','information_schema','public')) THEN 1 ELSE 0 END" 2>/dev/null) || { echo "target preflight failed" >&2; exit 1; }
[ "$empty" = 1 ] || { echo "target database is not empty" >&2; exit 1; }
if ! pg_restore --exit-on-error --no-owner --no-acl --single-transaction --host=127.0.0.1 --port="$PGPORT" --dbname="$target" "$archive" >/dev/null 2>"$tmp/restore-error"; then echo "restore failed; inspect disposable target privately" >&2; exit 1; fi
printf 'restore completed into disposable database %s; application was not started\n' "$target"
