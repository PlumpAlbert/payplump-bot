# Тексты Telegram

Редактируйте `templates/{group}/*.tmpl`. Тексты встроены в бинарник через Go
`embed`: после правки нужны тесты, **пересборка образа и pull + redeploy**.
Смонтированный файл, переменная окружения или перезапуск старого образа текст
не изменят. Runtime overrides и hot reload нет.

Это обычный текст Telegram, без HTML/Markdown и `parse_mode`. Например,
`<b>текст</b>` будет показан буквально. Сохраняйте переносы и пробелы: загрузчик
удаляет ровно один финальный `\n` редактора, остальное сохраняет. Некоторые
файлы являются фрагментами: их начальные и конечные переносы нужны при склейке.

Разрешены только текст и подстановки вида `{{.InvoiceNumber}}` (пробелы внутри
скобок допустимы). Доступные поля каждого файла перечислены ниже. Нет функций,
условий, циклов, комментариев, `define`, `template`, переменных или `{{- ...}}`.
Не добавляйте и не переименовывайте файлы/поля без изменения Go-манифеста.
Пустые/невалидные тексты, неизвестные файлы/поля и запрещённые конструкции
отклоняются при тестировании и инициализации пакета, до внешних действий.

Меняйте формулировки, не бизнес-логику: проверка оплаты, выбор ветки,
разрешения, адресаты, сроки и защита URL остаются в Go. Не меняйте имена
команд `/start`, `/invoice`, `/broadcast_confirm` и прочих команд, callback/action
значения или смысл финансовых/защитных предупреждений. Четыре `label_*`
задают одновременно кнопки и их распознавание; они должны быть непустыми,
различаться и не начинаться с `/` (чтобы не перекрыть команды).
После изменения названий кнопок ранее отправленные клавиатуры останутся старыми:
команда `/menu` или `/start` обновит их. Сами slash-команды не зависят от названий.

**Не помещайте секреты в публичные шаблоны**: токены, пароли, реальные
персональные ссылки, реквизиты и данные клиентов. Значения выбираются явно
в Go как строки, целые конфиги, snapshots и ответы API недоступны.
`SubscriptionURL` разрешён только в `configdelivery/subscription.tmpl`:
он подставляется в памяти внутри отдельной защищённой отправки, не в обычный
outbox. `InvoiceURL` — проверенная ссылка на счёт, не ссылка подписки.
`Body` — уже проверенный текст администратора только для предпросмотра;
сама рассылка отправляет исходный текст без шаблонизации.

## Значения полей

- `ActorID`, `ClientID`, `OperationID`, `JobID`, `InvoiceID`: строковое представление идентификатора.
- `InvoiceNumber`: номер счёта; `Amount`, `Balance`: точные десятичные суммы.
- `CurrencyID`: идентификатор валюты источника.
- `DueDate`: срок оплаты Invoice Ninja, не граница доступа.
- `SentDate`: каноническая фактическая дата отправки, граница определяется в Go.
- `PaymentDate`, `NextInvoiceDate`, `TargetDate`: проверенные даты платежа, следующего счёта и продления.
- `PaymentPhone`, `PaymentBank`, `PaymentRecipient`: выбранные реквизиты из настроек.
- `DisplayName`: отображаемое имя; `InvoiceURL`: проверенная ссылка приглашения счёта.
- `State`, `Status`, `StatusText`, `Verb`, `Reason`, `Subject`: выбранные в Go описание, статус, готовый текст состояния, действие, причина и тема.
- `ReviewCode`, `DiagnosticCode`, `ConfirmedSteps`: код проверки, короткий код диагностики и подтверждённые шаги (не переводите внутренние значения).
- `Count`, `Pending`, `Enqueued`, `Sent`, `Failed`, `Skipped`: строковые счётчики рассылки.
- `Body`, `Token`: текст предпросмотра и одноразовый токен команды рассылки (не API-токен).
- `SubscriptionURL`: персональный URL, только для защищённой выдачи.

## Поля по файлам

`—` означает, что подстановок нет. Все поля строковые; разрешены только
перечисленные для конкретного файла. Выбор варианта и сборка фрагментов остаются в Go.

