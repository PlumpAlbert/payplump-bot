-- Runtime adoption is a separate, default-off permission from DB admission.
CREATE TABLE imported_runtime_permissions (
    bot_id bigint PRIMARY KEY REFERENCES telegram_bots(bot_id),
    revision bigint NOT NULL CHECK (revision > 0),
    policy_hash bytea NOT NULL CHECK (octet_length(policy_hash)=32),
    source_scope_hash bytea NOT NULL CHECK (octet_length(source_scope_hash)=32),
    enabled boolean NOT NULL DEFAULT false,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE FUNCTION imported_runtime_permission_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.revision<OLD.revision OR (NEW.revision=OLD.revision AND
   (NEW.policy_hash<>OLD.policy_hash OR NEW.source_scope_hash<>OLD.source_scope_hash OR (NOT OLD.enabled AND NEW.enabled))) THEN
   RAISE EXCEPTION 'imported runtime permission revision conflict' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER imported_runtime_permission_revision BEFORE UPDATE ON imported_runtime_permissions
 FOR EACH ROW EXECUTE FUNCTION imported_runtime_permission_revision();
CREATE FUNCTION imported_runtime_permission_no_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'imported runtime permission cannot be deleted' USING ERRCODE='23514'; END $$;
CREATE TRIGGER imported_runtime_permission_no_delete BEFORE DELETE ON imported_runtime_permissions
 FOR EACH ROW EXECUTE FUNCTION imported_runtime_permission_no_delete();
