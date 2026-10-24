# Invoice lifecycle: sent-invoice notices and overdue enforcement

В обычном режиме lifecycle observation, overdue enforcement и renewal работают без
отдельных feature flags, pilot allowlists, exclusive-writer numbers или ручных
ревизий. Заполните бизнес-параметры по [инструкции обычного запуска](configuration.md),
включая продукт, родную валюту/ISO-код и `TZ`. Внешнее управление ограничено
доказанными managed provenance, private actor и permission-проверками; unknown
native-write результат оставляет общий барьер, который не снимается restart.
Исторические stage-параметры ниже сохранены для описания прежнего rollout, а не
как команды обычного запуска. Наблюдение использует GET; действия enforcement/
renewal — отдельные проверяемые записи. Ни одна стадия сама не восстанавливает
доступ.

## Safety and eligibility

Исполнитель работает только с подтверждёнными managed-записями и допустимыми
private actors. Он требует provenance регистрации клиента/счёта, совпадения ID
клиента и recurring в Invoice Ninja, подтверждённых шагов создания/запуска recurring
и привязки S-UI. В счёте recurring должен быть точный private marker из
подтверждённого токена `recurring_create`. Invoice Ninja 5.13.47 копирует
`private_notes` recurring-записи в каждый созданный счёт; provenance регистрации
отдельно сверяется с исходным первым счётом. Импортированные, несвязанные,
неоднозначные и неполные записи не принимаются, кроме случаев, явно поддержанных
отдельным проверенным imported-origin потоком.

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

Эти ограничения runtime и секреты применяются в обычном режиме; worker-параметры
можно оставить по умолчанию. Флаги включения функций, pilot-allowlist и
exclusive-writer из прежнего rollout не нужны при обычном запуске.

| Variable | Requirement |
| --- | --- |
| `INVOICE_LIFECYCLE_WEBHOOK_SECRET` | Separate 32–256 character ASCII secret; never reuse Telegram, payment-webhook, or API credentials |
| `INVOICE_LIFECYCLE_WORKERS` | 1–4; default 1 |
| `INVOICE_LIFECYCLE_POLL_INTERVAL` | 100 ms–1 minute; default 1 second |
| `INVOICE_LIFECYCLE_RECHECK_INTERVAL` | 30 seconds–1 hour; default 5 minutes |
| `INVOICE_LIFECYCLE_HTTP_TIMEOUT` | 1–10 seconds; default 5 seconds |
| `INVOICE_LIFECYCLE_MAX_ACCOUNTS` | 1–1000; default 1000 |
| `INTEGRATION_PAGE_SIZE` / `INTEGRATION_MAX_PAGES` | Shared bounded Invoice Ninja pagination; defaults 50 / 20 |
| `INVOICE_NINJA_BASE_URL` / `INVOICE_NINJA_API_TOKEN` | Existing Invoice Ninja GET configuration; not stored in source or logs |

`REGISTRATION_EXCLUDED_TELEGRAM_IDS` excludes explicitly unmanaged actors. Read [configuration.md](configuration.md) for normal business inputs. The lifecycle webhook and worker start only after Telegram webhook setup succeeds.

## Stage 2: overdue enforcement

Enforcement использует обычные проверки managed-записей и разрешений, S-UI 1.6.4
и свежие финальные чтения. Явный pilot allowlist и startup-флаги exclusive-writer
были настройками прежнего rollout и не требуются сервису в обычном режиме.
Создание клиентов не выполняет API-запросов; действия запускают workers.

An invoice is eligible only when it is the managed subscription's current invoice and the locally persisted Stage 1 observation and a fresh exact-ID Invoice Ninja GET agree on identity, recurring/customer provenance, private marker, invoice number, canonical `invitations[].sent_date`, amount and balance. The status must still be `sent` (status ID 2), `paid_to_date` must be zero and `balance` must equal `amount`. The next local calendar day begins at 00:00 in the process timezone (`TZ`, UTC fallback); `due_date` is deliberately ignored. Partial, paid-to-date, unknown, missing, conflicting, stale, reviewed, or incomplete data is deferred or sent for manual review, never treated as proof of nonpayment. A confirmed payment, payment review, or active payment-verifier lease prevents the write.

