# Ручная подготовка выпуска

## Статус и границы

Ветка `feat/manual-release-tooling` содержит подготовительные скрипты и
`workflow_dispatch` workflow. PR #38 и synthetic restore rehearsal прошли.
Release tooling реализовано и прошло независимое review: review A завершился
без замечаний; review B выявил P2, исправление подтверждено финальным re-review
`43d6862f-9f7a-477` без доказуемых замечаний. Exact-HEAD required CI и PR #39
ещё pending. Workflow не запускался; образ не собирался и не публиковался,
registry digest отсутствует. Подготовка не является разрешением на публикацию,
развёртывание или доступ к VPS.

[Release Compose](../compose.release.yaml) — шаблон отдельного будущего
развёртывания. Он требует immutable image digest и внешние настройки PostgreSQL;
Флаги `IMPORTED_RUNTIME_ENABLED=false` и `IMPORTED_EFFECTS_ENABLED=false` заданы
в Compose явно; Dockerfile задаёт `USER 65532:65532`. Образ и runtime на VPS не
проверялись, sample resource limits не измерены. Валидатор
synthetic Compose проверяет структуру разрешённой конфигурации, не состояние
VPS, владение секретами или пригодность production.

## Предусмотренный workflow

Workflow принимает только ручной запуск с полным commit SHA, платформой
`linux/amd64` или `linux/arm64` и `publish=false` по умолчанию. Предварительная
проверка из trusted workflow tooling на `main` подтверждает репозиторий, что
указанный commit является ancestor `origin/main`, и последние успешные GitHub
Actions checks `fast-checks`, `postgres-integration`, `test` именно для этого
SHA. Сборка использует отдельный source checkout выбранного commit; helper
запускается из trusted workflow SHA, а не из непроверенного target source.

Проверка source CI динамически получает workflow ID через GitHub API и
проверяет, что он соответствует активному workflow `CI` с путём
`.github/workflows/ci.yml`, а также main push точного SHA. Полученный ранее ID
`387650116` — только сверенный пример, не константа.
Она проверяет текущий run detail, три exact-head успешных job, затем повторно
читает detail, чтобы обнаружить конкурентный rerun. Устаревший зелёный run,
другой workflow, неактуальная попытка, pending/failure или несовпадающий SHA
не дают пройти gate. Финальный re-review проверил, что чужой workflow или
старая попытка не авторизуют выпуск, а повторный GET обнаруживает смену attempt.
Обычный source CI подтверждает исходный код, но не является запуском release
workflow.

Dry-run ветка не имеет package-write permission, не выполняет registry login и
push. Предусмотрены локально собранный image и private artifact с manifest.
Manifest фиксирует commit, migration filenames/SHA-256, PostgreSQL major 17,
target platform/architecture, image labels, image user `65532:65532`,
entrypoint/healthcheck, image ID и размер/SHA-256 локального Docker archive.
В dry-run `published=false` и `registry_digest=null`. Локальный archive size не
равен размеру скачивания из registry, а runner architecture не подтверждает
архитектуру VPS. Реальные manifest и source/image data не производились этим
workflow.

## Publish gates

Публикация требует явных `publish=true`, подтверждения
`publish-reviewed-commit`, owner-provided previous same-repository
`ghcr.io/...@sha256:...`, свежего exact-commit CI и одобрения через уже
существующую GitHub Environment `release`. Approval Environment происходит до
шагов publish-job. Workflow не создаёт Environment и не меняет её настройки.
Кодовая конфигурационная проверка требует непустой `required_reviewers` только
из явных User IDs; Team-only и смешанный Team/User список не поддерживаются и
fail closed. Также нужны `prevent_self_review=true`, вложенный
`deployment_branch_policy` с `protected_branches=true`,
`custom_branch_policies=false` и workflow ref `main`. Неизвестное/отсутствующее
поле или API-ошибка означает отказ.

