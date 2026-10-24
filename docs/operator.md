# Operator-only tools and release preparation

For the current normal runtime configuration and automatic import behavior, see
[configuration.md](configuration.md). Historical sections below describe the
operator tools as they were introduced; they are not a recipe for normal startup.

## Local health-only startup

Use `payplump serve-health` only for local CI/development readiness checks that
need migrations and HTTP health endpoints but no Telegram or business workers.
It explicitly disables Telegram and every service feature; it does not infer
health-only mode from environment flags. Ordinary no-argument startup enables
the full runtime and rejects legacy false feature switches, including
`TELEGRAM_ENABLED=false`.

`serve-health` still opens the database, runs migrations and applies startup
fences. Those fences can revoke existing permissions. Use only an operator-owned
disposable database; never use this command against production. The existing
`payplump healthcheck` command remains an HTTP client for `/readyz`, not a server
mode.
Do not set old `IMPORTED_*_ENABLED`, revision, allowlist, or EXCLUSIVE_WRITER
values in the normal environment. Legacy origins whose trusted policy hash does
not match the normal policy require an explicit verified migration.

For backup and isolated restore, see [backup-restore.md](backup-restore.md), an owner-gated runbook template.

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

This is a historical pre-merge snapshot only: implementation and local checks
were ready, but PostgreSQL runtime/required CI/PR #35 delivery had not yet
occurred at that time. Superseded by the delivered status immediately below;
this remains no authorization for live providers, manifests, or databases.

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
zero proven behavior/design findings. PR #36 merged at `a5fd93b28f0c0fe78e9485ddbe169837af49544b`; exact code HEAD
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
clarifies admission versus read-runtime permission terms. Historical pre-PR #37 checkpoint: new startup SQL runtime and exact-HEAD CI
were pending at that snapshot. Superseded by the delivered PR #37 status below;
Stage 11 owner live gates remain incomplete.

Flags `IMPORTED_EFFECTS_ENABLED=false` / `IMPORTED_EFFECTS_REVISION=` are
independent of source-policy fingerprint, admission, read-runtime and
configuration-secret permissions. Effects require read runtime; pure provider
construction requires exact source scope and makes no HTTP request. Read-enabled
/effects-off may support fresh-GET completion of a known original receipt under
existing authorization, not automatic writes or broader protected-reader
capabilities. Existing native known/unknown mutation workflows are prior
coverage; this slice added no new native mutation workflow. Historical PR #37
scope was 13 paths (seven implementation/config paths, no source fields/secrets,
plus six docs); PR #37 has since merged. The sequence proceeded through stages
12–14; current delivery evidence is recorded in the later status sections.

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

## Release preparation

`compose.release.yaml` remains a template: supply `PAYPLUMP_IMAGE` as an
immutable `@sha256:` digest, remote PostgreSQL settings and a private PGPASSFILE
through the deployment owner's secret mechanism. It does not create PostgreSQL,
publish ports, or deploy. Flags are false; sample resource limits are unmeasured.
The validator checks resolved Compose structure, not remote secrets or host state.
File-backed Compose secrets may ignore UID/mode: pgpass must be readable by
container UID 65532 or use an owner-approved secret mechanism. The helper does
not change permissions. See [release.md](release.md) for the separate manual
release workflow contract. Implementation and independent review are complete
(A zero findings; B P2 fixed and confirmed by final re-review `43d6862f-9f7a-477`,
zero findings); PR #39 merged, exact code HEAD `8824bc1263184e0a3fa80893ab4a8e4458f5c9ee` passed required CI `40056489006`, and merge base `0f1f1deb8dcd7d1bfec5e6d2bcac7c43a5b12d3d` passed post-merge CI `40057042166`. These source checks do not execute the release workflow. No release workflow has been dispatched (including dry-run); no release image has been produced through that manual workflow, and no deployment has occurred. Ordinary source CI build/smoke images are not release artifacts. Dispatch is forbidden in this task.

## Latest imported-effects and restore status

11b.10 merged in PR #37 at `b33fac67a1e19c8a3da00b2d7d261b170e0f02e8`;
exact final HEAD `9d93f649249f491ff70ec7a6c34af959b48fde3a` passed required CI
`40048848558`, and post-merge main CI `40049317238` succeeded. The disposable
PostgreSQL 17 workflow `TestImportedEffectsStartupWorkflowRealPostgres` passed.
This is synthetic CI evidence, not live readiness. Effects remain default-off;
explicit canonical revision and enabled imported read-runtime are required.

Provider construction is zero-HTTP but returns guarded native billing mutators
to ordinary consumers, not GET-only clients. ProtectedRecordReader,
RecurringReader, inventory and settings remain strict GET-only interfaces.
Never reset/rearm native global guard/epoch or per-chat secret-delivery barrier.
Real backup/export/restore and live application validation remain owner gates;
Invoice Ninja export mechanics are unknown until owner confirmation. Stage 12
synthetic restore CI does not satisfy them. Stage 12 code and local checks are
complete. Full independent review `9b8a652d-7b32-488` checked final 925-line
patch `/tmp/payplump-restore-rehearsal-review-fixed.patch`, verified the fix and
source rollback probe, and reported zero final findings. Initial P2: schema
digest omitted trigger enabled state and `attgenerated`/`attidentity`; test-only
hashing was corrected, retaining logical active-column ordinals. The source-only
probe temporarily disables `verified_import_origin_immutable` inside a source-only
read-write transaction (it does not drop/remove the trigger), confirms hash
change, rolls back and verifies the full baseline snapshot. Target remains
read-only. Parent normal/race/vet/CGO0/module/build/sh-n/operator checks and PG
compile-only passed. Operator-helper log reports 13 tests total: 12 passed and
one existing Docker-dependent test skipped. Both new negative-test functions
passed using a fake-Docker marker; the fake Docker was never invoked. PR #38
merged at base `15210d3ceac191dc467046a8be0d2cd6807265d7`; required CI
`40052967054` and post-merge CI `40053518866` passed. The synthetic restore
rehearsal ran in CI on disposable PostgreSQL 17. Real backup/export/restore and
live validation remain owner-gated.

## Safe resource measurements

See [resource-measurement.md](resource-measurement.md) for harness usage,
profile scope, the verified first CI artifact and the owner-only VPS measurement
worksheet. The bounded unit harness passed independent final review
`1dd52647-2607-400` with zero findings and required PR #40 CI run `40059203387`
(all three jobs). Artifact `12545794446` contains four validated public-safe
JSON profiles. These are runner unit metrics, not app/VPS capacity measurements.
The public repository's artifact access inherits repo read access; do not upload
private data or infer production limits from runner figures or sample Compose
values.
