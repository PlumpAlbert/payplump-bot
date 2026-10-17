CREATE TABLE telegram_polling_state (
    bot_id bigint PRIMARY KEY CHECK (bot_id > 0),
    next_offset bigint NOT NULL DEFAULT 0 CHECK (next_offset >= 0),
    last_update_at timestamptz NOT NULL DEFAULT now()
);

-- Keep only deduplication metadata, not message text or bot credentials.
CREATE TABLE telegram_processed_updates (
    bot_id bigint NOT NULL REFERENCES telegram_polling_state(bot_id),
    update_id bigint NOT NULL CHECK (update_id >= 0),
    processed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (bot_id, update_id)
);