The action order is fixed: (1) pause the managed Invoice Ninja recurring record, (2) disable only the S-UI record matched by its persisted binding, and (3) request `closeSessions` for that disabled record. Every step has a durable immutable request intent before transmission and a fresh final invoice check. The same singleton `activation_write_guard` and monotonic epoch serialize these writes with activation; there is no second guard. Only a confirmed adapter result releases the guard. A timeout or otherwise unknown external-write outcome pins the guard for manual reconciliation and is never automatically retried. If payment or a review condition appears between steps, subsequent writes stop; no automatic restoration occurs.

Client and admin notices are enqueued only after all three steps have confirmed results. The `closeSessions` response confirms acceptance of the request, not that every active connection has terminated. This stage does not change Invoice Ninja email reminders, does not issue refunds, and does not restore S-UI access after payment; restoration requires a separate reviewed workflow.

| Variable | Requirement |
| --- | --- |
| `INVOICE_OVERDUE_WORKERS` | 1–4; default 1 |
| `INVOICE_OVERDUE_POLL_INTERVAL` | 100 ms–1 minute; default 1 second |
| `INVOICE_OVERDUE_HTTP_TIMEOUT` | 1–10 seconds; default 5 seconds |
| `INVOICE_OVERDUE_MAX_ACCOUNTS` | 1–1000; default 1000 |
| `INVOICE_NINJA_BASE_URL` / `INVOICE_NINJA_API_TOKEN` | Existing pinned Invoice Ninja 5.13.47 configuration |
| `SUI_BASE_URL` / `SUI_API_TOKEN` / `SUI_VERSION` | S-UI writer configuration; version must be `1.6.4` or `v1.6.4` |

## Stage 9a: paid renewal and overdue restoration

Для продления используется явно заданная родная валюта платежей; валюта не
определяется автоматически. Создание provider-клиента не выполняет запросов.
Этот worker не является второй проверкой оплаты: он использует существующий
GET-only путь `payments.Observe` и требует свежее точное allocation-доказательство,
hash которого совпадает с сохранённым подтверждением счёта. Pilot allowlist и
exclusive-writer переменные прежнего rollout не требуются при обычном запуске.

Own accounts continue to require the completed registration and original first-activation provenance described above. This implementation slice also supports a newly observed invoice on an imported recurring subscription only when the immutable verified-import origin, imported recurring profile, exact S-UI binding, inbound-ID evidence, and currently enabled admission/runtime/effect permissions all match. It requires a current non-first invoice in the ordinary invoice lifecycle ledger and the fresh invoice must match its customer, recurring ID, frozen private notes, and persisted ledger identity. An imported subscription's baseline invoice is not seeded: its `first_invoice_id` remains SQL NULL. The follow-up 11b.6.2 path admits an initially paused imported baseline only when the immutable origin still identifies its exact current unpaid debt, with a distinct historical paid anchor and a newly verified full payment for that same current debt. It uses an explicit `imported_initial_paused` action mode and FK to the verified origin, never a fabricated lifecycle-ledger row or OWN overdue journal. The plan schedules only the future period from the new payment date, then enables the existing S-UI binding and resumes the imported recurring record. The original paused source profile, notes, credentials and inbound IDs remain frozen. Full manual-cash native-currency evidence is required; partial, refunded, stale, changed, ambiguous, or unsupported proof is held for review. The next local billing date comes from `billing.NextBillingDate(Payment.date)`, not webhook or worker arrival. An elapsed target is held for manual review; the worker does not catch up or advance another month. A valid past UTC `next_send_datetime` baseline is allowed only when the frozen plan will reschedule the recurring record to the strictly future target. Without a schedule, the existing receipt must still be future. A newly confirmed schedule receipt and the receipt observed before resume must be valid and future. An active account paused by this bot's own confirmed overdue pause may be rescheduled and resumed after payment only when its overdue action is in payment review, its known pause is the complete recorded prefix, no overdue step is pending, and the shared writer guard is free; no second pause or S-UI disable/enable is inferred for that prefix.

