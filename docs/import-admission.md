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
remains null. Historical lifecycle ledgers are **not** initialized here. The
payment-tracking slice below is the only current runtime consumer; other
lifecycle workers still do not treat imported origins as eligible, and this
store API alone does not make an imported client fully managed or historically
suppressed.

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
is escalated for review. This is payment observation only: it does not grant
access, create activation history, or make the other lifecycle workers manage
imports. The existing OWN registration-provenance predicate remains unchanged.

The overall 11b workflow remains incomplete: there is no apply CLI, lifecycle
discovery/effect consumer, history-ledger initialization/suppression, protected
origin-mode configuration, or secret-mode design. The payment-tracking slice
awaits independent review and required PR #28 CI. Frozen invoice
history must later be suppressed via the immutable origin's seen-invoice set;
legacy invoice sent dates must not be fabricated from payment dates. The
adapter is not an import consumer and does not enable imported clients. Never
use this API for a real import until the exact implementation has passed
independent review and required PostgreSQL 17 CI.
