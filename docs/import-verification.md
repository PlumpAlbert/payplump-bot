# Legacy import verification (11a.2 core; advanced operator CLI only)

Обычный запуск сам выполняет строгий ограниченный auto-import подходящих
клиентов в собственную БД; он не создаёт клиентов у провайдеров. Администраторы
исключены только из legacy-import, но могут стать новым OWN-клиентом. Этот
документ описывает отдельные ручные `import-verify`/`import-apply` инструменты,
не обычный startup. Обычный auto-import принимает только полностью проверенных
кандидатов и пишет результат только в БД, без создания объектов у провайдеров.
О неподходящем кандидате бот пишет `ERROR` в лог и уведомляет администратора
в приватном чате; повторяющиеся сообщения сохраняют дедупликацию. Проверяется только complete source inventory в пределах bounded
pagination/cap; это не unlimited inventory и не live API health check. Старый
legacy-origin policy hash может быть несовместим с normal profile и требует
отдельной проверенной миграции; автоматического переписывания нет. Подробнее:
[конфигурация обычного запуска](configuration.md).

`internal/importverify` is the legacy GET-only reconciliation report. It does not
admit records, grant access, create database rows, send messages, or authorize a
later write. The report's original contract anticipated a future admission stage;
normal auto-import is now a separate, current implementation that repeats fresh
provider reads and independently checks source/policy facts. Neither report nor
private evidence is an admission capability.

The operator verifier requires an explicit bot ID and pilot/exclusion sets, a
trusted lowercase source-scope hash (normalized bases and token fingerprints,
never raw tokens), native
currency ID and ISO code, product key, public subscription base, expected inbound
IDs, candidate and inventory bounds, retained page/historical-anchor settings and
evidence TTL. The active path enforces the returned inventory cap; actual paging
is bounded by each reader's constructor settings. `MaxHistoricalAnchors` is
validated at 1..20 and enforced without truncation. Historical release labels
remain only in the stable serialized provenance contract, not as installed-version
requirements or build attestation. The operator CLI still takes an explicit ISO
code; normal startup can discover the currency from Ninja. Exclusions win over pilot membership before
I/O. Active verification
requires exactly one live recurring, complete bounded invoices with zero unpaid
debts, exact native-currency fully allocated cash proof, and matching S-UI
identity. Its current-invoice rule is deliberately date-based and conservative:
the claimed invoice must be the unique latest-dated live issued (sent, partial, or
paid) invoice for that recurring. Older paid history is allowed; newer or same-date
issued records and unknown relevant statuses require manual review. This is not a
general Invoice Ninja current-cycle authority; ambiguous same-date invoices and
manual schedule edits remain unsupported. A narrow paused profile requires the
unique latest-issued current invoice to be the sole fully unpaid sent debt, and
one distinct older fully allocated native-cash paid invoice whose amount and
payment-period dates match the explicit claim. The current debt is not an anchor;
partial/multiple debts, ambiguous or excessive historical candidates, unsupported
payment facts, and unverifiable sources remain manual. Paused recurring status
must be 3 and the matching S-UI record disabled. Its actual schedule is frozen
and may be past; a paid period ending in the future is unsupported. No source is
resumed, rescheduled, repaired, or rewritten. The report never assumes a bot
provenance marker or treats manifest proof booleans as provider evidence.

The report contains counts, copied plan reason codes, the plan hash, and a hash
of canonical explicit private evidence (not its redacted generic JSON encoding).
The private evidence freezes the actual recurring record/template and hash,
invoice IDs/watermark/current invoice lifecycle/status and distinct paid anchor,
S-UI name/binding/config fingerprint,
public-base hash and source-facts hash. Generic JSON/log formatting of evidence is
redacted; `PrivateJSON` is explicitly sensitive. Never expose its output, store it without access controls,
or interpret it as a grant. Raw subscription URLs are not requested. S-UI
settings must match the explicitly configured public base when an override is
present; an empty override uses the configured base without fabricating source
settings. The personal subscription URL is never fetched. Credential fingerprints are compared to the
manifest's explicit hash claim, not treated as authorization.

## Operator CLI (11a.3)

Run `payplump import-verify --file PRIVATE_PATH --expected-hash PLAN_HASH --read-only`.
The required `--read-only` switch is a confirmation, not an effectful mode;
`--apply`, unknown flags, and positional arguments are rejected. The command
validates and hashes the mode-0600 manifest before reading policy environment
variables or constructing HTTP clients. It does not open PostgreSQL, migrate,
start workers, configure Telegram, or call `getMe`. All provider capabilities are
GET-only, and S-UI personal subscription URLs are never fetched.

Policy comes from explicit operator-only `IMPORT_VERIFY_*` keys plus the legacy
source settings documented in `.env.example`. `IMPORT_VERIFY_BOT_ID` is a
canonical positive ID; pilot IDs are required, canonical, unique, and limited to
1..20. Optional exclusions are unioned with registration exclusions and Telegram
admins; exclusions (including the bot itself) win over pilot membership. The
currency code is an explicit owner acknowledgement for the native currency ID,
never an inferred API mapping. The product key is an explicit selected legacy
key: source verification checks the actual recurring line item and freezes its
observed legacy price/template, not the current catalog price. This SKU
acknowledgement is not financial proof. Future admission must re-read sources and
revalidate both source and policy bindings.

The report is safe JSON counts, reasons, plan hash, and evidence hash only. An
optional `--evidence-file` creates a new, exclusive mode-0600 private file; it
contains sensitive customer/provider facts and must be protected accordingly.
It never overwrites a file or follows a symlink. Evidence is an audit snapshot,
not a grant or reusable capability. Limits are bounded (20 records, 1000
inventory records, 20 historical anchors, 15-minute evidence TTL, three-minute
overall context). Use synthetic settings for tests only. The operator CLI is
implemented as a legacy advanced tool, including its real-reader synthetic HTTP
CLI workflow bridge. The following test and merge notes are historical evidence,
not current readiness or startup instructions. PR #24 merged as `9fe0c2aaec847a4fe2948f09d755a346318e5c4c` from exact code HEAD
`b24bf849ca4e3f6589c82ef2bebb885b5419702a`; required CI `39989051793` and
post-merge CI `39989900953` passed, including PG17/smoke/Docker. No live
verification has been performed. Current auto-import behavior and legacy-origin
migration limits are described above and in [configuration.md](configuration.md).
