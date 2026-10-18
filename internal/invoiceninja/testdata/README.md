# Synthetic Invoice Ninja v5 fixtures

These records mirror the verified API field types and envelope shapes. All record IDs, names, dates, amounts and identity values are invented; these are not captured service responses. Contacts are omitted. Read-only fixtures omit invitation URLs; `billing_*` fixtures use invented HTTPS invitation links on reserved domains.

Money remains JSON numeric text, archives use integer timestamps, status/frequency IDs use strings, and payment relationships use arrays. Pagination links use a reserved `.invalid` host and must never be followed.

`billing_*` write contracts are source-inspected at Invoice Ninja 5.13.47, commit `493c3884e4214028bc5970cb41bafbd9c4d96401`. No live writes have been verified. Draft balance is zero; `mark_sent` returns an array and adds the amount to balance. First-invoice fixtures have no discounts, invoice-level taxes, surcharges, deposits or prior payments. Oversized decimal values are parser/precision stress data, not PHP-calculation golden results.
