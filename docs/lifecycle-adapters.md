# Контракты продления и просрочки: этап 8a

> Историческое описание контрактов. Обычный запуск: [deployment.md](deployment.md).
> Номера релизов ниже обозначают изученные исходники, а не условия запуска.
> Проверки точной версии и запросы только ради неё удалены; проверки реальных
> записей, владельцев и результатов операций остаются.

Этот документ фиксирует контрактный слой этапа 8a. Отдельный default-off Stage 2
использует часть этих адаптеров; его текущий runtime-контракт описан в
[invoice-lifecycle.md](invoice-lifecycle.md). Рабочие Invoice Ninja и S-UI в ходе
разработки не вызывались. Синтетические тесты не подтверждают поведение
установленного сервиса или прекращение соединений.

## Закреплённые источники

- S-UI: `db7f9545e7e2907a5d46a2b6bfc4a0b64c2de348` (1.6.4), frontend
  `e4525297b002c1c3be234cc1c9695ef84be741a0`.
- Invoice Ninja: `493c3884e4214028bc5970cb41bafbd9c4d96401` (5.13.47).

[S-UI router](https://github.com/alireza0/s-ui/blob/db7f9545e7e2907a5d46a2b6bfc4a0b64c2de348/web/web.go),
[v2 handler](https://github.com/alireza0/s-ui/blob/db7f9545e7e2907a5d46a2b6bfc4a0b64c2de348/api/apiV2Handler.go),
[API service](https://github.com/alireza0/s-ui/blob/db7f9545e7e2907a5d46a2b6bfc4a0b64c2de348/api/apiService.go),
[client save](https://github.com/alireza0/s-ui/blob/db7f9545e7e2907a5d46a2b6bfc4a0b64c2de348/service/client.go).
[Ninja update request](https://github.com/invoiceninja/invoiceninja/blob/493c3884e4214028bc5970cb41bafbd9c4d96401/app/Http/Requests/RecurringInvoice/UpdateRecurringInvoiceRequest.php),
[base repository](https://github.com/invoiceninja/invoiceninja/blob/493c3884e4214028bc5970cb41bafbd9c4d96401/app/Repositories/BaseRepository.php),
[recurring service](https://github.com/invoiceninja/invoiceninja/blob/493c3884e4214028bc5970cb41bafbd9c4d96401/app/Services/Recurring/RecurringService.php).

## S-UI

`PrepareDisable` замораживает полный enabled before-image и собственную
сохранённую привязку ID/name/config hash. `DisableAuthorized` заново проверяет
запись и последний полный GET; только после callback-разрешения отправляет
один `/apiv2/save` с form `object=clients`, `action=edit`, полным `data` и
единственным изменением `enable:false`. Затем обязательно читает результат.

Сохраняются credentials, порядок inbounds, безопасные неизвестные вложенные
поля и нелокальные ссылки. Трафик/серверные даты не становятся локальной
истиной: источник может обновлять их параллельно. При лимитированной квоте и
нулевых up/down запись запрещена: в этой версии нули вызывают rollup/reset.
Неподдерживаемые auto-reset/delay политики не принимаются. Уже disabled запись
сама по себе не доказывает успешное выполнение прежнего неизвестного запроса.

Отдельный `PrepareCloseSessions` требует disabled before-image. Его authorized
метод отправляет ровно `/apiv2/closeSessions` с form `u=<сохранённый name>`.
Это **имя, не числовой ID**. Проверяется `success` даже при HTTP 200 и повторный
disabled readback. Успех означает принятую команду, не доказанный нулевой счётчик
всех соединений. Источник реализует закрытие по-разному для разных протоколов;
проверка фактического обрыва и запрета повторного подключения остаётся владельцу.

## Invoice Ninja

Адаптеры принимают только собственную исходную месячную template policy,
конкретный ID/клиента/приватный маркер и замороженные факты. Первый start и
renewal lifecycle разделены: новый слой допускает непустой `last_sent_date`
прошлых циклов, но не меняет прежний activation validator.

- `PauseAuthorized`: active → paused одним bulk action `stop`.
- `RescheduleAuthorized`: только paused, строго будущая календарная дата,
  sparse `PUT /api/v1/recurring_invoices/{id}` с **двумя** полями:
  `next_send_date` и фактический `exchange_rate`. Оставленный exchange rate
  серверный request иначе заменяет на 1. Полный GET JSON обратно не отправляется.
- `ResumeAuthorized`: только paused и только при совпадении исходной amount/UTC
  receipt, будущей даты и реального UTC instant. Отдельный bulk `start`, без
  `send_now`, `start` в PUT, клонирования или догоняющих счетов.

Последний GET сравнивается с первоначальным before-image, а не с обновлённой
основой. После ожидания callback ещё раз проверяются дата/UTC и контекст.
POST/PUT receipts и обязательный GET должны совпасть по финансовой/template
политике и сохранённому last_sent. UTC сравнивается как instant, суммы — точно.
Сервер пересчитывает surcharge-tax flags при любом update; поддерживается только
собственная template с нулевыми surcharges, после записи проверяются amount,
tax/lines. Семантика клиента/таймзоны определяется сервером: полученный новый
UTC instant сохраняется как известная receipt для последующего resume.

`CycleReader` — отдельная GET-only возможность. Она читает полный ограниченный
company inventory обычных счетов со всеми lifecycle states и страницами,
проверяет уникальность IDs/стабильность pagination и лишь затем выбирает
сохранённого клиента локально. Не полагается на недоказанный `recurring_id`
фильтр. Пустые даты черновиков сохраняются как наблюдение и не разрешают доступ.
Связь с собственным recurring, приватным маркером, периодом и proof проверяет
будущий lifecycle workflow; один этот reader никого не принимает под управление.

Recurring `stop` не отзывает уже выбранную cron-работу. Несколько неоплаченных
счетов или новая неожиданная генерация требуют сверки/hold, а не удаления
счетов или повторного POST.

## Общая защита внешней записи

`integrationhttp.Writer.Put`, как POST, не следует redirect и не предоставляет
replayable body. У всех новых операций обязателен final authorization callback.
Любая ошибка **после выдачи разрешения**, даже отмена до фактического HTTP,
сохраняет unknown/hold классификацию. Восстановление не основано на lease expiry.

Миграция `010` сохраняет **тот же** singleton `activation_write_guard`, его
owner и монотонный epoch. Общий `external_write_intents` становится FK-якорем
для activation и будущих lifecycle шагов. Все старые шаги, включая неизвестные,
переносятся без сброса guard. Новый activation-step получает постоянный якорь
атомарно; его удаление/повторная вставка не разрешает новый send. Подтверждение
может освободить guard, но не удаляет историю. Отдельный независимый барьер
для нового writer запрещён.

Сам callback адаптера не проверяет оплату: исполнитель обязан **после**
preflight адаптера заново проверить актуальные финансовые факты, роль/allowlist,
собственную provenance, срок и неизменность локального снимка, затем получить
новое synchronous-committed одноразовое разрешение в общем журнале.

## Напоминания и оставшиеся границы

В проверенных источниках нет подтверждённого узкого per-invoice API-флага,
который безопасно прекращает собственные почтовые напоминания Invoice Ninja.
Пауза recurring останавливает будущую генерацию, **не** напоминания уже
выставленного счёта. Код не меняет общие company/client settings и не подделывает
reminder timestamps. Будущий workflow останавливает только свои напоминания;
внешние настройки/email-очередь проверяются владельцем перед внедрением.

В исходном этапе 8a не было runtime wiring для отключений. Позднее Stage 2
подключил default-off enforcement только для текущего управляемого счёта; он не
включает автоматическое восстановление после оплаты, импорт, reminders или новые
grant capabilities. Каждый внешний эффект требует свежей авторизации. Никакие
установленные сервисы, реальные пользователи или действующие ссылки разработкой
не изменялись.
