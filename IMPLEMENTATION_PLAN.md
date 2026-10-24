# План реализации payplump-bot

Основа — [spec.md](spec.md). Бизнес-правила определяет ТЗ, этот файл задаёт
порядок работы и отслеживает её выполнение. PR #22 с provider foundation 11a.1
влит коммитом `e81034a88c05dae56cde0d1a45ddf5a0d5791e77`; проверенный HEAD
`674f838e80c6bc6b89e240fee79b219311b5f1e8` прошёл required CI и post-merge CI.
11a.2 завершён. PR #26 с 11b.1 DB-only foundation влит коммитом `76dfcda`;
точный code HEAD `d0a2ca665be507d1913db9bbe2301e945be34e0a` прошёл required CI
`40001910084` и post-merge CI `40002786608`. 11b.2 provider-only adapter merged in PR #27; exact HEAD
`0fad9355c818424089a72aa1211bf33be5ab6ced` passed required CI `40005099529`
and post-merge CI `40005923953`, both green. 11b.3 payment tracking merged as
`925d994619864e4f07b9d72fcb2e68cdfcd8e449`; exact HEAD
`839144922e91fb27bfea7b08b8f204b437c38545` passed required CI
`40010097193` and post-merge CI `40010923644`, all green. 11b.4 read-only invoice tracking merged as PR #29 at
`149d6b679ff007b1cff6d9679862675f03048412`; exact HEAD
`4c7a879da681ea75410526fc98f7bc37ab18e096` passed required CI `40014461532`
and post-merge CI `40015300957`, including all three imported lifecycle tests
and the full three-phase worker/actual CycleReader PostgreSQL workflow. The
nullable-first-invoice CAS defect was fixed before merge. 11b.5 OVERDUE-only
code merged as PR #30 at `19d45cf7f85c784b74cdc0cb2b71f5d15cddb758`; exact
code HEAD `b8efc247fe09b78e078b0c115c52317d47311939` passed required CI
`40019995948` and post-merge CI `40020785993`, all green. Imported paid renewal 11b.6.1 merged in PR #31 as `a92198b`; exact code HEAD
`012e1249a0720b79993ce55206413ecae06b178b` passed required CI `40024818619`
and post-merge CI `40025798271` is green. PR #32 (11b.6.2 initial-paused imported-debt restoration) merged at `57a6e09d1bd06b806e79145aba065c870b023934`. Exact final code HEAD `cd0ff229ed6ecb6ee0f81d5ceeee1c7cc7769502` passed required CI `40031643624` (all three jobs, including PostgreSQL 17/race/smoke/Docker); post-merge main CI `40032288658` completed successfully on the exact base. Runtime passed `TestImportedInitialRestorationPaymentProofTargetsCurrentDebt` and all three `TestImportedInitialPausedDebtRestorationWorker` cases: expired and empty original schedules, plus resume mutation followed by 503/revision 2/restart without replay. The expired-schedule workflow continued through a real new normal-cycle GET/adoption/payment-proof/ledger renewal seed+load while preserving the immutable origin's paused lifecycle and unchanged initial audit. No fake ledger, first invoice, OWN activation, or overdue prefix was created. Review `6ac88f9e-0961-4fd`'s P1 initial-mode claim-lock finding was fixed and rechecked; final review had zero findings. Parent-mode IFF/batch-bound checks were verified. CI `40029673909` and `40030663139` exposed test-fixture issues only; both fixes preserved production checks. PR #33 (11b.7.1) merged as `4a6522d1b881ef031ef580c67486406237ecf004`; exact final code HEAD `e2ca6863aa785681433932f3a9caf82a0d46319b` passed required CI `40035675235` (all three jobs, including PostgreSQL 17 integration/race and test aggregator/smoke/Docker), and post-merge main CI `40036327450` succeeded on the exact merge base. Three imported-configuration SQL workflows passed runtime. No live API, Telegram, payment, working DB, publish, or deploy was used.

## Как вести план

- `[ ]` — работа ещё не завершена; `[x]` — выполнена и проверена.
- Реализацию отмечаем в соответствующем PR после выполнения проверок;
  в `main` обновлённый статус попадает после ревью и слияния PR.
  Испытания на установленном сервисе или VPS отмечаем только после их проведения.
- В PR указываем пункты этого плана и обновляем их состояние. Для внешних
  испытаний сохраняем результат, версии сервисов и условия проверки без секретов.
- Изменения делаем небольшими PR с независимым review; implementation worker не
  меняет Git. Родитель проверяет обязательный CI на точном HEAD и выполняет merge
  в рамках явного разрешения владельца. Напрямую в `main` не пушим. Останавливаемся
  только для отдельного решения владельца по live- или инфраструктурному gate.
- Новые сведения об API уточняют технические шаги; изменения бизнес-правил
  отражаем также в `spec.md`. Рабочие секреты не помещаем в Git или этот файл.

## 0. Уже реализованная основа