The worker freezes a small typed plan before any write. Its only possible actions are pause an active recurring record when its date must change, reschedule that same record, enable the already-bound disabled S-UI account for a proven overdue restoration, and resume the same recurring record after a future receipt is confirmed. Each adapter performs its own final GET preflight and then a fresh payment observation and exact invoice/marker check before the store may reserve the next immutable intent under the shared `activation_write_guard` and epoch. Confirmed results release the guard. Unknown outcomes are never replayed and remain pinned for manual review. Existing recurring identity, product price, and invoice history are preserved; imported writes use the immutable imported profile and native pause/reschedule/resume operations only. For active imported subscriptions, restoration still requires the known pause/disable prefix from the real overdue workflow; arbitrary paused/disabled state is not adopted. The narrowly separate initial-paused mode may replace that prefix only when the immutable verified origin is paused and its exact current debt and distinct paid anchor are validated. In both modes the existing current-proof, three permission-revision, private-actor, S-UI binding, shared guard/epoch, immutable-intent and unknown-outcome barriers remain mandatory. This stage does not create or delete invoices, alter reminders, or mark invoices sent.

A successful database finalization records the paid period and known recurring receipt, changes the local subscription back to active, and queues deduplicated notices. It does not claim that the client connected successfully or that protected configuration was delivered. Protected subsequent-period configuration delivery is separately guarded and still requires its own current entitlement and send checks.

| Variable | Requirement |
| --- | --- |
| `INVOICE_RENEWAL_WORKERS` | 1–4; default 1 |
| `INVOICE_RENEWAL_POLL_INTERVAL` | 100 ms–1 minute; default 1 second |
| `INVOICE_RENEWAL_HTTP_TIMEOUT` | 1–10 seconds; default 5 seconds |
| `INVOICE_RENEWAL_MAX_ACCOUNTS` | 1–1000; default 1000 |
| `PAYMENTS_CURRENCY_ID` / `PAYMENTS_CURRENCY_CODE` | Native currency ID and confirmed ISO code required by payment verification |
| `INVOICE_NINJA_BASE_URL` / `INVOICE_NINJA_API_TOKEN` | Existing pinned Invoice Ninja 5.13.47 configuration |
| `SUI_BASE_URL` / `SUI_API_TOKEN` / `SUI_VERSION` | Existing S-UI writer configuration; version must be `1.6.4` or `v1.6.4` |

## Historical imported renewal rollout evidence (not current startup instructions)

11b.6.1 (ordinary imported-invoice renewal) merged in PR #31 as `a92198b`; exact HEAD `012e1249a0720b79993ce55206413ecae06b178b` passed required CI `40024818619` and post-merge CI `40025798271`, both green. 11b.6.2 initial-paused current-debt restoration merged at `57a6e09d1bd06b806e79145aba065c870b023934`; exact final code HEAD `cd0ff229ed6ecb6ee0f81d5ceeee1c7cc7769502` passed all required CI jobs in `40031643624`, including PG17/race/smoke/Docker. Post-merge CI `40032288658` completed successfully on the exact base. `TestImportedInitialRestorationPaymentProofTargetsCurrentDebt` and all three `TestImportedInitialPausedDebtRestorationWorker` cases passed runtime: expired/empty original schedule, and resume mutation followed by 503/revision-2/restart without replay. The expired case proceeded through actual new normal-cycle GET adoption, paid proof and ledger renewal seed/load while immutable origin lifecycle stayed paused and initial audit stayed unchanged. Current-debt proof never uses the historical anchor; no fake ledger/first invoice, OWN activation or overdue prefix was created. Review P1 on initial-mode claim locking was fixed/rechecked with zero final findings; parent-mode IFF and SQL batch bounds were verified. CI `40029673909` and `40030663139` were test-fixture failures only, fixed without weakening production checks. No live API, Telegram, payment, working DB, publish or deploy was used.

