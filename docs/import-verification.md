# Legacy import verification (11a.2 core)

`internal/importverify` is a GET-only reconciliation report. It does not admit
records, grant access, create database rows, send messages, or authorize a later
write. A future admission stage must repeat fresh provider reads and independently
compare source and policy facts; neither a report nor its private evidence is a
capability.

The verifier requires an explicit bot ID and pilot/exclusion sets, pinned version
claims (Invoice Ninja 5.13.47 and S-UI 1.6.4), a trusted lowercase source-scope
hash (normalized bases, token fingerprints and versions, never raw tokens), native
currency ID and ISO code, product key, public subscription base, expected inbound
IDs, candidate and inventory bounds, retained page/historical-anchor settings and
evidence TTL. The active path enforces the returned inventory cap; actual paging
is bounded by each reader's constructor settings. The historical-anchor setting
is reserved and not used by this active-only slice. Version claims are adapter
checks, not build attestation. The ISO code is an operator acknowledgement: it
is not inferred from Invoice Ninja. The service currently verifies the supported
active/paid profile only. Exclusions win over pilot membership before I/O. Paused
accounts receive `paused_history_not_supported`; historical-debt, unsupported
legacy recurring and unverifiable profiles remain manual. Active verification
requires exactly one live recurring, complete bounded invoices with zero unpaid
debts, exact native-currency fully allocated cash proof, and matching S-UI
identity. It never repairs or rewrites legacy data, assumes a bot provenance
marker, or treats manifest proof booleans as provider evidence.

The report contains counts, copied plan reason codes, the plan hash, and a hash
of canonical explicit private evidence (not its redacted generic JSON encoding).
The private evidence freezes the actual recurring record/template and hash,
invoice IDs/watermark/current paid anchor, S-UI name/binding/config fingerprint,
public-base hash and source-facts hash. Generic JSON/log formatting of evidence is
redacted; `PrivateJSON` is explicitly sensitive. Never expose its output, store it without access controls,
or interpret it as a grant. Raw subscription URLs are not requested. S-UI
settings must match the explicitly configured public base; the personal
subscription URL is never fetched. Credential fingerprints are compared to the
manifest's explicit hash claim, not treated as authorization.

Current limits: the service accepts only candidates selected by the existing
inert import plan, requires a bounded configured pilot, and does not implement
CLI/config delivery, database persistence, managed admission, or paused-account
historical-anchor proof. Unit tests cover policy, redaction, exclusion/digest
preflight and an active paid-account bridge through real read adapters against
synthetic loopback HTTP. Adversarial scenarios use synthetic sources only;
installed providers and live systems were not contacted.
