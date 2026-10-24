#!/bin/sh
# End-to-end restore rehearsal, restricted to this workflow's disposable PG service.
set -eu
umask 077

fail() { printf '%s\n' "restore rehearsal: $*" >&2; exit 1; }
[ "${GITHUB_ACTIONS:-}" = true ] || fail 'must run inside GitHub Actions'
container=${RESTORE_REHEARSAL_CONTAINER:-}
printf '%s\n' "$container" | grep -Eq '^[0-9a-fA-F]{12,64}$' || fail 'missing or invalid service container ID'
valid_positive_id() {
  value=$1
  case "$value" in ''|*[!0-9]*|0|0*) return 1 ;; esac
  [ "${#value}" -le 12 ]
}
valid_positive_id "${GITHUB_RUN_ID:-}" || fail 'invalid GITHUB_RUN_ID'
valid_positive_id "${GITHUB_RUN_ATTEMPT:-}" || fail 'invalid GITHUB_RUN_ATTEMPT'
run_id=$GITHUB_RUN_ID
attempt=$GITHUB_RUN_ATTEMPT
source_db="payplump_restore_source_${run_id}_${attempt}"
target_db="payplump_restore_check_${run_id}_${attempt}"
[ "${#source_db}" -le 63 ] && [ "${#target_db}" -le 63 ] || fail 'derived database name is too long'

# Require the workflow's disposable endpoint before clearing inherited libpq settings.
[ "${PGHOST:-}" = 127.0.0.1 ] && [ "${PGPORT:-}" = 15433 ] &&
[ "${PGDATABASE:-}" = workflow_test ] && [ "${PGUSER:-}" = workflow_test ] &&
[ "${PGPASSWORD:-}" = synthetic-local-password ] && [ "${PGSSLMODE:-}" = disable ] &&
[ "${PGOPTIONS+x}" = x ] && [ -z "$PGOPTIONS" ] && [ "${DATABASE_URL:-}" = '' ] || fail 'workflow PostgreSQL environment does not match disposable service contract'
# These are the only accepted synthetic service credentials and endpoint.
pg_user=workflow_test
pg_password=synthetic-local-password
pg_port=5432
pg_host=127.0.0.1
work="/tmp/payplump-restore_${run_id}_${attempt}"
work_created=0
created_source=0
created_target=0
cleanup() {
  original=$?
  trap - EXIT HUP INT TERM
  cleanup_failed=0
  if [ "$created_target" -eq 1 ]; then
    if ! docker exec -e PGHOST="$pg_host" -e PGPORT="$pg_port" -e PGUSER="$pg_user" -e PGPASSWORD="$pg_password" -e PGSSLMODE=disable "$container" dropdb --no-password --force "$target_db"; then
      printf '%s\n' 'restore rehearsal: failed to drop owned target database' >&2
      cleanup_failed=1
    fi
  fi
  if [ "$created_source" -eq 1 ]; then
    if ! docker exec -e PGHOST="$pg_host" -e PGPORT="$pg_port" -e PGUSER="$pg_user" -e PGPASSWORD="$pg_password" -e PGSSLMODE=disable "$container" dropdb --no-password --force "$source_db"; then
      printf '%s\n' 'restore rehearsal: failed to drop owned source database' >&2
      cleanup_failed=1
    fi
  fi
  if [ "$work_created" -eq 1 ]; then
    if ! docker exec "$container" rm -rf -- "$work"; then
      printf '%s\n' 'restore rehearsal: failed to remove private container files' >&2
      cleanup_failed=1
    fi
  fi
  if [ -n "${host_work:-}" ]; then
    if ! rm -rf -- "$host_work"; then
      printf '%s\n' 'restore rehearsal: failed to remove private runner files' >&2
      cleanup_failed=1
    fi
  fi
  if [ "$cleanup_failed" -ne 0 ]; then exit 1; fi
  exit "$original"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

# Confirm both executable client and server are PostgreSQL 17 before any DB creation.
version=$(docker exec -e PGHOST="$pg_host" -e PGPORT="$pg_port" -e PGUSER="$pg_user" -e PGPASSWORD="$pg_password" -e PGSSLMODE=disable "$container" sh -eu -c '
  dump_client=$(pg_dump --version)
  restore_client=$(pg_restore --version)
  case "$dump_client" in "pg_dump (PostgreSQL) 17."*) ;; *) echo "unexpected pg_dump version" >&2; exit 1 ;; esac
  case "$restore_client" in "pg_restore (PostgreSQL) 17."*) ;; *) echo "unexpected pg_restore version" >&2; exit 1 ;; esac
  psql -X --no-password -A -t -v ON_ERROR_STOP=1 --dbname=workflow_test -c "SELECT current_setting('\''server_version_num'\'')::integer / 10000"
