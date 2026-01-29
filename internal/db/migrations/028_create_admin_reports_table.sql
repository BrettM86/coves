-- +goose Up
CREATE TABLE admin_reports (
    id BIGSERIAL PRIMARY KEY,
    reporter_did TEXT NOT NULL,
    target_uri TEXT NOT NULL,           -- AT-URI of post/comment
    target_type TEXT NOT NULL,          -- 'post' or 'comment'
    reason TEXT NOT NULL,               -- csam, doxing, harassment, spam, illegal, other
    explanation TEXT,                   -- optional details (max 1000 chars)
    status TEXT NOT NULL DEFAULT 'open',
    resolved_by TEXT,
    resolution_notes TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at TIMESTAMPTZ,

    CONSTRAINT valid_reason CHECK (reason IN ('csam', 'doxing', 'harassment', 'spam', 'illegal', 'other')),
    CONSTRAINT valid_status CHECK (status IN ('open', 'reviewing', 'resolved', 'dismissed'))
);

CREATE INDEX idx_admin_reports_status_created ON admin_reports(status, created_at DESC);
CREATE INDEX idx_admin_reports_target ON admin_reports(target_uri);

-- +goose Down
DROP TABLE IF EXISTS admin_reports;
