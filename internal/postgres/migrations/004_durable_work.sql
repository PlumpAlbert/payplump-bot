-- Attempts are monotonic lease generations: a reclaimed job fences its former
-- owner without keeping a transaction open while an external API is called.
ALTER TABLE operations ADD COLUMN last_error_code integer NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX operations_one_running_per_client ON operations (client_id)
    WHERE status = 'running';
CREATE INDEX operations_unfinished_client_idx ON operations (client_id, id)
    WHERE status IN ('pending', 'running');
CREATE INDEX operations_expired_idx ON operations (locked_until, id)
    WHERE status = 'running';

-- A Telegram rate limit survives process restarts and applies to this bot only.
ALTER TABLE telegram_bots ADD COLUMN outbox_not_before timestamptz NOT NULL DEFAULT '-infinity';

CREATE TABLE outbox (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    bot_id bigint NOT NULL REFERENCES telegram_bots(bot_id),
    chat_id bigint NOT NULL CHECK (chat_id > 0),
    message_key text NOT NULL CHECK (message_key <> '' AND octet_length(message_key) <= 256),
    body text NOT NULL CHECK (char_length(body) BETWEEN 1 AND 4096),
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'running', 'sent', 'failed')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    locked_until timestamptz,
    last_error_code integer NOT NULL DEFAULT 0,
    telegram_message_id bigint CHECK (telegram_message_id > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    UNIQUE (bot_id, message_key),
    CHECK ((status = 'running') = (locked_until IS NOT NULL)),
    CHECK ((status = 'sent') = (telegram_message_id IS NOT NULL)),
    CHECK ((status IN ('sent', 'failed')) = (finished_at IS NOT NULL))
);

CREATE INDEX outbox_due_idx ON outbox (bot_id, next_attempt_at, id)
    WHERE status IN ('pending', 'running');
CREATE INDEX outbox_unfinished_chat_idx ON outbox (bot_id, chat_id, id)
    WHERE status IN ('pending', 'running');
CREATE UNIQUE INDEX outbox_one_running_per_chat ON outbox (bot_id, chat_id)
    WHERE status = 'running';
