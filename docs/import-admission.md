# Verified import admission foundation (11b.1)

This slice adds a fresh-verification preparation type and a PostgreSQL-only
admission store API. It is not wired to a CLI, source-provider constructor, or
runtime consumer. The policy table is default-disabled. Callers must configure
it explicitly with `SyncImportAdmissionPolicy`; admission requires the same
bot, revision, policy hash, and source-scope hash. Disabling is allowed at the
current revision; re-enabling after a disable requires a strictly higher
revision, so previously prepared intents cannot be rearmed.

`ApplyVerifiedImport` accepts only an opaque `importadmission.PreparedPlan`.
Preparation performs the GET-only verification itself for the whole plan; saved
reports or evidence files are not accepted as authority. The store stages the
original plan and atomically checks policy and DB-clock freshness, inserts
clients/subscriptions, and persists immutable imported-origin history. Existing
clients are accepted only as exact same-origin idempotent replays. The store
creates no registration/activation operations, payment confirmations, outbox
rows, membership effects, or fake first invoice IDs. It checks but never clears
or advances the shared external-write guard. Conflicts roll back the batch.

Origins retain the paid anchor, invoice history and watermark, verified facts,
legacy recurring template, and credential fingerprint. `subscriptions.first_invoice_id`
remains null. Admission does not initialize historical lifecycle ledger rows.
Imported invoice tracking is a separate read-only lifecycle-worker capability
under the same explicit imported-runtime permission. It uses source GETs to
find ordinary newly issued recurring invoices, records those new observations
in the normal invoice ledger, advances the current invoice, and queues the
normal new-invoice notices. It unions frozen historical invoice IDs into the
seen set so old invoices do not generate replay notices; it never invents
historical ledger rows or sent dates. The tracking path performs no source
writes. The lifecycle worker is not wired into startup/runtime configuration,
so this capability is not automatically running.

PR #26 merged the 11b.1 DB-only foundation as `76dfcda`; exact code HEAD
`d0a2ca665be507d1913db9bbe2301e945be34e0a` passed required CI `40001910084` and
post-merge CI `40002786608`, including PostgreSQL 17 integration coverage.

The separate provider adapter in `internal/invoiceninja/imported_lifecycle.go`
adds an opaque frozen profile for compatible legacy monthly recurring records,
plus typed pause, reschedule, and resume capabilities. It preserves source
private notes and frozen price, requires fresh GET preflight and final caller
authorization before writes, and treats uncertain writes as hold-only. Its
profile JSON is sensitive journal material; do not log it. Independent provider
review's P3 journal-shape defect was fixed. PR #27 exact HEAD
`0fad9355c818424089a72aa1211bf33be5ab6ced` passed required CI `40005099529`
and post-merge CI `40005923953`, both all-green. The adapter is consumed by the
imported enforcement and renewal workers under their explicit gates; those
workers are still not wired into startup/runtime configuration.

The payment-tracking slice adds a separate default-off imported-runtime
permission. `SyncImportedRuntimePermission` is an explicit durable operator
update; admission does not enable it and startup does not synchronize it. With
current admission and runtime permissions matching the immutable origin, the
existing payment verifier may record GET-verified evidence for an imported
current invoice. For the already-paid active baseline invoice, it pins the
original verified proof hash and suppresses historical payment notices while
that proof matches; contradictory baseline proof goes to review. This observes
payment only: it does not grant access or create activation history. The OWN
registration-provenance predicate remains unchanged.

11b.4 imported invoice tracking performs GET discovery and normal
ledger/current-invoice/ordinary-notice updates for new invoices; it does not
write to Invoice Ninja or S-UI. PR #29 merged as
`149d6b679ff007b1cff6d9679862675f03048412`; exact HEAD
`4c7a879da681ea75410526fc98f7bc37ab18e096` passed required CI `40014461532`
and post-merge CI `40015300957`, including all three
`TestImportedInvoiceLifecycle...` workflows and the full three-phase worker /
actual CycleReader PostgreSQL workflow. The nullable-first-invoice CAS defect
was fixed before merge.

