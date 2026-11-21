-- +goose Up
-- The community moderation version of each subject: a post URI, a comment URI,
-- a banned account DID, or, for the community's roster, the community's own
-- DID. Every indexed community record change for the subject (acceptance,
-- removal, ban and role records and their deletes) advances the subject's
-- version by one in the transaction that made it, so a moderation client can
-- tell whether the state it read is current. AppView-local post statuses
-- (pending, pending_reacceptance, rejected) and a ban reaching its expiry do
-- not advance it. A missing row reads as version 0.
-- There is no foreign key: the community delete removes its rows.
CREATE TABLE community_subject_versions (
    community_did TEXT NOT NULL,
    subject TEXT NOT NULL,
    version BIGINT NOT NULL CHECK (version > 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (community_did, subject)
);

-- +goose Down
DROP TABLE IF EXISTS community_subject_versions;
