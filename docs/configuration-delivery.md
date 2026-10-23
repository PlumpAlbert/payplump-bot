# Защищённая выдача подписки: этапы 7c и 9b

`/config` и кнопка «Моя конфигурация» могут выдать личную JSON-ссылку
**только по запросу владельца**, после завершённой собственной первой активации
или подтверждённого продления собственного управляемого аккаунта. Функция
выключена по умолчанию. Она не импортирует прежние аккаунты, не пишет
в Invoice Ninja / S-UI и не проверяет работу sing-box.

Это ограниченный пилот, а не готовое развёртывание.
Отдельный default-off Stage 2 отключает только подходящий managed recurring-счёт
по правилам [invoice-lifecycle.md](invoice-lifecycle.md); `/config` сам не
управляет этим процессом. Stage 2 не восстанавливает доступ после оплаты и не
отзывает уже полученную ссылку. Истечение разрешения на выдачу **не отключает
доступ и не отзывает ранее полученную ссылку**.

## Настройки и включение

| Переменная | Условие |
| --- | --- |
| `CONFIGURATION_DELIVERY_ENABLED` | По умолчанию `false` |
| `CONFIGURATION_POLICY_REVISION` | Явное положительное целое при включении; монотонная ревизия политики |
| `CONFIGURATION_HTTP_TIMEOUT` | По умолчанию `3s`, допустимо `1s`–`5s` на один GET |
| `ACTIVATION_ALLOWED_TELEGRAM_IDS` | Тот же приватный список пилота: 1–20 точных уникальных Telegram ID |
| `REGISTRATION_EXCLUDED_TELEGRAM_IDS` | Исключения имеют приоритет; без чтения провайдеров |
| `TELEGRAM_ENABLED`, `PAYMENTS_ENABLED` | Оба должны быть включены |
| `PAYMENTS_CURRENCY_ID` | Явная native currency Invoice Ninja |
| `INVOICE_NINJA_TELEGRAM_ID_FIELD` | Только `id_number` |
| `SUI_VERSION` | `1.6.4` или точный `v`-префикс установленного version claim |
| `SUI_SUBSCRIPTION_BASE_URL` | Публичная HTTPS-база без имени клиента, query, fragment или userinfo |

Нужны настроенные адреса и API-токены обоих провайдеров. Для закрытой панели
S-UI поддерживается HTTP через защищённую NetBird-сеть; публичная ссылка требует
HTTPS. Проверка сертификатов и hostname у HTTPS не отключается. Границы
`INTEGRATION_PAGE_SIZE` / `INTEGRATION_MAX_PAGES` сохраняются. Задайте явный `TZ`
одинаково для всех экземпляров приложения.

Исполнитель активации не обязан работать в момент выдачи. Нужен его ранее
завершённый собственный журнал. Администратор не получает права запросить
конфигурацию другого клиента. Намеренно исключённые и несопоставленные старые
аккаунты остаются вне управления.

Политика закрепляет endpoints, публичную базу, currency/identity, version claim,
таймзону, полный allow/exclusion scope и версию домена политики; ротация токенов
и HTTP-лимиты её не меняют. Поддержка следующего оплаченного периода меняет
формат и смысл политики. Перед обновлением ранее включённой установки обязательно
задайте `CONFIGURATION_POLICY_REVISION` больше сохранённой ревизии; тот же номер
fail-closed. При любом последующем изменении политики также увеличивайте ревизию.
Процесс со старой ревизией
или другим содержимым той же ревизии не может перезаписать новую политику.
Проверка версии сервиса подтверждает лишь его заявление, не исходники сборки.

**Выключенный старт отзывает новые разрешения в БД до readiness**, даже при
выключенном Telegram или неработающей настройке webhook. Ревизия включённой
политики увеличивается, флаг становится `false`; повторный выключенный старт
не увеличивает её снова. Перед повторным включением узнайте сохранённую
ревизию и задайте большую. Не сбрасывайте таблицу и не переиспользуйте ревизию.
Изменения политик сериализуются на уровне схемы. Одна БД приложения предназначена
для одного бота и одной компании Invoice Ninja.

До обновления остановите старые экземпляры и дождитесь завершения их HTTP.
Новая политика запрещает **новые** разрешения, но не отменяет уже разрешённый
или переданный запрос. Для подтверждения отзыва требуется покой прежних
отправителей, а не только смена флага.

## Запрос и проверка

1. Принимается только проверенное личное сообщение: положительный `from.id`,
   `chat.id == from.id`, не бот, команда этому боту. Чужие ID в тексте, группы,
   callbacks и аргументы `/config` права не дают.
2. В транзакции проверяются собственная регистрация, завершённая активация и
   локальное состояние. В outbox записываются **маркер и неизменяемое намерение**:
   actor/bot/client, исходная активация, явный режим периода, его invoice/proof;
   для renewal — ID конкретной завершённой renewal operation. Сохраняются hash
   доказательства/привязки, ревизии и срок запроса. URL, credentials и копия
   S-UI record сюда не попадают. Обработка update, намерение и outbox фиксируются
   атомарно; повтор update не создаёт вторую выдачу.
