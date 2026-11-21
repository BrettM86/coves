-- +goose Up
-- One row per pending guarded delete of an erased account's role record from a
-- hosted community's repo. record_key is the record key derived from the
-- account's DID; the DID itself is not stored. record_cid is the CID the
-- erased index row held, sent as swapRecord so a newer record at the same key
-- is never deleted. A finished row is deleted, not kept. Enqueuing a key that
-- is already pending replaces its CID, resets its schedule and bumps
-- generation; id is the row identity, never reused, so a claim taken on an
-- earlier row cannot touch a later one. An attempt holds the row by its claim
-- number until lease_expires_at. There is no foreign key: the community delete
-- removes its rows.
CREATE TABLE community_role_record_deletes (
    id BIGSERIAL NOT NULL UNIQUE,
    community_did TEXT NOT NULL CHECK (community_did <> ''),
    record_key TEXT NOT NULL CHECK (record_key <> ''),
    record_cid TEXT NOT NULL CHECK (record_cid <> ''),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    lease_expires_at TIMESTAMPTZ,
    last_failure_code TEXT,
    generation BIGINT NOT NULL DEFAULT 1,
    claim BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (community_did, record_key)
);

CREATE INDEX community_role_record_deletes_due ON community_role_record_deletes (next_attempt_at);

-- One row per record of an open community write intent (a create or update)
-- whose subject was erased while the intent was open: paired is false for the
-- intent's record, true for a transfer's paired record. Whichever completion
-- settles the intent (the sweep, a retry or the firehose copy) treats that
-- record's subject as erased, even after the account signs up again and its
-- erasure marker is gone, so the record is never indexed and its repo delete
-- is queued instead. The DID itself is not stored. There is no foreign key: a
-- referencing insert would lock the intent row, which erasure must not wait
-- on; the community delete removes its rows.
CREATE TABLE community_write_intent_erasures (
    intent_id TEXT NOT NULL,
    paired BOOLEAN NOT NULL,
    PRIMARY KEY (intent_id, paired)
);

-- +goose Down
DROP TABLE IF EXISTS community_write_intent_erasures;
DROP TABLE IF EXISTS community_role_record_deletes;
