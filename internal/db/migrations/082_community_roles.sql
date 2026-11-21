-- +goose Up
-- Index of social.coves.community.moderator records. A community grants an
-- account a role by writing the record in its own repo at a record key derived
-- from the account's DID, so there is at most one row per (community, subject).
-- A delete commit carries no record, so the revocation is resolved by
-- (community DID, record key). role is stored as written: an unknown value is
-- kept and grants nothing. There is no cap on rows per community.
CREATE TABLE community_roles (
    community_did TEXT NOT NULL,
    subject_did TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role <> ''),
    record_key TEXT NOT NULL,
    record_cid TEXT NOT NULL,
    record_created_at TIMESTAMPTZ NOT NULL,
    indexed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (community_did, subject_did),
    CONSTRAINT community_roles_record_key UNIQUE (community_did, record_key)
);

-- "What do I moderate?" reads by subject.
CREATE INDEX community_roles_subject_did ON community_roles (subject_did);

-- +goose Down
DROP TABLE IF EXISTS community_roles;