11b.5 adds imported overdue-only effects behind a separate default-off effect
permission. Each immutable action freezes current admission, runtime, and effect
permission revisions; the historical origin admission revision remains distinct.
Imported recurring pause uses the typed provider capability. Review
`b5148ca3-905d-495` found zero behavioral/design findings; no legacy-overdue
fixture predating migration 020 was found. PostgreSQL workflow coverage includes
GET/admit/baseline-observe/CycleWorker/EnforcementWorker success, known
closeSessions response after effect disable, and unknown pause/503/revision-2/
restart with the guard pinned. PR #30 exact code HEAD
`b8efc247fe09b78e078b0c115c52317d47311939` passed required CI `40019995948`
and post-merge CI `40020785993`, all green.

11b.6.1 imported paid renewal supports ordinary new lifecycle-ledger invoices.
PR #31 exact HEAD `012e1249a0720b79993ce55206413ecae06b178b` passed required CI
`40024818619` (PostgreSQL 17/race, native renewal success/late/unknown cases,
smoke and Docker) and post-merge CI `40025798271`.

## Delivered 11b.6.2 initial-debt restoration

PR #32 merged at `57a6e09d1bd06b806e79145aba065c870b023934`. Exact final code
HEAD `cd0ff229ed6ecb6ee0f81d5ceeee1c7cc7769502` passed required CI `40031643624`
(all three jobs, including PostgreSQL 17/race/smoke/Docker); post-merge CI
`40032288658` completed successfully on the exact base. Runtime passed
`TestImportedInitialRestorationPaymentProofTargetsCurrentDebt` and all three
`TestImportedInitialPausedDebtRestorationWorker` workflows: expired original
schedule, empty original schedule, and resume mutation followed by 503/revision
2/restart without replay. The expired-schedule workflow also completed a new
normal-cycle GET adoption, current paid proof, and normal ledger renewal
seed/load while the immutable origin remained lifecycle-paused and its initial
audit unchanged. Current-debt proof never uses the historical paid anchor; no
fake ledger/first invoice, OWN activation, or overdue prefix was created.

Independent review `6ac88f9e-0961-4fd` initially found a P1 initial-mode claim
lock issue. The mode-aware ledger-versus-origin lock was fixed and rechecked;
final findings were zero. Parent-mode IFF and SQL double-batch bounds were
verified. CI `40029673909` and `40030663139` found test-fixture issues only; the
fixes preserved production checks. No live API, Telegram, payment, working DB,
publish, or deployment was used.

## Delivered 11b.7.1: original active imported baseline

PR #33 merged as `4a6522d1b881ef031ef580c67486406237ecf004`; exact final code
HEAD `e2ca6863aa785681433932f3a9caf82a0d46319b` passed required CI
`40035675235` (all three jobs, including PostgreSQL 17/race and test aggregator
with smoke/Docker), and post-merge main CI `40036327450` succeeded on the exact
base. Runtime passed all three imported-configuration SQL workflows.

This slice protects only the ORIGINAL ACTIVE imported baseline: current invoice
must equal immutable origin current invoice and paid anchor, with current normal
proof, strictly future UTC date, static binding/inbounds, legacy price/notes,
source-scope/public-base GET fences, legitimate immutable origin FK/mode, and
current configuration/admission/runtime revisions. `first_invoice_id` remains
SQL NULL; no fake activation or OWN history is created. OWN first/renewal paths
remain unchanged. GET-only consumers do not require billing-effects permission;
configuration policy separately and explicitly controls secret Telegram
delivery. No auto-enable; preserve the per-chat unknown-send barrier.

