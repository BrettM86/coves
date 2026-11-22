-- +goose Up
-- Completed image batches of a classification job
-- (docs/PRD_CONTENT_CLASSIFICATION.md R4). A subject's images go out at most
-- four to a request; each answered request stores its batch here, so a job
-- stopped by the daily cap, a failed request or the cycle deadline continues
-- at its first unfinished batch_index (0-based). The job's result is the
-- per-question maximum across its batches. Rows hold no content, only the
-- model, its probabilities and image counts; closing a job deletes them, and
-- deleting it cascades.
CREATE TABLE classification_job_batches (
    job_id BIGINT NOT NULL REFERENCES classification_jobs(id) ON DELETE CASCADE,
    batch_index INTEGER NOT NULL CHECK (batch_index >= 0),
    model_id TEXT NOT NULL,
    probabilities JSONB NOT NULL,
    images_sent INTEGER NOT NULL DEFAULT 0 CHECK (images_sent >= 0),
    images_skipped INTEGER NOT NULL DEFAULT 0 CHECK (images_skipped >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (job_id, batch_index)
);

-- +goose Down
DROP TABLE classification_job_batches;
