-- +goose Up
-- Signed com.atproto.label.defs#label rows this instance publishes. seq is the
-- queryLabels cursor and the order labels are served in. Rows are never pruned:
-- a negation stays alongside the label it negates.
CREATE TABLE labels (
    seq BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    src TEXT NOT NULL,
    uri TEXT NOT NULL,
    cid TEXT,
    val TEXT NOT NULL,
    neg BOOLEAN NOT NULL DEFAULT false,
    -- cts is the signed RFC 3339 string, stored as signed so the signature
    -- verifies byte for byte.
    cts TEXT NOT NULL,
    exp TEXT,
    sig BYTEA NOT NULL,
    ver BIGINT NOT NULL,
    action_id TEXT NOT NULL REFERENCES moderation_actions(id),
    -- signer_key is the public multibase of the key that produced sig.
    signer_key TEXT NOT NULL
);

-- queryLabels matches exact URIs and URI prefixes in seq order.
CREATE INDEX labels_uri_seq ON labels (uri text_pattern_ops, seq);

-- Every label signing key this labeler has used, and when it was retired.
CREATE TABLE label_signing_keys (
    public_multibase TEXT PRIMARY KEY,
    began_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    retired_at TIMESTAMPTZ
);

-- The single row recording that the one-time export of active decisions ran.
CREATE TABLE label_publication_state (
    singleton BOOLEAN PRIMARY KEY DEFAULT true CHECK (singleton),
    export_completed_at TIMESTAMPTZ
);

-- +goose Down
DROP TABLE IF EXISTS label_publication_state;
DROP TABLE IF EXISTS label_signing_keys;
DROP TABLE IF EXISTS labels;
