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

## Next slice: 11b.7.2 imported current-renewal delivery

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

The first PR CI run `40038835592` passed fast checks, but PostgreSQL integration
failed before all feature scenarios while parsing migration 024 (`SQLSTATE
42601`, mismatched parentheses). This is a real migration syntax defect, not a
fixture-only failure; Store producer is correcting syntax without weakening
predicates. No SQL scenario has run in CI; local PG workflows are compile-only.
The independent full-patch review `e370ed66-465a-4ef` found zero behavioral/
design findings before this runtime parse failure; the migration fix needs a
narrow recheck. Parent local unit/race/vet/CGO0 build/module/gofmt and PG
compile-only checks passed before the migration correction. Corrected-HEAD CI,
SQL runtime and merge remain pending. Apply CLI/default-off startup wiring is
later work; 11b and live verification/admission remain incomplete.
