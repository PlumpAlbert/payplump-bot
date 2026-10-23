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
  inconsistency was fixed here. Full parent local
  unit/race/vet/CGO0/build/module checks and SQL compile verification passed.
  PR #25 is not open; its exact-HEAD CI/merge remain pending. No live verification.
  Whole 11 remains incomplete; no DB admission or capability/grant.

  Непереговорные TODO: 11b explicit managed
  admission только после свежего GET, с imported origin/binding/proof
  anchor/revision, без имитации registration/activation journals. Сохранить
  actor/pilot ограничения; будущий admission обязан подавлять устаревшие
  операции/уведомления. Неоплаченные долги остаются ручными, без прощения или
  автоматизации. Не добавлять 11c или новое архитектурное решение.
- [ ] 12: дальнейшая подготовка синтетической isolated restore rehearsal;
  recovery report, restore helper и docs уже есть. Не выполнять автоматические
  rearm/снятие барьеров; реальные backup/restore требуют владельца.
- [ ] 13: release manifest и ручной opt-in build/publish workflow; Compose и
  validator уже есть. Никаких реальных публикаций или deploy.
- [ ] 14: синтетическая подготовка и тесты safe resource harness уже есть;
  целевые измерения VPS не выполнены и требуют владельца.

Очередь последовательная по небольшим PR: 9a и 9b завершены; 11a.1 и 11a.2
core/history merged. PR #24 и required/post-merge CI зелёные. Operator CLI и
synthetic real-reader workflow прошли независимый review (zero code defects;
P3 docs inconsistency fixed) и parent local checks; PR #25 ещё не открыт, exact-HEAD
CI/merge pending. Затем остаётся 11b, 12, 13 и 14. Весь этап 11 не завершён;
live verification/admission не выполнялись.

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
