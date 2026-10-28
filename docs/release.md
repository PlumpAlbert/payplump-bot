# Ручная подготовка выпуска

## Статус и границы

Release tooling и `workflow_dispatch` workflow доставлены в PR #39. Review A
завершился без замечаний; P2 из review B исправлен и подтверждён финальным
re-review `43d6862f-9f7a-477` без доказуемых замечаний. PR #39 merged.
Точный code HEAD `8824bc1263184e0a3fa80893ab4a8e4458f5c9ee` прошёл required CI
`40056489006` (fast-checks, postgres-integration, test); merge base
`0f1f1deb8dcd7d1bfec5e6d2bcac7c43a5b12d3d` прошёл post-merge main CI
`40057042166` (3 required jobs). Это обычные source CI проверки, а не запуск
release workflow. Manual workflow не запускался; release image этим workflow
не собран и не опубликован, registry digest отсутствует. Обычный source CI
собирал и запускал свои source/test images; это не release artifact. Подготовка не является разрешением на публикацию,
развёртывание или доступ к VPS.

[Release Compose](../compose.release.yaml) — шаблон отдельного будущего
развёртывания. Он требует immutable image digest, private runtime env-file
`PAYPLUMP_ENV_FILE` вне checkout и внешние настройки PostgreSQL. Dockerfile
задаёт `USER 65532:65532`; сервис запускается с обычным full-service профилем.
Release validator рендерит Compose с `config --no-env-resolution` и `/dev/null`
как interpolation source, проверяет private file paths и не читает/не печатает
runtime-файл. Helper до этого проверяет opaque-поведение renderer на собственном
некорректном синтетическом файле. Compose v2.38.2 имеет флаг, но всё ещё разбирает
содержимое env-файла и отклоняется до runtime-конфигурации. CI использует
закреплённый Compose v5.6.1 с проверкой SHA-256. Он проверен offline без daemon
на синтетических приватных файлах; опущенный
`required` в его JSON означает стандартное `true`, явное `false` запрещено.
Release-шаблон принудительно обнуляет неиспользуемые PG-настройки, включая
`PGOPTIONS`, `PGSERVICE*` и SSL/auth overrides: runtime-файл не переопределяет
отдельно заданное подключение и его TLS-политику. Это не запуск контейнера. Образ и runtime
на VPS не проверялись, sample resource limits не измерены. Synthetic Compose
validation не доказывает состояние VPS, владение секретами или production readiness;
см. [configuration.md](configuration.md).

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
Обычный source CI подтверждает исходный код, но не является запуском ручного
release workflow. Отдельная job `publish-main-image` описана ниже.

## Автоматическая публикация main

После merge workflow CI для push в `main` запускает `publish-main-image` только
после успешного агрегатора `test`, который требует зелёные `fast-checks` и
`postgres-integration` exact-head. Job ограничена `contents:read` и
`packages:write`, проверяет canonical repository, использует `GITHUB_TOKEN` через
stdin и отдельный временный Docker config с очисткой. PR, ручные Neon jobs и
остальные ветки образы не публикуют. Перед push сверяется, что commit всё ещё
текущий `main`; параллельные публикации сериализованы.

Job строит multi-platform `linux/amd64` + `linux/arm64` из проверенного commit и
обновляет GHCR-теги `ghcr.io/plumpalbert/payplump-bot:main` и
`:sha-<commit>`. Полный `sha256` digest выводится в summary успешного main CI.
Тег `main` намеренно изменяемый канал образов; `sha-<commit>` указывает на сборку
из конкретного commit, но registry tag сам по себе не pin. Для точного pin
используйте digest из summary. При пропуске устаревшего push остаётся новая
попытка только от актуального main commit. Эта job **публикует контейнер, но
не вызывает Komodo и не разворачивает стек**. Изменение `main` в registry само по себе не обновляет
уже запущенный контейнер.

