-- Per-bot private policy, frozen audience, and ordinary (at-least-once) outbox jobs.
CREATE TABLE broadcast_policies (
 bot_id bigint PRIMARY KEY REFERENCES telegram_bots(bot_id) ON DELETE CASCADE,
 revision bigint NOT NULL CHECK (revision>=0),
 policy_hash bytea NOT NULL CHECK (octet_length(policy_hash)=32),
 enabled boolean NOT NULL,
 max_recipients integer NOT NULL CHECK(max_recipients BETWEEN 0 AND 10000),
 admin_ids bigint[] NOT NULL DEFAULT '{}' CHECK(cardinality(admin_ids)<=1000),
 CHECK((enabled AND revision>0 AND max_recipients BETWEEN 1 AND 10000 AND cardinality(admin_ids) BETWEEN 1 AND 1000)
    OR (NOT enabled AND ((max_recipients=0 AND cardinality(admin_ids)=0) OR (max_recipients BETWEEN 1 AND 10000 AND cardinality(admin_ids) BETWEEN 1 AND 1000))))
);
CREATE TABLE broadcast_memberships (
 bot_id bigint NOT NULL REFERENCES telegram_bots(bot_id) ON DELETE CASCADE,
 client_id bigint NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
 actor_id bigint NOT NULL CHECK(actor_id>0), created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(bot_id,client_id), UNIQUE(bot_id,actor_id)
);
CREATE TABLE broadcast_drafts (
 id bigserial PRIMARY KEY, bot_id bigint NOT NULL REFERENCES telegram_bots(bot_id),
 actor_id bigint NOT NULL CHECK(actor_id>0), chat_id bigint NOT NULL CHECK(chat_id=actor_id),
 policy_revision bigint NOT NULL CHECK(policy_revision>0), policy_hash bytea NOT NULL CHECK(octet_length(policy_hash)=32),
 body text NOT NULL CHECK(octet_length(body) BETWEEN 1 AND 3000), token_hash bytea NOT NULL CHECK(octet_length(token_hash)=32),
 state text NOT NULL CHECK(state IN ('text','audience','confirmed','cancelled')),
 expires_at timestamptz NOT NULL, created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE(bot_id,token_hash), UNIQUE(id,bot_id,actor_id,policy_revision,policy_hash)
);
CREATE UNIQUE INDEX broadcast_one_active_draft ON broadcast_drafts(bot_id,actor_id) WHERE state IN ('text','audience');
CREATE TABLE broadcasts (
 id bigserial PRIMARY KEY, draft_id bigint NOT NULL UNIQUE, bot_id bigint NOT NULL REFERENCES telegram_bots(bot_id),
 creator_id bigint NOT NULL CHECK(creator_id>0), policy_revision bigint NOT NULL CHECK(policy_revision>0), policy_hash bytea NOT NULL CHECK(octet_length(policy_hash)=32),
 state text NOT NULL CHECK(state IN ('pending','done','cancelled')), summary_outbox_id bigint UNIQUE REFERENCES outbox(id),
 FOREIGN KEY(draft_id,bot_id,creator_id,policy_revision,policy_hash) REFERENCES broadcast_drafts(id,bot_id,actor_id,policy_revision,policy_hash),
 CHECK((state='done')=(summary_outbox_id IS NOT NULL))
);
CREATE TABLE broadcast_recipients (
 draft_id bigint NOT NULL REFERENCES broadcast_drafts(id), client_id bigint NOT NULL REFERENCES clients(id),
 actor_id bigint NOT NULL CHECK(actor_id>0), chat_id bigint NOT NULL CHECK(chat_id=actor_id),
 PRIMARY KEY(draft_id,client_id), UNIQUE(draft_id,chat_id)
);
CREATE TABLE broadcast_deliveries (
 job_id bigint NOT NULL REFERENCES broadcasts(id), client_id bigint NOT NULL REFERENCES clients(id),
 actor_id bigint NOT NULL CHECK(actor_id>0), chat_id bigint NOT NULL CHECK(chat_id=actor_id),
 status text NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','enqueued','skipped')), outbox_id bigint UNIQUE REFERENCES outbox(id),
 PRIMARY KEY(job_id,client_id), CHECK((status='enqueued')=(outbox_id IS NOT NULL))
);

