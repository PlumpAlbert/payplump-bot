-- Protected rows contain an intent marker only, never a personal URL.
ALTER TABLE clients ADD COLUMN configuration_revision bigint NOT NULL DEFAULT 0 CHECK (configuration_revision >= 0);
ALTER TABLE outbox ADD COLUMN protected_configuration boolean NOT NULL DEFAULT false;
ALTER TABLE outbox ADD CONSTRAINT protected_configuration_marker CHECK (
    NOT protected_configuration OR (body = '[protected_configuration_v1]' AND NOT client_menu));

CREATE TABLE configuration_policies (
    bot_id bigint PRIMARY KEY REFERENCES telegram_bots(bot_id),
    revision bigint NOT NULL CHECK (revision >= 0),
    hash bytea NOT NULL CHECK (octet_length(hash) = 32),
    enabled boolean NOT NULL,
    CHECK (NOT enabled OR (revision > 0 AND hash <> decode(repeat('00', 32), 'hex')))
);
CREATE TABLE configuration_requests (
    outbox_id bigint PRIMARY KEY REFERENCES outbox(id),
    bot_id bigint NOT NULL REFERENCES telegram_bots(bot_id),
    client_id bigint NOT NULL REFERENCES clients(id),
    actor_id bigint NOT NULL CHECK (actor_id > 0),
    chat_id bigint NOT NULL CHECK (chat_id = actor_id),
    activation_operation_id bigint NOT NULL REFERENCES activation_flows(operation_id),
    invoice_id text NOT NULL REFERENCES payment_confirmations(invoice_id),
    proof_hash bytea NOT NULL CHECK (octet_length(proof_hash) = 32),
    client_revision bigint NOT NULL CHECK (client_revision >= 0),
    policy_revision bigint NOT NULL CHECK (policy_revision > 0),
    policy_hash bytea NOT NULL CHECK (octet_length(policy_hash) = 32),
    entitlement_hash bytea NOT NULL CHECK (octet_length(entitlement_hash) = 32),
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    CHECK (expires_at = created_at + interval '5 minutes')
);
CREATE TABLE configuration_dispatches (
    outbox_id bigint PRIMARY KEY REFERENCES configuration_requests(outbox_id),
    attempt integer NOT NULL CHECK (attempt > 0),
    kind text NOT NULL CHECK (kind IN ('secret', 'denial')),
    snapshot_hash bytea CHECK (octet_length(snapshot_hash) = 32),
    not_after timestamptz NOT NULL,
    authorized_at timestamptz NOT NULL,
    telegram_message_id bigint CHECK (telegram_message_id > 0),
    outcome text NOT NULL DEFAULT 'reserved' CHECK (outcome IN ('reserved', 'sent', 'rejected', 'unknown')),
    error_code integer NOT NULL DEFAULT 0 CHECK (error_code >= 0),
    CHECK (kind <> 'secret' OR snapshot_hash IS NOT NULL),
    CHECK ((outcome = 'sent') = (telegram_message_id IS NOT NULL)),
    UNIQUE (outbox_id, attempt)
);
-- No deadline: an unknown original sender can still transmit after any lease.
CREATE TABLE configuration_chat_guards (
    bot_id bigint NOT NULL REFERENCES telegram_bots(bot_id),
    chat_id bigint NOT NULL CHECK (chat_id > 0),
    outbox_id bigint UNIQUE,
    attempt integer,
    PRIMARY KEY (bot_id, chat_id),
    CHECK ((outbox_id IS NULL) = (attempt IS NULL)),
    FOREIGN KEY (outbox_id, attempt) REFERENCES configuration_dispatches(outbox_id, attempt)
);

CREATE FUNCTION configuration_immutable_request() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'immutable configuration request' USING ERRCODE = '23514';
END;
$$;
CREATE TRIGGER configuration_request_immutable BEFORE UPDATE OR DELETE ON configuration_requests
    FOR EACH ROW EXECUTE FUNCTION configuration_immutable_request();

CREATE FUNCTION configuration_outbox_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.protected_configuration IS DISTINCT FROM NEW.protected_configuration OR
       (OLD.protected_configuration AND ROW(OLD.bot_id, OLD.chat_id, OLD.body, OLD.client_menu, OLD.message_key)
            IS DISTINCT FROM ROW(NEW.bot_id, NEW.chat_id, NEW.body, NEW.client_menu, NEW.message_key)) THEN
        RAISE EXCEPTION 'immutable configuration intent' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER configuration_outbox_immutable BEFORE UPDATE ON outbox
    FOR EACH ROW EXECUTE FUNCTION configuration_outbox_immutable();

CREATE FUNCTION configuration_request_pair() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE target bigint;
BEGIN
    IF TG_TABLE_NAME = 'outbox' THEN target := NEW.id; ELSE target := NEW.outbox_id; END IF;
    IF EXISTS (SELECT 1 FROM outbox WHERE id = target AND protected_configuration) THEN
        IF NOT EXISTS (SELECT 1 FROM configuration_requests r JOIN outbox o ON o.id = r.outbox_id
            WHERE r.outbox_id = target AND o.bot_id = r.bot_id AND o.chat_id = r.chat_id
                AND o.protected_configuration AND NOT o.client_menu AND o.body = '[protected_configuration_v1]') THEN
            RAISE EXCEPTION 'configuration request pair required' USING ERRCODE = '23514';
        END IF;
    ELSIF TG_TABLE_NAME = 'configuration_requests' THEN
        RAISE EXCEPTION 'configuration intent required' USING ERRCODE = '23514';
    END IF;
    RETURN NULL;
