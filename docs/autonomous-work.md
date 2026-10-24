# Автономный проход по оставшейся реализации

## Разрешения и границы

Владелец разрешил автономно продолжать безопасную реализацию без ожидания owner-gate.
Для каждой небольшой единицы сначала обновить план, затем выполнить код и тесты и
организовать независимое read-only review. Родитель выполняет commit/push, создаёт
PR, ждёт зелёный CI на точном HEAD и сливает PR; после этого обновляет план и
выбирает следующий срез. Останавливаться только когда действительно требуется
решение или действие владельца. Не выполнять Git/VCS, PR, CI или другие
API-действия из implementation worker.

Invoice Ninja и S-UI — **production с действующими пользователями**. Не обращаться
к установленным API даже для GET; контракты изучать по закреплённым публичным
исходникам. HTTP-тесты используют только синтетические loopback-серверы. Не
использовать production API, живые данные БД, настоящие Telegram-доставки или
платежи, публикацию в registry и Komodo deploy. Не читать и не менять `.env` и
`.pi`; реальные идентификаторы и конфигурации не нужны.

Локально PostgreSQL integration tests только компилировать, не запускать runtime
SQL. Runtime integration SQL выполняется только в GitHub Actions на disposable
PostgreSQL 17. Текущий required CI по умолчанию использует эту локальную ephemeral
БД без GitHub Environment secrets; Neon доступен только через ручной
`workflow_dispatch` opt-in и не является обычным gate. Родитель проверяет все
обязательные jobs именно для HEAD PR перед merge.

Все новые effectful-функции выключены по умолчанию. Неизвестная внешняя запись
не переотправляется по истечении lease. Сохранять существующие барьеры/epoch,
immutable intent, проверку текущей оплаты, собственную provenance и private actor
scope; не снимать и не обходить их и не расширять capabilities чужих компонентов.
Использовать простые штатные extension points: без generic Saga и DI-фреймворков.
Отдельные несогласованные writer-барьеры не допускаются.

## Рабочая очередь

- [x] 8a: source-pinned disable / closeSessions, recurring pause / schedule / resume,
  GET-only discovery управляемых recurring-счетов, синтетические контракты.
- [x] 8b: Stage 1/2 — ledger, notices, overdue workflow и подтверждения внешних
  шагов; runtime integration проверен полным CI PR #18 на disposable PG17.
- [x] 9a: durable renewal/reactivation PR #20 merged in base `94095a8`; its
  final HEAD `952bce4063065c71462d936f71507ee151d557f3` passed required Actions
  run `39961598201`, including PG17/race, normal/late/unknown renewal and smoke.