Публичная REST-схема описывает environment-поля `id`, `name`,
`protection_rules` и `deployment_branch_policy`; ни REST, ни публичная GraphQL
схема Environment не предоставляет `can_admins_bypass`. Поэтому API-проверкой
нельзя подтвердить, что admin bypass выключен. Владелец может отключить bypass
в UI как дополнительную защиту, но это не программально подтверждённый gate.
Сверенные публичные определения: [GitHub REST OpenAPI](https://raw.githubusercontent.com/github/rest-api-description/main/descriptions/api.github.com/api.github.com.json)
и [GraphQL Environment schema](https://docs.github.com/public/fpt/schema.docs.graphql).
Фактическое состояние Environment проекта не запрашивалось.

Approval history не содержит timestamp или workflow-attempt number. В publish
режиме `GITHUB_RUN_ATTEMPT` должен быть `1`; это проверяется до любых Git/GitHub
API calls, чтобы rerun не переиспользовал approval первой попытки. Dry-run
можно повторять. Повтор публикации требует нового `workflow_dispatch`, нового
run ID и нового независимого owner approval. Перед Docker build helper требует
`--require-approval`; он проверяет конфигурацию и заново читает фактическую
read-only историю approvals текущего run через
`/repos/{owner}/{repo}/actions/runs/{run_id}/approvals`. Проверка повторяется
непосредственно перед token login/push. Сохранённый intent не является
источником авторизации.

Approval response — массив записей с `state`, `user{id,login}`, `environments`
(массив `{id,name}`) и `comment`; timestamp и гарантированного порядка нет.
Проверка выбирает запись именно для текущей release Environment ID/name,
текущего explicit reviewer user ID и независимого от `GITHUB_ACTOR` пользователя.
Подходящая rejected/pending запись, self-approval, approval от неразрешённого
reviewer, неоднозначность, отсутствие записи, неизвестное состояние,
недостаточные permissions или API-ошибка означают отказ. Нельзя считать
«последней» запись по порядку массива. Без наблюдаемого owner approval push не
разрешается, даже если UI bypass был использован. Реальный API approval этого
workflow не запрашивался.

Проверка owner-provided previous digest подтверждает привязку к тому же GitHub
repository и наличие SHA-256 digest. Это pin, а не доказательство, что образ
сейчас работает или пригоден для rollback. После будущего одобренного push
manifest может содержать registry digest опубликованного образа; локальный image
ID или archive hash registry digest не заменяют. В dry-run
`published=false`, `registry_digest=null`.

## Секреты и артефакты

Intent — приватный файл mode 0600 только для текущего запуска; каждый preflight
использует новый файл. Его сохранение или artifact upload не даёт права на
последующую публикацию. Каждая проверка approval читает фактическую историю
текущего run. Local-build validation manifest и финальный post-push manifest
имеют разные пути, чтобы исключить коллизию и отказ после успешного push.
Registry credential передаётся через stdin, используется отдельный приватный
Docker config и удаляется при завершении. Не помещать секреты в repository,
manifest, логи или артефакты; не загружать image archive или registry credentials
в общедоступное место. Dry-run artifact содержит локальный image archive, поэтому
доступ и retention должны быть ограничены.

## Что не подтверждено

Родительские безопасные проверки кода (Go unit/race/vet, CGO=0 build,
`go mod verify`, tagged PostgreSQL compile-only, gofmt и shell syntax) прошли;
операторские проверки: 14 всего, 13 passed и один локальный Docker-dependent
skip. Policy tests: 7 групп passed; fake-build tests: 7 passed. Это локальные
проверки и не actual Docker runtime. Exact-HEAD required CI ещё pending.
PR #38 synthetic restore CI не является доказательством работы release workflow.
Сам manual release workflow запрещено dispatch-ить, включая dry-run, в рамках
этого этапа. Не выполнялись Docker runtime, registry login/push, publication,
deploy или SSH.

Реальные registry size, target VPS architecture/ресурсы, health/signals в фоне,
compatibility rollback, backup и живой цикл остаются непроверенными. Реальные
manifest/source-image данные не измерялись. Owner-provided предыдущий digest не
проверен как рабочий rollback образ. Down migrations отсутствуют; не
предполагать, что предыдущий бинарник совместим с новой схемой.