11b.7.1 original-active-baseline protected delivery merged as PR #33 at `4a6522d1b881ef031ef580c67486406237ecf004`; exact final HEAD `e2ca6863aa785681433932f3a9caf82a0d46319b` passed required CI `40035675235` (all three jobs, including PG17 integration/race and smoke/Docker), and post-merge main CI `40036327450` succeeded. All three imported-configuration SQL workflows passed runtime. Scope is only the ORIGINAL ACTIVE imported baseline: current invoice equals immutable origin current invoice and paid anchor; current proof, strictly future UTC date, static binding/inbounds, legacy price/notes, source-scope/public-base GET fences, legitimate immutable origin FK/mode, and current configuration/admission/runtime revisions. `first_invoice_id` stays SQL NULL; no fake OWN history; OWN first/renewal paths unchanged. GET-only reads need no billing-effects permission; configuration policy separately and explicitly gates secret Telegram delivery. No auto-enable; keep the existing per-chat unknown-send barrier.

Independent review `34c46fce-9abc-42a` covered production, both domain/unit tests and the PostgreSQL workflow. Two P2 findings (currency-policy drift and test-worker bot scope) were fixed and confirmed; final P1/P2/P3 findings are zero. The three DB workflows passed runtime in required CI; coverage includes successful GET-to-protected delivery/guard release, pre-GET policy denial, GET-verified changed-payment sticky review (2007), runtime permission ABA denial, and unknown-send 503 with permanent chat barrier across revocation/new request/restart. This is not live consumer readiness. Historical pre-merge CI status for 11b.7.2 (superseded by the latest status below): PR #34 was at `815d54c6e019e03e5a8391dccaec21e2e7a39fd3`; four new workflow cases are authored and locally compile-only. First CI `40038835592` passed fast checks but PostgreSQL integration failed before any feature scenario on migration 024 SQL syntax (`SQLSTATE 42601`, mismatched parentheses). No SQL workflow runtime has occurred. It covers protected imported CURRENT COMPLETED renewals for ordinary ledger renewal and initial-restored current debt: explicit current payment proof, completed native plan/journal/receipts, strictly future current UTC date. Immutable origin history/paid anchor is never current entitlement; current proof/audit stays separate and `SharedImportedBaseline` immutable. Add `RenewalAudit.OriginMode` as new `omitempty` (`ledger` or `imported_initial_paused`); renewal request keeps `activation_id` SQL NULL, conditional generated baseline-origin invoice FK and existing current-renewal composite FK. Preserve old requests/fingerprints, with no fake activation or ledger. Current admission/runtime/config policy must match; effects may be off after completed known actions, and historical completion permission revisions need not equal current read/request revisions. Preserve GET-only SUI/Ninja/profile/inbounds/source-scope/public-base/native-currency/date/UTC checks and in-memory-only URL assembly after final DB authorization. Keep the unknown-outcome barriers distinct: an uncertain native Invoice Ninja/S-UI write pins the shared global `activation_write_guard`/epoch and leaves local workflow held; an unknown secret Telegram transmission pins `configuration_chat_guards` for that chat. Neither clears or substitutes for the other. The new Resume-mutation/503 workflow tests the native global guard/local overdue state and ordinary menu denial without a configuration request; it is not a Telegram unknown-send case. Do not broaden baseline denial until full audit verification. Domain/test/Store producers report implementation ready; Store is correcting the migration syntax without weakening predicates. Parent full normal unit/race/vet/CGO0 build/module/gofmt checks and PostgreSQL compile-only passed before that correction. Independent review `e370ed66-465a-4ef` found zero behavioral/design findings on the pre-CI full patch; reviewer ran no checks. The migration fix requires narrow recheck. Corrected-HEAD CI, SQL runtime and merge remain pending. Apply CLI/default-off startup wiring remains later work; Stage 11 is incomplete. No live provider/Telegram/payment/working DB/publish/deploy is claimed.

## Historical imported-delivery evidence (not current startup instructions)

