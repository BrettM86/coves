-- +goose Up
-- Classifier score alerts (docs/PRD_CONTENT_CLASSIFICATION.md R5): one row per
-- stored result and question whose probability met its CLASSIFIER_SCORE_ALERTS
-- threshold. The classification repository writes the rows in the same
-- statement as the result, so a discarded result leaves no alert. Deleting a
-- result, as author erasure does, deletes its alerts.
--
-- classification.ScoreAlertService sends each alert to the operators after
-- commit and its sweep retries failed sends: attempts counts failed sends,
-- next_attempt_at is when the alert is next due (NULL for a new alert, due at
-- once) and doubles as the claim lease while a send runs, and an alert at the
-- configured attempt limit stays undelivered.
CREATE TABLE classification_score_alerts (
    id BIGSERIAL PRIMARY KEY,
    result_id BIGINT NOT NULL REFERENCES classification_results(id) ON DELETE CASCADE,
    question_key TEXT NOT NULL,
    subject_kind TEXT NOT NULL CHECK (subject_kind IN ('post', 'comment', 'community')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    delivered_at TIMESTAMPTZ,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error_code TEXT CHECK (last_error_code IN ('send_failed')),
    next_attempt_at TIMESTAMPTZ,
    UNIQUE (result_id, question_key)
);

CREATE INDEX classification_score_alerts_due_idx
    ON classification_score_alerts (next_attempt_at NULLS FIRST, id)
    WHERE delivered_at IS NULL;

-- +goose Down
DROP TABLE classification_score_alerts;
