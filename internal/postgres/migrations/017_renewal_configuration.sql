-- Renewal authorization is an explicit immutable request mode. Existing rows
-- remain first-cycle requests and cannot be rearmed or converted.
CREATE UNIQUE INDEX invoice_renewal_configuration_identity_idx
    ON invoice_renewal_actions(operation_id,client_id,bot_id,invoice_id);

ALTER TABLE configuration_requests
    ADD COLUMN period_kind text NOT NULL DEFAULT 'first',
    ADD COLUMN renewal_operation_id bigint;
ALTER TABLE configuration_requests
    ADD CONSTRAINT configuration_request_period_kind CHECK (period_kind IN ('first', 'renewal')),
    ADD CONSTRAINT configuration_request_renewal_identity CHECK (
        (period_kind = 'first' AND renewal_operation_id IS NULL) OR
        (period_kind = 'renewal' AND renewal_operation_id > 0)
    ),
    ADD CONSTRAINT configuration_request_renewal_fk FOREIGN KEY (renewal_operation_id)
        REFERENCES invoice_renewal_actions(operation_id),
    ADD CONSTRAINT configuration_request_renewal_owner_fk FOREIGN KEY (renewal_operation_id,client_id,bot_id,invoice_id)
        REFERENCES invoice_renewal_actions(operation_id,client_id,bot_id,invoice_id);

CREATE FUNCTION configuration_renewal_revision() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE old_client bigint; new_client bigint; owner bigint; before_row jsonb; after_row jsonb;
BEGIN
    IF TG_OP <> 'INSERT' THEN before_row := to_jsonb(OLD); END IF;
    IF TG_OP <> 'DELETE' THEN after_row := to_jsonb(NEW); END IF;
    IF TG_TABLE_NAME = 'operations' THEN
        IF COALESCE(before_row->>'kind','') <> 'invoice_paid_renewal' AND COALESCE(after_row->>'kind','') <> 'invoice_paid_renewal' THEN RETURN NULL; END IF;
        old_client := (before_row->>'client_id')::bigint;
        new_client := (after_row->>'client_id')::bigint;
        IF TG_OP = 'UPDATE' AND ROW(before_row->>'kind',before_row->>'status') IS NOT DISTINCT FROM ROW(after_row->>'kind',after_row->>'status') THEN RETURN NULL; END IF;
    ELSIF TG_TABLE_NAME = 'invoice_renewal_actions' THEN
        old_client := (before_row->>'client_id')::bigint;
        new_client := (after_row->>'client_id')::bigint;
        IF TG_OP = 'UPDATE' AND
           (SELECT jsonb_object_agg(key,value) FROM jsonb_each(before_row)
             WHERE key = ANY(ARRAY['outcome','review_code','plan_frozen_at','target_date','requires_pause','requires_schedule','requires_enable','requires_resume','baseline','recurring_amount','recurring_next_send_datetime']))
           IS NOT DISTINCT FROM
           (SELECT jsonb_object_agg(key,value) FROM jsonb_each(after_row)
             WHERE key = ANY(ARRAY['outcome','review_code','plan_frozen_at','target_date','requires_pause','requires_schedule','requires_enable','requires_resume','baseline','recurring_amount','recurring_next_send_datetime'])) THEN
            RETURN NULL;
        END IF;
    ELSE
        SELECT client_id INTO old_client FROM invoice_renewal_actions WHERE operation_id=(before_row->>'operation_id')::bigint;
        SELECT client_id INTO new_client FROM invoice_renewal_actions WHERE operation_id=(after_row->>'operation_id')::bigint;
        IF TG_OP = 'UPDATE' AND
           (SELECT jsonb_object_agg(key,value) FROM jsonb_each(before_row)
             WHERE key = ANY(ARRAY['step','request','dispatch_attempt','external_id','receipt','confirmed_at']))
           IS NOT DISTINCT FROM
           (SELECT jsonb_object_agg(key,value) FROM jsonb_each(after_row)
             WHERE key = ANY(ARRAY['step','request','dispatch_attempt','external_id','receipt','confirmed_at'])) THEN
            RETURN NULL;
        END IF;
    END IF;
    FOR owner IN SELECT DISTINCT id FROM unnest(ARRAY[old_client,new_client]) AS ids(id)
        WHERE id IS NOT NULL ORDER BY id LOOP
        UPDATE clients SET configuration_revision=configuration_revision+1 WHERE id=owner;
    END LOOP;
    RETURN NULL;
END;
$$;
CREATE TRIGGER configuration_renewal_operation_revision AFTER INSERT OR UPDATE OR DELETE ON operations
    FOR EACH ROW EXECUTE FUNCTION configuration_renewal_revision();
CREATE TRIGGER configuration_revision AFTER INSERT OR UPDATE OR DELETE ON invoice_renewal_actions
    FOR EACH ROW EXECUTE FUNCTION configuration_renewal_revision();
CREATE TRIGGER configuration_revision AFTER INSERT OR UPDATE OR DELETE ON invoice_renewal_steps
    FOR EACH ROW EXECUTE FUNCTION configuration_renewal_revision();

-- These current-cycle ledger fields participate in admission. Ignore polling
-- timestamps/counters while fencing status, proof association and financial facts.
CREATE TRIGGER configuration_revision AFTER INSERT OR UPDATE OR DELETE ON invoice_lifecycle_invoices
    FOR EACH ROW EXECUTE FUNCTION configuration_owner_revision('invoice_id,client_id,bot_id,customer_id,recurring_id,invoice_number,status,sent_date,amount,balance,paid_to_date,notice_state');
CREATE TRIGGER configuration_revision AFTER INSERT OR UPDATE OR DELETE ON invoice_lifecycle_checks
    FOR EACH ROW EXECUTE FUNCTION configuration_owner_revision('client_id,bot_id,review_code');
