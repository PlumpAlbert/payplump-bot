-- No historical intent or receipt is reclassified by this migration. Empty,
-- unreviewed flows may transition only under the activation worker's locks.
ALTER TABLE activation_flows ADD COLUMN flow_mode text NOT NULL DEFAULT ''
    CHECK (flow_mode IN ('', 'ninja_authority'));
