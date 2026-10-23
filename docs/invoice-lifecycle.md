# Invoice lifecycle: sent-invoice notices and overdue enforcement

Stage 1 and Stage 2 are independently **default-off**. Stage 1 observes Invoice Ninja recurring invoices using GET requests, records canonical observations in PostgreSQL, queues Telegram notices in the existing outbox, and hands the new `current_invoice_id` to the existing payment verifier. It never writes to Invoice Ninja or S-UI and does not change access. Stage 2 is a separate writer gate described below. Neither stage restores access.

## Safety and eligibility

Enable only after the private pilot allowlist, Invoice Ninja identity mapping, Telegram bot, payment verifier, and lifecycle secret have been configured deliberately. `INVOICE_LIFECYCLE_ENABLED=true` requires `TELEGRAM_ENABLED=true` and `PAYMENTS_ENABLED=true`. The lifecycle worker acts only on allowlisted private chats (`chat_id == telegram_id`) whose registration and first activation are proven by completed local journals for this bot. It requires the registration customer/invoice provenance, the matching Invoice Ninja customer and recurring IDs, the confirmed recurring create/start steps, and the activation S-UI binding. The recurring invoice must also carry the exact private marker derived from the confirmed activation `recurring_create` token. Invoice Ninja 5.13.47 copies the recurring record's `private_notes` into each generated invoice; registration provenance is independently checked against the original first invoice. Imported, unlinked, ambiguous, or incomplete records are not adopted.

The first invoice (`first_invoice_id`) is suppressed because it already has its separate registration/payment flow. Draft invoices are ignored. A newly observed invoice is eligible only after a fresh exact-ID GET confirms it belongs to the managed customer and recurring ID, has the private marker, is not deleted or archived, and has a complete amount/balance/status contract. More than one new invoice, unknown statuses, inconsistent balances, missing provenance, or missing/ambiguous sent dates go to durable manual review; API, version, pagination, and database failures are deferred without guessing.

Invoice Ninja status IDs used here are 2 (sent), 3 (partial), and 4 (paid). Sent/partial observations queue one invoice notice for the client and an admin summary. A paid invoice found before the first sweep is recorded and handed to payment verification without an unpaid notice. Payment verification still requires its own complete allocation evidence; Invoice Ninja's `status_id` alone never confirms payment. The previous `current_invoice_id` must have a non-reviewed payment confirmation before it can advance. If it is not yet verified, the lifecycle check is deferred; if that proof is in manual review, the lifecycle check is held for review.

The bot uses the earliest valid non-empty `invitations[].sent_date` converted into the process timezone (`TZ`, normally UTC). The Invoice Ninja `date` or `due_date` may differ; that mismatch does not block this stage and the bot never changes either field. `sent_date` is required for sent, partial, and paid candidates. The first observation is deduplicated by stable invoice ID and lifecycle ledger state; Telegram outbox keys are stable per invoice and notice state. Telegram delivery remains at-least-once, so an unknown send outcome can still produce a duplicate delivery.

## Event signal

Configure Invoice Ninja's **sent_invoice** event to POST to:

```
https://<public-host>/invoiceninja/sent-invoice-webhook
```

Set the separate `X-Payplump-Webhook-Secret` header to `INVOICE_LIFECYCLE_WEBHOOK_SECRET`. This event-specific route accepts a bounded JSON object only as a wake signal. It ignores all body fields, including IDs and event names, and makes no resource decision from the webhook. Invoice Ninja 5.13.47 has no unique webhook delivery/event-instance ID and no built-in webhook signature/authentication; repeated signals are therefore coalesced by a generation counter, then resolved by canonical API reads and the durable invoice ledger. Do not send payment events to this route; the existing payment verifier has its own event-specific endpoint and secret.

The worker reads the complete company invoice inventory with bounded pagination and locally selects the managed customer; it then exact-GETs a candidate before recording or notifying. The public Invoice Ninja API read is version-pinned by response header to 5.13.47. No startup API probe is performed.

## Configuration

