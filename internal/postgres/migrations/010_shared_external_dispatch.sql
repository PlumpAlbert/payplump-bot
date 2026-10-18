-- Keep ONE non-expiring cross-provider guard and its monotonic epoch. Its
-- original owner fields remain compatible with activation readers/finalizers.
-- Only the FK anchor is generalized so later subscription effects share it.
CREATE TABLE external_write_intents (
    operation_id bigint NOT NULL REFERENCES operations(id),
    step text NOT NULL CHECK (octet_length(step) BETWEEN 1 AND 64),
    origin text NOT NULL CHECK (origin IN ('activation', 'lifecycle')),
    request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
    dispatch_attempt integer NOT NULL CHECK (dispatch_attempt > 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (operation_id, step)
);

-- Every old step, including an unresolved dispatch, retains a permanent anchor.
-- Hash canonical JSONB bytes, not presentation/number spellings from a caller.
INSERT INTO external_write_intents (operation_id,step,origin,request_hash,dispatch_attempt)
    SELECT operation_id,step,'activation',sha256(convert_to(request::text,'UTF8')),dispatch_attempt
    FROM activation_steps;

ALTER TABLE activation_write_guard
    DROP CONSTRAINT activation_write_guard_operation_id_step_fkey;
ALTER TABLE activation_write_guard ADD CONSTRAINT activation_write_guard_intent_fkey
    FOREIGN KEY (operation_id,step) REFERENCES external_write_intents(operation_id,step);

CREATE FUNCTION external_write_intent_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'external write intent is immutable' USING ERRCODE='23514';
END;
$$;
CREATE TRIGGER external_write_intent_immutable BEFORE UPDATE OR DELETE ON external_write_intents
    FOR EACH ROW EXECUTE FUNCTION external_write_intent_immutable();

CREATE FUNCTION anchor_activation_write_intent() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS(SELECT 1 FROM operations WHERE id=NEW.operation_id AND kind='activate_first_subscription') THEN
        RAISE EXCEPTION 'activation intent owner mismatch' USING ERRCODE='23514';
    END IF;
    -- Strict insert: deleting/reinserting a leaf step must NEVER rearm an old
    -- transmission, even if its prior guard was released after known success.
    INSERT INTO external_write_intents(operation_id,step,origin,request_hash,dispatch_attempt)
        VALUES(NEW.operation_id,NEW.step,'activation',sha256(convert_to(NEW.request::text,'UTF8')),NEW.dispatch_attempt);
    RETURN NEW;
END;
$$;
CREATE TRIGGER anchor_activation_write_intent AFTER INSERT ON activation_steps
    FOR EACH ROW EXECUTE FUNCTION anchor_activation_write_intent();
