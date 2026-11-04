-- Complete inventory adoption is historical, not evidence of a new invoice.
-- Retain missing invoices: absence is neither deletion nor a reason to rearm.
CREATE TABLE authority_invoice_baselines (
 bot_id bigint NOT NULL REFERENCES telegram_bots(bot_id),
 client_id bigint NOT NULL REFERENCES clients(id),
 customer_id text NOT NULL CHECK(octet_length(customer_id) BETWEEN 1 AND 256),
 scope_hash text NOT NULL CHECK(octet_length(scope_hash)<=256),
 PRIMARY KEY(bot_id,client_id),
 UNIQUE(bot_id,customer_id),
 UNIQUE(bot_id,client_id,customer_id,scope_hash)
);
CREATE TABLE authority_invoice_ledger (
 bot_id bigint NOT NULL,
 invoice_id text NOT NULL CHECK(octet_length(invoice_id) BETWEEN 1 AND 256),
 client_id bigint NOT NULL,
 customer_id text NOT NULL,
 scope_hash text NOT NULL,
 snapshot jsonb NOT NULL CHECK(jsonb_typeof(snapshot)='object' AND octet_length(snapshot::text)<=8192),
 fingerprint bytea NOT NULL CHECK(octet_length(fingerprint)=32),
 generation bigint NOT NULL CHECK(generation>0),
 PRIMARY KEY(bot_id,invoice_id),
 FOREIGN KEY(bot_id,client_id,customer_id,scope_hash)
 REFERENCES authority_invoice_baselines(bot_id,client_id,customer_id,scope_hash)
);