END;
$$;
CREATE CONSTRAINT TRIGGER configuration_outbox_pair AFTER INSERT ON outbox
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.protected_configuration)
    EXECUTE FUNCTION configuration_request_pair();
CREATE CONSTRAINT TRIGGER configuration_request_pair AFTER INSERT ON configuration_requests
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION configuration_request_pair();

-- Freeze local ABA changes without coupling authorization to polling counters.
CREATE FUNCTION configuration_client_identity_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(OLD.telegram_id, OLD.chat_id, OLD.invoice_ninja_id, OLD.sui_id)
        IS DISTINCT FROM ROW(NEW.telegram_id, NEW.chat_id, NEW.invoice_ninja_id, NEW.sui_id) THEN
        NEW.configuration_revision := OLD.configuration_revision + 1;
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER configuration_client_identity_revision BEFORE UPDATE OF telegram_id, chat_id, invoice_ninja_id, sui_id
    ON clients FOR EACH ROW EXECUTE FUNCTION configuration_client_identity_revision();

CREATE FUNCTION configuration_owner_revision() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE before_row jsonb; after_row jsonb; old_client bigint; new_client bigint; owner bigint;
BEGIN
    IF TG_OP <> 'INSERT' THEN before_row := to_jsonb(OLD); END IF;
    IF TG_OP <> 'DELETE' THEN after_row := to_jsonb(NEW); END IF;
    IF TG_TABLE_NAME = 'operations' THEN
        IF COALESCE(before_row->>'kind', '') NOT IN ('register_client', 'activate_first_subscription')
            AND COALESCE(after_row->>'kind', '') NOT IN ('register_client', 'activate_first_subscription') THEN
            RETURN NULL;
        END IF;
    END IF;
    -- Compare only explicitly authorization-relevant columns supplied by each trigger.
    IF TG_OP = 'UPDATE' AND
        (SELECT jsonb_object_agg(key, value) FROM jsonb_each(before_row) WHERE key = ANY(string_to_array(TG_ARGV[0], ',')))
        IS NOT DISTINCT FROM
        (SELECT jsonb_object_agg(key, value) FROM jsonb_each(after_row) WHERE key = ANY(string_to_array(TG_ARGV[0], ','))) THEN
        RETURN NULL;
    END IF;
    IF TG_TABLE_NAME IN ('registration_flows', 'operation_steps', 'activation_flows', 'activation_steps') THEN
        SELECT client_id INTO old_client FROM operations WHERE id = (before_row->>'operation_id')::bigint;
        SELECT client_id INTO new_client FROM operations WHERE id = (after_row->>'operation_id')::bigint;
    ELSE
        old_client := (before_row->>'client_id')::bigint;
        new_client := (after_row->>'client_id')::bigint;
    END IF;
    -- Missing parents are safe; changed owners are both fenced, in stable order.
    FOR owner IN SELECT DISTINCT id FROM unnest(ARRAY[old_client, new_client]) AS ids(id) WHERE id IS NOT NULL ORDER BY id LOOP
        UPDATE clients SET configuration_revision = configuration_revision + 1 WHERE id = owner;
    END LOOP;
    RETURN NULL;
END;
$$;
CREATE TRIGGER configuration_revision AFTER INSERT OR UPDATE OR DELETE ON subscriptions FOR EACH ROW
    EXECUTE FUNCTION configuration_owner_revision('client_id,state,desired_access,first_invoice_id,current_invoice_id,recurring_id,payment_date,next_invoice_date');
CREATE TRIGGER configuration_revision AFTER INSERT OR UPDATE OR DELETE ON payment_confirmations FOR EACH ROW
    EXECUTE FUNCTION configuration_owner_revision('invoice_id,client_id,bot_id,customer_id,invoice_number,amount,currency_id,payment_date,next_invoice_date,proof_hash,evidence,review_required,review_code');
CREATE TRIGGER configuration_revision AFTER INSERT OR UPDATE OR DELETE ON payment_checks FOR EACH ROW
    EXECUTE FUNCTION configuration_owner_revision('client_id,bot_id,review_code');
CREATE TRIGGER configuration_revision AFTER INSERT OR UPDATE OR DELETE ON activation_flows FOR EACH ROW
    EXECUTE FUNCTION configuration_owner_revision('operation_id,bot_id,token,invoice_id,proof_hash,policy_hash,review_code');
CREATE TRIGGER configuration_revision AFTER INSERT OR UPDATE OR DELETE ON activation_steps FOR EACH ROW
    EXECUTE FUNCTION configuration_owner_revision('operation_id,step,request,dispatch_attempt,external_id,recurring_amount,recurring_datetime,confirmed_at');
CREATE TRIGGER configuration_revision AFTER INSERT OR UPDATE OR DELETE ON activation_sui_bindings FOR EACH ROW
    EXECUTE FUNCTION configuration_owner_revision('client_id,bot_id,sui_id,name,credentials_hash,operation_id');
CREATE TRIGGER configuration_revision AFTER INSERT OR UPDATE OR DELETE ON operations FOR EACH ROW
    EXECUTE FUNCTION configuration_owner_revision('client_id,kind,status');
CREATE TRIGGER configuration_revision AFTER INSERT OR UPDATE OR DELETE ON registration_flows FOR EACH ROW
    EXECUTE FUNCTION configuration_owner_revision('operation_id,bot_id,token,review_code');
CREATE TRIGGER configuration_revision AFTER INSERT OR UPDATE OR DELETE ON operation_steps FOR EACH ROW
    EXECUTE FUNCTION configuration_owner_revision('operation_id,step,request,external_id,confirmed_at');
