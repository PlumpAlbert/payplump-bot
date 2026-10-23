# Автономный проход по оставшейся реализации

## Разрешения и границы

Владелец разрешил автономно продолжать код и тесты с субагентом-разработчиком
`openai-codex/gpt-6-luna`, reasoning `xhigh`. Для каждой небольшой единицы сначала
обновить план, затем поручить реализацию разработчику и независимую проверку другому
агенту. Родитель выполняет commit/push, создаёт PR, ждёт зелёный CI на точном HEAD
и сливает PR; после этого обновляет план и начинает следующий пункт. Останавливаться
только когда действительно требуется решение или действие владельца. Не выполнять
Git/VCS, PR, CI или другие API-действия из implementation worker.

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
- [x] 9a: код durable renewal/reactivation и синтетические тестовые сценарии добавлены
  в draft PR [#20](https://github.com/PlumpAlbert/payplump-bot/pull/20); ожидается
  required CI на окончательном HEAD до merge. Локальный SQL integration runtime
  не запускался и успешный SQL runtime результат не заявляется.
- [ ] 9b: расширить protected config delivery на следующие оплаченные периоды;
  текущая выдача ограничена первым циклом.
- [x] 10: приватная административная рассылка с неизменяемым подтверждением,
  снимком получателей, долговечным прогрессом и ограниченной доставкой.
- [ ] 11: далее — проверенная capability admission и managed import. Инертный
  import-plan/staging уже есть; live-сверка и реальные импорты не выполняются.
- [ ] 12: дальнейшая подготовка синтетической isolated restore rehearsal;
  recovery report, restore helper и docs уже есть. Не выполнять автоматические
  rearm/снятие барьеров; реальные backup/restore требуют владельца.
- [ ] 13: release manifest и ручной opt-in build/publish workflow; Compose и
  validator уже есть. Никаких реальных публикаций или deploy.
- [ ] 14: синтетическая подготовка и тесты safe resource harness уже есть;
  целевые измерения VPS не выполнены и требуют владельца.

Очередь строго последовательная: 9a, 9b, затем 11, 12, 13 и 14. Для каждого пункта
создаётся небольшой PR; перед merge родитель ждёт зелёные обязательные проверки
точного HEAD. Отсутствие owner-gate для live-проверок не задерживает код и
синтетические тесты, которые безопасно подготовить без владельца.

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
Stage 9a реализован в коде draft PR [#20](https://github.com/PlumpAlbert/payplump-bot/pull/20)
и ждёт обязательный CI; stage 9b, выдача конфигурации следующего периода, остаётся
в очереди. Локальный runtime SQL не запускался, результаты CI на изменённом HEAD
не подтверждены. Использовать существующий payment-check pipeline, не создавать
новый verifier-worker. Автоматизация не получает разрешение на production API даже
GET. Recurring может создавать draft, поэтому workflow проверяет фактическое
состояние и не предполагает issued invoice. Не менять чужие company/client-wide
настройки.

## Отдельные gates владельца

Владельцу нужны реальные проверки на выделенном клиенте и установленных версиях:
полный платёжный цикл, отключение/разрыв сессий/восстановление, импорт и связь
sing-box, webhooks и Telegram. К его инфраструктурным решениям относятся рабочий
импорт, внешние backup, restore/rollback инфраструктуры, целевые измерения VPS,
публикация и Komodo deploy. Не обращаться к этим системам и не использовать
живые данные без такого gate. Успешные исходники и синтетические тесты их не
заменяют; ожидание gate не задерживает безопасный код и synthetic preparation.
