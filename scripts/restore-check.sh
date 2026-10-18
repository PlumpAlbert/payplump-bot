#!/bin/sh
# Safe PostgreSQL custom-archive restore harness. Dry run is the default.
set -eu
umask 077
apply=0
if [ "${1:-}" = "--apply" ]; then apply=1; shift; fi
if [ "$#" -ne 2 ]; then echo "usage: restore-check.sh [--apply] CUSTOM_ARCHIVE TARGET_DB" >&2; exit 2; fi
archive=$1
target=$2
printf '%s\n' "$target" | grep -Eq '^payplump_restore_check_[A-Za-z0-9_]+$' && [ "${#target}" -le 63 ] || { echo "invalid target database name" >&2; exit 2; }
source=${PGDATABASE:-}
printf '%s\n' "$source" | grep -Eq '^[A-Za-z0-9_]+$' && [ "${#source}" -le 63 ] || { echo "PGDATABASE must be a simple source database name" >&2; exit 2; }
[ "$target" != "$source" ] || { echo "target must differ from source database" >&2; exit 2; }
case "${PGHOST:-}" in localhost|127.0.0.1|::1) ;; *) echo "restore-check only permits loopback PGHOST" >&2; exit 2 ;; esac
unset PGSERVICE PGSERVICEFILE PGHOSTADDR PGOPTIONS PGHOST
export PGHOST=127.0.0.1 PGDATABASE="$source" PGCONNECT_TIMEOUT=5
# Preserve an explicitly supplied password/TLS policy without ever printing it.
# Every connection is forced to loopback, and tools must never prompt.
port=${PGPORT:-5432}
printf '%s\n' "$port" | grep -Eq '^[0-9]{1,5}$' && [ "$port" -ge 1 ] && [ "$port" -le 65535 ] || { echo "invalid PostgreSQL port" >&2; exit 2; }
export PGPORT=$port
# GNU `stat -f` can print a filesystem report even when it exits with an
# error. Never concatenate that stdout with a fallback permission value.
private_mode() {
  case "$(uname -s)" in
    Linux) mode=$(stat -c '%a' -- "$1" 2>/dev/null) || return 1 ;;
    Darwin|FreeBSD) mode=$(stat -f '%Lp' -- "$1" 2>/dev/null) || return 1 ;;
    *) return 1 ;;
  esac
  [ "$mode" = 600 ]
}
[ -f "$archive" ] && [ ! -L "$archive" ] && [ -r "$archive" ] || { echo "archive must be a readable regular non-symlink file" >&2; exit 2; }
private_mode "$archive" || { echo "archive file must have mode 0600" >&2; exit 2; }
if [ -n "${PGPASSFILE:-}" ]; then
  [ -f "$PGPASSFILE" ] && [ ! -L "$PGPASSFILE" ] && private_mode "$PGPASSFILE" || { echo "PGPASSFILE must be a regular mode-0600 file" >&2; exit 2; }
  export PGPASSFILE
fi
command -v pg_restore >/dev/null 2>&1 || { echo "pg_restore is required" >&2; exit 2; }
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
if ! (ulimit -f 2048; exec pg_restore --list --no-password -- "$archive") >"$tmp/list.raw" 2>/dev/null; then echo "archive preflight failed" >&2; exit 1; fi
[ "$(wc -c <"$tmp/list.raw" | tr -d ' ')" -le 1048576 ] || { echo "archive listing exceeds 1 MiB" >&2; exit 1; }
awk '/ EXTENSION - / && $NF != "plpgsql" { bad=1 }
     / EVENT TRIGGER / { bad=1 }
     / DATABASE - / { bad=1 }
     / PROCEDURAL LANGUAGE - / && $NF != "plpgsql" { bad=1 }
     END { exit bad }' "$tmp/list.raw" || { echo "archive contains unsupported executable objects" >&2; exit 1; }
if [ "$apply" -eq 0 ]; then echo "dry-run: custom archive readable; restore not performed"; exit 0; fi
command -v psql >/dev/null 2>&1 || { echo "psql is required for apply" >&2; exit 2; }
query="SELECT CASE WHEN NOT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema' UNION ALL SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema' UNION ALL SELECT 1 FROM pg_type t JOIN pg_namespace n ON n.oid=t.typnamespace WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema' UNION ALL SELECT 1 FROM pg_namespace WHERE nspname !~ '^pg_' AND nspname <> 'information_schema' AND nspname <> 'public') THEN 1 ELSE 0 END"
if ! psql -X --no-password -A -t -v ON_ERROR_STOP=1 --host=127.0.0.1 --port="$PGPORT" --dbname="$target" -c "$query" >"$tmp/query.out" 2>/dev/null; then echo "target preflight failed" >&2; exit 1; fi
empty=$(tr -d '[:space:]' <"$tmp/query.out")
[ "$empty" = 1 ] || { echo "target database is not empty" >&2; exit 1; }
if ! pg_restore --no-password --exit-on-error --no-owner --no-acl --single-transaction --host=127.0.0.1 --port="$PGPORT" --dbname="$target" -- "$archive" >/dev/null 2>"$tmp/restore-error"; then echo "restore failed; inspect disposable target privately" >&2; exit 1; fi
printf 'restore completed into disposable database %s; application was not started\n' "$target"
