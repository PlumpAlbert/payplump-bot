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

## Read-only legacy import verification

`payplump import-verify --file PRIVATE_PATH --expected-hash PLAN_HASH --read-only`
validates a private mode-0600 manifest and performs bounded fresh GET-only
reconciliation. It requires an explicit bot ID, private pilot allowlist, exclusions,
source settings, currency-code acknowledgement, and selected legacy product key;
see [import-verification.md](import-verification.md) and `.env.example`. This
command does not use runtime feature flags, database settings, Telegram tokens,
or startup workers. Output is redacted JSON. The optional `--evidence-file`
contains sensitive private facts, is create-only mode 0600, and is not admission
authority. Do not use production provider credentials in local tests. No live
verification or managed admission has been performed.

## Historical checkpoint: import-apply before PR #35 merge (superseded below)

The `payplump import-apply` CLI implementation, expanded unit tests/finite
fixture, and isolated PostgreSQL workflows are ready for parent verification.
Parent normal unit/race/vet/CGO0 build/module/gofmt/diff and command+PostgreSQL
compile-only checks passed. Independent review `67ca1346-a4f9-49c` covered the
full patch (four command files, main/CI and admission/source contracts) with
zero proven behavior/design findings. PostgreSQL SQL runtime has NOT run, exact-
HEAD CI is pending, and PR #35 is not open. This is not a live operator
procedure. Do not run against real providers, manifests, or databases.

The contract is
`payplump import-apply --file PRIVATE --expected-hash HASH --admission-revision N --apply [--enable-admission]`.
Arguments, canonical positive revision, private regular mode-0600 nonsymlink
manifest, and hash are checked before environment, source-provider construction,
or database access. The command then performs fresh GET-only verification of the
complete plan and prepares an opaque plan before DB open; saved reports and
evidence are never authority. Output is limited to safe plan hash/counts. It
reuses the `import-verify` source-policy settings and pinned versions/public
base/inbounds. A known pre-existing `telegram_bots` row is required; the command
does not create bot metadata or call GetMe. `DB_MAX_CONNS` defaults to 4 and
accepts canonical values 1..64.

`--apply` is mandatory and omission rejects. Without `--enable-admission`, apply
requires a matching currently enabled admission policy. `--enable-admission` is
a separate explicit permission change, default off: it syncs the prepared
bot/revision/policy hash/source scope enabled, then applies the batch. This
permission update commits in a separate durable transaction and can persist if
batch application conflicts; permission enable and batch apply are not atomic
together. There is no automatic rollback/rearm. Re-enabling after disable
requires a higher revision. Applying does not enable imported runtime/effects
or protected configuration delivery; startup wiring remains separate. A live
operator would need an already-known bot record and service-owner permission,
but there is no authorization here to run against live systems.

## Current imported runtime and effects wiring status (supersedes the checkpoint above)

The 11b.8 `import-apply` command is delivered. PR #35 merged at
`c0c5f41de7dc98eb03530bebda394b0024842061`; exact HEAD
`2a69caa2802e3ad78a67da023a68334f8566e377` passed required CI
`40044946187` and post-merge main CI `40045550655`. Disposable PostgreSQL 17
runtime passed `TestImportApplyWorkflowRealPostgres` subtests
`explicit_enable_and_idempotent_replay` and `default_off_denies_empty_schema`.
The separate clean schema denied apply without `--enable-admission` and left no
rows; explicit enable/apply/replay was idempotent. Earlier CI failures were test
fixture issues: `40043431687` failed while scanning textual server version in
test setup, and `40044064678` denied a stale synthetic date under the correct
production freshness check. The final CI passed without weakening those
production guards. No live execution is authorized or claimed.

11b.9 implementation and tests are complete; parent normal unit/race/vet/CGO0
build/module/gofmt/diff checks and tagged command+PostgreSQL compile-only passed.
Independent review `164166cb-25b2-465` examined exact patch
`/tmp/payplump-import-runtime-review.patch` (850 lines) plus related contracts;
zero proven behavior/design findings. PR #36 merged at `a5fd93b28f0c0fe78e9485ddbe169837af49544`; exact code HEAD
`277b37c12433f271ee72d60d486551644749de92` passed required CI `40046909184`
(all three jobs: fast, race, disposable PostgreSQL 17 and smoke/Docker), and
post-merge main CI `40047448373` succeeded. `TestImportedRuntimeReadOnlyStartupWorkflowRealPostgres`
passed at SQL runtime. It adds default-off `IMPORTED_RUNTIME_ENABLED` and explicit canonical
`IMPORTED_RUNTIME_REVISION`. Startup fences imported effect permissions before
readiness; when runtime is disabled it also disables imported runtime. Enabling
installs only a matching runtime policy after normal verified bot setup. This
does not grant admission, effects, or configuration delivery; startup does not
alter the separately CLI-owned admission permission.

The authored real PostgreSQL workflow checks an empty schema before bot setup:
there are no admission/workflow/permission rows and no source GET, but this is
not an assertion that the entire database has no rows (known bot metadata and
migration/guard infrastructure may exist). It checks the guard epoch/owner
across all steps, actual read-worker completion/GET count, revision-1 denial and
explicit revision-3 read permission success, effects still disabled at revision
2, admission still enabled at revision 1, unchanged origin, and full normal
payment-confirmation proof. The workflow verified actual CLI fresh admission/GET-only planning/read-worker
completion and payment confirmation, read revisions 1 denied/3 accepted,
effects disabled at revision 2, admission still enabled at revision 1, unchanged
origin/guard epoch/owner, and no source GET by loader/fence/install. The
fresh-schema assertion means no admission/workflow/permission rows before bot
setup, not an empty DB; known bot metadata and migration/guard infrastructure
may exist.

11b.10 implementation and tests are complete; all three code producers are done.
Parent full normal unit/race/vet/CGO0 build/module verification/gofmt/diff
passed (logs `/tmp/payplump-import-effects-{unit,race}.log`); tagged
command+PostgreSQL compile-only passed. Independent review `e72f63f9-cad9-4ed`
covered the exact 827-line patch `/tmp/payplump-import-effects-review.patch` and
related durable-effects/source-scope/core contracts; zero proven behavior/design
findings. The parent-fixed bot-mismatch test keeps the read row when effects are
denied (counts `1,1,0`); bad-effects-revision uses valid read revision 7, reaches
the effects-revision rejection and asserts no source GET. `.env.example` now
clarifies admission versus read-runtime permission terms. New startup SQL runtime
has not run; exact-HEAD CI and PR #37/merge are pending. Stage 11 and owner live
gates remain incomplete.

Flags `IMPORTED_EFFECTS_ENABLED=false` / `IMPORTED_EFFECTS_REVISION=` are
independent of source-policy fingerprint, admission, read-runtime and
configuration-secret permissions. Effects require read runtime; pure provider
construction requires exact source scope and makes no HTTP request. Read-enabled
/effects-off may support fresh-GET completion of a known original receipt under
existing authorization, not automatic writes or broader protected-reader
capabilities. Existing native known/unknown mutation workflows are prior
coverage; this slice adds no new native mutation workflow. PR #37 currently
covers 13 paths (seven implementation/config paths for initial staging, no
source fields/secrets, plus the six docs). After PR #37, next stages are strictly
12 synthetic restore CI → 13 manual release preparation with publishing off →
14 synthetic measurements and owner live gates. Do not skip 12.

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