Для нового GHCR package владелец должен один раз проверить и выставить package
visibility `Public`; GitHub может создать package закрытым по умолчанию. Перед
использованием проверяйте digest из CI summary, не полагайтесь только на moving
tag. Публикация содержит только приложение без runtime env, provider tokens,
DB credentials или backups. Ручной `release.yml` остаётся отдельным owner-gated
workflow и не вызывается автоматически.

Dry-run ветка не имеет package-write permission, не выполняет registry login и
push. Будущая artifact payload по workflow contract включает code image archive
`image.tar` и manifest для выбранного SHA. Это не private artifact: read-only
GitHub repository metadata подтвердили `isPrivate:false` для
`PlumpAlbert/payplump-bot`;
GitHub Actions artifact ACL наследует read access репозитория, и
`upload-artifact` не предоставляет per-artifact private ACL. Seven-day retention
ограничивает срок хранения, но не доступ. Сам release workflow, включая
`dry-run`, никогда не исполнялся; ни archive, ни manifest этим workflow ещё не
создавались и не загружались.
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
источником авторизации. Artifact retention не является access control.

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

Intent — локальный файл mode 0600 только для текущего запуска; каждый preflight
использует новый файл. Локальный mode 0600 не создаёт ACL для upload. Будущий
workflow намеренно загружает code image archive `image.tar` и final manifest;
в публичном репозитории они доступны читателям с repo read access. Image
предназначен содержать только код, без environment/credentials; перед upload
владелец должен проверить build context и payload, не полагаясь на локальные
права. Не включать private intent/auth, секреты, backup или другие файлы,
требующие приватной доставки. `upload-artifact` не предоставляет per-artifact
private ACL, а семь дней retention не ограничивают доступ. Для private image
distribution нужна отдельная одобренная private storage/ACL; текущий workflow
такого режима не предоставляет. Manual release workflow, даже dry-run,
никогда не запускался; никакие release image archive/manifest не создавались
или не загружались. Будущий artifact contract `image.tar` + final manifest
сохраняется. Каждая проверка approval читает фактическую историю
текущего run. Local-build validation manifest и финальный post-push manifest
имеют разные пути, чтобы исключить коллизию и отказ после успешного push.
Registry credential передаётся через stdin, используется отдельный приватный
Docker config и удаляется при завершении. Не помещать секреты в repository,
manifest, логи или артефакты; registry credentials и любые private-only файлы
нельзя включать в upload. Будущий dry-run artifact содержит code image archive
(`image.tar`) и manifest, публичные для repo readers. Workflow не запускался,
artifact не создан/не загружен.

## Что не подтверждено

Родительские безопасные проверки кода (Go unit/race/vet, CGO=0 build,
`go mod verify`, tagged PostgreSQL compile-only, gofmt и shell syntax) и
локальные operator/policy/fake-build проверки прошли на подготовительном
изменении. Required CI `40056489006` и post-merge CI `40057042166` прошли на
указанных выше точных revision. Обычный CI выполняет исходные проверки,
включая Docker/smoke и PostgreSQL 17 restore rehearsal, но не строит/не
публикует release artifact и не доказывает фактический запуск release workflow.
PR #38 synthetic restore CI также не является доказательством release pipeline.
Сам manual release workflow/build/runtime не выполнялись; dispatch запрещён,
включая dry-run, в рамках этого этапа. Обычный source CI выполнял сборку и
Docker/smoke проверки, что не подтверждает release artifact. Локально Docker
runtime не запускался. Не выполнялись registry login/push, publication, deploy
или SSH.

Реальные registry size, target VPS architecture/ресурсы, health/signals в фоне,
compatibility rollback, backup и живой цикл остаются непроверенными. Реальные
manifest/source-image данные не измерялись. Owner-provided предыдущий digest не
проверен как рабочий rollback образ. Down migrations отсутствуют; не
предполагать, что предыдущий бинарник совместим с новой схемой.