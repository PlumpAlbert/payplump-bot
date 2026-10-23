CREATE TABLE imported_effect_permissions (
    bot_id bigint PRIMARY KEY REFERENCES telegram_bots(bot_id),
    revision bigint NOT NULL CHECK (revision > 0),
    policy_hash bytea NOT NULL CHECK (octet_length(policy_hash)=32),
    source_scope_hash bytea NOT NULL CHECK (octet_length(source_scope_hash)=32),
    enabled boolean NOT NULL DEFAULT false,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE FUNCTION imported_effect_permission_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.bot_id IS DISTINCT FROM OLD.bot_id THEN
   RAISE EXCEPTION 'imported effect permission bot identity is immutable' USING ERRCODE='23514';
 END IF;
 IF NEW.revision<OLD.revision OR (NEW.revision=OLD.revision AND
   (NEW.policy_hash<>OLD.policy_hash OR NEW.source_scope_hash<>OLD.source_scope_hash OR (NOT OLD.enabled AND NEW.enabled))) THEN
   RAISE EXCEPTION 'imported effect permission revision conflict' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER imported_effect_permission_revision BEFORE UPDATE ON imported_effect_permissions
 FOR EACH ROW EXECUTE FUNCTION imported_effect_permission_revision();
CREATE FUNCTION imported_effect_permission_no_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'imported effect permission cannot be deleted' USING ERRCODE='23514'; END $$;
CREATE FUNCTION imported_permission_bot_identity_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.bot_id IS DISTINCT FROM OLD.bot_id THEN
   RAISE EXCEPTION 'imported permission bot identity is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER import_admission_policy_bot_identity_immutable BEFORE UPDATE OF bot_id ON import_admission_policy
 FOR EACH ROW EXECUTE FUNCTION imported_permission_bot_identity_immutable();
CREATE TRIGGER imported_runtime_permission_bot_identity_immutable BEFORE UPDATE OF bot_id ON imported_runtime_permissions
 FOR EACH ROW EXECUTE FUNCTION imported_permission_bot_identity_immutable();
CREATE TRIGGER imported_effect_permission_no_delete BEFORE DELETE ON imported_effect_permissions
 FOR EACH ROW EXECUTE FUNCTION imported_effect_permission_no_delete();

ALTER TABLE invoice_overdue_actions
    ADD COLUMN imported_origin_client_id bigint REFERENCES verified_import_origins(client_id),
    ADD COLUMN imported_admission_permission_revision bigint,
    ADD COLUMN imported_runtime_revision bigint,
    ADD COLUMN imported_effect_revision bigint,
    ADD CONSTRAINT invoice_overdue_imported_origin_shape CHECK (
      (imported_origin_client_id IS NULL AND imported_admission_permission_revision IS NULL AND imported_runtime_revision IS NULL AND imported_effect_revision IS NULL)
      OR (imported_origin_client_id IS NOT NULL AND imported_origin_client_id = client_id AND imported_admission_permission_revision IS NOT NULL AND imported_admission_permission_revision > 0 AND imported_runtime_revision IS NOT NULL AND imported_runtime_revision > 0 AND imported_effect_revision IS NOT NULL AND imported_effect_revision > 0)
    );

CREATE OR REPLACE FUNCTION invoice_overdue_action_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'invoice overdue action is immutable' USING ERRCODE='23514';
    END IF;
    IF NEW.operation_id <> OLD.operation_id OR NEW.client_id <> OLD.client_id OR NEW.bot_id <> OLD.bot_id
       OR NEW.invoice_id <> OLD.invoice_id
       OR NEW.imported_origin_client_id IS DISTINCT FROM OLD.imported_origin_client_id
       OR NEW.imported_admission_permission_revision IS DISTINCT FROM OLD.imported_admission_permission_revision
       OR NEW.imported_runtime_revision IS DISTINCT FROM OLD.imported_runtime_revision
       OR NEW.imported_effect_revision IS DISTINCT FROM OLD.imported_effect_revision THEN
        RAISE EXCEPTION 'invoice overdue action identity is immutable' USING ERRCODE='23514';
    END IF;
    IF OLD.outcome <> 'pending' THEN
        RAISE EXCEPTION 'invoice overdue action cannot be rearmed' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
