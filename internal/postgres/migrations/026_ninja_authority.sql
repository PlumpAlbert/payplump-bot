-- Observations convey source state, never payment evidence or write permission.
CREATE TABLE authority_observations (
 bot_id bigint NOT NULL REFERENCES telegram_bots(bot_id),
 client_id bigint NOT NULL REFERENCES clients(id),
 actor_id bigint NOT NULL CHECK(actor_id>0),
 observation jsonb NOT NULL CHECK(jsonb_typeof(observation)='object' AND octet_length(observation::text)<=262144),
 observation_hash bytea NOT NULL CHECK(octet_length(observation_hash)=32),
 source_hash bytea NOT NULL CHECK(octet_length(source_hash)=32),
 sui_id bigint CHECK(sui_id>0),
 sui_name text CHECK(octet_length(sui_name) BETWEEN 1 AND 256),
 credentials_hash bytea CHECK(octet_length(credentials_hash)=32),
 observed_at timestamptz NOT NULL,
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 PRIMARY KEY(bot_id,client_id), UNIQUE(bot_id,actor_id), UNIQUE(bot_id,sui_id), UNIQUE(bot_id,sui_name),
 CHECK((sui_id IS NULL) OR (sui_name IS NOT NULL AND credentials_hash IS NOT NULL))
);
CREATE TABLE authority_create_intents (
 bot_id bigint NOT NULL REFERENCES telegram_bots(bot_id),
 actor_id bigint NOT NULL CHECK(actor_id>0),
 draft jsonb NOT NULL CHECK(jsonb_typeof(draft)='object' AND octet_length(draft::text)<=4194304),
 source_hash bytea NOT NULL CHECK(octet_length(source_hash)=32),
 customer_id text NOT NULL CHECK(octet_length(customer_id) BETWEEN 1 AND 256),
 scope_hash text NOT NULL CHECK(octet_length(scope_hash)<=256),
 dispatched boolean NOT NULL DEFAULT false,
 completed boolean NOT NULL DEFAULT false,
 record_id bigint CHECK(record_id>0),
 PRIMARY KEY(bot_id,actor_id),
 CHECK(completed=(record_id IS NOT NULL))
);
CREATE TABLE authority_notices (
 bot_id bigint NOT NULL REFERENCES telegram_bots(bot_id),
 notice_key text NOT NULL CHECK(octet_length(notice_key) BETWEEN 1 AND 256),
 actor_id bigint NOT NULL CHECK(actor_id>=0),
 reason text NOT NULL CHECK(reason ~ '^[a-z][a-z0-9_]{0,63}$'),
 facts_hash text NOT NULL CHECK(octet_length(facts_hash) BETWEEN 1 AND 256),
 generation bigint NOT NULL CHECK(generation>0),
 resolved boolean NOT NULL DEFAULT false,
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(bot_id,notice_key)
);