| Variable | Requirement |
| --- | --- |
| `INVOICE_LIFECYCLE_ENABLED` | `false` by default; set `true` only for an approved pilot |
| `INVOICE_LIFECYCLE_WEBHOOK_SECRET` | Separate 32–256 character ASCII secret; never reuse Telegram, payment-webhook, or API credentials |
| `INVOICE_LIFECYCLE_ALLOWED_TELEGRAM_IDS` | Required comma-separated allowlist of 1–20 canonical positive Telegram IDs; never commit real IDs |
| `INVOICE_LIFECYCLE_WORKERS` | 1–4; default 1 |
| `INVOICE_LIFECYCLE_POLL_INTERVAL` | 100 ms–1 minute; default 1 second |
| `INVOICE_LIFECYCLE_RECHECK_INTERVAL` | 30 seconds–1 hour; default 5 minutes |
| `INVOICE_LIFECYCLE_HTTP_TIMEOUT` | 1–10 seconds; default 5 seconds |
| `INVOICE_LIFECYCLE_MAX_ACCOUNTS` | 1–1000; default 1000 (the explicit pilot allowlist remains capped at 20) |
| `INTEGRATION_PAGE_SIZE` / `INTEGRATION_MAX_PAGES` | Shared bounded Invoice Ninja pagination; defaults 50 / 20 |
| `INVOICE_NINJA_BASE_URL` / `INVOICE_NINJA_API_TOKEN` | Existing Invoice Ninja GET configuration; not stored in source or logs |

`REGISTRATION_EXCLUDED_TELEGRAM_IDS` wins over the lifecycle allowlist. Read `.env.example` for the surrounding integration settings. The lifecycle webhook and Stage 1 worker start only after Telegram webhook setup succeeds.

## Stage 2: overdue enforcement (default-off)

`INVOICE_OVERDUE_ENFORCEMENT_ENABLED=true` is an independent write gate. It additionally requires Stage 1, Telegram, payment verification, the same explicit lifecycle pilot allowlist and exclusions, S-UI 1.6.4, and `INVOICE_OVERDUE_EXCLUSIVE_WRITER=true`. That acknowledgement means no other application, operator, or automation may write to the managed recurring records or S-UI clients during enforcement. Constructing the clients performs no API request; all actions are worker-driven.

An invoice is eligible only when it is the managed subscription's current invoice and the locally persisted Stage 1 observation and a fresh exact-ID Invoice Ninja GET agree on identity, recurring/customer provenance, private marker, invoice number, canonical `invitations[].sent_date`, amount and balance. The status must still be `sent` (status ID 2), `paid_to_date` must be zero and `balance` must equal `amount`. The next local calendar day begins at 00:00 in the process timezone (`TZ`, UTC fallback); `due_date` is deliberately ignored. Partial, paid-to-date, unknown, missing, conflicting, stale, reviewed, or incomplete data is deferred or sent for manual review, never treated as proof of nonpayment. A confirmed payment, payment review, or active payment-verifier lease prevents the write.

The action order is fixed: (1) pause the managed Invoice Ninja recurring record, (2) disable only the S-UI record matched by its persisted binding, and (3) request `closeSessions` for that disabled record. Every step has a durable immutable request intent before transmission and a fresh final invoice check. The same singleton `activation_write_guard` and monotonic epoch serialize these writes with activation; there is no second guard. Only a confirmed adapter result releases the guard. A timeout or otherwise unknown external-write outcome pins the guard for manual reconciliation and is never automatically retried. If payment or a review condition appears between steps, subsequent writes stop; no automatic restoration occurs.

Client and admin notices are enqueued only after all three steps have confirmed results. The `closeSessions` response confirms acceptance of the request, not that every active connection has terminated. This stage does not change Invoice Ninja email reminders, does not issue refunds, and does not restore S-UI access after payment; restoration requires a separate reviewed workflow.

| Variable | Requirement |
| --- | --- |
| `INVOICE_OVERDUE_ENFORCEMENT_ENABLED` | `false` by default; independent Stage 2 opt-in |
| `INVOICE_OVERDUE_EXCLUSIVE_WRITER` | Must be explicitly `true` as an operational acknowledgement |
| `INVOICE_LIFECYCLE_ALLOWED_TELEGRAM_IDS` | Same 1–20 ID allowlist as Stage 1; exclusions win |
| `INVOICE_OVERDUE_WORKERS` | 1–4; default 1 |
| `INVOICE_OVERDUE_POLL_INTERVAL` | 100 ms–1 minute; default 1 second |
| `INVOICE_OVERDUE_HTTP_TIMEOUT` | 1–10 seconds; default 5 seconds |
| `INVOICE_OVERDUE_MAX_ACCOUNTS` | 1–1000; default 1000 |
| `INVOICE_NINJA_BASE_URL` / `INVOICE_NINJA_API_TOKEN` | Existing pinned Invoice Ninja 5.13.47 configuration |
| `SUI_BASE_URL` / `SUI_API_TOKEN` / `SUI_VERSION` | S-UI writer configuration; version must be `1.6.4` or `v1.6.4` |