Independent review `34c46fce-9abc-42a` covered production, both domain/unit tests
and the PostgreSQL workflow. Two P2 findings (currency-policy drift and
test-worker bot scope) were fixed and confirmed; final findings were zero. The
three runtime workflows covered original-baseline GET/verify/admit/observe/menu/
outbox/protected-service/Telegram success with `sent` and guard release;
public-base policy denial before GET and changed-current native proof sticky
review 2007 after GET; runtime permission ABA denial; unknown-send 503 with a
durable chat barrier across revocation/new request/restart. The unknown case
performs no further URL/provider/chat GET. No live consumer readiness or live
provider/Telegram/payment/working-DB/publish/deploy result is claimed.

## Delivered 11b.7.2 and 11b.8; current delivery status

11b.7.2 protected imported CURRENT COMPLETED renewal merged in PR #34 as
`1c21061e0ad8f02fab45678c9d92d343855fd26a`. Exact final code HEAD
`1cdd30d4b4b4fbb0bf3544a64ff31156a705bd60` passed required CI `40040735229`
(all three jobs, including PostgreSQL 17/race/smoke/Docker); post-merge main CI
`40041362995` succeeded on the exact merge base. Runtime passed all four new SQL
workflows: `TestCompletedImportedRenewalAllowsProtectedConfiguration`,
`TestCompletedImportedInitialRestorationAllowsProtectedConfiguration`,
`TestUnknownInitialRestorationDeniesConfiguration`, and
`TestCompletedImportedRenewalStaleConfigurationRevisionDenied`. Earlier CI
history remains: `40038835592` was a real migration 024 syntax failure before
SQL scenarios; syntax fix narrowly reviewed (`6709e271-efcb-47e`); `40039692048`
passed migration and old SQL but found four test-only boundary/fixture failures;
`40040735229` passed all jobs. No production fences were weakened. This is historical 11b.7.2 evidence, not the current CLI status. Full review
`e370ed66-465a-4ef` and narrow syntax review completed. No live API, Telegram,
payment, working DB, publish or deploy.

### Delivered import-apply CLI (11b.8)

`payplump import-apply --file PRIVATE --expected-hash HASH --admission-revision N --apply [--enable-admission]`
production code, tests, and isolated PostgreSQL workflows merged in PR #35 at
`c0c5f41de7dc98eb03530bebda394b0024842061`. Exact final code HEAD
`2a69caa2802e3ad78a67da023a68334f8566e377` passed required CI `40044946187`
(all jobs: fast, race, disposable PostgreSQL 17, Docker smoke) and post-merge
main CI `40045550655`. `TestImportApplyWorkflowRealPostgres` passed
`explicit_enable_and_idempotent_replay` and `default_off_denies_empty_schema`.
The successful path used fresh GET-only verification and an existing known bot;
replay was idempotent without origin mutation. A fresh empty schema denied apply
without `--enable-admission`, with no rows and no automatic admission policy.
CI fixture failures and their bounded corrections are recorded in
[operator.md](operator.md) and the latest [autonomous handoff](autonomous-work.md).
Runtime permission/effects/configuration permissions remain separate.

The CLI checks invalid arguments, canonical positive revision, private regular
mode-0600 nonsymlink file, and hash before environment/factory/DB. It performs
fresh GET only via `VerifyProviders` and concrete `importverify.Service`, then
prepares the complete opaque plan before opening DB; saved reports/artifacts are
not authority. It reuses import-verification policy variables and pinned
provider versions/public-base/inbounds/TZ. `DB_MAX_CONNS` defaults to 4 and
accepts canonical 1..64. A pre-existing known `telegram_bots` row is required;
no bot metadata is manufactured and no GetMe/workers/migrations are invoked.
Output is safe plan hash/counts only.

