-- +goose Up
CREATE TABLE discover_hot_snapshots (
    id BIGSERIAL PRIMARY KEY,
    algorithm_version SMALLINT NOT NULL,
    viewer_scope TEXT NOT NULL,
    ranking_time TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_discover_hot_snapshots_scope
    ON discover_hot_snapshots (algorithm_version, viewer_scope, created_at DESC);
CREATE INDEX idx_discover_hot_snapshots_expiry
    ON discover_hot_snapshots (expires_at);

CREATE TABLE discover_hot_build_attempts (
    id BIGSERIAL PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_discover_hot_build_attempts_created_at
    ON discover_hot_build_attempts (created_at);

CREATE TABLE discover_hot_candidates (
    snapshot_id BIGINT NOT NULL REFERENCES discover_hot_snapshots(id) ON DELETE CASCADE,
    uri TEXT NOT NULL,
    community_did TEXT NOT NULL,
    base_rank DOUBLE PRECISION NOT NULL
        CHECK (base_rank > '-Infinity'::DOUBLE PRECISION AND base_rank < 'Infinity'::DOUBLE PRECISION),
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (snapshot_id, uri)
);

CREATE INDEX idx_discover_hot_candidates_stream
    ON discover_hot_candidates (snapshot_id, community_did, base_rank DESC, created_at DESC, uri DESC);

CREATE TABLE discover_hot_checkpoints (
    id BIGSERIAL PRIMARY KEY,
    snapshot_id BIGINT NOT NULL REFERENCES discover_hot_snapshots(id) ON DELETE CASCADE,
    state_digest BYTEA NOT NULL CHECK (octet_length(state_digest) = 32),
    community_positions JSONB NOT NULL,
    recent_communities TEXT[] NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT ck_discover_hot_checkpoint_state_size CHECK (
        octet_length(community_positions::TEXT)
            + octet_length(array_to_json(recent_communities)::TEXT) <= 65536
    ),
    CONSTRAINT uq_discover_hot_checkpoint_state
        UNIQUE (snapshot_id, state_digest)
);

CREATE INDEX idx_discover_hot_checkpoints_snapshot
    ON discover_hot_checkpoints (snapshot_id);

-- +goose Down
DROP TABLE IF EXISTS discover_hot_checkpoints;
DROP TABLE IF EXISTS discover_hot_candidates;
DROP TABLE IF EXISTS discover_hot_build_attempts;
DROP TABLE IF EXISTS discover_hot_snapshots;
