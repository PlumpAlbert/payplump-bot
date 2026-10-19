# Invoice lifecycle: sent-invoice notices (stage 1)

This stage is independently **default-off**. It observes Invoice Ninja recurring invoices using GET requests, records canonical observations in PostgreSQL, queues Telegram notices in the existing outbox, and hands the new `current_invoice_id` to the existing payment verifier. It does not create/update invoices, write to S-UI, disable access, pause recurring, decide overdue status, or restore access. Those actions require separate reviewed stages.

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

`REGISTRATION_EXCLUDED_TELEGRAM_IDS` wins over the lifecycle allowlist. Read `.env.example` for the surrounding integration settings. The lifecycle webhook and worker start only after Telegram webhook setup succeeds.

## Verification

Run `scripts/test-safe.sh` for package and race tests. PostgreSQL integration tests require a disposable test database and create a private schema; never point them at production. The worker, webhook, date selection, paid-before-first-sweep handoff, first-invoice suppression, and review paths are covered by synthetic tests. PostgreSQL integration tests were added and compile with the integration build tag, but must still be run against a disposable isolated schema before release. These tests do not validate a live Invoice Ninja installation, a Telegram delivery, or operational configuration.