`--apply` is mandatory and omission rejects. `--enable-admission` is separately
explicit and defaults off. Without it, a current enabled matching admission gate
is required. When supplied, sync prepared bot/revision/policy hash/source scope
enabled, then apply the verified batch. This permission sync commits in a
separate durable transaction and MAY persist if batch application later fails;
there is no automatic rollback/rearm. Re-enable after disable requires a higher
revision; same-revision replay is idempotent, not automatic old-origin
re-admission after revision change. Runtime/effects/configuration-delivery
permissions are not enabled. PG workflows require isolated loopback PG17,
`TEST_DATABASE_URL`, empty `PGOPTIONS`, one-query explicit SSL mode, and verify
direct `current_schema` plus exact `search_path` before store open/migration;
cleanup errors are reported. Covered fresh default-off/no-enable denial in a
separate empty schema, explicit enable/apply/replay, and revocation. Synthetic
known bot ID 9 is only the fixture precondition. Do not run against real
services or data; PostgreSQL runtime passed in the exact PR #35 CI as recorded above.

## Delivered 11b.7.2: imported current-renewal delivery

11b.7.2 implementation and four PostgreSQL workflow cases are in PR #34 at
`815d54c6e019e03e5a8391dccaec21e2e7a39fd3`. Eligibility is a protected imported
CURRENT COMPLETED renewal, covering normal ledger renewal and current debt after
initial-restoration mode. Require explicit current-payment proof, completed native
plan/journal/receipts and a strictly future current UTC date. Immutable shared
origin, historical paid anchor and original proof remain historical, never
current entitlement/selectors; keep current proof and renewal audit separate.

`RenewalAudit.OriginMode` is new `omitempty` (`ledger` or
`imported_initial_paused`). Renewal request `request_period='renewal'` retains SQL
NULL `activation_id`, uses the conditional generated baseline-origin invoice FK
and existing current-renewal composite FK; old requests/fingerprints remain
unchanged. No fake activation, ledger row, first invoice, or historical payment
confirmation. `Current.ImportedBaselineProofHash` is zero for renewal; origin
proof hash stays in `Entitlement.Imported`. Validate old proof through canonical
admitted evidence; current proof/date/amount are independent of anchor dates.
Native audit handles both imported modes, not OWN decoder; exact seven-key plan
and known receipts are required. Initial mode omits Schedule only when current
target already is future; empty/past old UTC is replaceable only by a known new
schedule/future receipt, without Pause. Current admission/runtime/config must
match; effects may be off after known completion and completion revisions may be
historical. Preserve GET-only provider/scope/public-base/native-currency/date/UTC
checks, final DB authorization and in-memory URL.

Keep barriers distinct: unknown Invoice Ninja/S-UI mutations pin the shared
`activation_write_guard`/epoch and local workflow remains held; unknown secret
Telegram sends pin `configuration_chat_guards` for that chat. Neither clears the
other. The new unknown-native case is Resume mutation→503, pinned global guard
and local overdue, followed by ordinary menu denial without a configuration
request; it is not a Telegram-unknown test.

CI history: `40038835592` passed fast checks but PostgreSQL integration failed
before feature scenarios while parsing migration 024 (`SQLSTATE 42601`), a real
syntax defect. The correction was narrowly reviewed (`6709e271-efcb-47e`, no
fence changes). `40039692048` passed migration and old SQL cases, then exposed
four test-only boundary/worker fixture failures; only fixture/assertion scope was
corrected without weakening protected predicates. `40040735229` passed all jobs
and the four new SQL workflows ran successfully.
Full review `e370ed66-465a-4ef` and narrow syntax review completed. Apply CLI
production, unit and isolated PG test code is complete; parent local checks and
independent review `67ca1346-a4f9-49c` found zero proven behavior/design
findings. Command SQL runtime and exact-HEAD CI have since passed in PR #35 as recorded above. The full stage 11 and live verification/admission remain incomplete.

## Delivered imported read-runtime startup (11b.9)

This is a separate read/runtime permission, not admission. The delivered
`import-apply` command grants admission only when the operator explicitly passes
`--enable-admission`; it does not grant imported runtime, billing effects or
protected configuration delivery. Admission permission persists independently
and is owner/CLI-controlled. Application startup must not synchronize or revoke
admission.

