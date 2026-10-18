-- First-payment activation uses the general per-client FIFO, but only its
-- bot-scoped claimant may dispatch it. Completed flows are never reseeded.
CREATE UNIQUE INDEX operations_one_activation_per_client ON operations (client_id)
    WHERE kind = 'activate_first_subscription';

CREATE TABLE activation_flows (
    operation_id bigint PRIMARY KEY REFERENCES operations(id),
    bot_id bigint NOT NULL REFERENCES telegram_bots(bot_id),
    token text NOT NULL UNIQUE CHECK (token ~ '^[A-Za-z0-9_-]{20,128}$'),
    invoice_id text NOT NULL UNIQUE REFERENCES payment_confirmations(invoice_id),
    proof_hash bytea NOT NULL CHECK (octet_length(proof_hash) = 32),
    policy_hash bytea NOT NULL CHECK (octet_length(policy_hash) = 32),
    review_code integer NOT NULL DEFAULT 0 CHECK (review_code >= 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX activation_flows_bot_idx ON activation_flows (bot_id, operation_id);

CREATE TABLE activation_steps (
    operation_id bigint NOT NULL REFERENCES activation_flows(operation_id),
    step text NOT NULL CHECK (step IN ('sui_create', 'recurring_create', 'sui_enable', 'recurring_start')),
    request jsonb NOT NULL CHECK (jsonb_typeof(request) = 'object' AND octet_length(request::text) <= 65536),
    dispatch_attempt integer NOT NULL CHECK (dispatch_attempt > 0),
    external_id text CHECK (octet_length(external_id) BETWEEN 1 AND 256),
    -- Only validated original recurring responses supply these facts. Neither a
    -- later lookup nor a locally calculated total may fill an unknown result.
    recurring_amount numeric CHECK (recurring_amount > 0 AND recurring_amount::text NOT IN ('NaN', 'Infinity')),
    recurring_datetime text CHECK (octet_length(recurring_datetime) BETWEEN 20 AND 30
        AND recurring_datetime ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{0,8}[1-9])?Z$'),
    confirmed_at timestamptz,
    PRIMARY KEY (operation_id, step),
    CHECK ((external_id IS NOT NULL) = (confirmed_at IS NOT NULL)),
    CHECK ((recurring_amount IS NOT NULL) = (step IN ('recurring_create', 'recurring_start') AND external_id IS NOT NULL)),
    CHECK ((recurring_datetime IS NOT NULL) = (step IN ('recurring_create', 'recurring_start') AND external_id IS NOT NULL))
);

-- This barrier has deliberately NO deadline or automatic takeover. Even a
-- paused original sender can still transmit after its operation lease expires.
CREATE TABLE activation_write_guard (
    id integer PRIMARY KEY CHECK (id = 1),
    epoch bigint NOT NULL DEFAULT 0 CHECK (epoch >= 0),
    operation_id bigint,
    step text,
    CHECK ((operation_id IS NULL) = (step IS NULL)),
    FOREIGN KEY (operation_id, step) REFERENCES activation_steps(operation_id, step)
);
INSERT INTO activation_write_guard (id) VALUES (1);

-- Names and credential hashes come exclusively from the frozen create intent,
-- never from display-name searches or pre-existing panel accounts.
CREATE TABLE activation_sui_bindings (
    client_id bigint PRIMARY KEY REFERENCES clients(id),
    bot_id bigint NOT NULL REFERENCES telegram_bots(bot_id),
    sui_id bigint NOT NULL UNIQUE CHECK (sui_id > 0),
    name text NOT NULL UNIQUE CHECK (name ~ '^pp-[0-9a-f]{40}$'),
    credentials_hash bytea NOT NULL CHECK (octet_length(credentials_hash) = 32),
    operation_id bigint NOT NULL UNIQUE REFERENCES activation_flows(operation_id),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
