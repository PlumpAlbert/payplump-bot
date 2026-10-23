-- Paid renewal is a distinct, proof-bound operation. All provider writes are
-- anchored to the existing external_write_intents and activation_write_guard.
CREATE TABLE invoice_renewal_actions (
    operation_id bigint PRIMARY KEY REFERENCES operations(id),
    client_id bigint NOT NULL REFERENCES clients(id),
    bot_id bigint NOT NULL REFERENCES telegram_bots(bot_id),
    invoice_id text NOT NULL,
    outcome text NOT NULL DEFAULT 'pending' CHECK (outcome IN ('pending', 'completed', 'review')),
    review_code integer NOT NULL DEFAULT 0 CHECK (review_code >= 0),
    plan_frozen_at timestamptz,
    target_date date,
    requires_pause boolean,
    requires_schedule boolean,
    requires_enable boolean,
    requires_resume boolean,
    baseline jsonb,
    recurring_amount numeric,
    recurring_next_send_datetime timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    finished_at timestamptz,
    UNIQUE (client_id, invoice_id),
    FOREIGN KEY (client_id, invoice_id) REFERENCES invoice_lifecycle_invoices(client_id, invoice_id),
    CHECK ((plan_frozen_at IS NULL AND target_date IS NULL AND requires_pause IS NULL AND requires_schedule IS NULL AND requires_enable IS NULL AND requires_resume IS NULL AND baseline IS NULL)
        OR (plan_frozen_at IS NOT NULL AND target_date IS NOT NULL AND requires_pause IS NOT NULL AND requires_schedule IS NOT NULL AND requires_enable IS NOT NULL AND requires_resume IS NOT NULL AND baseline IS NOT NULL AND jsonb_typeof(baseline) = 'object' AND octet_length(baseline::text) <= 65536)),
    CHECK (recurring_amount IS NULL OR (recurring_amount > 0 AND recurring_amount::text NOT IN ('NaN', 'Infinity'))),
    CHECK ((outcome IN ('completed', 'review')) = (finished_at IS NOT NULL)),
    CHECK ((outcome = 'review') = (review_code <> 0)),
    CHECK (outcome <> 'completed' OR (recurring_amount IS NOT NULL AND recurring_amount > 0 AND recurring_next_send_datetime IS NOT NULL))
);
CREATE INDEX invoice_renewal_actions_pending_idx ON invoice_renewal_actions (bot_id, created_at, operation_id)
    WHERE outcome = 'pending';

CREATE FUNCTION invoice_renewal_action_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN RAISE EXCEPTION 'invoice renewal action is immutable' USING ERRCODE='23514'; END IF;
    IF NEW.operation_id <> OLD.operation_id OR NEW.client_id <> OLD.client_id OR NEW.bot_id <> OLD.bot_id OR NEW.invoice_id <> OLD.invoice_id THEN
        RAISE EXCEPTION 'invoice renewal identity is immutable' USING ERRCODE='23514';
    END IF;
    IF OLD.outcome <> 'pending' THEN RAISE EXCEPTION 'invoice renewal action cannot be rearmed' USING ERRCODE='23514'; END IF;
    IF OLD.plan_frozen_at IS NOT NULL AND (NEW.plan_frozen_at IS DISTINCT FROM OLD.plan_frozen_at OR NEW.target_date IS DISTINCT FROM OLD.target_date
       OR NEW.requires_pause IS DISTINCT FROM OLD.requires_pause OR NEW.requires_schedule IS DISTINCT FROM OLD.requires_schedule
       OR NEW.requires_enable IS DISTINCT FROM OLD.requires_enable OR NEW.requires_resume IS DISTINCT FROM OLD.requires_resume
       OR NEW.baseline IS DISTINCT FROM OLD.baseline) THEN
        RAISE EXCEPTION 'invoice renewal plan is immutable' USING ERRCODE='23514';
    END IF;
    IF OLD.recurring_amount IS NOT NULL AND NEW.recurring_amount IS DISTINCT FROM OLD.recurring_amount THEN RAISE EXCEPTION 'invoice renewal receipt is immutable' USING ERRCODE='23514'; END IF;
    IF OLD.recurring_next_send_datetime IS NOT NULL AND NEW.recurring_next_send_datetime IS DISTINCT FROM OLD.recurring_next_send_datetime THEN RAISE EXCEPTION 'invoice renewal receipt is immutable' USING ERRCODE='23514'; END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER invoice_renewal_action_immutable BEFORE UPDATE OR DELETE ON invoice_renewal_actions
    FOR EACH ROW EXECUTE FUNCTION invoice_renewal_action_immutable();

