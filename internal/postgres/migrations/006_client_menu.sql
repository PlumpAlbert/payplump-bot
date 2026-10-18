-- Keyboard intent is part of the immutable outbox message, not a delivery flag.
ALTER TABLE outbox ADD COLUMN client_menu boolean NOT NULL DEFAULT false;
