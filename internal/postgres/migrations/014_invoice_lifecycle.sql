-- Invoice webhooks are event-specific wake signals only. Their bodies and
-- delivery identities are not retained; the worker reconciles canonical GETs.
CREATE TABLE invoice_lifecycle_wake_state (
    bot_id bigint PRIMARY KEY REFERENCES telegram_bots(bot_id),
    generation bigint NOT NULL DEFAULT 0 CHECK (generation >= 0)
);

CREATE TABLE invoice_lifecycle_checks (
    client_id bigint PRIMARY KEY REFERENCES clients(id),
    bot_id bigint NOT NULL REFERENCES telegram_bots(bot_id),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    locked_until timestamptz,
    next_check_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    observed_generation bigint NOT NULL DEFAULT 0 CHECK (observed_generation >= 0),
    lease_generation bigint NOT NULL DEFAULT 0 CHECK (lease_generation >= 0),
    last_error_code integer NOT NULL DEFAULT 0 CHECK (last_error_code >= 0),
    review_code integer NOT NULL DEFAULT 0 CHECK (review_code >= 0),
    last_checked_at timestamptz
);
CREATE INDEX invoice_lifecycle_checks_due_idx ON invoice_lifecycle_checks (bot_id, next_check_at, client_id)
    WHERE review_code = 0;

CREATE TABLE invoice_lifecycle_invoices (
    invoice_id text PRIMARY KEY CHECK (octet_length(invoice_id) BETWEEN 1 AND 256),
    client_id bigint NOT NULL REFERENCES clients(id),
    bot_id bigint NOT NULL REFERENCES telegram_bots(bot_id),
    customer_id text NOT NULL CHECK (octet_length(customer_id) BETWEEN 1 AND 256),
    recurring_id text NOT NULL CHECK (octet_length(recurring_id) BETWEEN 1 AND 256),
    invoice_number text NOT NULL CHECK (char_length(invoice_number) BETWEEN 1 AND 128),
    status text NOT NULL CHECK (status IN ('sent', 'partial', 'paid')),
    sent_date date NOT NULL,
    amount numeric NOT NULL CHECK (amount > 0 AND amount::text NOT IN ('NaN', 'Infinity')),
    balance numeric NOT NULL CHECK (balance >= 0 AND balance <= amount AND balance::text NOT IN ('NaN', 'Infinity')),
    paid_to_date numeric NOT NULL CHECK (paid_to_date >= 0 AND paid_to_date::text NOT IN ('NaN', 'Infinity')),
    notice_state text NOT NULL CHECK (notice_state IN ('issued', 'settled')),
    observed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (client_id, invoice_id),
    CHECK (balance + paid_to_date = amount),
    CHECK ((status = 'paid' AND notice_state = 'settled' AND balance = 0 AND paid_to_date = amount)
        OR (status = 'sent' AND notice_state = 'issued' AND balance > 0)
        OR (status = 'partial' AND notice_state = 'issued' AND balance > 0 AND paid_to_date > 0))
);
CREATE INDEX invoice_lifecycle_invoices_client_idx ON invoice_lifecycle_invoices (client_id, observed_at, invoice_id);
