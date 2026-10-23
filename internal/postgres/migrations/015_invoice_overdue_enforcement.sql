-- Stage 2 enforcement has its own durable saga, but every external write
-- anchors to the one shared activation_write_guard/epoch via external_write_intents.
CREATE TABLE invoice_overdue_actions (
    operation_id bigint PRIMARY KEY REFERENCES operations(id),
    client_id bigint NOT NULL REFERENCES clients(id),
    bot_id bigint NOT NULL REFERENCES telegram_bots(bot_id),
    invoice_id text NOT NULL,
    outcome text NOT NULL DEFAULT 'pending' CHECK (outcome IN ('pending', 'paid', 'completed', 'review')),
    review_code integer NOT NULL DEFAULT 0 CHECK (review_code >= 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    finished_at timestamptz,
    UNIQUE (client_id, invoice_id),
    FOREIGN KEY (client_id, invoice_id) REFERENCES invoice_lifecycle_invoices(client_id, invoice_id),
    CHECK ((outcome IN ('paid', 'completed', 'review')) = (finished_at IS NOT NULL)),
    CHECK ((outcome = 'review') = (review_code <> 0))
);
CREATE INDEX invoice_overdue_actions_pending_idx ON invoice_overdue_actions (bot_id, created_at, operation_id)
    WHERE outcome = 'pending';

CREATE FUNCTION invoice_overdue_action_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'invoice overdue action is immutable' USING ERRCODE='23514';
    END IF;
    IF NEW.operation_id <> OLD.operation_id OR NEW.client_id <> OLD.client_id OR NEW.bot_id <> OLD.bot_id
       OR NEW.invoice_id <> OLD.invoice_id THEN
        RAISE EXCEPTION 'invoice overdue action identity is immutable' USING ERRCODE='23514';
    END IF;
    IF OLD.outcome <> 'pending' THEN
        RAISE EXCEPTION 'invoice overdue action cannot be rearmed' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER invoice_overdue_action_immutable BEFORE UPDATE OR DELETE ON invoice_overdue_actions
    FOR EACH ROW EXECUTE FUNCTION invoice_overdue_action_immutable();

CREATE TABLE invoice_overdue_steps (
    operation_id bigint NOT NULL REFERENCES invoice_overdue_actions(operation_id),
    step text NOT NULL CHECK (step IN ('pause_recurring', 'disable_sui', 'close_sessions')),
    request jsonb NOT NULL CHECK (jsonb_typeof(request) = 'object' AND octet_length(request::text) <= 4194304),
    dispatch_attempt integer NOT NULL CHECK (dispatch_attempt > 0),
    external_id text,
    confirmed_at timestamptz,
    PRIMARY KEY (operation_id, step),
    CHECK ((external_id IS NULL) = (confirmed_at IS NULL))
);

CREATE FUNCTION invoice_overdue_step_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'invoice overdue write intent is immutable' USING ERRCODE='23514';
    END IF;
    IF NEW.operation_id <> OLD.operation_id OR NEW.step <> OLD.step
       OR NEW.request IS DISTINCT FROM OLD.request OR NEW.dispatch_attempt <> OLD.dispatch_attempt THEN
        RAISE EXCEPTION 'invoice overdue write intent is immutable' USING ERRCODE='23514';
    END IF;
    IF OLD.confirmed_at IS NOT NULL AND (NEW.confirmed_at IS DISTINCT FROM OLD.confirmed_at OR NEW.external_id IS DISTINCT FROM OLD.external_id) THEN
        RAISE EXCEPTION 'invoice overdue confirmation is immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER invoice_overdue_step_immutable BEFORE UPDATE OR DELETE ON invoice_overdue_steps
    FOR EACH ROW EXECUTE FUNCTION invoice_overdue_step_immutable();

CREATE FUNCTION anchor_invoice_overdue_write_intent() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM operations WHERE id = NEW.operation_id AND kind = 'invoice_overdue_enforcement'
    ) OR NOT EXISTS (
        SELECT 1 FROM invoice_overdue_actions WHERE operation_id = NEW.operation_id AND outcome = 'pending'
    ) THEN
        RAISE EXCEPTION 'invoice overdue intent owner mismatch' USING ERRCODE='23514';
    END IF;
    INSERT INTO external_write_intents(operation_id, step, origin, request_hash, dispatch_attempt)
        VALUES (NEW.operation_id, NEW.step, 'lifecycle', sha256(convert_to(NEW.request::text, 'UTF8')), NEW.dispatch_attempt);
    RETURN NEW;
END;
$$;
CREATE TRIGGER anchor_invoice_overdue_write_intent AFTER INSERT ON invoice_overdue_steps
    FOR EACH ROW EXECUTE FUNCTION anchor_invoice_overdue_write_intent();