') || fail 'could not verify PostgreSQL 17 service/client'
[ "$version" = 17 ] || fail 'service server is not PostgreSQL 17'

# Explicit URLs and cleared libpq overrides prevent inherited runner settings being used.
export GITHUB_ACTIONS RESTORE_REHEARSAL_MODE=prepare
export RESTORE_REHEARSAL_SOURCE_URL="postgres://${pg_user}:${pg_password}@127.0.0.1:15433/${source_db}?sslmode=disable"
export RESTORE_REHEARSAL_TARGET_URL="postgres://${pg_user}:${pg_password}@127.0.0.1:15433/${target_db}?sslmode=disable"
# Files are private on the runner; no archive/report is uploaded.
host_work=$(mktemp -d "${RUNNER_TEMP:-/tmp}/payplump-restore-${run_id}-${attempt}.XXXXXX")
chmod 700 "$host_work"
archive="$host_work/archive.dump"
manifest="$host_work/manifest.json"
report="$host_work/report.json"
export RESTORE_REHEARSAL_MANIFEST="$manifest"
export RESTORE_REHEARSAL_REPORT="$report"
unset PGSERVICE PGSERVICEFILE PGHOSTADDR PGOPTIONS PGHOST PGPORT PGDATABASE PGUSER PGPASSWORD PGSSLMODE DATABASE_URL TEST_DATABASE_URL
docker exec "$container" mkdir -m 700 "$work"
work_created=1
docker exec -e PGHOST="$pg_host" -e PGPORT="$pg_port" -e PGUSER="$pg_user" -e PGPASSWORD="$pg_password" -e PGSSLMODE=disable "$container" createdb --no-password --template=template0 "$source_db"
created_source=1
docker exec -e PGHOST="$pg_host" -e PGPORT="$pg_port" -e PGUSER="$pg_user" -e PGPASSWORD="$pg_password" -e PGSSLMODE=disable "$container" createdb --no-password --template=template0 "$target_db"
created_target=1

go test -v -tags=integration -run='^TestRestoreRehearsalPrepareSource$' -count=1 -timeout=5m ./internal/postgres

docker exec -e PGHOST="$pg_host" -e PGPORT="$pg_port" -e PGUSER="$pg_user" -e PGPASSWORD="$pg_password" -e PGSSLMODE=disable "$container" sh -eu -c 'umask 077; pg_dump --no-password --format=custom --no-owner --no-acl --file="$1" "$2"; chmod 600 "$1"' sh "$work/archive.dump" "$source_db"
docker cp "$container:$work/archive.dump" "$archive"
chmod 600 "$archive"
[ -f "$archive" ] && [ ! -L "$archive" ] || fail 'archive is not a regular private file'
mode=$(stat -c '%a' "$archive")
[ "$mode" = 600 ] || fail 'archive mode is not 0600'

docker cp scripts/restore-check.sh "$container:$work/restore-check.sh"
docker exec "$container" chmod 700 "$work/restore-check.sh"
docker exec -e PGHOST="$pg_host" -e PGPORT="$pg_port" -e PGUSER="$pg_user" -e PGPASSWORD="$pg_password" -e PGSSLMODE=disable -e PGDATABASE="$source_db" "$container" "$work/restore-check.sh" "$work/archive.dump" "$target_db"
docker exec -e PGHOST="$pg_host" -e PGPORT="$pg_port" -e PGUSER="$pg_user" -e PGPASSWORD="$pg_password" -e PGSSLMODE=disable -e PGDATABASE="$source_db" "$container" "$work/restore-check.sh" --apply "$work/archive.dump" "$target_db"

export RESTORE_REHEARSAL_MODE=verify
go test -v -tags=integration -run='^TestRestoreRehearsalVerifyTarget$' -count=1 -timeout=5m ./internal/postgres
[ -f "$report" ] && [ ! -L "$report" ] || fail 'verification report is not a regular file'
mode=$(stat -c '%a' "$report")
[ "$mode" = 600 ] || fail 'verification report mode is not 0600'
printf '%s\n' 'Verified restore rehearsal report:'
cat "$report"
printf '%s\n' 'Restore rehearsal passed; private files are removed by cleanup.'
