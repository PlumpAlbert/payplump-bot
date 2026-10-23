-- Imported requests may address their current paid cycle without changing the
-- immutable origin invoice or the historical imported baseline request shape.
ALTER TABLE verified_import_origins
    ADD CONSTRAINT verified_import_origins_client_bot_key UNIQUE (client_id, bot_id);

ALTER TABLE configuration_requests
    ADD COLUMN imported_baseline_invoice_id text GENERATED ALWAYS AS
        (CASE WHEN origin_kind='imported' AND period_kind='imported_baseline' THEN invoice_id ELSE NULL END) STORED;

ALTER TABLE configuration_requests
    DROP CONSTRAINT configuration_request_imported_origin_fk,
    ADD CONSTRAINT configuration_request_imported_origin_fk
        FOREIGN KEY (imported_origin_client_id, bot_id)
        REFERENCES verified_import_origins(client_id, bot_id),
    ADD CONSTRAINT configuration_request_imported_baseline_fk
        FOREIGN KEY (imported_origin_client_id, bot_id, imported_baseline_invoice_id)
        REFERENCES verified_import_origins(client_id, bot_id, current_invoice_id);

ALTER TABLE configuration_requests
    DROP CONSTRAINT configuration_request_renewal_identity,
    ADD CONSTRAINT configuration_request_renewal_identity CHECK (
        (period_kind = 'first' AND renewal_operation_id IS NULL)
        OR (period_kind = 'renewal' AND renewal_operation_id IS NOT NULL AND renewal_operation_id > 0)
        OR (period_kind = 'imported_baseline' AND renewal_operation_id IS NULL)
    ),
    DROP CONSTRAINT configuration_request_origin_kind_shape,
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
            AND ((period_kind = 'imported_baseline' AND renewal_operation_id IS NULL)
                OR (period_kind = 'renewal' AND renewal_operation_id IS NOT NULL AND renewal_operation_id > 0)))
    );

CREATE OR REPLACE FUNCTION configuration_imported_origin_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.origin_kind = 'imported' AND NEW.period_kind = 'imported_baseline' AND NOT EXISTS (
        SELECT 1 FROM verified_import_origins io
        WHERE io.client_id = NEW.imported_origin_client_id
          AND io.client_id = NEW.client_id AND io.bot_id = NEW.bot_id
          AND io.lifecycle = 'active' AND io.current_invoice_id = io.paid_anchor_invoice_id
          AND io.current_invoice_id = NEW.invoice_id
    ) THEN
        RAISE EXCEPTION 'imported configuration requires active paid baseline' USING ERRCODE='23514';
    END IF;
    IF NEW.origin_kind = 'imported' AND NEW.period_kind = 'renewal' AND NOT EXISTS (
        SELECT 1 FROM invoice_renewal_actions a JOIN operations o ON o.id=a.operation_id
        WHERE a.operation_id=NEW.renewal_operation_id AND a.client_id=NEW.client_id AND a.bot_id=NEW.bot_id
          AND a.invoice_id=NEW.invoice_id AND a.imported_origin_client_id=NEW.imported_origin_client_id
          AND a.outcome='completed' AND a.review_code=0 AND o.kind='invoice_paid_renewal' AND o.status='completed'
          AND ((a.origin_mode='ledger' AND EXISTS (
                SELECT 1 FROM invoice_lifecycle_invoices li WHERE li.client_id=a.client_id AND li.bot_id=a.bot_id
                  AND li.invoice_id=a.invoice_id AND li.status='paid' AND li.notice_state='settled')
              AND EXISTS(SELECT 1 FROM verified_import_origins io WHERE io.client_id=a.imported_origin_client_id
                AND io.client_id=a.client_id AND io.bot_id=a.bot_id
                AND (io.lifecycle='active' OR (io.lifecycle='paused' AND EXISTS(
                  SELECT 1 FROM invoice_renewal_actions initial JOIN operations initial_op ON initial_op.id=initial.operation_id
                    WHERE initial.client_id=io.client_id AND initial.bot_id=io.bot_id AND initial.imported_origin_client_id=io.client_id
                    AND initial.origin_mode='imported_initial_paused' AND initial.outcome='completed' AND initial.review_code=0
                    AND initial_op.kind='invoice_paid_renewal' AND initial_op.status='completed'))))
            OR (a.origin_mode='imported_initial_paused' AND EXISTS (
                SELECT 1 FROM verified_import_origins io WHERE io.client_id=a.imported_origin_client_id
                  AND io.client_id=a.client_id AND io.bot_id=a.bot_id AND io.lifecycle='paused'
                  AND io.current_invoice_id=a.invoice_id AND io.paid_anchor_invoice_id<>io.current_invoice_id))))
    ) THEN
        RAISE EXCEPTION 'imported renewal requires completed imported renewal authority' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