3. Для обоих режимов проверяются четыре подтверждённых шага первой активации и
   неизменная S-UI binding. First требует исходный счёт и исходную квитанцию.
   Renewal требует отдельные current paid proof и completed renewal audit без
   review; первоначальный proof остаётся только историческим. Нужны `active`,
   желаемый доступ и отсутствие payment/activation review.
4. GET-only адаптеры заново читают строгий enabled S-UI record и active recurring.
   First сохраняет точные проверки первоначального recurring. Renewal проверяет
   тот же recurring в lifecycle-режиме: private marker, исходный продукт и цену,
   текущую дату платежа, будущую target date и сумму/UTC квитанции renewal.
   Полный `payments.Observe` читает запрошенный invoice и два полных инвентаря;
   требуется тот же proof hash. После проверки оплаты S-UI и recurring читаются
   повторно. Telegram `getChat` подтверждает тот же личный чат.
5. В последней транзакции снова проверяются владелец, политика, FIFO, cooldown,
   попытка/аренда, полный снимок и ревизия. Ревизии изменяются при существенных
   локальных переходах, в том числе при изменении и возврате прежнего значения
   (ABA). Обычные счётчики опроса и имя для показа ревизию не меняют.
6. Только новый intent отправки с подтверждённым synchronous COMMIT разрешает
   один POST. После него URL собирается в памяти из настроенной базы и своего
   сохранённого имени, как одного escaped segment, с единственным `format=json`.
   Бот не читает URL подписки и не берёт адрес из ссылок панели.

Запрос живёт пять минут. Секретное разрешение ограничено также концом аренды,
локальной границей следующей даты счёта и UTC instant recurring, сохранённым для
соответствующего периода; выбирается самый ранний срок. Проверки повторяются
после ожидания блокировок, после COMMIT и перед сетевой передачей. Весь preflight/отправка ограничен 30
секундами при аренде 45 секунд; финализация — пятью секундами. Общая остановка
приложения по-прежнему имеет бюджет десять секунд и ждёт workers до закрытия БД.

Для первого периода `current_invoice_id` должен оставаться `first_invoice_id`.
Выдача следующего периода требует отдельной immutable renewal-записи,
подтверждения текущего счёта и сохранённой квитанции завершённого renewal.
Первая активация остаётся только историческим аудитом; старый запрос режима
`first` никогда не преобразуется в запрос продления.

## Отказы, повторы и секреты

При отказе отправляется только постоянный несекретный текст. Изменившаяся
авторитетная оплата оставляет sticky review `2007` на подтверждении
**запрошенного периода** и платёжной проверке, сохраняя proof и даты. Сбой,
неполный/нестабильный GET или несовместимая версия провайдера не объявляются неоплатой. Противоречивый JSON с повторными
или case-folded полями отвергается до утраты wire evidence.

Защищённая отправка использует отдельную capability: `protect_content=true`,
`disable_notification=true`, `link_preview_options.is_disabled=true`, без
parse mode и клавиатуры. Нет fallback в обычный sender, редиректов или
автоматического повтора POST. Эти флаги не защищают от скриншота и не отзывают
уже прочитанную ссылку. Реальный импорт и поведение приложений ещё не проверены.

**Любая ошибка защищённого запроса терминальна для этого запроса.** Даже после
явного Telegram-отказа нужен новый `/config`, повторяющий все проверки.
Валидированный однозначный 4xx-отказ, кроме timeout, может снять собственную
резервацию. При 429 сохраняется общий cooldown бота не меньше минуты и
`retry_after`. 5xx, timeout, противоречивый/malformed ответ, потеря результата,
COMMIT acknowledgement или аренды не доказывают отсутствие доставки.

Неизвестная секретная отправка оставляет **неистекающий барьер чата**. Истечение
аренды, перезапуск, новая политика или новый `/config` не разрешают ещё один
секрет. Сам outbox-запрос завершается failed: обычное меню/help не блокируются,
новый запрос получает только отказ. Поддерживаемого инструмента снятия барьера
пока нет. Ручное восстановление требует доказать отсутствие живого старого
отправителя и установить фактический результат; удалять intent нельзя.

Логи содержат только технические ID заданий и числовые коды. Application wrappers
и чувствительные wire-типы редактируют обычное и прямое structured logging;
application wrappers и protected message также редактируют generic JSON.
Явные `ClientRecord.Raw`/JSON и activation payload содержат credentials для
защищённого журнала: их нельзя вкладывать в произвольные диагностические DTO.

## Проверки и оставшиеся ограничения

Используются синтетические HTTP-сервисы и отдельные схемы PostgreSQL: контракты,
ownership/reviews, сохранённые receipts, ABA, сроки после ожидания блокировок,
однократное разрешение, неизвестная доставка, FIFO/cooldown и отсутствие URL
в outbox. Сквозной тест проходит реальную активацию, GET-only проверку,
Telegram renderer, outbox worker и финализацию в БД.

