-- Authority is a fresh observation, not a payment confirmation. Old immutable
-- requests retain their origin and cannot be upgraded by a newer observation.
ALTER TABLE configuration_requests
 ALTER COLUMN invoice_id DROP NOT NULL,
 ALTER COLUMN proof_hash DROP NOT NULL,
 ADD COLUMN authority_revision bigint,
 ADD COLUMN authority_hash bytea,
 DROP CONSTRAINT configuration_request_period_kind,
 DROP CONSTRAINT configuration_request_renewal_identity,
 DROP CONSTRAINT configuration_request_origin_kind_shape;
ALTER TABLE configuration_requests
 ADD CONSTRAINT configuration_request_period_kind CHECK (period_kind IN ('first','renewal','imported_baseline','authority')),
 ADD CONSTRAINT configuration_request_renewal_identity CHECK (
 (period_kind IN ('first','imported_baseline','authority') AND renewal_operation_id IS NULL) OR
 (period_kind='renewal' AND renewal_operation_id IS NOT NULL AND renewal_operation_id>0)),
 ADD CONSTRAINT configuration_request_origin_kind_shape CHECK (
 (invoice_id IS NOT NULL AND proof_hash IS NOT NULL AND authority_revision IS NULL AND authority_hash IS NULL AND (

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
 )) OR
 (origin_kind='authority' AND period_kind='authority' AND activation_operation_id IS NULL AND renewal_operation_id IS NULL
 AND imported_origin_client_id IS NULL AND imported_admission_permission_revision IS NULL AND imported_runtime_revision IS NULL
 AND invoice_id IS NULL AND proof_hash IS NULL AND authority_revision IS NOT NULL AND authority_revision>0
 AND authority_hash IS NOT NULL AND octet_length(authority_hash)=32)
 );
