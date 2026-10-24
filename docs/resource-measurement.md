# Безопасные измерения ресурсов

## Что измеряется

Stage 14 harness предназначен для bounded синтетических Go unit-тестов. Он не
запускает приложение, не подключается к PostgreSQL, Docker, провайдерам или
внешней нагрузке. Полученные CI-результаты описывают только runner и Go toolchain
при неконтролируемом состоянии cache: предыдущие CI-команды могут прогреть
часть toolchain. Это не cold-start benchmark, не benchmark приложения, оценка
RPS, production/VPS показатель или подтверждение лимита контейнера. Профили и harness tests
подготовлены; независимый review `1dd52647-2607-400` завершился финальным
нулём findings после исправления `--prof` abbreviation и fixture SHA/marker.
Первый required CI PR #40 завершил успешно все три required jobs; optional
validation и Neon integration jobs были skipped. Четыре JSON artifact проверены;
подробности и границы измерений приведены ниже. Публичность
подтверждена read-only GitHub repository metadata (`isPrivate:false`).
Будущие JSON artifacts должны содержать только safe public metadata: source
revision, runner/toolchain, test counts и OS resource data; никаких private env,
локальных путей или raw Go output.

Четыре дополнительных профиля намеренно закрыты и unit-only:

| Профиль | Ограниченный сценарий |
| --- | --- |
| `unit-smoke` | Существующий краткий набор: три package, import/plan и command checks; только untagged package unit tests, без запуска PostgreSQL server или SQL. |
| `worker-poll` | `./internal/broadcast`, `TestWorkerPollsWithinConcurrencyAndJoinsCancellation`. |
| `invoice-observation` | `./internal/payments`, проверенные повторные чтения и negative/transient read cases. |
| `restart-fences` | `./internal/lifecycle`, known-confirm failure и reclaimed unknown без replay; unit mocks. |
| `dispatch-retries` | `./internal/outbox`, ordinary outcomes, 429/shutdown/503 classification и bounded parallelism; unit mocks. |

Профиль `unit-smoke` сохраняет legacy-набор из трёх пакетов; всего закрытые
разрешённые имена:
`unit-smoke`, `worker-poll`, `invoice-observation`, `restart-fences`,
`dispatch-retries`. Любые tagged PostgreSQL integration tests исключены.
Compile-only integration command, если потребуется отдельно, должен быть
явно ограничен `-run '^$'`; этот runbook не предлагает запускать integration
runtime.

## Запуск harness

Путь `scripts/measure-safe.py` dry-run по умолчанию: сначала можно посмотреть
план, не выполняя Go workload. Профильные команды для текущего изменения:

```sh
python3 scripts/measure-safe.py
python3 scripts/measure-safe.py --run --profile unit-smoke
python3 scripts/measure-safe.py --run --profile worker-poll
python3 scripts/measure-safe.py --run --profile invoice-observation
python3 scripts/measure-safe.py --run --profile restart-fences
python3 scripts/measure-safe.py --run --profile dispatch-retries
```

Профильный интерфейс относится к текущему Stage 14 изменению; до подтверждения
review/CI не считать его доставленным в main. Не передавайте произвольные
команды или тестовые patterns за пределами разрешённых профилей. Локальный unit workload допустим; ручной
integration runtime, запрос к рабочей БД, Docker и provider API не допускаются.

Harness должен очистить дочернее окружение от provider/database credentials,
Go overrides (`GOFLAGS`, `GOENV`, `GOWORK`), задать offline proxy/sumdb и
отключить автоматическую загрузку toolchain. Источники измерений должны
включать полное время workload и затраты процессов `go test`/compiler/linker,
а не только тестовый бинарник. Снимать `RUSAGE_CHILDREN` до чтения Git/version metadata, чтобы дочерние
команды не загрязняли high-water.
JSON должен отражать реальные matched/pass counts; сценарий с нулём тестов
не считается успешным измерением.

В JSON `source_commit` — фактический `git HEAD` checkout (например, merge SHA
в CI); optional `source_head_sha` — canonical исходный PR head SHA. Это разные
значения, не подменять одно другим. Версию Go получать отдельным read-only
запросом после снятия `RUSAGE_CHILDREN`, чтобы metadata-команда не загрязняла
измерение; в dry-run `go_version` равен `null` с причиной not measured.
Указывайте runner OS/architecture и эти два SHA-поля отдельно. CPU totals и wall
относятся к child tooling/test workload. `RUSAGE_CHILDREN.ru_maxrss` —
OS-reported child high-water; на Linux это largest child, а не одновременный
aggregate RSS всего compiler/test process tree. Поле `child_max_rss_bytes`
ограничено значением child high-water: это не aggregate tree peak и не RSS
приложения. Не суммируйте child RSS между процессами или профилями и не
выбирайте по этим цифрам `GOMEMLIMIT`. Cgroup `memory.current`, если доступен,
является одной выборкой после workload, не peak, не памятью приложения и не
изолированной статистикой runner. Container memory для legacy runner harness
должна быть `null` с причиной `not measured`. Размер архива указывается
только для явно переданного существующего regular nonsymlink file; registry
compressed/download size не вычисляется и не выводится из него.

