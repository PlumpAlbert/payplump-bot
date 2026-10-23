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

The separate provider-only adapter in `internal/invoiceninja/imported_lifecycle.go`
adds an opaque frozen profile for compatible legacy monthly recurring records,
plus typed pause, reschedule, and resume capabilities. It preserves source
private notes and the frozen price, requires fresh GET preflight and a final
caller authorization before writes, and treats uncertain writes as hold-only.
It is intentionally not wired to this admission API, any worker, or runtime
configuration. The profile JSON is sensitive journal material; do not log it.
Independent provider review is complete; its reported P3 journal-shape defect
is fixed in this follow-up. PR #27 exact HEAD
`0fad9355c818424089a72aa1211bf33be5ab6ced` passed required CI `40005099529`
and post-merge main CI `40005923953`, both all-green.

The payment-tracking slice adds a separate default-off imported-runtime
permission. `SyncImportedRuntimePermission` is an explicit durable operator
update; admission does not enable it and startup does not synchronize it. With
both current admission and runtime permissions matching the immutable origin,
the existing payment verifier may record GET-verified payment evidence for an
imported current invoice. For the already-paid active invoice in the imported
baseline, it pins the original verified proof hash and suppresses historical
payment notices when that proof still matches. A contradictory baseline proof
is escalated for review. This is payment observation only: it does not grant access or create
activation history. The existing OWN registration-provenance predicate remains
unchanged. Imported invoice tracking separately performs only Invoice Ninja
GET discovery and normal ledger/current-invoice/ordinary-notice updates for
new invoices; neither tracking path writes to Invoice Ninja or S-UI. Imported overdue-only code and tests merged in PR #30 (status and CI are recorded below). A separate default-off effect permission is used; each immutable action freezes current admission-permission, runtime-permission, and effect-permission revisions. The historical origin admission revision remains distinct from the action's current admission-permission revision. Imported recurring pause uses the typed provider capability. Review `b5148ca3-905d-495` found zero behavioral/design findings; no legacy-overdue fixture predating migration 020 was found. The independent PG workflow covers full GET/admit/baseline-observe/CycleWorker/EnforcementWorker success, known closeSessions response after effect disable, and unknown pause/503/revision-2/restart with the guard pinned. Imported paid renewal 11b.6.1 supports only new ordinary lifecycle-ledger invoices; its three PostgreSQL workflow cases are implemented. Parent local unit/race/vet/CGO0 build/module/gofmt/diff checks and PostgreSQL integration compile passed. Independent review `e06c5abf-0d4d-484` completed with zero behavioral and design findings; SQL runtime, PR #31, CI, and merge remain pending. Do not treat this as complete. 11b.6.2 baseline-debt restoration, protected origin/configuration state, apply CLI, and runtime wiring remain future work.

The overall 11b workflow remains incomplete: there is no apply CLI, protected
origin-mode/configuration with origin FK and current database proof, or
runtime-configuration wiring. Imported-runtime permission is explicitly stored
and is not enabled automatically at startup. The lifecycle discovery/notice
code exists but is not wired into startup. Frozen invoice history is suppressed
through the immutable origin's seen-invoice set; legacy sent dates are never
fabricated from payment dates. The adapter is not an import consumer. No owner,
production API, deployment, or real-import permission has changed. 11b.4 is
complete: PR #29 merged as `149d6b679ff007b1cff6d9679862675f03048412`; exact
code HEAD `4c7a879da681ea75410526fc98f7bc37ab18e096` passed required CI
`40014461532` and post-merge CI `40015300957`, including all three
`TestImportedInvoiceLifecycle...` workflows and the full three-phase worker/
actual CycleReader PostgreSQL workflow. The nullable-first-invoice CAS defect
found during validation was fixed before merge. 11b.5 overdue-only code merged
in PR #30 as `19d45cf7f85c784b74cdc0cb2b71f5d15cddb758`; exact code HEAD
`b8efc247fe09b78e078b0c115c52317d47311939` passed required CI `40019995948`
and post-merge CI `40020785993`, all green.

11b.6.1 imported paid renewal supports new ordinary lifecycle-ledger invoices
only; three PostgreSQL workflow cases are implemented. Parent unit/race/vet/
CGO0 build/module/gofmt/diff and PostgreSQL integration compile checks passed.
Independent review `e06c5abf-0d4d-484` completed with zero behavioral and design findings. SQL runtime, PR #31,
CI, and merge remain pending; do not mark this slice complete before CI. The
separate 11b.6.2 initial paused-baseline/current-debt restoration is future work
requiring an explicit origin mode/FK; do not fabricate ledger dates or OWN
overdue journals. Protected imported configuration/menu/current proof, apply
CLI, and default-off startup wiring remain future work. Stage 11 is incomplete.