CREATE FUNCTION protect_broadcast_freeze() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE draft_key bigint;
BEGIN
 IF TG_TABLE_NAME='broadcast_drafts' THEN
  IF TG_OP<>'UPDATE' THEN RAISE EXCEPTION 'immutable broadcast draft' USING ERRCODE='23514'; END IF;
  IF ROW(NEW.id,NEW.bot_id,NEW.actor_id,NEW.chat_id,NEW.body,NEW.policy_hash,NEW.policy_revision,NEW.created_at,NEW.expires_at)
    IS DISTINCT FROM ROW(OLD.id,OLD.bot_id,OLD.actor_id,OLD.chat_id,OLD.body,OLD.policy_hash,OLD.policy_revision,OLD.created_at,OLD.expires_at)
    THEN RAISE EXCEPTION 'immutable broadcast content' USING ERRCODE='23514'; END IF;
  IF NOT ((OLD.state='text' AND NEW.state IN ('text','audience','cancelled')) OR (OLD.state='audience' AND NEW.state IN ('confirmed','cancelled')) OR (OLD.state IN ('confirmed','cancelled') AND NEW.state=OLD.state))
    THEN RAISE EXCEPTION 'invalid broadcast transition' USING ERRCODE='23514'; END IF;
  IF NEW.token_hash IS DISTINCT FROM OLD.token_hash AND NOT (OLD.state='text' AND NEW.state='audience') THEN RAISE EXCEPTION 'immutable broadcast token' USING ERRCODE='23514'; END IF;
 ELSE
  IF TG_OP='UPDATE' AND OLD.draft_id IS DISTINCT FROM NEW.draft_id THEN RAISE EXCEPTION 'immutable broadcast audience' USING ERRCODE='23514'; END IF;
  draft_key:=CASE WHEN TG_OP='DELETE' THEN OLD.draft_id ELSE NEW.draft_id END;
  IF NOT EXISTS(SELECT 1 FROM broadcast_drafts WHERE id=draft_key AND state='text') THEN RAISE EXCEPTION 'immutable broadcast audience' USING ERRCODE='23514'; END IF;
  IF TG_OP='DELETE' THEN RETURN OLD; END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER broadcast_draft_immutable BEFORE UPDATE OR DELETE ON broadcast_drafts FOR EACH ROW EXECUTE FUNCTION protect_broadcast_freeze();
CREATE TRIGGER broadcast_recipient_immutable BEFORE INSERT OR UPDATE OR DELETE ON broadcast_recipients FOR EACH ROW EXECUTE FUNCTION protect_broadcast_freeze();

CREATE FUNCTION protect_broadcast_job() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'immutable broadcast job' USING ERRCODE='23514'; END IF;
 IF TG_OP='INSERT' THEN
  IF NEW.state<>'pending' OR NEW.summary_outbox_id IS NOT NULL OR NOT EXISTS(SELECT 1 FROM broadcast_drafts WHERE id=NEW.draft_id AND state='audience') THEN RAISE EXCEPTION 'invalid broadcast job' USING ERRCODE='23514'; END IF;
 ELSE
  IF ROW(NEW.id,NEW.draft_id,NEW.bot_id,NEW.creator_id,NEW.policy_revision,NEW.policy_hash) IS DISTINCT FROM ROW(OLD.id,OLD.draft_id,OLD.bot_id,OLD.creator_id,OLD.policy_revision,OLD.policy_hash)
   OR NOT ((OLD.state='pending' AND NEW.state IN ('pending','done','cancelled')) OR (OLD.state IN ('done','cancelled') AND NEW.state=OLD.state))
   OR (NEW.summary_outbox_id IS DISTINCT FROM OLD.summary_outbox_id AND NOT (OLD.state='pending' AND NEW.state='done' AND OLD.summary_outbox_id IS NULL AND NEW.summary_outbox_id IS NOT NULL))
   THEN RAISE EXCEPTION 'immutable broadcast job' USING ERRCODE='23514'; END IF;
 END IF;
 IF NEW.state='done' AND (
  EXISTS(SELECT 1 FROM broadcast_deliveries d LEFT JOIN outbox o ON o.id=d.outbox_id WHERE d.job_id=NEW.id AND (d.status='pending' OR (d.status='enqueued' AND o.status NOT IN ('sent','failed'))))
  OR NOT EXISTS(SELECT 1 FROM outbox o WHERE o.id=NEW.summary_outbox_id AND o.bot_id=NEW.bot_id AND o.chat_id=NEW.creator_id AND o.message_key='broadcast-summary:'||NEW.id::text AND NOT o.client_menu AND NOT o.protected_configuration))
  THEN RAISE EXCEPTION 'broadcast not terminal' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER broadcast_job_immutable BEFORE INSERT OR UPDATE OR DELETE ON broadcasts FOR EACH ROW EXECUTE FUNCTION protect_broadcast_job();

