-- Preserve processed update IDs when switching from getUpdates to webhooks.
-- HTTP acknowledgement replaces the polling checkpoint.
ALTER TABLE telegram_polling_state RENAME TO telegram_bots;
ALTER TABLE telegram_bots DROP COLUMN next_offset, DROP COLUMN last_update_at;
