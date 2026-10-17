CREATE TABLE clients (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    telegram_id bigint NOT NULL UNIQUE CHECK (telegram_id > 0),
    chat_id bigint NOT NULL CHECK (chat_id > 0),
    display_name text NOT NULL,
    invoice_ninja_id text UNIQUE,
    sui_id text UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE subscriptions (
    client_id bigint PRIMARY KEY REFERENCES clients(id),
    state text NOT NULL DEFAULT 'awaiting_payment'
        CHECK (state IN ('awaiting_payment', 'active', 'overdue')),
    desired_access boolean NOT NULL DEFAULT false,
    current_invoice_id text UNIQUE,
    recurring_id text UNIQUE,
    payment_date date,
    next_invoice_date date
);

-- External side effects are recorded in the same transaction as registration.
-- A future worker must reconcile uncertain API outcomes before retrying them.
CREATE TABLE operations (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    client_id bigint NOT NULL REFERENCES clients(id),
    operation_key text NOT NULL UNIQUE,
    kind text NOT NULL CHECK (kind <> ''),
    status text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'running', 'completed')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    locked_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz
);

CREATE INDEX operations_pending_idx ON operations (next_attempt_at, id)
    WHERE status = 'pending';
