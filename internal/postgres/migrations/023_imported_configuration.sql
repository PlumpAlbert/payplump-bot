-- Protected configuration may be authorized from either the owned activation
-- journal or an immutable, verified active-import baseline. Existing requests
-- retain the owned mode through the default and are never backfilled.
ALTER TABLE verified_import_origins
    ADD CONSTRAINT verified_import_origins_client_bot_invoice_key
        UNIQUE (client_id, bot_id, current_invoice_id);

ALTER TABLE configuration_requests
    ALTER COLUMN activation_operation_id DROP NOT NULL,
    ADD COLUMN origin_kind text NOT NULL DEFAULT 'owned',
    ADD COLUMN imported_origin_client_id bigint,
    ADD COLUMN imported_admission_permission_revision bigint,
    ADD COLUMN imported_runtime_revision bigint;

ALTER TABLE configuration_requests
    DROP CONSTRAINT configuration_request_period_kind,
    DROP CONSTRAINT configuration_request_renewal_identity,
    ADD CONSTRAINT configuration_request_period_kind
        CHECK (period_kind IN ('first', 'renewal', 'imported_baseline')),
    ADD CONSTRAINT configuration_request_renewal_identity CHECK (
        (period_kind = 'first' AND renewal_operation_id IS NULL)
        OR (period_kind = 'renewal' AND renewal_operation_id IS NOT NULL AND renewal_operation_id > 0)
        OR (period_kind = 'imported_baseline' AND renewal_operation_id IS NULL)
    ),
    ADD CONSTRAINT configuration_request_imported_origin_fk
        FOREIGN KEY (imported_origin_client_id, bot_id, invoice_id)
        REFERENCES verified_import_origins(client_id, bot_id, current_invoice_id),
    ADD CONSTRAINT configuration_request_origin_kind_shape CHECK (
        (origin_kind = 'owned'
            AND activation_operation_id IS NOT NULL
            AND imported_origin_client_id IS NULL
            AND imported_admission_permission_revision IS NULL
            AND imported_runtime_revision IS NULL
            AND ((period_kind = 'first' AND renewal_operation_id IS NULL)
                OR (period_kind = 'renewal' AND renewal_operation_id IS NOT NULL AND renewal_operation_id > 0)))
        OR
        (origin_kind = 'imported'
            AND activation_operation_id IS NULL
            AND imported_origin_client_id IS NOT NULL
            AND imported_origin_client_id > 0
            AND imported_origin_client_id = client_id
            AND imported_admission_permission_revision IS NOT NULL
            AND imported_admission_permission_revision > 0
            AND imported_runtime_revision IS NOT NULL
            AND imported_runtime_revision > 0
            AND period_kind = 'imported_baseline'
            AND renewal_operation_id IS NULL)
    );

-- An active origin is eligible only while its original paid anchor is still
-- the account's current invoice. This guard prevents arbitrary paused-origin
-- use at request creation; readers independently revalidate current state.
CREATE FUNCTION configuration_imported_origin_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.origin_kind = 'imported' AND NOT EXISTS (
        SELECT 1 FROM verified_import_origins io
        WHERE io.client_id = NEW.imported_origin_client_id
          AND io.client_id = NEW.client_id
          AND io.bot_id = NEW.bot_id
          AND io.lifecycle = 'active'
          AND io.current_invoice_id = io.paid_anchor_invoice_id
          AND io.current_invoice_id = NEW.invoice_id
    ) THEN
        RAISE EXCEPTION 'imported configuration requires active paid baseline' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER configuration_imported_origin_guard BEFORE INSERT ON configuration_requests
    FOR EACH ROW EXECUTE FUNCTION configuration_imported_origin_guard();
