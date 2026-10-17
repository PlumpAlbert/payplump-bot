-- Every external registration write has a durable, immutable intent before
-- transmission. An unresolved intent must never be automatically rearmed.
ALTER TABLE subscriptions ADD COLUMN first_invoice_id text UNIQUE;
CREATE UNIQUE INDEX operations_one_registration_per_client ON operations (client_id)
    WHERE kind = 'register_client';

CREATE TABLE registration_flows (
    operation_id bigint PRIMARY KEY REFERENCES operations(id),
    bot_id bigint NOT NULL REFERENCES telegram_bots(bot_id),
    token text NOT NULL UNIQUE CHECK (char_length(token) BETWEEN 20 AND 128),
    review_code integer NOT NULL DEFAULT 0 CHECK (review_code >= 0),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE operation_steps (
    operation_id bigint NOT NULL REFERENCES registration_flows(operation_id),
    step text NOT NULL CHECK (step IN ('customer_create', 'invoice_create', 'invoice_issue')),
    request jsonb NOT NULL CHECK (jsonb_typeof(request) = 'object'),
    external_id text CHECK (external_id <> ''),
    created_at timestamptz NOT NULL DEFAULT now(),
    confirmed_at timestamptz,
    PRIMARY KEY (operation_id, step),
    CHECK ((external_id IS NOT NULL) = (confirmed_at IS NOT NULL))
);
