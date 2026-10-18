-- Inert private staging only: no application identity, journal or access binding.
CREATE TABLE import_batches (
    plan_hash bytea PRIMARY KEY CHECK (octet_length(plan_hash)=32),
    total_records integer NOT NULL CHECK (total_records BETWEEN 0 AND 10000),
    candidate_count integer NOT NULL CHECK (candidate_count BETWEEN 0 AND total_records),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE import_candidates_staging (
    plan_hash bytea NOT NULL REFERENCES import_batches(plan_hash),
    source_index integer NOT NULL CHECK (source_index>=0),
    candidate_facts jsonb NOT NULL CHECK (jsonb_typeof(candidate_facts)='object' AND octet_length(candidate_facts::text)<=65536),
    PRIMARY KEY(plan_hash,source_index)
);
CREATE FUNCTION import_staging_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'import staging is immutable' USING ERRCODE='23514'; END;
$$;
CREATE TRIGGER import_batch_immutable BEFORE UPDATE OR DELETE ON import_batches
 FOR EACH ROW EXECUTE FUNCTION import_staging_immutable();
CREATE TRIGGER import_candidate_immutable BEFORE UPDATE OR DELETE ON import_candidates_staging
 FOR EACH ROW EXECUTE FUNCTION import_staging_immutable();

-- A sealed batch cannot gain extra candidates after its original commit.
CREATE FUNCTION check_import_candidate_insert() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE total integer; wanted integer; present integer;
BEGIN
 SELECT total_records,candidate_count INTO total,wanted FROM import_batches WHERE plan_hash=NEW.plan_hash FOR NO KEY UPDATE;
 IF NOT FOUND OR NEW.source_index>=total THEN RAISE EXCEPTION 'invalid staging index' USING ERRCODE='23514'; END IF;
 SELECT count(*) INTO present FROM import_candidates_staging WHERE plan_hash=NEW.plan_hash;
 IF present>=wanted THEN RAISE EXCEPTION 'import batch is sealed' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER import_candidate_insert BEFORE INSERT ON import_candidates_staging
 FOR EACH ROW EXECUTE FUNCTION check_import_candidate_insert();
CREATE FUNCTION check_import_batch_complete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF (SELECT count(*) FROM import_candidates_staging WHERE plan_hash=NEW.plan_hash)<>NEW.candidate_count
  THEN RAISE EXCEPTION 'incomplete import batch' USING ERRCODE='23514'; END IF;
 RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER import_batch_complete AFTER INSERT ON import_batches
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_import_batch_complete();