CREATE FUNCTION protect_broadcast_delivery() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'immutable broadcast delivery' USING ERRCODE='23514'; END IF;
 IF TG_OP='INSERT' THEN
  IF NEW.status<>'pending' OR NEW.outbox_id IS NOT NULL OR NOT EXISTS(SELECT 1 FROM broadcasts b JOIN broadcast_drafts d ON d.id=b.draft_id JOIN broadcast_recipients r ON r.draft_id=d.id WHERE b.id=NEW.job_id AND b.state='pending' AND d.state='audience' AND r.client_id=NEW.client_id AND r.actor_id=NEW.actor_id AND r.chat_id=NEW.chat_id) THEN RAISE EXCEPTION 'delivery outside frozen audience' USING ERRCODE='23514'; END IF;
 ELSE
  IF ROW(NEW.job_id,NEW.client_id,NEW.actor_id,NEW.chat_id) IS DISTINCT FROM ROW(OLD.job_id,OLD.client_id,OLD.actor_id,OLD.chat_id)
    OR NOT ((OLD.status='pending' AND NEW.status IN ('pending','enqueued','skipped')) OR (OLD.status IN ('enqueued','skipped') AND NEW.status=OLD.status AND NEW.outbox_id IS NOT DISTINCT FROM OLD.outbox_id))
    THEN RAISE EXCEPTION 'immutable broadcast delivery' USING ERRCODE='23514'; END IF;
 END IF;
 IF NEW.status='enqueued' AND NOT EXISTS(SELECT 1 FROM broadcasts b JOIN broadcast_drafts d ON d.id=b.draft_id JOIN outbox o ON o.id=NEW.outbox_id WHERE b.id=NEW.job_id AND o.bot_id=b.bot_id AND o.chat_id=NEW.chat_id AND o.message_key='broadcast:'||b.id::text||':'||NEW.client_id::text AND o.body=d.body AND NOT o.client_menu AND NOT o.protected_configuration)
  THEN RAISE EXCEPTION 'broadcast outbox mismatch' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER broadcast_delivery_immutable BEFORE INSERT OR UPDATE OR DELETE ON broadcast_deliveries FOR EACH ROW EXECUTE FUNCTION protect_broadcast_delivery();

-- A confirmed draft and a job have exactly the frozen recipients in the same commit.
CREATE FUNCTION check_broadcast_confirmation() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE draft_key bigint;
BEGIN
 IF TG_TABLE_NAME='broadcasts' THEN draft_key:=NEW.draft_id; ELSE draft_key:=NEW.id; END IF;
 IF NOT EXISTS(SELECT 1 FROM broadcast_drafts d JOIN broadcasts b ON b.draft_id=d.id WHERE d.id=draft_key AND d.state='confirmed')
  OR EXISTS(SELECT 1 FROM broadcast_recipients r JOIN broadcasts b ON b.draft_id=r.draft_id WHERE r.draft_id=draft_key AND NOT EXISTS(SELECT 1 FROM broadcast_deliveries x WHERE x.job_id=b.id AND x.client_id=r.client_id AND x.actor_id=r.actor_id AND x.chat_id=r.chat_id))
  THEN RAISE EXCEPTION 'incomplete broadcast confirmation' USING ERRCODE='23514'; END IF;
 RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER broadcast_confirmation_pair AFTER INSERT ON broadcasts DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_broadcast_confirmation();
CREATE CONSTRAINT TRIGGER broadcast_draft_confirmation_pair AFTER UPDATE ON broadcast_drafts DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.state='confirmed' AND OLD.state='audience') EXECUTE FUNCTION check_broadcast_confirmation();

-- Only provable own registration/menu activity establishes existing bot membership.
INSERT INTO broadcast_memberships(bot_id,client_id,actor_id)
SELECT b.bot_id,c.id,c.telegram_id FROM telegram_bots b CROSS JOIN clients c
WHERE c.telegram_id>0 AND c.chat_id=c.telegram_id AND (
 EXISTS(SELECT 1 FROM registration_flows r JOIN operations o ON o.id=r.operation_id WHERE r.bot_id=b.bot_id AND o.client_id=c.id AND o.kind='register_client' AND o.status='completed') OR
 EXISTS(SELECT 1 FROM outbox o WHERE o.bot_id=b.bot_id AND o.chat_id=c.telegram_id AND o.client_menu)
) ON CONFLICT DO NOTHING;