- [x] Go-модуль с закреплённой версией Go и зависимостями в `go.mod` / `go.sum`.
- [x] HTTP-сервис с `/livez`, `/readyz`, подкомандой `healthcheck` и остановкой по SIGINT/SIGTERM.
- [x] Подключение PostgreSQL через `PG*` или `DATABASE_URL`, ограниченный пул и ошибки подключения без вывода пароля.
- [x] Встроенные транзакционные миграции, advisory lock и проверка контрольных сумм / неизвестных миграций.
- [x] Начальные таблицы `clients`, `subscriptions`, `operations` и Telegram ID в `bigint`.
- [x] Атомарная регистрация в слое хранения: один клиент, одна подписка без доступа и одно долговечное задание.
- [x] Тесты конкурентной регистрации, повторного обращения и отката при ошибке записи задания.
- [x] Календарные функции: таймзона сервера с fallback на UTC, полночь после дня оплаты, следующий месяц от даты платежа с ограничением последним днём.
- [x] Тесты календарных границ, високосного года, переходов DST и недоступной таймзоны.
- [x] Текущий обязательный CI запускает интеграционные/race-тесты на отдельном disposable PostgreSQL 17, smoke и Docker-проверки без GitHub Environment secrets; полный Neon suite доступен только как ручной opt-in `workflow_dispatch`.
- [x] Подготовлены и проверены статическая сборка, Distroless-образ по digest, nonroot, exec-healthcheck и Compose для разработки.
- [x] Документированы локальный запуск и настройки PostgreSQL; в CI и Compose используется SCRAM-SHA-256.
- [x] Исторический [GitHub Actions run](https://github.com/PlumpAlbert/payplump-bot/actions/runs/39046124393) проверял раннюю конфигурацию с Variables / Secret Environment `test` и интеграционными тестами на Neon. Это запись о раннем CI, не описание текущего required workflow.

Эти галочки относятся к основе сервиса. Испытания с настоящим Telegram-ботом,
Invoice Ninja и S-UI, полный платёжный цикл и рабочее развёртывание ещё не проверены.

## 1. Telegram: приём личного `/start`

Можно реализовать независимо от доступности Invoice Ninja и S-UI.

Этап реализован в ветке `feat/telegram-start`: локально проверены
API-клиент на тестовом HTTP-сервере, транзакции на PostgreSQL и жизненный цикл
webhook. Проверка настоящего бота относится к настройке тестовой
интеграции в этапе 2; ответы и меню — к этапам 3–5.

- [x] Добавить конфигурацию токена Telegram, ID администраторов, публичного URL и отдельного секрета webhook через окружение, без вывода секретов в лог.
- [x] Реализовать клиент Telegram Bot API: проверку ответов, таймауты, обработку ошибок и `retry_after`.
- [x] Добавить `POST /telegram/webhook` с проверкой секретного заголовка, лимитом тела, таймаутом БД и корректным завершением запросов при остановке.
- [x] Настраивать `setWebhook` при старте, сохраняя pending updates; readiness подтверждает завершение настройки и доступность PostgreSQL.
- [x] Долговечно хранить обработанные updates: возвращать HTTP 200 после фиксации результата в PostgreSQL, при ошибке возвращать 503 для повторной доставки.
- [x] Принимать `/start` только из личного чата, использовать ID отправителя для идентификации и существующую транзакционную регистрацию.
- [x] Повторный `/start` открывает существующую запись; повтор одного update не создаёт новых заданий или повторной бизнес-обработки.
- [x] Проверить секрет, JSON и лимиты запроса, групповые сообщения, повторную и внеочередную доставку, ошибки БД, миграцию с polling и повтор после коммита до подтверждения HTTP.

## 2. Проверка установленных API и параметров интеграций

Сбор сведений можно вести параллельно с этапом 1. Подтверждение контрактов
обязательно перед вызовами, которые меняют внешние системы.

Ветка `feat/integration-adapters` добавляет read-only адаптеры и команду
`check-integrations`. Чтение проверено на Invoice Ninja 5.13.47 и S-UI 1.6.4;
доступ к панели — через NetBird. Версия Invoice Ninja получена из API, версия
S-UI подтверждена владельцем; отдельный GET status/sys на этапе 7a также
показал `1.6.4`. Реальные параметры остаются в `.env`, контракты
и оставшиеся испытания описаны в [docs/integrations.md](docs/integrations.md).
Команда не импортирует клиентов и не выполняет операций записи. При проверках
все активные клиенты Invoice Ninja имели точные соответствия S-UI;
несопоставленные записи S-UI остаются вне управления. Источник ID — `id_number`.

- [x] Зафиксировать адреса и установленные версии Invoice Ninja и S-UI; безопасно настроить API-токены и сетевой доступ для чтения.
- [ ] Настроить тестового Telegram-бота и тестовые записи отдельно от рабочих клиентов; проверить публичный HTTPS-маршрут, заголовок секрета и реальную доставку webhook.
- [x] Для Invoice Ninja проверить API на чтение: клиенты, продукт, счета, платежи, recurring; сохранить синтетические примеры по проверенной структуре для контрактных тестов, без реальных данных.
- [x] Добавить read-only адаптер S-UI: клиенты, `desc` / `remark`, inbounds, плоские settings, проверка `success` при HTTP 200 и безопасная сборка URL подписки без её запроса.
- [x] Добавить ограниченную read-only сверку с явным полем Telegram ID, проверкой дублей и JSON-отчётом без персональных данных. Несопоставленные записи S-UI не изменяются.
- [ ] Зафиксировать ID продукта, поле Telegram ID, статусы выставленного / оплаченного / отменённого счёта, поля суммы и остатка, привязку платежей и `Payment.date`.
- [ ] Уточнить публичный адрес endpoint, формат вебхуков, их события и авторизацию; проверить поля due date, месячную частоту, `next_send_date` и действия stop/start.
- [ ] Для S-UI проверить API на чтение, формат клиента, `success`, назначение inbounds, JSON-подписку и доступность `closeSessions`.
- [ ] Зафиксировать inbound ID, публичный адрес подписки и реквизиты оплаты; проверить поведение дат 29–31 в установленном Invoice Ninja.

## 3. Долговечные задания, события и исходящие сообщения

Долговечные очереди и миграции уже используются реализованными workflow
регистрации, платежей, активации/выдачи конфигурации, рассылки и lifecycle Stage 1/2.
Существующая схема включает `payment_processed_events`, `outbox`, `activation_steps`
и `desired_access`. Подтверждения хранятся по invoice; `FinishPaymentCheck`
атомарно фиксирует proof, даты и outbox без выдачи доступа. Для реализованных
workflow действуют leases/fencing, rollback/restart recovery и known receipts;
неизвестные внешние POST не переотправляются. Расширение этих гарантий для
renewal/reactivation (PR #20, stage 9a) влито в основу 94095a8; выдача конфигурации следующих периодов реализуется этапом 9b.

- [x] Добавить и проверить миграции для `payment_processed_events`, `outbox`, `activation_steps`, `desired_access` и необходимых полей операций / подписок, сохраняя уже применённые миграции.
- [x] Реализовать выборку заданий порциями, блокировку, срок аренды, число попыток, отложенные повторы и восстановление незавершённой работы.
- [x] Раздельно хранить `desired_access` и результаты шагов внешних систем в реализованных workflows; расширение для последующих оплаченных периодов — в 9b.
- [x] Атомарно фиксировать доказательства оплаты, даты, бизнес-переходы, durable intents и outbox в реализованных workflows; применять их существующие fencing/сериализацию по клиенту. Дальнейшее расширение гарантий для следующих периодов — в 9b.
- [x] Добавить outbox-доставку с таймаутами и учётом `retry_after`; ошибка одного адресата не останавливает остальных.
- [x] Для реализованных внешних записей сохранять immutable intents и known receipts; неоднозначный результат удерживать для ручной сверки, не повторять неизвестный POST автоматически. Неоднозначный `mark_sent` также требует ручного разбора.
- [x] Проверить конкуренцию исполнителей, lease expiry, fencing, rollback и restart/recovery для реализованных registration/payment/activation/configuration/broadcast/Stage 1–2 workflows; расширение этих проверок на следующие оплаченные периоды — в 9b. CI для 9a прошёл в PR #20 на точном HEAD.
- [x] Для регистрации добавить `operation_steps`, `registration_flows`, `first_invoice_id`, атомарное завершение с outbox, ручную остановку FIFO и возобновление без удаления намерений; fencing, порядок блокировок и recovery были проверены на Neon в историческом прогоне.
- [x] Для основы очередей конкурирующие захваты, порядок по клиенту / чату, восстановление аренды, отказ устаревшему исполнителю, атомарность завершения и уведомлений, постоянная ошибка адресата и `retry_after` были проверены в историческом Neon-прогоне. Отдельно проверены ограничение параллельности, остановка и сохранение известного результата на тестовом отправителе.
- [x] Расширить эти гарантии на защищённую выдачу конфигурации последующих оплаченных периодов; см. 9b. Проверки 9b прошли required CI в PR #21 на точном HEAD.

Outbox защищает бизнес-обработку от повторов, но при сетевом сбое с неизвестным
результатом абсолютная гарантия ровно одного Telegram-сообщения не обещается.

## 4. Регистрация в Invoice Ninja и первый счёт

Зависит от этапов 1–3. Код ветки `feat/first-invoice` проверен синтетическим HTTP
и изолированными схемами Neon, включая полный путь worker → HTTP → PostgreSQL.
Операции записи изучены по исходникам Invoice Ninja 5.13.47; реальные POST ещё
не испытывали. Флаг выключен по умолчанию. Условия включения, ограничения
восстановления и исключения пользователей — в [docs/registration.md](docs/registration.md).

- [x] Реализовать отдельный write-адаптер Invoice Ninja 5.13.47 с проверкой версии перед каждым POST и синтетическими контрактными тестами; сохранить read-only клиент.
- [x] По `register_client` создавать клиента с Telegram ID в `id_number`; ранее созданного этим заданием находить по ID и приватному маркеру. Чужие существующие записи направлять на контролируемый импорт.
- [x] Создавать один обычный счёт с выбранным продуктом, переводить его в выставленное состояние и задавать срок оплаты в день выставления.
- [x] Сохранять ID счёта; брать номер, сумму и HTTPS invitation link из конкретного счёта, без локально заданной цены.
- [x] Через outbox уведомлять клиента о сумме, реквизитах и сроке, администратора — о регистрации и счёте.
- [x] До первой полной подтверждённой оплаты сохранять отсутствие доступа и не запускать recurring; явно исключённых пользователей не отправлять в Invoice Ninja.
- [x] Проверить повторы, истечение аренды, потерю ответа / подтверждения коммита и перезапуск: намерения не разрешают второй POST; неоднозначный `mark_sent` остаётся на ручной проверке.
- [ ] После отдельного разрешения владельца проверить реальное создание клиента, черновика, выставление, due date, invitation link и доставку настоящим тестовым ботом; подтвердить отсутствие неожиданных автоматизаций установки.

## 5. Клиентское меню и инструкции

Ветка `feat/client-menu`: read-only меню работает независимо от флага создания
счёта. Статус явно показывает только сохранённые сведения, не актуальную проверку
платежа / подключения. На этапе 5 кнопка конфигурации оставалась закрытой для всех;
отдельная защищённая выдача добавлена в этапе 7c. Ограничения и протокол фиксации
описаны в [docs/client-menu.md](docs/client-menu.md).

- [x] Добавить четыре действия и постоянную текстовую клавиатуру, `/menu` и команды `/invoice`, `/status`, `/config`, `/help`; повторный `/start` открывает меню без второго задания.
- [x] Проверять отправителя каждого действия и владельца конкретного счёта; перед записью ответа повторно проверять привязки под блокировками. ID из текста не использовать.
- [x] Для неоплаченного первого счёта объяснять условие получения конфигурации; работающую персональную подписку не выдавать.
- [x] Сохранять возможность открыть старый неоплаченный счёт и реквизиты после отключения; инструкцию сделать доступной независимо от оплаты.
- [x] Встроить тексты и инструкцию в бинарник; проверить новый / известный аккаунт, повторы, неверного владельца, частичную оплату, неизменяемость клавиатуры, гонки и откат на синтетическом HTTP и изолированном Neon.
- [ ] Проверить четыре действия настоящим тестовым Telegram-ботом и импорт JSON-профиля после реализации этапа 7.

## 6. Вебхуки Invoice Ninja и сверка платежей

Зависит от подтверждённого API, состояния в БД и outbox.

Ветка `feat/payment-verification`: проверяется только текущий счёт клиента с
завершённым собственным журналом регистрации. Принимаются ручные cash-платежи
в явно подтверждённой собственной валюте; кредиты / FX / частичные refund
остаются вне автоматического подтверждения. Наблюдение и даты не выдают доступ,
не создают recurring и не ставят активацию до реализации этапа 7. Флаг выключен.
Контракт проверен по исходникам 5.13.47 и синтетическим тестам, не реальным
webhook / платежом. Подробности: [docs/payments.md](docs/payments.md).

- [x] Добавить HTTP endpoint с отдельным секретным заголовком, лимитом тела и долговечной фиксацией хеша события без хранения клиентского payload.
- [x] Использовать webhook только как сигнал GET-сверки; не доверять его ID, статусам или суммам. Отдельного исходящего invoice-paid события в этой версии нет.
- [x] Проверять принадлежность, `paid` / balance / paid_to_date и полное точное покрытие allocation реальными завершёнными ручными платежами; исключать черновики, отмены, кредиты и возвраты как самостоятельное доказательство.
- [x] Вычислять дату полной оплаты из `Payment.date` нескольких частичных платежей; проверять следующий месяц с ограничением последним днём.
- [x] Добавить независимую очередь проверок, ограниченные порции и периодическую сверку для пропущенных событий; ошибка API переносит запрос без инвалидирования подтверждённого платежа.
- [x] Сохранять первое доказательство / даты / outbox атомарно, не продлевать повторно; изменение уже подтверждённого доказательства требует ручного разбора и не переписывает исходные факты.
- [x] Проверить неподходящий счёт, partial / pending / cancelled / refunded, повторы, поколения wake, fencing, stale binding, rollback, остановку и полный GET-worker→HTTP→Neon путь на синтетических данных.
- [ ] После отдельного разрешения проверить настоящие исходящие webhook и ручной тестовый платёж установленного Invoice Ninja; подтвердить валюту / права токена и реальные даты. Проверку банковского поступления и будущих действий S-UI эти тесты не заменяют.

## 7. S-UI и выдача доступа после первой оплаты

Зависит от этапов 2, 3 и 6.

Этап 7a в ветке `feat/activation-adapters` добавляет только контрактный слой:
полные записи S-UI, отключённый клиент со случайными реквизитами, guarded enable
и создание / первый запуск будущего recurring. В этом контрактном PR не
подключались исполнитель, новый журнал, локальная активация или выдача конфигурации.
Условия и оставшиеся реальные испытания: [docs/activation-adapters.md](docs/activation-adapters.md).

Этап 7b (`feat/activation-workflow`) добавляет отдельный default-off исполнитель
первой активации с пилотным allowlist до 20 Telegram ID. Перед каждым POST нужны
новая полная GET-проверка оплаты, неизменяемое намерение, fencing и глобальный
барьер dispatch с монотонным epoch. Неизвестные исходы удерживают барьер до
ручного разбора. Этот исполнитель не выдаёт конфигурацию; просрочка и реальные
записи этим этапом не испытываются. Подробности: [docs/activation.md](docs/activation.md).

Этап 7c (`feat/configuration-delivery`) добавляет отдельный default-off запрос
личной JSON-подписки только в первом собственном оплаченном периоде. Outbox
хранит намерение без URL; перед отправкой проверяются текущие payment/S-UI/
recurring/private-chat факты и неизменность локального снимка. Одноразовый
synchronous intent и неистекающий барьер неизвестной доставки не разрешают
автоматический повтор секрета. Выключенный старт отзывает новые разрешения даже
без Telegram. Детали и ограничения: [docs/configuration-delivery.md](docs/configuration-delivery.md).

- [x] Реализовать отдельный write-адаптер S-UI 1.6.4: полные записи, отключённый create, guarded enable, version preflight, проверка `success` при HTTP 200, коллизий credentials и обязательное чтение результата; проверить синтетическим HTTP без подключения к исполнителю.
- [x] Добавить отдельный recurring-адаптер 5.13.47: будущий черновик, месячная частота 5, `on_receipt`, auto-bill off и guarded первый start; проверить контракт без реальных POST.
- [x] Добавить default-off пилот первой активации: свежая GET-оплата в последнем callback, четыре неизменяемых шага, original-receipt checks, epoch / неистекающий барьер, fencing и атомарный outbox; проверить на синтетическом HTTP и изолированном Neon.
- [x] После подтверждённой оплаты создавать / включать только собственного нового клиента с согласованными inbounds и сохранением остальных полей; без автоматического импорта прежних аккаунтов.
- [x] Для нового клиента использовать случайный идентификатор подписки и сохранять ID / name / fingerprint в БД; существующие имена и реквизиты не менять.
- [x] Добавить защищённую выдачу своей JSON-подписки с `format=json` по запросу, без URL в outbox, с отдельным default-off пилотом, повторной авторизацией и защитой от неизвестной отправки; проверить синтетическим HTTP и изолированным Neon.
- [ ] После отдельного разрешения проверить настоящую Telegram-доставку, импорт JSON и подключение sing-box на выделенном тестовом клиенте.
- [x] Создавать один месячный recurring с первым запуском через месяц от `Payment.date`, без второго счёта за уже оплаченный период; проверено синтетическим HTTP, не настоящим POST.
- [x] После локального завершения первой активации атомарно уведомлять клиента / администраторов без персональной конфигурации; частичный или неизвестный исход оставлять незавершённым / held, без обещания неизменности доступа.
- [x] Проверить синтетическими HTTP / Neon отсутствие выдачи без собственной завершённой активации и актуальной оплаты, изменение proof / binding / receipts, ABA / сроки / политику, неизвестную Telegram-доставку без автоматического повтора и сохранность имени подписки.
- [ ] Повторить полный цикл на согласованной установленной тестовой записи, включая ошибки API и фактическое подключение; не считать синтетические проверки подтверждением рабочих записей.

## 8. Счета, вебхуки и отключение при просрочке — упрощённый объём

Новый целевой поток: Invoice Ninja присылает событие о счёте или оплате; событие
только будит обработчик, который затем читает актуальное состояние через API.
Вебхук не содержит уникального delivery/event-instance ID и сам по себе не
аутентифицирован. События `sent_invoice` и `create_payment` привязываются к
разным фиксированным wake-маршрутам с настроенным секретным заголовком; body не
выбирает ресурс и не доказывает счёт или оплату. Повторы коалесцируются и
обезвреживаются по устойчивому invoice ID, состоянию ledger и ключам outbox, а
не по ID события. Только после проверки связи с управляемым клиентом, счётом и
его свежим состоянием обработчик записывает переходы. Просроченные счета
хранятся в PostgreSQL, а долговечная сверка обрабатывает их с начала
следующего локального календарного дня после канонического `sent_date`.

В закреплённом Invoice Ninja 5.13.47 подтверждены события `create_invoice` (2),
`sent_invoice` (60), `update_invoice` (8) и `create_payment` (4); отдельного
события полной оплаты и уникального ID доставки нет. `InvoiceTransformer` по
умолчанию включает `invitations[].sent_date`; `MarkSent` записывает это поле,
но не меняет `invoice.date` и уже заполненный `due_date`. Для срока бота
используется фактический `sent_date`; расхождение с датами самого счёта не
блокирует обработку и не приводит к записи в Invoice Ninja.

PR #12 добавил ограниченный GET inventory Invoice Ninja, S-UI disable / closeSessions
и общий неистекающий writer guard. Stage 1 (наблюдение текущих managed recurring
счетов) вошёл в PR #16, Stage 2 (overdue enforcement) — в PR #17; оба выключены
по умолчанию. PR #18 исправил замечания review R1–R25. Текущие условия и порядок
описаны в [docs/invoice-lifecycle.md](docs/invoice-lifecycle.md). Stage 2 сверяет
текущий invoice и каноническую `invitations[].sent_date`, а с начала следующего
локального календарного дня после даты отправки приостанавливает recurring,
отключает S-UI по сохранённой привязке и запрашивает `closeSessions`. `due_date`
не меняется. Неизвестные записи удерживают общий guard/epoch и не переигрываются.
Возврат после оплаты, reminders, импорт и live rollout в этот срез не входят.

PR #18 влит в `main` коммитом `ebf8dc0d854ce8b138a77061b8a5578f478d8f5f`;
проверенный HEAD — `40c486ff9f3e850be41e2c677f90fbb97e95733e`. GitHub Actions run
[39947501730](https://github.com/PlumpAlbert/payplump-bot/actions/runs/39947501730)
успешно завершил `fast-checks`, `postgres-integration` и `test` на точном HEAD.
Полный runtime suite выполнен на disposable PostgreSQL 17 командой
`go test -v -race -tags=integration -count=1 -timeout=60m ./internal/postgres`;
`TestInvoiceOverdueWorkerStoreAdaptersLoopbackWorkflow` прошёл в обоих случаях.
Также прошли smoke, Compose/Docker build/start/restart и release Compose validation.
Это текущая runtime-проверка, а не прежняя compile-only проверка review fixes.
Текущий стандартный CI использует disposable PostgreSQL без Environment secrets;
Neon остаётся только ручным opt-in. Установленные API не вызывались.

Ранее согласованное восстановление после полной оплаты остаётся планом, а не
реализованной функцией: предполагается сдвиг только будущей `next_send_date` на
один календарный месяц от `Payment.date`, включение сохранённой S-UI-записи и
resume recurring. Повтор оплаты не должен сдвигать дату повторно. Уведомления
бота нужны только в Telegram; настройки email reminders Invoice Ninja вне scope.

- [x] Добавить ограниченный source-pinned Invoice Ninja GET inventory, S-UI disable / closeSessions и общий immutable guard / epoch; production API не вызывались.
- [x] По закреплённому публичному исходнику подтвердить события, поля sent-date и отсутствие уникального delivery ID / встроенной авторизации webhook; зафиксировать контракт event-specific сигналов и настроенного secret header.
- [x] Добавить durable event-specific wake и API lookup; дубликаты обезвреживать по invoice ID, ledger-state и outbox key, а не по delivery ID. Повторный сигнал или повторное наблюдение состояния не создаёт новых уведомлений или записей.
- [x] Уведомлять и записывать только когда свежий exact GET подтверждает принадлежащий managed recurring счёт со статусом sent/partial; draft и первый invoice подавляются. Связь client / invoice / managed subscription, private marker, pilot allowlist и канонический статус проверяются.
- [x] Добавить ledger на каждый обнаруженный собственный invoice: внутренний client, bot, customer/recurring ID, сумма, `invitations[].sent_date`, balance, paid-to-date и состояние. Уникальный invoice ID и outbox keys не позволяют повторить запись или уведомление.
- [x] Через Telegram outbox отправлять клиенту и администратору одно уведомление о новом sent/partial счёте; если счёт уже полностью оплачен к первому sweep, не отправлять неоплаченный notice, а обновить `current_invoice_id` и разбудить существующий payment verifier. Проверка полной оплаты и клиентское подтверждение остаются в платежном worker.
- [x] Реализовать срок доступа по календарному дню фактической отправки (`invitations[].sent_date`): счёт просрочен с начала следующего локального календарного дня; `due_date` Invoice Ninja не изменяется.
- [x] Реализовать durable overdue sweep при старте / периодической сверке и перед каждым действием заново проверять связь, status и balance в Invoice Ninja.
- [x] Реализовать последовательность: pause recurring, disable связанной сохранённой S-UI-записи, запрос `closeSessions`; не отключать при изменившемся / неполном / противоречивом состоянии.
- [x] Фиксировать immutable intents и known receipts через общий writer guard / epoch. Неизвестный результат удерживает guard для ручной сверки; автоматического replay нет.
- [x] Добавить синтетические проверки Stage 1 и Stage 2; review fixes R1–R25 проверены полным CI на точном PR #18 HEAD.
- [x] Запустить полный PostgreSQL integration/race suite на disposable PostgreSQL 17 в GitHub Actions; обе части `TestInvoiceOverdueWorkerStoreAdaptersLoopbackWorkflow` прошли.
- [ ] После отдельного разрешения владельца проверить полный lifecycle на установленных сервисах и тестовой записи, включая фактическое отключение и доставку уведомлений.


## 9. Оплаченные recurring-периоды: 9a и 9b merged

`payments.Observe` / `payments.Evaluate` уже проверяют финансовое доказательство
для non-first `current_invoice_id` при точном recurring ID. `payment_confirmations`
хранит подтверждения по invoice; `FinishPaymentCheck` фиксирует доказательство
и обновляет наблюдаемые `payment_date` / `next_invoice_date`, не выдавая доступ.
Использовать существующий payment-check pipeline: новый payment-prover worker не
нужен. Код 9a merged в основу `94095a8`; финальный PR HEAD
`952bce4063065c71462d936f71507ee151d557f3` прошёл required Actions run
`39961598201`. Защищённая выдача следующего периода реализуется в 9b.
Неизвестные внешние исходы остаются на hold;
нельзя снимать или обходить существующий guard/epoch/immutable intent либо
автоматически повторять неизвестную запись.

### 9a. Durable renewal/reactivation workflow

- [x] Использовать существующую проверку полной оплаты текущего invoice с точным recurring ID, per-invoice confirmation и наблюдаемыми датами; повтор того же доказательства не меняет их. Отдельный payment-prover worker не добавлять.
- [x] Добавить durable workflow обычного renewal и восстановления после просрочки: pause/schedule/resume существующего recurring и enable прежней S-UI-записи с теми же credentials, binding и URL; обновлять только будущую `next_send_date` на месяц от подтверждённой `Payment.date`, без замены recurring и catch-up счетов. Перед каждым внешним действием повторно проверять актуальное подтверждение и локальную привязку; каждый шаг требует immutable intent и известной квитанции. Реализация PR #20 merged in base `94095a8`; required CI passed on its final HEAD.
- [x] Добавить тесты обычного и позднего renewal, календарного расчёта и границ дат; runtime PostgreSQL suite должен пройти в required CI до merge.
- [x] Добавить проверки гонки оплаты после собственного подтверждённого overdue pause, а также неизвестного intent/receipt без replay и обхода guard/epoch. SQL/race runtime прошёл required CI PR #20 на disposable PostgreSQL 17, Actions run `39961598201`.

### 9b. Защищённая выдача конфигурации следующего оплаченного периода

Код этапа 9b расширяет ту же защищённую выдачу. Режим периода закрепляется в
неизменяемом запросе; provenance первой активации остаётся прежней, а renewal
использует отдельное актуальное подтверждение счёта и завершённую квитанцию
renewal. PR #21 влит в `b98d351d20e2cf2a420ba773c3af464ba1564e64`, финальный
HEAD `1c63194dde4920c0e8157be59f533a16ea4352e6`. Код и тесты прошли required CI
на исходном code HEAD `925364d85ac733d6f582d5f2bc83d5f2a0b40525`:
[run 39975961988](https://github.com/PlumpAlbert/payplump-bot/actions/runs/39975961988);
после merge зелёный required main CI — [run 39978010690](https://github.com/PlumpAlbert/payplump-bot/actions/runs/39978010690).
Проверки включали `fast-checks`, `postgres-integration`, `test`, PostgreSQL
SQL/race, smoke и Docker checks. В runtime прошли `TestRealRenewalToProtectedConfigurationWorkflow`
для обычного и позднего renewal, а также `TestConfigurationUnknownSecretCannotReplayAfterNewPaidPeriod`.
Live-проверки не проводились.

- [x] Расширить защищённую выдачу на текущий оплаченный период с explicit mode/FK, current-proof GET и неизменной S-UI binding; URL остаётся вне outbox.
- [x] Добавить renewal lifecycle-recurring validation, финальную квитанцию/даты и fencing изменений invoice/renewal authority.
- [x] Добавить сквозной loopback-сценарий с настоящими адаптерами, renewal worker и защищённым Telegram-отправителем; включить обычное и позднее продление, историческое подтверждение, проверки до записи и до секретной отправки, а также неизвестную доставку без повтора. Runtime обоих сценариев и защита от replay прошли required CI.
- [x] Подтвердить runtime этого сценария и прогнать весь PostgreSQL integration suite в required CI на точном code HEAD `925364d85ac733d6f582d5f2bc83d5f2a0b40525` до слияния; все обязательные jobs прошли (run `39975961988`).
- [ ] После отдельного разрешения владельца проверить обычное и позднее восстановление на выделенном тестовом клиенте.

## 10. Административная рассылка

Локальная реализация добавляет default-off policy gate, приватный preview и подтверждение с ротируемыми короткоживущими токенами, снимок только существующих членов бота, bounded DB fanout и отправку через существующий outbox. Отключённый старт отзывает разрешение даже при выключенном Telegram; рабочая доставка и production API не проверялись. Подробности и эксплуатационные ограничения: [docs/admin-broadcast.md](docs/admin-broadcast.md).

- [x] Проверять права по ID отправителя на каждом административном действии; callbacks не используются.
- [x] Добавить таблицы `broadcasts` и `broadcast_deliveries` с состоянием по получателю и общим результатом.
- [x] Реализовать ввод сообщения, предпросмотр, замороженную аудиторию и явное подтверждение.
- [x] Отправлять ограниченными порциями через outbox, учитывая существующую политику повторов и постоянных ошибок адресатов.
- [x] Сохранять прогресс после перезапуска и предоставлять административный статус / итоговый отчёт; внешняя доставка не подтверждена.

## 11. Сверка и импорт существующих клиентов

Инертные синтетические `import-plan` и DB staging валидируют план, сохраняют
неизменяемые факты и не выдают capability; они не являются verified reconciliation.
Live-проверки и реальные импорты не выполняются.

- [x] Добавить строгую проверку синтетического import plan и идемпотентный inert DB staging без создания клиентов, привязок, подтверждений, outbox-сообщений или доступа.
- [x] 11a.1 provider foundation: bounded read-only Invoice Ninja `FindCustomers` / `ListRecurring`, закреплённый source 5.13.47; bounded strict S-UI `ListIdentities`, версия 1.6.4 с redaction. PR #22 merged коммитом `e81034a88c05dae56cde0d1a45ddf5a0d5791e77`; exact HEAD `674f838e80c6bc6b89e240fee79b219311b5f1e8` прошёл required CI run `39980684412` и post-merge CI run `39981437193`, оба all-green. Исправлены capability separation `sui.IdentityReader(ListIdentities)` / `RecordReader(GetRecord only)` и совместимость `metadata.pagination.links`. Это не завершённый этап 11 и не доказательство состояния production.
- [x] 11a.2 core: GET-only active/paid verification, private canonical-source evidence и repeatability. PR #23 code HEAD `d792ffb39aae8d56bc09cfc2a25be0a755d5ba5c`; squash merge в main `0131f618b2ba4ca364be2a8a9e7fe0f38160c32f`. Required CI `39986043209` и post-merge main CI `39986939558` зелёные. Без БД-записей, admission и CLI.
- [x] 11a.2 history-only paused verification: GET-only exact-current-debt + unique fully paid historical-anchor report, real-reader loopback positives/negatives и private snapshots фактического долга/settings реализованы. PR #24 exact code HEAD `b24bf849ca4e3f6589c82ef2bebb885b5419702a` merged as `9fe0c2aaec847a4fe2948f09d755a346318e5c4c`; required CI `39989051793` and post-merge CI `39989900953` all green. Это не capability/grant и не поддержка произвольных manual suspensions.
- [x] Ограничить `MaxHistoricalAnchors` диапазоном 1..20; превышение проверяется без усечения списка.
- [x] 11a.2 operator CLI: command and real-reader synthetic HTTP workflow bridge pass positive factory/source-reader and private-artifact checks plus exclusion, bad-hash and no-payment-source cases. PR #25 merged as `3b54c82`; exact code HEAD `02b0c8c7d882e37c713416f39ddcad42ff711baa` passed required CI `39992168476` and post-merge main CI `39992922113`. No live reads, DB admission or capability grants.
- [x] 11b.1 DB-only foundation: opaque typed preparation performs fresh GET verification of the complete plan; default-disabled, revisioned policy; atomic PostgreSQL admission/staging with immutable imported origin, actual paid anchor and invoice history. No fake first invoice, registration/activation journals, outbox, or external writes. Policy disable/re-enable requires revision advancement to prevent ABA; shared write guard and cross-origin SUI identity checks are preserved. PR #26 merged as `76dfcda`; exact HEAD `d0a2ca665be507d1913db9bbe2301e945be34e0a` passed required CI `40001910084` and post-merge CI `40002786608`, including PostgreSQL 17 integration coverage.
- [x] 11b.2 provider-only slice: separate imported recurring profile and typed pause/schedule/resume adapter preserves legacy notes and frozen price, with GET preflight and final authorization. PR #27 exact HEAD `0fad9355c818424089a72aa1211bf33be5ab6ced` passed required CI `40005099529` and post-merge CI `40005923953`, both green. It remains unwired and does not make imported origins generally managed.
- [x] 11b.3 payment-tracking-only: separate default-off imported-runtime permission; DB-validated comparable imported-origin snapshot with current configuration revision; imported-only payment tracking; active paid-baseline proof pin and storage-boundary suppression of historical confirmation notices. PR #28 squash `925d994619864e4f07b9d72fcb2e68cdfcd8e449`; exact HEAD `839144922e91fb27bfea7b08b8f204b437c38545` passed required CI `40010097193` and post-merge CI `40010923644`, all green. Coverage included real GET active/paused workflows, first-proof pinning, multibot scoping, due/privacy and pre-010 unknown-migration fixtures, full PG17, smoke, and Docker. Permission sync is explicit, not startup configuration. No fake first invoice or source effects.
- [x] 11b.4 read-only imported invoice tracking: GET discovery of ordinary new recurring invoices, normal new-invoice ledger/current-ID updates/notices, and frozen historical-ID suppression without fabricated dates. PR #29 merged as `149d6b679ff007b1cff6d9679862675f03048412`; exact HEAD `4c7a879da681ea75410526fc98f7bc37ab18e096` passed required CI `40014461532` and post-merge CI `40015300957`, including all three `TestImportedInvoiceLifecycle...` tests and the full three-phase worker/actual CycleReader PostgreSQL workflow. Nullable-first-invoice CAS defect was fixed before merge.
- [x] 11b.5 imported overdue-only effects: separate default-off effects permission, immutable action-frozen current admission/runtime/effects permission revisions (distinct from historical origin admission revision), imported profile/S-UI binding, snapshot and authorization fencing. PR #30 merged as `19d45cf7f85c784b74cdc0cb2b71f5d15cddb758`; exact code HEAD `b8efc247fe09b78e078b0c115c52317d47311939` passed required CI `40019995948` and post-merge CI `40020785993`, all green. Its CI included two loopback workflows and three permission fences.
- [x] 11b.6.1 imported paid renewal: new ordinary lifecycle-ledger invoices only. PR #31 merged as `a92198b`; exact code HEAD `012e1249a0720b79993ce55206413ecae06b178b` passed required CI `40024818619` and post-merge CI `40025798271`.
- [x] 11b.6.2 initial paused/current-debt restoration: explicit `imported_initial_paused` mode, immutable current-debt origin FK and separate historical-anchor proof; no synthetic ledger rows, fake first invoice, OWN activation, or OWN overdue prefix. PR #32 merged at `57a6e09d1bd06b806e79145aba065c870b023934`; exact final HEAD `cd0ff229ed6ecb6ee0f81d5ceeee1c7cc7769502` passed all required CI jobs in `40031643624`, including PG17/race/smoke/Docker; post-merge CI `40032288658` succeeded. Current-debt proof target and all three initial-restoration workflows passed runtime, including expired/empty schedule and mutate-then-503/revision-2/restart no-replay. Expired case also completed new normal-cycle GET adoption, paid proof and ledger renewal seed/load without changing immutable origin lifecycle/audit. Independent review final findings: zero; lock defect fixed and rechecked. Earlier CI failures `40029673909` and `40030663139` were test-only fixture issues; production checks were preserved.
- [x] 11b.7.1 protected ORIGINAL ACTIVE imported baseline only: current invoice equals immutable origin current invoice and paid anchor; current normal proof, strictly future UTC date, static binding/inbounds, legacy price/notes, source-scope and public-base GET fences; legitimate immutable origin FK/mode and frozen current configuration/admission/runtime revisions. SQL NULL first invoice; no fake OWN history; OWN first/renewal unchanged. Billing-effects permission is not required for GET-only consumers; configuration policy separately gates secret Telegram delivery. PR #33 merged at `4a6522d1b881ef031ef580c67486406237ecf004`; exact final HEAD `e2ca6863aa785681433932f3a9caf82a0d46319b` passed required CI `40035675235` and post-merge main CI `40036327450`, both all-success. All three SQL workflows passed runtime: original paid-baseline success/guard release; public-base policy denial before GET and changed-current native proof review 2007 after GET; runtime permission ABA denial and unknown-send 503 with a durable per-chat barrier across revocation, new request and restart. Review `34c46fce-9abc-42a` fixed both P2 findings (currency policy drift, test-worker bot scope), final findings zero. No live provider/Telegram/payment/working DB/publish/deploy. This verifies the tested synthetic flow, not live consumer readiness.
- [x] 11b.7.2 protected imported CURRENT COMPLETED renewal, including ordinary ledger renewals and initial-restored current debt. PR #34 merged as `1c21061e0ad8f02fab45678c9d92d343855fd26a`; exact final HEAD `1cdd30d4b4b4fbb0bf3544a64ff31156a705bd60` passed required CI `40040735229` and post-merge main CI `40041362995`, all jobs green. Runtime passed `TestCompletedImportedRenewalAllowsProtectedConfiguration`, `TestCompletedImportedInitialRestorationAllowsProtectedConfiguration`, `TestUnknownInitialRestorationDeniesConfiguration`, and `TestCompletedImportedRenewalStaleConfigurationRevisionDenied`. CI history retained: first `40038835592` caught migration syntax before SQL cases; second `40039692048` passed migration/old SQL but had four test-only fixture/boundary failures; third `40040735229` passed all jobs. No predicates/fences were relaxed. Full review and narrow syntax review completed. No live API, Telegram, payment, working DB, publish or deploy.
- [x] 11b.8 verified import-apply CLI: PR #35 merged at `c0c5f41de7dc98eb03530bebda394b0024842061`; exact final code HEAD `2a69caa2802e3ad78a67da023a68334f8566e377` passed required CI `40044946187` (fast, race, disposable PostgreSQL 17 and Docker smoke) and post-merge main CI `40045550655`. `TestImportApplyWorkflowRealPostgres` passed both `explicit_enable_and_idempotent_replay` and `default_off_denies_empty_schema`. The first real-client workflow used fresh GET-only verification, opaque preparation and an already-known bot; replay was idempotent without origin mutation. A separate fresh empty schema denied apply without `--enable-admission`, with no rows and no automatic policy grant. CI history is recorded in the operator handoff: initial fixture `server_version_num` text scan failed before scenarios; a later fixture date was stale against the correctly enforced production freshness check; neither issue weakened production guards. No live API, Telegram, payment, working DB, publish or deploy was used.
- [x] 11b.9 imported read-runtime startup: PR #36 merged at `a5fd93b28f0c0fe78e9485ddbe169837af49544b`; exact code HEAD `277b37c12433f271ee72d60d486551644749de92` passed required CI `40046909184` (fast, race, disposable PostgreSQL 17 and smoke/Docker), and post-merge main CI `40047448373` succeeded. `TestImportedRuntimeReadOnlyStartupWorkflowRealPostgres` passed the fresh CLI/admission/GET-only plan/read-worker/normal payment-confirmation flow. No fake OWN/first invoice/activation/ledger/configuration request was created; local fences/guard ownership stayed unchanged. Startup effect/runtime fences, fresh permission revisions, source GET count and persisted admission were verified. See [import-admission.md](docs/import-admission.md), [operator.md](docs/operator.md), and [autonomous-work.md](docs/autonomous-work.md) for scoped assertions and historical CI. No live API, Telegram, payment, working DB, publish or deploy was used.
- [x] 11b.10 imported native-effects startup/provider wiring merged at exact final HEAD `9d93f649249f491ff70ec7a6c34af959b48fde3a`; PR #37 required CI `40048848558` passed all jobs, including fast, race, disposable PostgreSQL 17, full native known/unknown workflows and Docker smoke. Post-merge main CI `40049317238` also succeeded. `TestImportedEffectsStartupWorkflowRealPostgres` passed: fresh CLI/admission/imported read, effects-off denial, explicit matching-bot revision grant, idempotent enabled restart, effect-only revocation, stale-revision denial and explicit newer revision; read permission stayed unchanged where required, admission/origin/current invoice/proof/first_invoice_id and guard epoch/owner stayed intact. No covert source GET or writes resulted from permission grant. This is synthetic CI proof, not live readiness. Provider construction is pure (zero HTTP) and returns guarded native billing-mutator clients to ordinary native consumers; it is not GET-only. `ProtectedRecordReader`, `RecurringReader`, inventory and settings interfaces remain separate strict GET-only capabilities. Read-enabled/effects-off may finalize a known original receipt, while durable effects-off denies all new/incomplete writes. Preserve exact source scope and existing actor/private-writer/pilot restrictions; no new queue, DI framework or no-op wrappers. Historical CI/test assertions remain recorded in the handoff, not as current pending status. Stage 11 implementation is delivered; live verification/admission/rollout remain owner-gated.
- [ ] Preserve actor/pilot restrictions. Historical invoices must not produce stale notices or operations; 11b.4 suppresses their IDs without fabricated sent dates or first-activation history. Leave unpaid imported debts for manual review, without forgiveness or automation. Keep admission, read-runtime, effects and configuration-delivery permissions independent: read-runtime startup does not grant admission or effects; effects require their separate explicit flag/revision, matching private actor/source scope and durable gates. Do not disable already authorized native workers when those gates match.
- [ ] Перед любым будущим admission выделять дубли, отсутствующие пары и противоречия для ручного разбора; реальные данные и live API требуют отдельного owner gate.

## 12. Резервное копирование и восстановление

Read-only recovery report, `restore-check.sh` и runbook подготовлены. CI-only
rehearsal выполнена на disposable PostgreSQL 17; реальные backup/restore и
внешний экспорт остаются отдельным owner gate.

- [x] Добавить read-only recovery report, guarded restore helper и owner-gated runbook; наличие реальных backup этим не подтверждается.
- [x] Проверить restore rehearsal в required CI на disposable PostgreSQL 17. PR #38 влит в base `15210d3ceac191dc467046a8be0d2cd6807265d7`; exact final HEAD `fec675eecfbf6367c2ab0c4722955058b4799ce6` прошёл третий required CI `40052967054` со всеми тремя jobs: fast, full PG17/race/smoke/Docker и отдельный фактический restore rehearsal. Restore прошёл `TestRestoreRehearsalPrepareSource`, `restore-check.sh` default dry-run, явный `--apply` в собственную пустую loopback target DB и `TestRestoreRehearsalVerifyTarget`; safe report synthetic/recovery_verified, 41 tables, 5 sequences. Report и read-only target snapshot после проверки не изменились. Приложение после restore не запускалось; source/working DB, providers и Telegram не затрагивались. Архивы/manifest не загружались; в CI log попали только safe hash/counts, приватные файлы и созданная тестовая DB очищены, job завершился 0. Предыдущие CI runs: `40051659078` остановился до dump из-за некорректного overdue fixture; `40052328493` прошёл HoldOverdue setup/journal, но `TestRestoreRehearsalPrepareSource` завершился FAIL на assertion `UnresolvedKnownReceipts`: фактическое значение для completed operation было 0. До dump/restore в этом CI run не дошло. Исправили ожидание теста; финальный run сохранил проверки guards, source hash и данных.
- [ ] Уточнить у владельца внешний механизм экспорта/резервирования Invoice Ninja; установленный механизм неизвестен и не предполагается.
- [ ] Реальные backup/restore, запуск восстановленного приложения и live-сверка остаются отдельным owner gate.
- [ ] Сверить клиентов, ссылки подписок, оплаченные периоды и незавершённые операции; проверить отсутствие повторных счетов и продлений.
- [ ] Сохранить проверенные инструкции восстановления и совместимого отката схемы / приложения.

## 13. Финальный образ, CI и выпуск

Release tooling и workflow подготовлены в текущей ветке; независимый review
завершён без доказуемых замечаний. Exact-HEAD required CI и PR #39 ещё pending.
Подробный контракт и owner gates описаны в [docs/release.md](docs/release.md).
Это подготовка к будущему ручному выпуску, не подтверждение registry-артефакта,
разрешение на публикацию или рабочего deployment.

- [x] Подготовить digest-based release Compose template и валидатор synthetic config; публикация и запуск не выполнялись.
- [x] Реализовать и независимо проверить release tooling: schema-only Environment gates плюс проверка фактического owner approval текущего run перед Docker и повторно перед registry credentials/push; publish разрешать только при `GITHUB_RUN_ATTEMPT=1` до Git/API-вызовов, retry требует нового run и нового review. Review A завершён с нулём доказуемых замечаний; review B обнаружил P2 о привязке workflow/job, исправление повторно проверено review `43d6862f-9f7a-477` с нулём замечаний. Публичный REST/GraphQL не раскрывает `can_admins_bypass`; нельзя выдавать UI-настройку за API-проверку.
- [ ] Дождаться required CI на точном HEAD и открытия PR #39 родителем; до этого этап 13 не завершён.
- [ ] Проверить образ и сигналы на целевой VPS: architecture, ресурсы, PostgreSQL, HTTPS, timezone, healthcheck и SIGTERM. Архитектура/лимиты VPS неизвестны и требуют владельца.
- [ ] Проверить интеграционные контракты и сквозные сценарии приложения в штатном CI; это не доказывает release workflow.
- [ ] После review/CI и отдельного разрешения владельца выполнить только выбранный ручной workflow. Publish gate должен требовать точный reviewed main ancestor, exact green required CI, явное подтверждение, same-repo owner-provided previous digest, проверенную конфигурацию Environment и одобрение из actual current-run approval history; реальная публикация не выполнялась.
- [ ] Подготовить и проверить deployment для Komodo после отдельного owner gate; никаких deployment/SSH в текущей работе.
- [ ] Проверить совместимость схемы и приложения при обновлении/rollback, не предполагать наличие down migrations.

## 14. Измерение ресурсов и запуск

Безопасный harness для локальных синтетических измерений подготовлен; измерений
целевой машины или рабочих нагрузок нет. Целевые параметры и запуск требуют
участия владельца и не блокируют подготовку harness и синтетических тестов.

- [x] Подготовить safe resource-measurement harness с dry-run и ограниченным synthetic workload; не выдавать CI/локальные показатели за VPS-метрики.
- [ ] Зафиксировать архитектуру, RAM / SSD VPS, число клиентов и настройки пула БД, размеров порций, параллельности и API-таймаутов.
- [ ] Измерить простой, обработку счетов, сверку после перезапуска и рассылку с повторами неудачных запросов.
- [ ] Отдельно записать размер образа на диске / загрузки из реестра, RSS процесса, память контейнера, CPU и пики с версией образа и длительностью сценариев.
- [ ] По измерениям выбрать лимиты RAM / CPU и `GOMEMLIMIT` с запасом, настроить ротацию логов и проверить отсутствие потери заданий при этих настройках.
- [ ] Оценить SSD с учётом текущего и предыдущего образа, рабочих файлов и максимального объёма логов; локальные лимиты Compose не считать готовыми рабочими значениями.
- [ ] На тестовом клиенте выполнить полный цикл: регистрация → счёт → оплата → доступ → просрочка → пауза → поздняя оплата → новая дата.
- [ ] На установленной S-UI и используемом протоколе подтвердить прекращение активных соединений, запрет повторного подключения и восстановление прежней подписки.
- [ ] После сверки, резервного копирования, восстановления и проверки отката развернуть согласованный выпуск в Komodo, подключить управляемые подписки и включить фоновые действия.
- [ ] Подтвердить все критерии приёмки, сохранить результаты и рекомендации по эксплуатации.

## Покрытие критериев приёмки из spec.md

Это указатель на будущие проверки, а не отметка об их прохождении.

| Критерий ТЗ | Где реализовать и проверить |
| --- | --- |
| 1. Повторы `/start` и update без дублей | 1, 3, 4, 7 |
| 2. Известный Telegram ID открывает существующую подписку | 1, 5 |
| 3. Нет доступа и конфигурации до полной оплаты | 4–7 |
| 4. Первая оплата, JSON-подписка, инструкция, уведомления | 5–7, 14 |
| 5. Доступ до конца дня оплаты | 8 |
| 6. Повторно проверенный долг и отключение со следующего дня | 8 |
| 7. Подтверждённая оплата и уведомление без повторов | 6, 8, 9 |
| 8. Поздняя оплата и новый расчётный срок | 9 (после решения о восстановлении) |
| 9. Возврат без счетов за пропущенные месяцы | 9 (если авто-восстановление согласовано) |
| 10. Сохранение ссылки и реквизитов при смене доступа | 7, 9, 11, 14 |
| 11. Повторы, перестановки и гонки событий | 3, 6, 8, 9 |
| 12. Восстановление после перезапуска и отказа API | 3, 6, 8, 9, 12–14 |
| 13. Административные права и независимые доставки рассылки | 10 |
| 14. Статическая Go-сборка и состав Distroless-образа | 0, 13 |
| 15. Целевая архитектура, nonroot, PostgreSQL, HTTPS, таймзона, файлы | 13, 14 |
| 16. Healthcheck, SIGTERM и сохранность незавершённой работы | 3, 13, 14 |
| 17. Выпуск вне VPS, digest, Komodo и совместимый откат | 12–14 |
| 18. Измерения ресурсов, лимиты, логи и ограничение параллельности | 3, 10, 14 |