CREATE TABLE invoice_renewal_steps (
    operation_id bigint NOT NULL REFERENCES invoice_renewal_actions(operation_id),
    step text NOT NULL CHECK (step IN ('pause_recurring', 'schedule_recurring', 'enable_sui', 'resume_recurring')),
    request jsonb NOT NULL CHECK (jsonb_typeof(request) = 'object' AND octet_length(request::text) <= 4194304),
    dispatch_attempt integer NOT NULL CHECK (dispatch_attempt > 0),
    external_id text CHECK (external_id IS NULL OR octet_length(external_id) BETWEEN 1 AND 256),
    receipt jsonb CHECK (receipt IS NULL OR jsonb_typeof(receipt) = 'object'),
    confirmed_at timestamptz,
    PRIMARY KEY (operation_id, step),
    CHECK ((external_id IS NULL) = (confirmed_at IS NULL)),
    CHECK ((step = 'schedule_recurring' AND ((external_id IS NULL) = (receipt IS NULL))) OR (step <> 'schedule_recurring' AND receipt IS NULL))
);

CREATE FUNCTION invoice_renewal_step_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN RAISE EXCEPTION 'invoice renewal intent is immutable' USING ERRCODE='23514'; END IF;
    IF NEW.operation_id <> OLD.operation_id OR NEW.step <> OLD.step OR NEW.request IS DISTINCT FROM OLD.request OR NEW.dispatch_attempt <> OLD.dispatch_attempt THEN
        RAISE EXCEPTION 'invoice renewal intent is immutable' USING ERRCODE='23514';
    END IF;
    IF OLD.confirmed_at IS NOT NULL AND (NEW.confirmed_at IS DISTINCT FROM OLD.confirmed_at OR NEW.external_id IS DISTINCT FROM OLD.external_id OR NEW.receipt IS DISTINCT FROM OLD.receipt) THEN
        RAISE EXCEPTION 'invoice renewal confirmation is immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER invoice_renewal_step_immutable BEFORE UPDATE OR DELETE ON invoice_renewal_steps
    FOR EACH ROW EXECUTE FUNCTION invoice_renewal_step_immutable();

CREATE FUNCTION anchor_invoice_renewal_write_intent() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM operations WHERE id = NEW.operation_id AND kind = 'invoice_paid_renewal')
       OR NOT EXISTS (SELECT 1 FROM invoice_renewal_actions WHERE operation_id = NEW.operation_id AND outcome = 'pending' AND plan_frozen_at IS NOT NULL) THEN
        RAISE EXCEPTION 'invoice renewal intent owner mismatch' USING ERRCODE='23514';
    END IF;
    INSERT INTO external_write_intents(operation_id, step, origin, request_hash, dispatch_attempt)
        VALUES (NEW.operation_id, NEW.step, 'lifecycle', sha256(convert_to(NEW.request::text, 'UTF8')), NEW.dispatch_attempt);
    RETURN NEW;
END;
$$;
CREATE TRIGGER anchor_invoice_renewal_write_intent AFTER INSERT ON invoice_renewal_steps
    FOR EACH ROW EXECUTE FUNCTION anchor_invoice_renewal_write_intent();
