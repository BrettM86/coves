-- +goose Up
-- Partial classification results (docs/PRD_CONTENT_CLASSIFICATION.md R4, R5).
-- A job whose request fails at the attempt limit after some of its image
-- batches were answered stores a result from those batches instead of
-- discarding them: the per-question maximum across the answered batches, with
-- the images never sent counted as skipped. partial marks such a result; its
-- job ends failed with its attempt history kept. A job revived by re-enqueue
-- that later completes replaces its partial result in place, and partial goes
-- back to false.
ALTER TABLE classification_results
    ADD COLUMN partial BOOLEAN NOT NULL DEFAULT FALSE;

-- +goose Down
ALTER TABLE classification_results DROP COLUMN partial;
