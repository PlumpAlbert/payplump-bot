# Operator-only tools and release preparation

These tools do not make the production Invoice Ninja or S-UI API calls. Never use
production data as an import manifest or restore target. All examples and tests
must use synthetic identifiers. Live reconciliation, import admission, and
external verification are owner-only follow-up work.

## Read-only recovery report

Build the binary and run `payplump recovery-report [--operation LOCAL_ID]` with
explicit PostgreSQL connection settings supplied by the operator. It opens a
read-only transaction; it never migrates, starts workers, constructs providers,
expires leases, or retries anything. The optional ID is the local `operations.id`,
not an external identifier. Output includes local operation/status/attempt and
numeric review codes, operation counts, the activation guard epoch/owner, local
protected-delivery outbox indexes/attempts, and unresolved known-receipt counts.
It omits chats, Telegram/provider IDs, names, URLs and payloads. It requires the
`009_protected_configuration.sql` schema feature and fails closed when absent;
010 shared-dispatch data is included only when that optional feature is present.
Protected unknown-delivery rows are capped at 250 and include total/truncated
indicators. No reset/rearm/clear API exists. An expired lease does not prove that an external
write or secret delivery did not happen.

## Controlled import plan and inert staging

Prepare a private synthetic manifest matching the `importplan.Record` JSON
structure. The file must be a regular mode-0600 file. `payplump import-plan
--file PATH` validates strict bounded JSON (8 MiB, 10,000 records, depth 32,
duplicate-key and trailing-value rejection) and prints counts, reason codes,
and a private-plan hash only. It emits no external identifiers or binding facts.
The canonical Telegram ID and Invoice Ninja `id_number` must be the same
positive decimal string. Keys are exact-case schema keys; case-fold aliases,
missing/null fields and unknown fields are rejected. Excluded accounts win over
all other reasons. Duplicates, incomplete evidence, multiple unpaid debts and
invalid lifecycle/period facts are manual-review reasons, not candidates.
Manifest `complete_proof` and `pinned_provenance` values are untrusted claims,
not verified evidence. A staged record is inert and is not adopted or verified.

For explicit staging, repeat the dry run, note its hash, and run
`payplump import-stage --file PATH --expected-hash HASH --apply`. This requires
an already-migrated database; the command does not run migrations. It atomically
stores immutable private plan/candidate facts in migration 013 tables. Repeating
an exact plan is idempotent; conflicting facts are rejected. Staging does not
create clients, subscriptions, bindings, confirmations, operation journals,
outbox messages, active flags, or access capability. It is not a managed import.
Before any live adoption, the owner must verify current invoices, paid periods,
existing S-UI identities/links, old debts, and all external facts, then design a
separately reviewed explicit capability-admission operation. None is implemented
here. Historical invoices are never sent again by staging.

## Backup and isolated restore check

An owner must arrange independent backups of the bot PostgreSQL database, bot
configuration/secrets, S-UI data and Invoice Ninja data through the respective
owners' approved facilities. This repository does not fetch or export any of
those systems. Invoice Ninja export/restore mechanics remain **PENDING owner
verification**. Protect backup encryption keys separately and test recovery
access without placing secrets in Git.

`restore-check.sh` defaults to dry run and accepts only PostgreSQL custom-format
archives with a regular non-symlink mode-0600 archive, loopback connection,
a simple canonical source database name, and a target named
`payplump_restore_check_[A-Za-z0-9_]+` (at most 63 characters) different from
the source. It clears libpq routing overrides, enforces a five-second connection
timeout, and forces IPv4 loopback with a validated port. Dry run invokes only
`pg_restore --list`; it never connects to PostgreSQL. Apply is explicit, requires
a privately protected password file if needed, and checks that the target has no
user objects. Built-in `pg_*` objects and the empty built-in `public` schema are
permitted. The archive filters are guardrails, not proof that an archive is safe:
PostgreSQL archive DDL and functions can execute code, so use only a trusted,
owner-produced archive. If interrupted, inspect/destroy the disposable target
manually. The helper does not create/drop databases or touch a production source.

After restore, inspect `schema_migrations` checksums against the intended binary.
The application refuses unknown migration versions and checksum changes; do not
edit migrations 001–009 or assume a newer binary can be rolled back across a
schema migration. Compatibility and app rollback need an isolated rehearsal.
No real backup, restore, or DB operation was run by the implementation agent.

## Release template

`compose.release.yaml` is a template only: supply `PAYPLUMP_IMAGE` as a
registry-qualified immutable `@sha256:` digest, and remote PostgreSQL variables
and a private PGPASSFILE via the deployment owner's secret mechanism. It does
not create a PostgreSQL server, host networking, publish ports, or deploy.
All feature flags, including future broadcast/lifecycle placeholders, are false.
The image runs nonroot, read-only with dropped capabilities, no-new-privileges,
log rotation, and sample memory/CPU values explicitly marked unmeasured.
Run `scripts/validate-release-config.sh REGISTRY/IMAGE@sha256:DIGEST` to resolve
Compose configuration with an explicit empty env file and validate its JSON
structure. This checks the supplied resolved config, not remote secret ownership
or host state. Compose file-backed secrets may ignore configured UID/mode: the
host pgpass file must be readable by container UID 65532 (for example, mode 0600
owned by that UID), or mapped through an owner-approved secret mechanism. The
helper never changes ownership or permissions. No registry login, build
publication, deployment, or workflow run is part of this change. A future release
workflow must be workflow_dispatch only, with publication default-off,
digest-pinned actions, and protected approval.

## Safe resource measurements

`scripts/measure-safe.py` is dry-run by default. With `--run`, it executes only
an allowlist of Go unit-test packages, strips database/provider credential
variables from the child environment, suppresses test output, and reports runner
architecture, commit, workload, actual duration, child RSS/CPU when available,
and cgroup memory only when exposed. Optional image file bytes are reported only
for a file actually supplied; compressed registry size is explicitly unavailable
unless measured independently. This does not measure VPS/business workloads,
external services, or application credentials. Report measurements with runner,
package selection and duration; never infer production limits from the example
Compose values. `scripts/measure-safe.py` was not used to claim VPS metrics.
