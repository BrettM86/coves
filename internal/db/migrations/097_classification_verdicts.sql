-- +goose Up
-- Admin verdicts on classification results (docs/PRD_CONTENT_CLASSIFICATION.md
-- R6, D18): for each question the result scored, whether the admin judged the
-- harm present. One verdict per result and admin; a re-record replaces it.
-- Deleting a result, as author erasure does, deletes its verdicts.
CREATE TABLE classification_verdicts (
    result_id BIGINT NOT NULL REFERENCES classification_results(id) ON DELETE CASCADE,
    admin_did TEXT NOT NULL,
    present JSONB NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (result_id, admin_did)
);

-- The admin result listing pages newest first by (created_at, id).
CREATE INDEX classification_results_created_idx ON classification_results (created_at DESC, id DESC);

-- +goose Down
DROP INDEX classification_results_created_idx;
DROP TABLE classification_verdicts;
