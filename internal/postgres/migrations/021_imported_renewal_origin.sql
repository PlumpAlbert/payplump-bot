ALTER TABLE invoice_renewal_actions
    ADD COLUMN imported_origin_client_id bigint REFERENCES verified_import_origins(client_id),
    ADD COLUMN imported_admission_permission_revision bigint,
    ADD COLUMN imported_runtime_revision bigint,
    ADD COLUMN imported_effect_revision bigint,
    ADD CONSTRAINT invoice_renewal_imported_origin_shape CHECK (
      (imported_origin_client_id IS NULL AND imported_admission_permission_revision IS NULL AND imported_runtime_revision IS NULL AND imported_effect_revision IS NULL)
      OR (imported_origin_client_id IS NOT NULL AND imported_origin_client_id = client_id
        AND imported_admission_permission_revision IS NOT NULL AND imported_admission_permission_revision > 0
        AND imported_runtime_revision IS NOT NULL AND imported_runtime_revision > 0
        AND imported_effect_revision IS NOT NULL AND imported_effect_revision > 0)
    );

CREATE OR REPLACE FUNCTION invoice_renewal_action_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN RAISE EXCEPTION 'invoice renewal action is immutable' USING ERRCODE='23514'; END IF;
    IF NEW.operation_id <> OLD.operation_id OR NEW.client_id <> OLD.client_id OR NEW.bot_id <> OLD.bot_id OR NEW.invoice_id <> OLD.invoice_id
       OR NEW.imported_origin_client_id IS DISTINCT FROM OLD.imported_origin_client_id
       OR NEW.imported_admission_permission_revision IS DISTINCT FROM OLD.imported_admission_permission_revision
       OR NEW.imported_runtime_revision IS DISTINCT FROM OLD.imported_runtime_revision
       OR NEW.imported_effect_revision IS DISTINCT FROM OLD.imported_effect_revision THEN
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