11b.9 implementation and tests passed local normal/race/vet/CGO0 build/module,
gofmt/diff, and tagged compile-only checks. Full review `164166cb-25b2-465` of
`/tmp/payplump-import-runtime-review.patch` (850 lines) found zero proven
behavior/design findings. The PostgreSQL workflow
`TestImportedRuntimeReadOnlyStartupWorkflowRealPostgres` passed. PR #36 merged
as `a5fd93b28f0c0fe78e9485ddbe169837af49544`; exact code HEAD
`277b37c12433f271ee72d60d486551644749de92` passed required CI `40046909184`
(fast, race, disposable PostgreSQL 17 and smoke/Docker), and post-merge main CI
`40047448373` succeeded. Its config adds only
`IMPORTED_RUNTIME_ENABLED=false` and `IMPORTED_RUNTIME_REVISION=`. When disabled,
unused import source-policy settings are ignored. Enabling requires Telegram
and payments plus the same strict source/pilot/exclusion/currency/product/public
base/inbound/time-zone policy validation as fresh import verification; revision
is canonical positive only when explicitly enabled. Persist only enablement,
revision, bot and policy/source-scope hashes, never tokens.

Startup ordering is deliberate: after migrations and before readiness or any
GetMe call, fence off existing imported effect permissions on every start. If
runtime is configured disabled, also revoke the imported runtime permission.
If enabled, do not first disable or bump an unchanged runtime revision: after
normal webhook setup and ordinary actual bot verification, install only the
current explicit matching runtime permission before activation. This install
must not grant effects, admission or configuration delivery. It introduces no
provider GET, startup worker or new bot identity. Existing service bot
verification remains the normal capability. The fresh-schema test asserts that no admission/workflow/permission rows were
created before bot setup; it does not assert an empty database, because known
bot metadata and migration/guard infrastructure may exist. The workflow also
checks guard epoch/owner unchanged across the full sequence, no source GET before
the fence, read permission revision 1 denied then explicit revision 3 accepted,
effects remain disabled at revision 2, admission remains enabled at revision 1,
origin stays unchanged, and the actual normal payment-confirmation proof is
preserved. Exact CI, merge and runtime verification are complete; 11b.9 is delivered.

## Imported native effects startup (11b.10 implementation ready; CI pending)

The 11b.10 implementation and tests are complete; all three producers are done
and no active producers remain. Parent full normal unit/race/vet/CGO0
build/module verification/gofmt/diff passed (logs
`/tmp/payplump-import-effects-{unit,race}.log`); tagged command+PostgreSQL
compile-only passed. Independent full review `e72f63f9-cad9-4ed` of exact 827-line
patch `/tmp/payplump-import-effects-review.patch` plus durable-effects,
source-scope and core contracts found zero proven behavior/design findings.
Parent fixed test assertions: effects-denied bot mismatch retains read
permission (counts `1,1,0`, not `1,0,0`); effects bad-revision uses read
revision 7, valid for read, and reaches the effects-revision rejection with no
source GET. `.env.example` clarifies admission versus read-runtime permission
terms. The new startup test has not run PostgreSQL SQL runtime; exact-HEAD CI,
PR #37 and merge remain pending. Stage 11 and owner live gates remain incomplete.

Flags are `IMPORTED_EFFECTS_ENABLED=false` and `IMPORTED_EFFECTS_REVISION=`;
revision is canonical positive iff effects are explicitly enabled, and effects
require read runtime. Source-policy fingerprint is unchanged; admission,
read-runtime, effects and configuration-secret permissions remain independent.
The current PR #37 write set is 13 paths: seven implementation/config paths
(initial staging only; no source fields or secrets) and these six docs. After
PR #37, preserve sequence 12 synthetic restore CI → 13 manual release
preparation with publishing off → 14 synthetic measurements/owner live gates;
do not skip 12. See [operator.md](operator.md) and
[autonomous-work.md](autonomous-work.md) for fences, provider constraints and
test scope.
