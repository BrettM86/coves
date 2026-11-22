-- +goose Up
-- The content classification queue (docs/PRD_CONTENT_CLASSIFICATION.md).
--
-- classification_jobs holds one job per (subject URI, record CID, policy
-- version, text model, image model). The post consumer enqueues a job in the
-- indexing transaction; the classification worker claims due non-terminal jobs
-- (queued, deferred, retrying) whose next_attempt_at has passed, and ends each
-- one classified, closed (close_reason superseded or not_visible) or failed
-- (attempt limit reached; last_error_code names the last provider error). A
-- claim pushes next_attempt_at forward as a lease so an in-flight job is not
-- claimed twice. No foreign key to posts: a deleted or removed post keeps its
-- jobs and results; account erasure deletes them by author_did.
CREATE TABLE classification_jobs (
    id BIGSERIAL PRIMARY KEY,
    subject_uri TEXT NOT NULL,
    subject_kind TEXT NOT NULL CHECK (subject_kind IN ('post', 'comment', 'community')),
    record_cid TEXT NOT NULL,
    policy_version TEXT NOT NULL,
    text_model_id TEXT NOT NULL,
    image_model_id TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued', 'deferred', 'retrying', 'classified', 'closed', 'failed')),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    last_error_code TEXT,
    close_reason TEXT,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    author_did TEXT NOT NULL,
    community_did TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (subject_uri, record_cid, policy_version, text_model_id, image_model_id)
);

CREATE INDEX classification_jobs_due_idx
    ON classification_jobs (status, next_attempt_at)
    WHERE status IN ('queued', 'deferred', 'retrying');

CREATE INDEX classification_jobs_author_idx ON classification_jobs (author_did);

CREATE INDEX classification_jobs_subject_idx ON classification_jobs (subject_uri);

-- One classification result per classified job: the provider's probability
-- per policy question key. It holds no content (no title, body or sent text).
CREATE TABLE classification_results (
    id BIGSERIAL PRIMARY KEY,
    job_id BIGINT NOT NULL UNIQUE REFERENCES classification_jobs(id) ON DELETE CASCADE,
    subject_uri TEXT NOT NULL,
    subject_cid TEXT NOT NULL,
    subject_kind TEXT NOT NULL CHECK (subject_kind IN ('post', 'comment', 'community')),
    community_did TEXT NOT NULL,
    author_did TEXT NOT NULL,
    model_id TEXT NOT NULL,
    policy_version TEXT NOT NULL,
    probabilities JSONB NOT NULL,
    images_sent INTEGER NOT NULL DEFAULT 0 CHECK (images_sent >= 0),
    images_skipped INTEGER NOT NULL DEFAULT 0 CHECK (images_skipped >= 0),
    text_truncated BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX classification_results_author_idx ON classification_results (author_did);

CREATE INDEX classification_results_subject_idx ON classification_results (subject_uri);

-- Outbound classification requests per model per UTC day, against the daily
-- request cap. Every request counts, failed ones included; input_tokens adds
-- the provider's reported usage of successful requests.
CREATE TABLE classification_usage (
    model_id TEXT NOT NULL,
    usage_date DATE NOT NULL,
    requests INTEGER NOT NULL DEFAULT 0 CHECK (requests >= 0),
    input_tokens BIGINT NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    PRIMARY KEY (model_id, usage_date)
);

-- +goose Down
DROP TABLE classification_usage;
DROP TABLE classification_results;
DROP TABLE classification_jobs;