## Stage 9a: paid renewal and overdue restoration (default-off)

`INVOICE_RENEWAL_ENABLED=true` is a separate writer gate. It requires Stage 1, Telegram, payment verification, the same 1–20 account pilot allowlist, exclusions, and `INVOICE_RENEWAL_EXCLUSIVE_WRITER=true`. It uses the explicitly configured native payment currency; currency is never inferred. Provider construction performs no request, and this worker is not a second payment verifier: it calls the existing GET-only `payments.Observe` path and requires a fresh, exact allocation proof whose hash matches the persisted per-invoice confirmation.

Only an already-managed, non-first current invoice with completed registration and original first-activation provenance is considered. The fresh invoice read must match its customer, recurring ID, activation private marker, and persisted ledger identity. Full manual-cash native-currency evidence is required; partial, refunded, stale, changed, ambiguous, or unsupported proof is held for review. The next local billing date comes from `billing.NextBillingDate(Payment.date)`, not webhook or worker arrival. An elapsed target is held for manual review; the worker does not catch up or advance another month. A valid past UTC `next_send_datetime` baseline is allowed only when the frozen plan will reschedule the recurring record to the strictly future target. Without a schedule, the existing receipt must still be future. A newly confirmed schedule receipt and the receipt observed before resume must be valid and future. An active account paused by this bot's own confirmed overdue pause may be rescheduled and resumed after payment only when its overdue action is in payment review, its known pause is the complete recorded prefix, no overdue step is pending, and the shared writer guard is free; no second pause or S-UI disable/enable is inferred for that prefix.

The worker freezes a small typed plan before any write. Its only possible actions are pause an active recurring record when its date must change, reschedule that same record, enable the already-bound disabled S-UI account for a proven overdue restoration, and resume the same recurring record after a future receipt is confirmed. Each adapter performs its own final GET preflight and then a fresh payment observation and exact invoice/marker check before the store may reserve the next immutable intent under the shared `activation_write_guard` and epoch. Confirmed results release the guard. Unknown outcomes are never replayed and remain pinned for manual review. Existing recurring identity, product price, and invoice history are preserved; this stage does not create or delete invoices, alter reminders, or mark invoices sent.

A successful database finalization records the paid period and known recurring receipt, changes the local subscription back to active, and queues deduplicated notices. It does not claim that the client connected successfully or that protected configuration was delivered. Protected subsequent-period configuration delivery is a separate later stage and is not enabled by this feature.

| Variable | Requirement |
| --- | --- |
| `INVOICE_RENEWAL_ENABLED` | `false` by default; separate Stage 9a opt-in |
| `INVOICE_RENEWAL_EXCLUSIVE_WRITER` | Must be explicitly `true` as an operational acknowledgement |
| `INVOICE_LIFECYCLE_ALLOWED_TELEGRAM_IDS` | Shared 1–20 ID pilot allowlist; exclusions win |
| `INVOICE_RENEWAL_WORKERS` | 1–4; default 1 |
| `INVOICE_RENEWAL_POLL_INTERVAL` | 100 ms–1 minute; default 1 second |
| `INVOICE_RENEWAL_HTTP_TIMEOUT` | 1–10 seconds; default 5 seconds |
| `INVOICE_RENEWAL_MAX_ACCOUNTS` | 1–1000; default 1000 (the explicit allowlist remains capped at 20) |
| `PAYMENTS_CURRENCY_ID` | Existing explicit native-currency setting required by the payment verifier |
| `INVOICE_NINJA_BASE_URL` / `INVOICE_NINJA_API_TOKEN` | Existing pinned Invoice Ninja 5.13.47 configuration |
| `SUI_BASE_URL` / `SUI_API_TOKEN` / `SUI_VERSION` | Existing S-UI writer configuration; version must be `1.6.4` or `v1.6.4` |

## Verification

Run `scripts/test-safe.sh` for package and race tests. PostgreSQL integration tests require a disposable test database and create a private schema; never point them at production. The worker, webhook, date selection, paid-before-first-sweep handoff, first-invoice suppression, and review paths are covered by synthetic tests. Stage 2 PostgreSQL tests cover seeding, shared-guard reservation/release, and no replay after an unresolved intent. In this review-fix iteration, new or changed PostgreSQL integration tests are compile-checked only and are not run against PostgreSQL. The base PR17 had a successful Neon CI run before these fixes, which does not validate the fixed branch or live compatibility. Run the tests only against a disposable isolated schema before release.
