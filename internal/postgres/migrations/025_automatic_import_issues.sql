-- Durable import diagnostics and notification deduplication. No provider
-- mutation or authority is represented by this table.
CREATE TABLE automatic_import_issues (
    bot_id bigint NOT NULL REFERENCES telegram_bots(bot_id),
    candidate_key text NOT NULL CHECK (candidate_key ~ '^[0-9a-f]{64}$'),
    actor_id bigint CHECK (actor_id > 0),
    reason text NOT NULL CHECK (reason IN ('unsupported_profile','ambiguous_source','source_failure','payment_unverified','unpaid_debt','active_period_expired','source_changed','excluded','invalid_policy','paused_history_not_supported','historical_anchor_limit','admission_conflict')),
    facts_hash text NOT NULL CHECK (facts_hash ~ '^[0-9a-f]{64}$'),
    generation bigint NOT NULL CHECK (generation > 0),
    resolved boolean NOT NULL DEFAULT false,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (bot_id, candidate_key)
);
CREATE INDEX automatic_import_issues_actor ON automatic_import_issues(bot_id,actor_id) WHERE NOT resolved AND actor_id IS NOT NULL;
