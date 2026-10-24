# Резервное копирование и изолированное восстановление

Это runbook-шаблон для владельца. Backup и реальное восстановление не выполнялись;
этот документ не является разрешением на доступ к production или live rollout.

## Что резервировать

Владелец организует независимые копии:

- PostgreSQL payplump-bot: согласованная копия, версия PostgreSQL 17,
  версия схемы/миграций и время UTC.
- S-UI: база данных и необходимые данные подписок/идентификаторов.
- Invoice Ninja: экспорт или backup базы и исходных связей/счетов. Механизм
  экспорта установленной версии неизвестен; владелец должен подтвердить его
  до процедуры. Не предполагать путь, endpoint или способ экспорта.
- Конфигурация и исходные ссылки/связи, необходимые для проверки: хранить
  приватно. Токены, пароли, webhook/API/TG/PG secrets и ключи шифрования
  хранить отдельно от архивов в одобренном secret store, никогда в Git, отчётах
  или логах.

Архивы должны быть зашифрованы при хранении и передаче, иметь ограниченный
доступ и документированный срок хранения. Записать backup UTC, app commit и
image digest, версии миграций и PG, hash архива, checkpoint/результат и
ответственного. Не публиковать приватные архивы и manifests.

## Изолированная rehearsal

В required CI PR #38 synthetic rehearsal завершилась успешно на disposable
PostgreSQL 17: source preparation, default dry-run, явный restore в отдельную
пустую loopback target DB, затем target verification. Report указал
`synthetic=true`, `recovery_verified=true`, 41 таблицу и 5 sequences; read-only
target report/snapshot после проверки не изменились. Приложение не запускалось,
провайдеры/Telegram/рабочая БД не затрагивались; приватные archive/manifest не
публиковались. Это проверяет синтетический путь инструмента, но не реальные
backup или восстановление. Архитектурные детали и CI history см. в
[autonomous-work.md](autonomous-work.md).

Использовать только доверенный, owner-produced PostgreSQL custom-format архив.
Ни `pg_restore --list`, ни фильтрация объектов не делают произвольный архив
безопасным: DDL и функции могут выполнять код. Не восстанавливать неизвестные
или пользовательские SQL-архивы.

`scripts/restore-check.sh` сначала запускать в dry-run (по умолчанию), затем
только с явным `--apply` и разрешением владельца. CI rehearsal выполнила apply
только в созданную job собственную пустую disposable target DB; это не разрешает
локальный SQL runtime или применение к реальной базе. Для owner-run apply нужен
отдельный пустой target с loopback-доступом, отличающийся от source; не подключать
production source как target. Проверить режим файла, отсутствие symlink и
private passfile. Не использовать обычную рабочую базу. В CI rehearsal
использует отдельные source/target disposable PostgreSQL 17 DBs, создаваемые
только самим job; локальный PostgreSQL SQL runtime не запускать.

Новый образец rehearsal синтетический: он проверяет snapshot/restored state
классов данных, guards, leases, платежных периодов и admission provenance,
но не является восстановлением реальной клиентской базы и не выдаёт доступ.
До запуска приложения сверить структуру, migration checksums, ограничения,
FK, sequences и безопасный read-only отчёт. Сохранённые отчёты и доказательства
не являются финансовой authority. Не считать прошедшую CI rehearsal
подтверждением настоящего backup или внешнего экспорта.

## Барьеры и запуск

Не выполнять автоматический down migration. Старый образ не считается
совместимым с более новой схемой без доказанной совместимости. Rollback означает
либо совместимый проверенный код со схемой, либо восстановление согласованного
предыдущего snapshot в отдельной изолированной среде с quiescence и разрешением
владельца. Сохранять immutable intents, receipts, FK, pinned payment proofs и
неизвестные операции.

После восстановления не снимать и не переармливать `activation_write_guard` /
epoch или per-chat `configuration_chat_guards`; неизвестные native и secret
действия остаются held. Не отправлять ссылки, создавать счета/продления или
менять доступ для «проверки» restore. Сначала завершить read-only сверку и
проверить соответствие кода, digest, schema и отдельно предоставленных secrets.

Запуск приложения допустим только в защищённой owner-controlled среде после
явного одобрения, с согласованным commit/digest и проверенными секретами.
Все feature flags остаются default-off. Admission не выдаётся startup; imported
runtime/effects также выключены по умолчанию. Startup fencing может намеренно
durable-revoke effect/read permissions и продвинуть ревизию, когда флаг был
включён; это ожидаемое permission-поведение после read-only snapshot проверки,
а не часть сравнения неизменённых backup данных. Новое включение требует
явного актуального scope и более высокой ревизии после disable.

## Owner gates

Пока не подтверждены владельцем: реальные внешние backup/export, доступность
архивов/ключей, восстановление S-UI и Invoice Ninja, изолированный запуск
реального приложения и проверка связей. Эти работы не выполнены и не должны
отмечаться завершёнными по результатам синтетического CI. Release preparation
(13) реализована и независимо проверена; финальный re-review
`43d6862f-9f7a-477` не нашёл доказуемых замечаний. Exact-HEAD required CI и PR
#39 остаются pending; workflow не запускали, образ не собирали и не
публиковали. Смежные этапы идут строго в порядке: synthetic restore CI (12),
release preparation без публикации (13), synthetic resource preparation и
owner inputs (14). Никаких SSH, чтения real env, production GET/DB, публикации
или deployment в рамках этого runbook.
