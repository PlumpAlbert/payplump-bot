-- Webhooks are wake signals only. No customer payload or secret is retained.
CREATE TABLE payment_wake_state (
    bot_id bigint PRIMARY KEY REFERENCES telegram_bots(bot_id),
    generation bigint NOT NULL DEFAULT 0 CHECK (generation >= 0)
);
CREATE TABLE payment_processed_events (
    bot_id bigint NOT NULL REFERENCES telegram_bots(bot_id),
    body_hash bytea NOT NULL CHECK (octet_length(body_hash) = 32),
    received_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (bot_id, body_hash)
);
CREATE TABLE payment_checks (
    client_id bigint PRIMARY KEY REFERENCES clients(id),
    bot_id bigint NOT NULL REFERENCES telegram_bots(bot_id),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    locked_until timestamptz,
    next_check_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    observed_generation bigint NOT NULL DEFAULT 0 CHECK (observed_generation >= 0),
    lease_generation bigint NOT NULL DEFAULT 0 CHECK (lease_generation >= 0),
    last_error_code integer NOT NULL DEFAULT 0,
    review_code integer NOT NULL DEFAULT 0,
    last_checked_at timestamptz
);
CREATE INDEX payment_checks_due_idx ON payment_checks (bot_id, next_check_at, client_id);
CREATE TABLE payment_confirmations (
    invoice_id text PRIMARY KEY CHECK (invoice_id <> ''),
    client_id bigint NOT NULL REFERENCES clients(id),
    bot_id bigint NOT NULL REFERENCES telegram_bots(bot_id),
    customer_id text NOT NULL CHECK (customer_id <> ''),
    invoice_number text NOT NULL CHECK (char_length(invoice_number) BETWEEN 1 AND 128),
    amount numeric NOT NULL CHECK (amount > 0),
    currency_id text NOT NULL CHECK (currency_id ~ '^[1-9][0-9]{0,9}$'),
    payment_date date NOT NULL,
    next_invoice_date date NOT NULL,
    proof_hash bytea NOT NULL CHECK (octet_length(proof_hash) = 32),
    evidence jsonb NOT NULL CHECK (jsonb_typeof(evidence) = 'object' AND octet_length(evidence::text) <= 65536),
    review_required boolean NOT NULL DEFAULT false,
    review_code integer NOT NULL DEFAULT 0,
    observed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (next_invoice_date > payment_date)
);
CREATE INDEX payment_confirmations_client_idx ON payment_confirmations (client_id, invoice_id);