В обычном CI этот положительный сквозной тест запускается отдельным обязательным
шагом на loopback PostgreSQL 17: реальный 5-секундный DB budget не должен
зависеть от WAN-задержки до Neon. Только этот top-level test явно исключён из
Neon-шага; остальные тесты остаются на Neon. Ни контексты, ни проверки E2E не
ослаблены. Нефильтрованный набор, включая этот тест, доступен локальной командой
и через ручной CI `full_neon=true`. Увеличен только внешний бюджет CI-сервиса.
При сбое тест выводит лишь разрешённые технические признаки и длительности,
без исходных ошибок подключения, URL или тел сообщений.

Разрешение на squash merge не разрешает deployment, реальные платежи, POST
провайдерам или Telegram-доставку. Для них нужны отдельное согласие владельца
и обозначенная тестовая запись. Без них не подтверждены установленная write
совместимость, корректность публичной базы подписки, импорт и связь sing-box.

Провайдеры не дают общего транзакционного снимка: внешнее изменение после
последнего GET всё ещё может опередить отправку. Сетевой запрос и отзыв доступа
не атомарны; персональная ссылка — bearer secret, её нельзя «забрать обратно».
Для строгого отзыва требуется отдельный управляемый слой доступа. Широкий
rollout остаётся закрыт до отключений/просрочки и согласованных живых испытаний.

## Imported-origin delivery status

11b.7.1 original-active-baseline protected delivery merged as PR #33 at `4a6522d1b881ef031ef580c67486406237ecf004`. Exact final HEAD `e2ca6863aa785681433932f3a9caf82a0d46319b` passed required CI `40035675235` (all three jobs, including PG17 integration/race and smoke/Docker); post-merge main CI `40036327450` succeeded. All three imported-configuration SQL workflows passed runtime. Scope is only the ORIGINAL ACTIVE imported baseline: current invoice equals immutable origin current invoice and paid anchor; require current normal proof, strictly future UTC date, static binding/inbounds, legacy price/notes, source-scope/public-base GET fences, legitimate immutable origin FK/mode and current configuration/admission/runtime revisions. `first_invoice_id` remains SQL NULL; no fake OWN history; OWN first/renewal paths stay unchanged. GET-only provider reads do not require billing-effects permission; configuration policy separately gates secret Telegram delivery. No auto-enable; preserve final checks, in-memory URL assembly and per-chat unknown-send barrier.

Independent review `34c46fce-9abc-42a` fixed two P2 findings (currency-policy drift and test-worker bot scope); final P1/P2/P3 findings were zero. The CI SQL workflows passed runtime for GET-to-protected delivery/guard release, public-base denial before GET, changed-current proof sticky review 2007 after GET, runtime permission ABA denial, and unknown-send 503 with durable permanent chat barrier across revocation/new request/restart. This is synthetic CI evidence, not live consumer readiness.

11b.7.2 implementation and four PostgreSQL workflow cases are ready; PR #34 is not open. The cases compile only and have not run SQL runtime. It expands imported delivery only for a CURRENT COMPLETED renewal, including ordinary ledger renewal and initial-restored current debt. Require explicit current payment proof, completed native renewal plan/journal/receipts, and strictly future current UTC date; historical origin proof/paid anchor is never current entitlement. Keep the shared imported baseline immutable and current proof/audit separate. `RenewalAudit.OriginMode` is new `omitempty` (`ledger` or `imported_initial_paused`). The renewal request keeps SQL NULL `activation_id`, adds a conditional generated baseline-origin invoice FK, and retains the existing current-renewal composite FK; old requests/fingerprints remain unchanged. No fake activation, ledger, first invoice, or proof. Current admission/runtime/config policy must match; effects may be off after known completion, and historical action permission revisions need not equal current read/request revisions. Preserve GET-only source/SUI/Ninja/profile/inbounds/public-base/native-currency/date/UTC checks and URL in memory after final DB authorization. Unknown send preserves the existing per-chat barrier; do not replay/rearm or make additional provider/chat GETs after denial/restart. Do not broaden baseline denial before the full audit is verified. All three producers (domain `dcb4a0e2-70f3-4ca`, Store `e7576737-ba1d-471`, tests `bda905ec-faef-45c`) report complete. Parent full normal unit/race/vet/CGO0 build/module/gofmt checks and PostgreSQL compile-only passed. Independent review `e370ed66-465a-4ef` read the full patch/current sources and found zero behavioral/design findings; reviewer ran no checks and code is unchanged since review. Exact-HEAD CI, SQL runtime, PR #34 and merge remain pending. Apply CLI/default-off startup wiring comes later. Stage 11 remains incomplete; no live provider, Telegram, payment, working DB, publish, or deployment result is claimed.

11b.6.2 initial-debt restoration is merged and runtime-verified; details and CI evidence are in [import-admission.md](import-admission.md) and [invoice-lifecycle.md](invoice-lifecycle.md). The full 11b workflow remains incomplete: audited 11b.7.2, apply CLI and default-off startup wiring are later work. No live provider, Telegram, payment, working database, publish or deployment result is claimed.
