-- +goose Up
-- The reason a community gave when it removed a post, from its removal record.
-- It is held only while the removal stands: every transition out of 'removed'
-- clears it. The removal code stays in decision_code. Existing rows stay NULL.
ALTER TABLE community_post_admissions ADD COLUMN removal_reason TEXT;
ALTER TABLE community_post_admissions ADD CONSTRAINT chk_admission_removal_reason
    CHECK (removal_reason IS NULL OR status = 'removed');

-- +goose Down
ALTER TABLE community_post_admissions DROP CONSTRAINT chk_admission_removal_reason;
ALTER TABLE community_post_admissions DROP COLUMN removal_reason;