- [x] 9b: PR #21 влит в базу `b98d351d20e2cf2a420ba773c3af464ba1564e64`;
  финальный HEAD `1c63194dde4920c0e8157be59f533a16ea4352e6`. Required CI на
  исходном code HEAD `925364d85ac733d6f582d5f2bc83d5f2a0b40525` прошёл:
  [run 39975961988](https://github.com/PlumpAlbert/payplump-bot/actions/runs/39975961988),
  включая `fast-checks`, `postgres-integration`, `test`, PostgreSQL SQL/race,
  smoke и Docker checks. После merge required main CI также прошёл:
  [run 39978010690](https://github.com/PlumpAlbert/payplump-bot/actions/runs/39978010690).
  Устаревший статус «PR #21 ожидает merge» удалён. Live-проверки владельца
  остаются pending.
- [x] 11a.1: read-only inventory providers — Invoice Ninja `FindCustomers` /
  `ListRecurring` (source-pinned 5.13.47, bounded) и S-UI `ListIdentities`
  (strict 1.6.4, bounded, redacted). PR #22 merged коммитом
  `e81034a88c05dae56cde0d1a45ddf5a0d5791e77`; exact HEAD
  `674f838e80c6bc6b89e240fee79b219311b5f1e8` прошёл required CI
  `39980684412` и post-merge CI `39981437193` (оба all-green). RecordReader
  сохраняет только `GetRecord`, `sui.IdentityReader` отдельно владеет
  `ListIdentities`; исправлена совместимость `metadata.pagination.links`.
- [x] 10: приватная административная рассылка с неизменяемым подтверждением,
  снимком получателей, долговечным прогрессом и ограниченной доставкой.
- [ ] 11: выполнять последовательно небольшими срезами. Инертный
  import-plan/staging остаётся inert и не является verified reconciliation.
  11a.1 завершён (см. выше). PR #23 code HEAD
  `d792ffb39aae8d56bc09cfc2a25be0a755d5ba5c` влит squash-коммитом main
  `0131f618b2ba4ca364be2a8a9e7fe0f38160c32f`; required CI `39986043209` и
  post-merge main CI `39986939558` зелёные.

  History-only slice добавляет paused exact-current-debt + unique fully paid
  historical-anchor report через GET-only источники. Real-reader loopback tests
  включают несколько исторических месяцев, отрицательные сценарии и
  аудитируемые private snapshots долга/settings. Review завершён: исправлены
  valid older paid-history handling и P2 отсутствовавших snapshots. Parent full
  unit/race/vet/CGO0 build/module verification и PostgreSQL integration
  compile-only прошли. Исторический лимит 1..20; превышение не усекает inventory.
  PR #24 code HEAD `b24bf849ca4e3f6589c82ef2bebb885b5419702a` merged as main
  `9fe0c2aaec847a4fe2948f09d755a346318e5c4c`; required CI `39989051793` and
  post-merge CI `39989900953` passed, including PG17/smoke/Docker.
  11a.2 operator `import-verify --read-only` CLI and real-reader synthetic HTTP
  workflow bridge are implemented; positive factory/source-reader and private
  artifact checks pass, as do exclusion, bad-hash and no-payment-source cases.
  Independent review completed with no code defects; one P3 documentation
  inconsistency was fixed here. PR #25 merged as `3b54c82`; exact code HEAD
  `02b0c8c7d882e37c713416f39ddcad42ff711baa` passed required CI `39992168476`
  and post-merge main CI `39992922113`. No live verification.

  11b.1 DB-only foundation merged in PR #26 as
  `76dfcda8a13b88651cc42f8821e1cf2c0fb94191`; exact PR code HEAD
  `d0a2ca665be507d1913db9bbe2301e945be34e0a` passed required CI
  `40001910084` and post-merge main CI `40002786608`, both all-green. Required CI
  covered six fresh GET → opaque preparation → DB admission workflows, full
  PostgreSQL 17 integration, race, smoke, and Docker checks. This closes the
  earlier local PostgreSQL runtime-coverage gap; local SQL checks remain
  compile-only. No live provider/API access or deployment was performed.

  11b.2 provider-only imported Invoice Ninja profile and typed pause/schedule/
  resume adapter merged in PR #27. It preserves frozen source facts and legacy
  notes, remains unwired/default-inert, and adds strict exact-shape v1 journal
  decoding without tightening provider GET DTOs. Independent review
  `2b1af0d5-5d94-40f` completed with one P3 journal-shape finding fixed; parent
  also fixed current tax/surcharge and legacy zero-flag compatibility, durable
  expired-journal decoding, and resume local-date/UTC checks. PR #27 exact HEAD
  `0fad9355c818424089a72aa1211bf33be5ab6ced` passed required CI `40005099529`
  and post-merge main CI `40005923953`, all green. No live API access or
  unapproved deployment occurred.

  11b.3 payment-tracking-only imported-origin bridge merged in PR #28 as
  `925d994619864e4f07b9d72fcb2e68cdfcd8e449`; exact HEAD
  `839144922e91fb27bfea7b08b8f204b437c38545` passed required CI run
  `40010097193` all-green, including PostgreSQL 17 race/runtime imported
  payment cases, old-pre-010 migration fixture, smoke, and Docker checks.

  11b.4 read-only invoice lifecycle tracking completed in PR #29, merged as
  `149d6b679ff007b1cff6d9679862675f03048412`. Exact HEAD
  `4c7a879da681ea75410526fc98f7bc37ab18e096` passed required CI `40014461532`
  and post-merge CI `40015300957`; all three `TestImportedInvoiceLifecycle...`
  workflows passed, including the full three-phase worker/actual CycleReader
  PostgreSQL path. The nullable-first-invoice CAS defect was fixed before merge.

  11b.5 imported overdue-only code and tests merged in PR #30 as
  `19d45cf7f85c784b74cdc0cb2b71f5d15cddb758`. Exact code HEAD
  `b8efc247fe09b78e078b0c115c52317d47311939` passed required CI `40019995948`
  and post-merge CI `40020785993`, all green. The workflow covered full
  GET/admit/baseline-observe/CycleWorker/EnforcementWorker success, known
  closeSessions response after effect disable, and unknown pause/503/revision-2/
  restart with the guard pinned. No live API access or deployment was performed.

  11b.6.1 imported paid renewal supports ordinary lifecycle-ledger invoices.
  PR #31 merged as `a92198b`; exact code HEAD
  `012e1249a0720b79993ce55206413ecae06b178b` passed required CI `40024818619`
  and post-merge CI `40025798271`. 11b.6.2 merged at
  `57a6e09d1bd06b806e79145aba065c870b023934`; exact final code HEAD
  `cd0ff229ed6ecb6ee0f81d5ceeee1c7cc7769502` passed required CI `40031643624`
  (all three jobs, including PG17/race/smoke/Docker), and post-merge CI
  `40032288658` completed successfully on the exact base. Runtime passed
  `TestImportedInitialRestorationPaymentProofTargetsCurrentDebt` and all three
  `TestImportedInitialPausedDebtRestorationWorker` cases: expired/empty original
  schedules and resume mutation followed by 503/revision-2/restart without replay.
  The expired case also completed new normal-cycle GET adoption, paid proof and
  ledger renewal seed/load while preserving immutable origin lifecycle/audit.
  No fake ledger/first invoice, OWN activation or overdue prefix was created.
  Review P1 on initial-mode claim locking was fixed and rechecked; final findings
  zero. Parent-mode IFF/batch-bound checks were verified. CI `40029673909` and
  `40030663139` exposed test-only fixture issues; fixes preserved production
  checks. No live API, Telegram, payment, working DB, publish or deploy occurred.
  11b.7.1 protected delivery for the ORIGINAL ACTIVE imported baseline merged as
  PR #33 at `4a6522d1b881ef031ef580c67486406237ecf004`; exact final HEAD
  `e2ca6863aa785681433932f3a9caf82a0d46319b` passed required CI `40035675235`
  (all three jobs, including PostgreSQL 17 integration/race and smoke/Docker)
  and post-merge main CI `40036327450` succeeded on the exact base. All three
  imported-configuration SQL workflows passed: original paid-baseline GET→verify→
  admit→observe→private menu/outbox/protected service/Telegram success and guard
  release; public-base denial before provider GET and changed-current native proof
  sticky review 2007 after GET; runtime permission ABA denial and unknown-send 503
  with durable per-chat barrier across revocation/new request/restart. Review
  `34c46fce-9abc-42a` fixed both P2 findings (currency-policy drift, test-worker
  bot scope); final findings zero. No live provider/Telegram/payment/working DB/
  publish/deploy. This does not claim live consumer readiness.

  11b.7.2 implementation and four workflow cases were ready; PR #34 is now open
  at `815d54c6e019e03e5a8391dccaec21e2e7a39fd3`. Scope is protected imported
  CURRENT COMPLETED renewal: explicit current proof, completed native
  plan/journal/receipts, future current UTC date. Immutable origin/paid anchor is
  history, not entitlement/current selector; current proof/audit stays separate.
  Renewal `Current.ImportedBaselineProofHash` is zero; pinned origin proof stays
  in `Entitlement.Imported`. Preserve old OWN/baseline fingerprints and
  `RenewalAudit.OriginMode` `omitempty` (`ledger`/`imported_initial_paused`);
  renewal request `activation_id` remains SQL NULL with conditional generated
  baseline-origin invoice FK plus current-renewal composite FK. No fake
  activation, ledger, or historical payment-confirmation rows. Validate origin
  proof via canonical admitted evidence; current proof/date/amount are independent
  of anchor dates. Native audit covers both imported modes, not OWN decoder;
  require exact seven-key plan and known receipts. Initial mode may omit Schedule
  only if target is already future; past/empty old UTC needs a known new schedule
  and future receipt, with no Pause. Current gates match; completed actions may
  survive effects-off and their revisions may be historical.

  Distinguish barriers: unknown native Invoice Ninja/S-UI write pins the shared
  global `activation_write_guard`/epoch and leaves local workflow held. Unknown
  secret Telegram transmission pins `configuration_chat_guards` for that chat.
  Neither clears or substitutes for the other. The new unknown-native test is
  Resume mutation→503, pinned global guard/local overdue, then ordinary menu
  denial without a configuration request; it is not a Telegram-unknown test.

  Domain `dcb4a0e2-70f3-4ca`, tests `bda905ec-faef-45c`, and initial Store
  producer `e7576737-ba1d-471` completed implementation; Store is correcting a
  migration 024 syntax error without weakening predicates. Parent full
  `-count=1 ./...`, race, vet, CGO0 build, mod verify, gofmt and PG compile-only
  checks passed (logs `/tmp/payplump-imported-renewal-config-{unit,race}.log`
  and `/tmp/payplump-imported-renewal-config-domain-race.log`). Four workflows
  compile locally but have not reached SQL runtime. First PR CI `40038835592`:
  fast checks passed; PostgreSQL integration failed before scenarios parsing
  migration 024 (`SQLSTATE 42601`, mismatched parentheses). This is a real
  migration parse failure, not a fixture-only issue. Independent review
  `e370ed66-465a-4ef` had zero behavioral/design findings on the pre-CI full
  patch; narrow recheck after syntax fix is pending. Corrected-HEAD CI, SQL
  runtime, merge and PR34 delivery remain pending. No owner blocker; Stage 11
  remains incomplete.

  Непереговорные TODO: actor/pilot ограничения сохраняются; будущий managed
  consumer обязан подавлять устаревшие операции/уведомления. Неоплаченные долги
  остаются ручными, без прощения или автоматизации. Не добавлять 11c или новое
  архитектурное решение.
- [ ] 12: дальнейшая подготовка синтетической isolated restore rehearsal;
  recovery report, restore helper и docs уже есть. Не выполнять автоматические
  rearm/снятие барьеров; реальные backup/restore требуют владельца.
- [ ] 13: release manifest и ручной opt-in build/publish workflow; Compose и
  validator уже есть. Никаких реальных публикаций или deploy.
- [ ] 14: синтетическая подготовка и тесты safe resource harness уже есть;
  целевые измерения VPS не выполнены и требуют владельца.

Очередь последовательная по небольшим PR: 9a и 9b завершены; 11a.1, 11a.2
core/history и operator CLI merged. PR #25 exact-HEAD required CI `39992168476`
и post-merge CI `39992922113` зелёные. 11b.1 merged и прошёл полный required
PostgreSQL 17 CI; 11b.2 merged as PR #27, exact HEAD
`0fad9355c818424089a72aa1211bf33be5ab6ced`, required CI `40005099529` and
post-merge CI `40005923953` all green. 11b.3 payment-tracking-only merged as PR #28 (`925d994`), exact HEAD
`839144922e91fb27bfea7b08b8f204b437c38545`, required CI `40010097193` all green.
11b.4 is merged as `149d6b679ff007b1cff6d9679862675f03048412`; exact HEAD
`4c7a879da681ea75410526fc98f7bc37ab18e096` passed required CI `40014461532`
and post-merge CI `40015300957`, including the full PostgreSQL runtime workflow.
11b.5 overdue-only code and tests merged in PR #30 as
`19d45cf7f85c784b74cdc0cb2b71f5d15cddb758`; exact code HEAD
`b8efc247fe09b78e078b0c115c52317d47311939` passed required CI `40019995948`
and post-merge CI `40020785993`, all green. 11b.6.1/11b.6.2 merge, runtime
and CI evidence is recorded above. 11b.7.1 is merged and runtime/CI verified as
recorded above. 11b.7.2 implementation and four PG workflow cases are ready;
parent checks passed and independent review `e370ed66-465a-4ef` found zero
behavioral/design findings. The cases only compile; SQL runtime, exact-HEAD CI,
PR #34 and merge remain pending. Remaining 11b includes 11b.7.2 delivery,
apply CLI and default-off startup wiring. Stage 11 is incomplete.
No live verification/admission or deployment was done.

## Historical handoff status before PR #35

The above PR #34 status is superseded: PR #34 merged at
`1c21061e0ad8f02fab45678c9d92d343855fd26a`; exact final code HEAD
`1cdd30d4b4b4fbb0bf3544a64ff31156a705bd60` passed required CI `40040735229`
(all jobs, PG17/race/smoke/Docker), and post-merge main CI `40041362995`
succeeded. All four new SQL tests passed: `TestCompletedImportedRenewalAllowsProtectedConfiguration`,
`TestCompletedImportedInitialRestorationAllowsProtectedConfiguration`,
`TestUnknownInitialRestorationDeniesConfiguration`, and
`TestCompletedImportedRenewalStaleConfigurationRevisionDenied`. Preserve the
honest history: `40038835592` was a real migration syntax failure before cases;
`40039692048` passed migration/old SQL but failed four test-only boundaries;
`40040735229` passed all. No fence changes or live verification.

Apply CLI production code, expanded unit tests/finite fixture, and isolated PostgreSQL workflows are complete. Parent normal unit/race/vet/CGO0 build/module/gofmt/diff checks passed; tagged command+PostgreSQL compile-only passed:
`scripts/test-safe.sh -run='^$' -tags=integration ./cmd/payplump ./internal/postgres`.
Independent review `67ca1346-a4f9-49c` covered the full 936-line patch, four new command files, main/CI and admission/source contracts; zero proven behavior/design findings. SQL runtime has NOT run, exact-HEAD CI is pending, and PR #35 is not open. Proposed CI adds command tests only to disposable PG17; manual Neon remains unchanged.

Safety contract: validate arguments, canonical positive revision, private regular mode-0600 nonsymlink file and hash before env/factory/DB. Fresh GET-only `VerifyProviders` plus concrete `importverify.Service` prepares complete opaque plan before DB open; reports/artifacts are not authority. Reuse source policy/pinned versions/public base/inbounds/TZ. `DB_MAX_CONNS` defaults to 4, canonical 1..64. Existing known `telegram_bots` row required; no GetMe, bot metadata manufacture, workers or auto-migration. Safe plan hash/counts only. `--apply` mandatory; `--enable-admission` separate and default-off. Permission sync may persist if later batch apply fails; no automatic rollback/rearm. It does not enable imported runtime/effects/configuration. PG workflows are loopback PG17/`TEST_DATABASE_URL` only, empty `PGOPTIONS`, one-query explicit SSL mode; direct current schema and exact search_path checked before store open/migration, cleanup errors reported. They cover fresh empty-schema default-off denial/no rows, explicit enable/apply/replay, and revocation. Unit tests cover provider 503/unpaid, invalid config/TZ/pool/revision/hash/mode/symlink/evidence flag, early no-DB dispatch, explicit-only policy sync and safe errors/output. No live execution. Next: parent exact-HEAD CI and PR #35/merge, then default-off startup/import-permission/runtime wiring. Stage 11 remains incomplete.

## Проверенные срезы

- PR [#12](https://github.com/PlumpAlbert/payplump-bot/pull/12), squash
  `de18ff0ce1f1a6dad389523bce8205cfcb3ff35d`: lifecycle-адаптеры и общий
  immutable dispatch anchor `010`. Проверенный HEAD
  `dc88789b4b42821ed6eb4f534b47ab26cf1f42be`; прежние Neon/race результаты и
  loopback workflow — история того PR, не текущая конфигурация CI.
- PR [#13](https://github.com/PlumpAlbert/payplump-bot/pull/13), squash
  `7a5fbc5fd5fe6b8aec4cee806a4e51636c988913`: приватные подтверждённые рассылки.
  Проверенный HEAD `4b1989710701e4cb73e35bd69d1c87ed38612536`; прежние Neon/race
  результаты и локальный Neon runtime — исторические проверки того PR. Feature
  остаётся default-off.
- PR #16 добавил Stage 1, PR #17 — Stage 2; оба выключены по умолчанию. PR #18
  устранил замечания review R1–R25. Он влит коммитом
  `ebf8dc0d854ce8b138a77061b8a5578f478d8f5f`; проверенный PR HEAD
  `40c486ff9f3e850be41e2c677f90fbb97e95733e`.
- GitHub Actions run
  [39947501730](https://github.com/PlumpAlbert/payplump-bot/actions/runs/39947501730)
  зелёный на точном PR HEAD: `fast-checks`, `postgres-integration`, `test`.
  Runtime прошёл командой `go test -v -race -tags=integration -count=1 -timeout=60m ./internal/postgres` на disposable PostgreSQL 17; оба случая
  `TestInvoiceOverdueWorkerStoreAdaptersLoopbackWorkflow` прошли. Также прошли
  smoke, Compose/Docker build/start/restart и release Compose validation.
  Прежнее compile-only примечание относилось к локальной проверке; последующий runtime Stage 8 подтверждён указанным Actions run.
  Более ранний Neon CI PR #17 до review fixes остаётся только историческим
  результатом и не является подтверждением текущего конфигурационного пути.

Stage 8a/8b реализованы. Финансовая проверка уже принимает non-first текущий
invoice только с точным recurring ID; подтверждения хранятся per invoice, а
`FinishPaymentCheck` сохраняет доказательство и наблюдаемые даты без выдачи доступа.
Этапы 9a и 9b merged; код и сквозные тесты 9b прошли required CI на исходном
code HEAD `925364d85ac733d6f582d5f2bc83d5f2a0b40525`, включая PostgreSQL runtime;
см. run `39975961988` и job `120491239073` выше. Устаревшее ожидание merge PR #21
снято. Этот CI не подтверждает live-проверки владельца. Использовать существующий
payment-check pipeline, не создавать новый verifier-worker. Автоматизация не
получает разрешение на production API даже GET. Recurring может создавать draft,
поэтому workflow проверяет фактическое состояние и не предполагает issued invoice.
Не менять чужие company/client-wide настройки.

## Отдельные gates владельца

Владельцу нужны реальные проверки на выделенном клиенте и установленных версиях:
полный платёжный цикл, отключение/разрыв сессий/восстановление, импорт и связь
sing-box, webhooks и Telegram. К его инфраструктурным решениям относятся рабочий
импорт, внешние backup, restore/rollback инфраструктуры, целевые измерения VPS,
публикация и Komodo deploy. Не обращаться к этим системам и не использовать
живые данные без такого gate. Успешные исходники и синтетические тесты их не
заменяют; ожидание gate не задерживает безопасный код и synthetic preparation.

## Historical handoff before PR #37 (superseded below): 11b.10 pending

Repository `/Users/plumpalbert/dev/payplump-bot`, branch `feat/import-runtime-effects`, baseline/merged PR #36 `a5fd93b28f0c0fe78e9485ddbe169837af49544b` (exact main base). PR #36 code HEAD `277b37c12433f271ee72d60d486551644749de92` passed required CI `40046909184` (all three jobs: fast, race, disposable PostgreSQL 17, smoke/Docker) and post-merge main CI `40047448373` succeeded. `TestImportedRuntimeReadOnlyStartupWorkflowRealPostgres` passed. The successful fresh CLI/admission/GET-only preparation/current-manual-payment proof workflow never fabricated OWN/first invoice/activation/ledger/operation/outbox/configuration request. Effects row remained absent until test-only manual API setup; read-enabled startup fenced effects at revision 2 and left read permission revision 1 untouched. Default replacement runtime-off durably fenced both permissions without repeated revision bumps. Old read revision 1 was denied, explicit revision 3 accepted, effects remained off, and admission revision 1 remained enabled. Origin proof/evidence immutable; global guard epoch/owner unchanged. Loader/fence/install caused no source GET; worker GET stopped after worker completion. Fresh schema means no new admission/workflow/permission rows before known bot setup, not an entirely empty DB.

11b.10 implementation and tests are complete; all three code producers are done and no active producers remain. Current PR #37 write set is 13 paths: seven implementation/config paths (`cmd/payplump/import_runtime.go` and `_test.go`, limited `cmd/payplump/main.go`, `.env.example`, `cmd/payplump/import_runtime_providers.go` and `_test.go`, and `cmd/payplump/import_effects_workflow_integration_test.go`) plus these six documentation files. The seven implementation/config paths are the initial staging set; no source fields or secrets are included. Parent full normal unit/race/vet/CGO0 build/module verification/gofmt/diff passed (logs `/tmp/payplump-import-effects-{unit,race}.log`); tagged command+PostgreSQL compile-only passed. Parent caught and fixed test assertions: bot mismatch with effects denied must retain read permission (expected counts `1,1,0`, not `1,0,0`); the effects bad-revision case must use read revision 7, valid for read, so it reaches the effects revision check and confirms no source GET. `.env.example` terms clarify admission versus read-runtime permissions. Independent full review `e72f63f9-cad9-4ed` examined exact 827-line patch `/tmp/payplump-import-effects-review.patch` plus durable-effects/source-scope/core contracts and found zero proven behavior/design findings. Do not report SQL runtime for the new workflow: it has not run. Exact-HEAD CI, PR #37 and merge remain pending.

Contract: add default-off `IMPORTED_EFFECTS_ENABLED=false` and `IMPORTED_EFFECTS_REVISION=`. Revision is canonical positive 1..int64 iff explicit effects flag is true; effects require read runtime; invalid combinations fail before source/DB. Source-policy fingerprint and admission/config-secret policy remain unchanged/separate. Only install effects for actual verified configured bot after ordinary bot/config setup and successful read sync. False flag does not install/call permission sync. Startup fence matrix: effects false durably disables effects; read false durably disables runtime; both true do not bump current revisions on idempotent restart; read false/effects true rejects. This supersedes PR #36's always-fence-effects rule only when effects are explicitly enabled and acknowledged. No admission grant, secret-policy upgrade, global/unknown/chat-barrier rearm. Provider factory is pure/no HTTP, supplies `*invoiceninja.ImportedRecurringClient` only to enabled normal billing consumers when read runtime is enabled and source scope exactly matches; preserve ordinary consumer configs/actor bounds and use existing native worker constructors, with no new queue or fake mutators. Read runtime with effects false may support GET and known original receipt finalization; durable effects-off still denies new/incomplete native writes. The authored test is compile-only locally and must not be described as SQL runtime; it adds synthetic CLI/revision/fence assertions without new native mutation algorithms.

PR #35 remains delivered: required CI `40044946187` and post-merge `40045550655` passed; review `67ca1346-a4f9-49c` had zero proven behavior/design findings. Preserve its CI history: `40043431687` failed only the test fixture scanning textual server version; `40044064678` correctly denied stale fixture dates under production TTL, then fixture dates were aligned; the final run passed without weakening production guards. PR #36 is also delivered; do not revive older pre-CI snapshots. Stage 11 is still incomplete until 11b.10 review/required CI and separate owner live gate. After PR #37 exact-HEAD CI/merge, proceed in order: 12 synthetic restore CI, then 13 manual release preparation/artifacts with publication default-off (no actual publish), then 14 synthetic measurements only (not VPS metrics) and specifically scoped owner live inputs. Preserve 12→13→14; do not skip stage 12. No current owner blocker. No code/tests/envexample/CI changes by this documentation worker; no live API/GET, local SQL runtime, Telegram/payment, working DB, Docker, publish or deploy. Preserve native global guard/epoch and per-chat secret-delivery guard as separate barriers.

## Current handoff: stage 12 restore rehearsal

Repository `/Users/plumpalbert/dev/payplump-bot`, branch `feat/restore-rehearsal-ci`, base `b33fac67a1e19c8a3da00b2d7d261b170e0f02e8`. 11b.10/PR #37 delivered at exact final HEAD `9d93f649249f491ff70ec7a6c34af959b48fde3a`; required CI `40048848558` all-success (fast/race/disposable PG17/full native known+unknown workflows/Docker smoke), post-merge main CI `40049317238` success. `TestImportedEffectsStartupWorkflowRealPostgres` passed in CI. No pending CI claim.

Imported effects default off; explicit `IMPORTED_EFFECTS_ENABLED` plus canonical positive `IMPORTED_EFFECTS_REVISION` requires imported read-runtime and a matching verified bot. Grant does not change admission, immutable source policy, native global guard/epoch or per-chat secret barrier. Provider construction is zero-HTTP, but returns guarded native billing-mutator clients; separate protected-record/recurring/inventory/settings readers are GET-only. Permission grant causes no source GET/write. No live APIs, real DB, Telegram/payment, deployment or publication used.

Stage 12 code and local checks are complete; full independent review `9b8a652d-7b32-488` examined final 925-line patch `/tmp/payplump-restore-rehearsal-review-fixed.patch`, verified the fix and source rollback probe, and reported zero final findings. The review's initial P2 found that schema digest omitted trigger enabled state and column `attgenerated`/`attidentity`; only test schema hashing was fixed, retaining logical active-column ordinal (no physical dropped-column holes). Source-only negative probe transactionally disables `verified_import_origin_immutable`, confirms digest changes, rolls back, then confirms full baseline data/sequence/schema snapshot equality. Target remains read-only: no DDL, app start, replay or source GET. Parent normal/race/vet/CGO0 build/module/gofmt/diff/sh-n/operator checks and PG+command compile-only passed; 13 helper negative tests passed, one fake-Docker test skipped and Docker was not invoked. SQL/restore runtime remains unverified; PR #38 and exact CI are pending. Rehearsal is CI-only on disposable PG17. External backups/export/restore, Ninja export mechanism and live app validation remain owner-pending. Continue 12→13→14; no deployment or SSH.

Current docs-only handoff: `IMPLEMENTATION_PLAN.md`, `docs/autonomous-work.md`, `docs/import-admission.md`, `docs/operator.md`, `docs/configuration-delivery.md`, `docs/invoice-lifecycle.md`, new `docs/backup-restore.md`, and `/tmp/payplump-autonomy-handoff.md`. No tests run; `git diff --check` passed.