| Файл | Разрешённые поля |
| --- | --- |
| `activation/complete_admin.tmpl` | `OperationID`, `ActorID`, `NextInvoiceDate` |
| `activation/complete_client.tmpl` | `NextInvoiceDate` |
| `activation/review_admin.tmpl` | `OperationID` |
| `broadcast/audience_limit.tmpl` | — |
| `broadcast/cancelled.tmpl` | — |
| `broadcast/confirm_audience.tmpl` | `Count`, `Token` |
| `broadcast/draft_changed.tmpl` | — |
| `broadcast/draft_expired.tmpl` | — |
| `broadcast/draft_missing.tmpl` | — |
| `broadcast/failed.tmpl` | — |
| `broadcast/instructions.tmpl` | — |
| `broadcast/invalid_body.tmpl` | — |
| `broadcast/invalid_command.tmpl` | — |
| `broadcast/missing.tmpl` | — |
| `broadcast/policy_changed.tmpl` | — |
| `broadcast/preview.tmpl` | `Body`, `Token` |
| `broadcast/status.tmpl` | `Pending`, `Enqueued`, `Sent`, `Failed`, `Skipped` |
| `broadcast/status_unconfirmed.tmpl` | — |
| `broadcast/summary.tmpl` | `JobID`, `Enqueued`, `Sent`, `Failed`, `Skipped` |
| `broadcast/unauthorized.tmpl` | — |
| `clientmenu/access_deadline.tmpl` | `SentDate` |
| `clientmenu/access_denied.tmpl` | — |
| `clientmenu/access_requested.tmpl` | — |
| `clientmenu/actions.tmpl` | — |
| `clientmenu/configuration_unavailable.tmpl` | — |
| `clientmenu/excluded.tmpl` | — |
| `clientmenu/home.tmpl` | `StatusText` |
| `clientmenu/instructions.tmpl` | — |
| `clientmenu/invoice.tmpl` | `InvoiceNumber`, `Amount`, `Balance`, `DueDate`, `InvoiceURL` |
| `clientmenu/invoice_cancelled.tmpl` | — |
| `clientmenu/invoice_draft.tmpl` | — |
| `clientmenu/invoice_footer.tmpl` | — |
| `clientmenu/invoice_paid.tmpl` | — |
| `clientmenu/invoice_pending.tmpl` | — |
| `clientmenu/invoice_unavailable.tmpl` | — |
| `clientmenu/label_configuration.tmpl` | — |
| `clientmenu/label_help.tmpl` | — |
| `clientmenu/label_invoice.tmpl` | — |
| `clientmenu/label_status.tmpl` | — |
| `clientmenu/next_invoice_date.tmpl` | `NextInvoiceDate` |
| `clientmenu/payment_bank.tmpl` | `PaymentBank` |
| `clientmenu/payment_date.tmpl` | `PaymentDate` |
| `clientmenu/payment_details.tmpl` | `Balance`, `PaymentPhone` |
| `clientmenu/payment_details_missing.tmpl` | — |
| `clientmenu/payment_recipient.tmpl` | `PaymentRecipient` |
| `clientmenu/payment_unverified.tmpl` | — |
| `clientmenu/register_first.tmpl` | — |
| `clientmenu/state_active.tmpl` | — |
| `clientmenu/state_awaiting_payment.tmpl` | — |
| `clientmenu/state_overdue.tmpl` | — |
| `clientmenu/state_payment_review.tmpl` | — |
| `clientmenu/state_setting_up.tmpl` | — |
| `clientmenu/state_unknown.tmpl` | — |
| `clientmenu/status.tmpl` | `State` |
| `clientmenu/status_footer.tmpl` | — |
| `clientmenu/welcome.tmpl` | — |
| `configdelivery/denied.tmpl` | — |
| `configdelivery/subscription.tmpl` | `SubscriptionURL` |
| `import/active_period_expired.tmpl` | — |
| `import/admission_conflict.tmpl` | — |
| `import/ambiguous_source.tmpl` | — |
| `import/billing_paused.tmpl` | `ActorID`, `NextInvoiceDate` |
| `import/excluded.tmpl` | — |
| `import/historical_anchor_limit.tmpl` | — |
| `import/invalid_policy.tmpl` | — |
| `import/issue_admin.tmpl` | `Subject`, `Reason`, `DiagnosticCode` |
| `import/paused_history_not_supported.tmpl` | — |
| `import/payment_unverified.tmpl` | — |
| `import/registration_blocked.tmpl` | — |
| `import/source_changed.tmpl` | — |
| `import/source_failure.tmpl` | — |
| `import/subject_actor.tmpl` | `ActorID` |
| `import/subject_candidate.tmpl` | `DiagnosticCode` |
| `import/subject_general.tmpl` | — |
| `import/unpaid_debt.tmpl` | — |
| `import/unsupported_profile.tmpl` | — |
| `lifecycle/invoice_admin.tmpl` | `Status`, `ActorID`, `InvoiceNumber`, `Balance`, `SentDate` |
| `lifecycle/invoice_client.tmpl` | `Verb`, `InvoiceNumber`, `Balance`, `SentDate` |
| `lifecycle/invoice_issued.tmpl` | — |
| `lifecycle/invoice_updated.tmpl` | — |
| `lifecycle/overdue_admin.tmpl` | `ActorID`, `InvoiceNumber` |
| `lifecycle/overdue_client.tmpl` | `InvoiceNumber`, `SentDate` |
| `lifecycle/overdue_review_admin.tmpl` | `ActorID`, `InvoiceNumber`, `ReviewCode`, `ConfirmedSteps` |
| `lifecycle/renewal_admin.tmpl` | `ActorID`, `InvoiceNumber`, `Amount`, `CurrencyID`, `PaymentDate`, `TargetDate` |
| `lifecycle/renewal_client.tmpl` | `InvoiceNumber`, `TargetDate` |
| `lifecycle/renewal_review_admin.tmpl` | `ActorID`, `ClientID`, `InvoiceID`, `ReviewCode` |
| `lifecycle/review_admin.tmpl` | `ActorID`, `InvoiceNumber`, `Reason` |
| `lifecycle/review_association.tmpl` | — |
| `lifecycle/review_balance.tmpl` | — |
| `lifecycle/review_collision.tmpl` | — |
| `lifecycle/review_contract.tmpl` | — |
| `lifecycle/review_excluded.tmpl` | — |
| `lifecycle/review_invoice.tmpl` | — |
| `lifecycle/review_previous_payment.tmpl` | — |
| `lifecycle/review_sent_date.tmpl` | — |
| `lifecycle/unknown_invoice.tmpl` | — |
| `payments/confirmed_admin.tmpl` | `ActorID`, `InvoiceNumber`, `Amount`, `PaymentDate`, `NextInvoiceDate` |
| `payments/confirmed_client.tmpl` | `InvoiceNumber`, `Amount`, `PaymentDate`, `NextInvoiceDate` |
| `payments/review_admin.tmpl` | `ActorID`, `ClientID` |
| `registration/default_name.tmpl` | — |
| `registration/invoice.tmpl` | `InvoiceNumber`, `Amount`, `DueDate`, `PaymentPhone` |
| `registration/invoice_admin.tmpl` | `DisplayName`, `ActorID`, `InvoiceNumber`, `Amount`, `DueDate`, `InvoiceURL` |
| `registration/invoice_footer.tmpl` | `InvoiceURL` |
| `registration/payment_bank.tmpl` | `PaymentBank` |
| `registration/payment_recipient.tmpl` | `PaymentRecipient` |
| `registration/review_admin.tmpl` | `OperationID`, `ReviewCode`, `ActorID`, `Reason` |
| `registration/review_client.tmpl` | — |
| `registration/review_contract.tmpl` | — |
| `registration/review_customer_conflict.tmpl` | — |
| `registration/review_default.tmpl` | — |
| `registration/review_excluded.tmpl` | — |
| `registration/review_existing_customer.tmpl` | — |
| `registration/review_invoice_conflict.tmpl` | — |
| `registration/review_invoice_state.tmpl` | — |
| `registration/review_local_association.tmpl` | — |
| `registration/review_unknown_write.tmpl` | — |

## Проверки

`go test -race ./...` и `go vet ./...`. Полные проверки БД требуют отдельной
тестовой БД; для правки текста рабочие API и БД не нужны.
