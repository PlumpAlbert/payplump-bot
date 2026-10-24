# Безопасные измерения ресурсов

## Что измеряется

Stage 14 harness предназначен для bounded синтетических Go unit-тестов. Он не
запускает приложение, не подключается к PostgreSQL, Docker, провайдерам или
внешней нагрузке. CI-результаты, если будут опубликованы после реализации
закрытых профилей, описывают только runner и Go toolchain при неконтролируемом
состоянии cache: предыдущие CI-команды могут прогреть часть toolchain. Это не
cold-start benchmark, не benchmark приложения, оценка RPS, production/VPS
показатель или
подтверждение лимита контейнера. Профили, тест и CI artifact wiring есть
в рабочем изменении. Независимый review `1dd52647-2607-400` завершён:
исходный P3 на abbreviation `--prof` исправлен через `allow_abbrev=False`,
regression test добавлен; проверка fixture SHA/marker, ранее маскировавшая
исправление, также исправлена. Финальный recheck завершён с нулём замечаний.
Exact CI PR #40 и фактический CI artifact ещё не получены. Публичность
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

Ожидаемые четыре JSON-файла CI должны иметь только public-safe содержание.
Репозиторий публичный, и доступ к GitHub Actions artifacts наследует read access
репозитория: `upload-artifact` не предоставляет per-artifact private ACL.
Retention в 7 дней ограничивает срок хранения, но не доступ. Поэтому загружать
можно только указанные выше метаданные, не intent, архивы, локальные пути,
environment или raw output. Runtime artifact ещё не получен.

Parent checks passed: 22 Go packages for unit/race/vet, CGO0 build,
`go mod verify`, tagged PostgreSQL compile-only, gofmt and YAML parse.
Measurement/helper tests: 38 total, 37 passed, 1 existing Docker-dependent
operator skip (measurement 10, operator 14, policy 7, fake-build 7). Local run:
Darwin arm64, Go 1.27.2; four added profiles passed `1/1`, `2/2`, `2/2`, `4/4`;
legacy importplan `15/15`. Default `unit-smoke` had one unreproduced transient
155/156 run, correctly exited nonzero; after direct Go JSON diagnosis, manual
retry passed `156/156`. Do not describe this as first-try success or a production
defect. `RLIMIT_FSIZE` and EOF/wall-timeout fixes are done. Review
`1dd52647-2607-400` final recheck reports zero findings after fixing the P3
`--prof` abbreviation and the test fixture SHA/marker validation. Required CI
for PR #40 and actual CI artifact remain pending. До публикации
артефакта проверяйте точный source revision, попытку CI, имена профилей и
actual pass counts. Не подставляйте примерные числа и не превращайте runner
метрики в memory/CPU budget или основание для выбора `GOMEMLIMIT`. Никаких измеренных toolchain чисел здесь пока нет.

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