## Отчёт и ограничения

### Фактический CI artifact

Первый required CI PR #40 на code HEAD
`7d247b20da2ff0408a22382115ccad9918fc3d1c` завершился успешно: run
[40059203387](https://github.com/PlumpAlbert/payplump-bot/actions/runs/40059203387),
все три required jobs `fast-checks`, `postgres-integration`, `test` success.
Workflow checkout SHA в JSON — `source_commit=e4080c7b02984b9fb1e279724bcc6392d3e1fe34`,
а `source_head_sha=7d247b20da2ff0408a22382115ccad9918fc3d1c` — исходный PR head;
это разные commits. Runner Linux/x86_64, Go 1.27.2. GitHub artifact
`synthetic-unit-resources-40059203387-1` (ID `12545794446`, 4,445 bytes) был
загружен и проверен: четыре flat JSON, `expired=false`, retention 7 дней.
Все содержимое — public-safe metadata без raw output, private env или локальных
путей. Репозиторий публичный; artifact access наследует read access репозитория,
`upload-artifact` не имеет per-artifact private ACL, а retention не ограничивает
доступ.

| Profile | Selected/pass | Wall (s) | Child CPU total (s) | `child_max_rss_bytes` |
| --- | ---: | ---: | ---: | ---: |
| `worker-poll` | 1/1 | 5.537 | 18.659 | 164671488 |
| `invoice-observation` | 2/2 | 0.638 | 1.517 | 156200960 |
| `restart-fences` | 2/2 | 0.931 | 2.407 | 168546304 |
| `dispatch-retries` | 4/4 | 0.423 | 0.662 | 151429120 |

Все четыре workloads завершились `executed=true`, `exit_code=0`, failures `0`.
Для всех `container_memory_bytes`, `compressed_image_bytes` и
`image_file_bytes` равны `null`: эти величины не измерялись.

Это реальные bounded Go unit profile runs на CI runner, не фальшивые fixtures,
но и не приложение, production load, VPS или capacity benchmark. Cache scope
неконтролируемый и предыдущие CI команды могли прогреть toolchain; это не
cold-start измерение. CPU sums покрывают Go/compiler/linker/test descendants,
поэтому могут превышать wall time при параллельной работе. `ru_maxrss` здесь —
OS-reported child high-water (на Linux largest child), не concurrent aggregate
process-tree peak и не app RSS. Cgroup value, если доступно, является одним
sample после workload, не peak. Не суммировать RSS по профилям и не выбирать по
этим метрикам лимиты или `GOMEMLIMIT`. VPS и production measurements, лимиты и
capacity targets не определены.

Дополнительные проверки на исходниках: 22 Go packages unit/race/vet, CGO0
build, `go mod verify`, tagged PostgreSQL compile-only, gofmt и YAML parse прошли.
Helper tests: 38 total, 37 passed, 1 existing Docker-dependent operator skip
(measurement 10, operator 14, policy 7, fake-build 7). Darwin arm64/Go 1.27.2:
четыре локальных профиля 1/1, 2/2, 2/2, 4/4; legacy importplan 15/15. Один
первый локальный `unit-smoke` run вернул 155/156 и корректно завершился nonzero;
сбой не воспроизвёлся, после direct Go JSON diagnosis ручной retry прошёл
156/156. Не выдавать повтор за первый успешный запуск и не выводить из этого
дефект приложения.

Реальные оценки требуют отдельного owner-controlled измерения на целевой
машине. Заполнить worksheet фактическими данными, не догадками:

| Поле | Owner result |
| --- | --- |
| VPS OS/architecture; RAM, CPU, SSD | Не предоставлено |
| Число клиентов; платежи за период; размер batch; worker parallelism | Не предоставлено |
| DB pool и provider API timeouts | Не предоставлено |
| Текущий и предыдущий image: disk bytes; registry compressed bytes | Не предоставлено |
| Сценарий/длительность; container peak RSS, CPU и wall time; `GOMEMLIMIT` | Не измерено |
| Log rotation и контролируемые backup/rollback approvals | Не проверено |

До будущего измерения владелец выбирает scope и лимиты, подтверждает backup и
rollback, и организует данные/окружение. Нужно раздельно измерить idle,
business-hour invoice import, restart recovery, broadcast ordinary failures,
known retries и permanent/unknown barrier. Не сбрасывать барьер и никогда не
повторять неизвестный внешний POST. Фактические client workload, VPS,
container peak, limits и `GOMEMLIMIT` неизвестны; sample Compose values не
измерены. Не запускать SSH, рабочие provider GET, SQL/DB, Docker или внешнюю
нагрузку в рамках этой инструкции.
