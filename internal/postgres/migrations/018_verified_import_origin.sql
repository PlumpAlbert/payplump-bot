-- Database-only permission and immutable provenance for freshly verified imports.
CREATE TABLE import_admission_policy (
    bot_id bigint PRIMARY KEY REFERENCES telegram_bots(bot_id),
    revision bigint NOT NULL CHECK (revision > 0),
    policy_hash bytea NOT NULL CHECK (octet_length(policy_hash)=32),
    source_scope_hash bytea NOT NULL CHECK (octet_length(source_scope_hash)=32),
    enabled boolean NOT NULL DEFAULT false,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE verified_import_origins (
    client_id bigint PRIMARY KEY REFERENCES clients(id),
    bot_id bigint NOT NULL REFERENCES telegram_bots(bot_id),
    configuration_revision_at_admission bigint NOT NULL CHECK (configuration_revision_at_admission>=0),
    admission_revision bigint NOT NULL CHECK (admission_revision>0),
    plan_hash bytea NOT NULL CHECK (octet_length(plan_hash)=32),
    source_index integer NOT NULL CHECK (source_index>=0),
    FOREIGN KEY (plan_hash,source_index) REFERENCES import_candidates_staging(plan_hash,source_index),
    policy_hash bytea NOT NULL CHECK (octet_length(policy_hash)=32),
    source_scope_hash bytea NOT NULL CHECK (octet_length(source_scope_hash)=32),
    stable_hash bytea NOT NULL CHECK (octet_length(stable_hash)=32),
    source_facts_hash bytea NOT NULL CHECK (octet_length(source_facts_hash)=32),
    evidence jsonb NOT NULL CHECK (jsonb_typeof(evidence)='object' AND octet_length(evidence::text)<=262144),
    customer_id text NOT NULL CHECK (octet_length(customer_id) BETWEEN 1 AND 256),
    recurring_id text NOT NULL CHECK (octet_length(recurring_id) BETWEEN 1 AND 256),
    current_invoice_id text NOT NULL CHECK (octet_length(current_invoice_id) BETWEEN 1 AND 256),
    paid_anchor_invoice_id text NOT NULL CHECK (octet_length(paid_anchor_invoice_id) BETWEEN 1 AND 256),
    sui_id bigint NOT NULL UNIQUE CHECK (sui_id>0),
    sui_name text NOT NULL UNIQUE CHECK (sui_name ~ '^[1-9][0-9]{0,18}$'),
    credentials_hash bytea NOT NULL CHECK (octet_length(credentials_hash)=32),
    lifecycle text NOT NULL CHECK (lifecycle IN ('active','paused')),
    historical_invoice_ids jsonb NOT NULL CHECK (jsonb_typeof(historical_invoice_ids)='array' AND octet_length(historical_invoice_ids::text)<=65536),
    invoice_watermark text NOT NULL,
    template jsonb NOT NULL CHECK (jsonb_typeof(template)='object' AND octet_length(template::text)<=65536),
    proof jsonb NOT NULL CHECK (jsonb_typeof(proof)='object' AND octet_length(proof::text)<=65536),
    amount numeric NOT NULL CHECK (amount>0 AND amount::text NOT IN ('NaN','Infinity','-Infinity')),
    currency text NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    payment_date date NOT NULL,
    next_invoice_date date NOT NULL,
    admitted_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE(plan_hash,source_index)
);

CREATE FUNCTION verified_import_policy_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.revision<OLD.revision OR (NEW.revision=OLD.revision AND
   (NEW.policy_hash<>OLD.policy_hash OR NEW.source_scope_hash<>OLD.source_scope_hash OR (NOT OLD.enabled AND NEW.enabled))) THEN
   RAISE EXCEPTION 'import policy revision conflict' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER verified_import_policy_revision BEFORE UPDATE ON import_admission_policy
 FOR EACH ROW EXECUTE FUNCTION verified_import_policy_revision();
CREATE FUNCTION verified_import_policy_no_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'import admission policy cannot be deleted' USING ERRCODE='23514'; END $$;
CREATE TRIGGER verified_import_policy_no_delete BEFORE DELETE ON import_admission_policy
 FOR EACH ROW EXECUTE FUNCTION verified_import_policy_no_delete();

CREATE FUNCTION verified_import_origin_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'verified import origin is immutable' USING ERRCODE='23514'; END $$;
CREATE TRIGGER verified_import_origin_immutable BEFORE UPDATE OR DELETE ON verified_import_origins
 FOR EACH ROW EXECUTE FUNCTION verified_import_origin_immutable();

-- Serialize imported SUI IDs against own activation binding inserts. The
-- advisory key is deterministic and both insertion paths use the same key.
CREATE FUNCTION imported_sui_lock_key(v bigint) RETURNS bigint LANGUAGE sql IMMUTABLE AS $$
 SELECT hashtextextended('payplump-sui-id:'||v::text, 0)
$$;
CREATE FUNCTION imported_origin_excludes_owned_sui() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 PERFORM pg_advisory_xact_lock(imported_sui_lock_key(NEW.sui_id));
 IF EXISTS(SELECT 1 FROM activation_sui_bindings WHERE sui_id=NEW.sui_id OR name=NEW.sui_name) THEN
  RAISE EXCEPTION 'SUI identity conflict' USING ERRCODE='23505';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER verified_import_origin_excludes_owned BEFORE INSERT ON verified_import_origins
 FOR EACH ROW EXECUTE FUNCTION imported_origin_excludes_owned_sui();
CREATE FUNCTION owned_sui_excludes_imported_origin() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 PERFORM pg_advisory_xact_lock(imported_sui_lock_key(NEW.sui_id));
 IF EXISTS(SELECT 1 FROM verified_import_origins WHERE sui_id=NEW.sui_id OR sui_name=NEW.name) THEN
  RAISE EXCEPTION 'SUI identity conflict' USING ERRCODE='23505';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER activation_sui_binding_excludes_import BEFORE INSERT OR UPDATE OF sui_id,name ON activation_sui_bindings
 FOR EACH ROW EXECUTE FUNCTION owned_sui_excludes_imported_origin();