PR #34's 11b.7.2 protected current-renewal delivery merged at
`1c21061e0ad8f02fab45678c9d92d343855fd26a`; exact final HEAD
`1cdd30d4b4b4fbb0bf3544a64ff31156a705bd60` passed required CI `40040735229`
and post-merge main CI `40041362995`, all jobs green. Four new SQL workflows
passed runtime: `TestCompletedImportedRenewalAllowsProtectedConfiguration`,
`TestCompletedImportedInitialRestorationAllowsProtectedConfiguration`,
`TestUnknownInitialRestorationDeniesConfiguration`, and
`TestCompletedImportedRenewalStaleConfigurationRevisionDenied`. This does not
change Stage 1/2 default-off status, authorize live use, or mean imported runtime
is wired at startup. The `import-apply` CLI is delivered in PR #35 and passed
exact-head required CI `40044946187` and post-merge CI `40045550655`, including
both PostgreSQL CLI workflows. The separate 11b.9 startup/read-runtime implementation merged in PR #36 at
`a5fd93b28f0c0fe78e9485ddbe169837af49544b`; exact code HEAD
`277b37c12433f271ee72d60d486551644749de92` passed required CI `40046909184`
and post-merge main CI `40047448373`. Its PostgreSQL startup workflow passed;
full review `164166cb-25b2-465` had zero proven behavior/design findings. No live
API/Telegram/payment/database/publish/deploy was used.

## Imported read-runtime permission (historical rollout evidence)

Imported-origin lifecycle invoice observation remains GET-only: it can discover
ordinary new recurring invoices and update the normal local invoice ledger and
notices under the imported-runtime permission, but it does not write to Invoice
Ninja/S-UI, restore access, or deliver a protected URL. The imported runtime
permission is distinct from import admission and billing-effect permissions.

The 11b.9 startup wiring and tests are delivered and independently reviewed
(`164166cb-25b2-465`; zero proven behavior/design findings). Exact-head
PostgreSQL CI passed. Its startup fence disables imported effect permissions before
readiness; if runtime is configured off it also disables imported runtime.
Explicit runtime installation requires a matching configured policy after the
ordinary bot setup. It does not enable effectful workers, admission, or
protected configuration delivery.

The authored PostgreSQL workflow checks that before bot setup the empty schema
has no admission/workflow/permission rows and no source GET (known bot metadata
and migration/guard infrastructure may exist, so this does not mean an empty
DB). It checks guard epoch/owner unchanged across all steps, read-worker GET
count stopping after completion, revision-1 read denial and explicit revision-3
success, effects disabled at revision 2, admission enabled at revision 1,
unchanged origin, and the full normal payment-confirmation proof. These assertions passed in the exact required CI run recorded above.

## Imported effects startup delivered (11b.10; historical rollout evidence)

PR #37 merged at `b33fac67a1e19c8a3da00b2d7d261b170e0f02e8`; exact final HEAD
`9d93f649249f491ff70ec7a6c34af959b48fde3a` passed required CI
`40048848558`, including fast/race, disposable PostgreSQL 17, full native
known/unknown workflows and Docker smoke. Post-merge main CI `40049317238`
succeeded. `TestImportedEffectsStartupWorkflowRealPostgres` passed in CI.

The historical rollout used `IMPORTED_EFFECTS_ENABLED` and an explicit
`IMPORTED_EFFECTS_REVISION`; these are not normal startup inputs. In normal mode
imported runtime/effect permissions are internally managed and still require
matching current policy, admission, immutable origin/current invoice/proof and
provenance. Permission changes make no source GET or provider write and do not
alter `first_invoice_id`, global guard epoch/owner or configuration-delivery
barriers. Restart does not regrant a disabled permission or clear unknown barriers.
Provider construction is pure/no-HTTP but returns guarded native billing
mutators to ordinary consumers, not GET-only clients. `ProtectedRecordReader`,
`RecurringReader`, inventory and settings remain strict GET-only interfaces.
Effects-off may allow authorized fresh-GET finalization of known original
receipts, but denies new or incomplete native writes. Live rollout and owner
verification/admission remain unperformed. Next stages: 12 synthetic isolated
restore CI, then 13 release preparation (publish off), then 14 synthetic
measurements and owner gates.

## Verification

For local normal tests, use explicit scopes such as `scripts/test-safe.sh -count=1 ./...` or `go test ./internal/lifecycle ./internal/invoiceninja ./internal/postgres`; an explicit race run is `scripts/test-safe.sh -race -count=1 ./...`. The only permitted local PostgreSQL integration invocation for this work is compile-only: `scripts/test-safe.sh -run='^$' -tags=integration ./internal/postgres`. Do not run local PostgreSQL integration tests or configure a local database for this change. PostgreSQL runtime verification belongs in PR CI or disposable isolated infrastructure; no live API, Telegram, production database, publish, or deployment checks are authorized here.
