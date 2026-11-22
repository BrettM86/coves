-- +goose Up
-- Deferral backoff for classification jobs (docs/PRD_CONTENT_CLASSIFICATION.md
-- R2). deferral_count counts the deferrals in a row of a job whose subject
-- awaits admission; each one waits CLASSIFIER_PENDING_DEFERRAL doubled per
-- earlier deferral, up to six hours, and costs no attempt. Re-arming the job
-- when its post is accepted, and reviving a failed job by re-enqueue, reset it
-- to 0.
ALTER TABLE classification_jobs
    ADD COLUMN deferral_count INTEGER NOT NULL DEFAULT 0 CHECK (deferral_count >= 0);

-- +goose Down
ALTER TABLE classification_jobs DROP COLUMN deferral_count;
